# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.
"""
Module for base class of Functional Nvsentinel testing
"""

import random
import re
import os
import subprocess
import yaml
import threading
import time
import pytest
import requests
from testcases.common.base import Base
from kubernetes import client
import string
from testcases.utils.kubernetes_utils import KubernetesClient
from kubernetes.client import CustomObjectsApi
import psutil


class TestNVSentinelCaseBase(Base):
    daemonset_name = ""
    node_name = ""
    MONGO_URI = "mongodb://CN%3Dmongo-user-client%2COU%3DDGXC%2CO%3DNvidia%2CL%3DSantaClara%2CST%3DCalifornia%2CC%3DUS@nvsentinel-mongodb-0.nvsentinel-mongodb-headless.nvsentinel.svc.cluster.local:27017/HealthEventsDatabase?authMechanism=MONGODB-X509&authSource=$external&tls=true"

    @pytest.fixture(autouse=True)
    def setup_runai_test(self):
        time.sleep(10)
        self.default_namespace = "runai-" + self.project
        self.nv_namespace = "nvsentinel"
        self.client = KubernetesClient()
        self.pod_logs = []
        self.debug_pod = None
        self.gpu_healthy_node = None
        self.gpu_healthy_pods = []
        self.node_name = None
        pods, _ = self.client.list_pods("nvsentinel", name_pattern="mongo-client-pod")
        if pods:  # clean up existing debug pod before testing
            self.client.delete_pod(pod=pods[-1])
        try:
            yield
        finally:
            # Equivalent to addCleanup in unittest
            if self.debug_pod:
                self.client.delete_pod(pod=self.debug_pod)
            if self.node_name:
                self.client.remove_annotation_on_node(
                    self.node_name, "quarantineHealthEventAppliedTaints"
                )
                self.client.remove_annotation_on_node(
                    self.node_name, "quarantineHealthEventIsCordoned"
                )
                self.client.remove_annotation_on_node(
                    self.node_name, "quarantineHealthEvent"
                )
                self.client.remove_taint_on_node(self.node_name, "AggregatedNodeHealth")
                self.client.remove_taint_on_node(self.node_name, "GPUError")
                self.client.remove_taint_on_node(self.node_name, "GPUError")
                self.client.remove_taint_on_node(self.node_name, "GPUError")
                self.client.uncordon_node(self.node_name)
                self.client.remove_node_condition(
                    self.node_name, "NvswitchErrorFromKmsgWatch"
                )

    def verify_health_monitor_info(self, conditions, expected_result, assert_on_fail=True):
        ret = True
        condition_info = []
        for condition in conditions:
            info = f"{condition.type}  {condition.reason}  {condition.message}"
            self.logger.info(f"Condition Info: {info}")
            condition_info.append(info)

        for title in expected_result:
            keyword = expected_result[title]
            find_match = any(
                re.search(keyword, str_condition, re.IGNORECASE)
                for str_condition in condition_info
            )
            ret = ret and find_match
            if assert_on_fail:
                assert find_match, f"{title} : {keyword} is not found"
            else:
                self.logger.info(f"{title} : {keyword} - Result Matched: {find_match}")
        return ret

    def follow_pod_logs(self, namespace, pod_name, container=None):
        self.logger.info(
            f"Starting to stream logs for pod {pod_name} in namespace {namespace}"
        )

        try:
            logs = self.client.coreV1Api.read_namespaced_pod_log(
                name=pod_name,
                namespace=namespace,
                container=container,
                follow=True,
                _preload_content=False,
            )
            for line in logs:
                message = line.decode("utf-8").strip()
                self.pod_logs.append(message)
        except client.ApiException as e:
            self.logger.info(
                f"Exception when calling CoreV1Api->read_namespaced_pod_log: {e}"
            )

    def get_physical_interfaces_of_node(self, node_name):
        """Parse network interface from command 'ip link show' with enhanced logging and validation.

        This function identifies physical network interfaces by:
        1. Using ip link show to get interface info
        2. CSP-specific pattern matching
        3. Logging all discovered interfaces for analysis
        4. Assert on unknown interfaces for manual verification

        Args:
            node_name (str): Name of the node to check

        Returns:
            list: List of physical interface names

        Raises:
            AssertionError: If unknown interfaces are found or no physical interfaces are found
        """
        command = [
            "/bin/sh",
            "-c",
            'chroot /host bash -c "ip link show"',
        ]
        debug_pod = self.create_debug_pod(node_name)
        try:
            all_interfaces_output, _ = self.client.exec_command_in_pod(debug_pod, command)

            # Log all discovered interfaces for analysis
            self.logger.info(f"\nAll interfaces found on node {node_name}:")
            for line in all_interfaces_output.splitlines():
                if re.match(r"^\d+:", line):
                    self.logger.info(line.strip())

            # Known virtual interface patterns to exclude
            exclude_patterns = [
                r"veth[a-f0-9]+",  # Virtual ethernet
                r"docker\d+",  # Docker
                r"cni\d+",  # CNI
                r"lo",  # Loopback
                r"bond\d+",  # Bonded
                r"dummy\d+",  # Dummy
                r"virbr\d+",  # Virtual bridge
                r"lxc[a-f0-9]+",  # LXC container
                r"vxlan\d+",  # VXLAN
                r"flannel\d+",  # Flannel
                r"cali[a-f0-9]+",  # Calico
                r"tunl\d+",  # Tunnel
                r"eni[a-f0-9]+@",  # AWS ENI with @
                r"br-[a-f0-9]+",  # Bridge
                r"cilium_\w+@\w+",  # Cilium interfaces
                r"lxc_\w+@if\d+",  # LXC interfaces
                r"\w+@if\d+",  # Any interface with @if suffix
                r"[^@]+@[^@]+",  # Any interface containing @
            ]

            # Known physical interface patterns
            physical_patterns = [
                r"^eth\d+$",  # Standard ethernet
                r"^enp\d+s\d+$",  # PCI ethernet
                r"^rdma\d+(?:v[01])?$",  # RDMA interfaces (包含 v0 和 v1 后缀，或无后缀)
            ]

            # Find all UP interfaces
            up_pattern = r"\d+: ([^:]+): <[^>]*UP[^>]*> mtu \d+ qdisc [^ ]+ state UP"
            all_up_interfaces = re.findall(up_pattern, all_interfaces_output)

            # Categorize interfaces
            physical_interfaces = []
            virtual_interfaces = []
            unknown_interfaces = []

            for iface in all_up_interfaces:
                # Check if it's a known virtual interface
                if any(re.match(p, iface) for p in exclude_patterns):
                    virtual_interfaces.append(iface)
                    continue

                # Check if it's a known physical interface
                if any(re.match(p, iface) for p in physical_patterns):
                    physical_interfaces.append(iface)
                    continue

                # If we get here, it's an unknown interface
                unknown_interfaces.append(iface)

            # Log interface categorization
            self.logger.info(f"\nInterface categorization for node {node_name}:")
            self.logger.info(f"Physical interfaces: {physical_interfaces}")
            self.logger.info(f"Virtual interfaces: {virtual_interfaces}")

            # If we found any unknown interfaces, raise an error
            if unknown_interfaces:
                self.logger.error(f"\nFound unknown interfaces on node {node_name}:")
                for iface in unknown_interfaces:
                    self.logger.error(f"  {iface}")
                raise AssertionError(
                    f"Found unknown interfaces on node {node_name}: {unknown_interfaces}\n"
                    "Please manually verify these interfaces and update the patterns accordingly."
                )

            # Verify we found at least one physical interface
            if not physical_interfaces:
                self.logger.error(f"No physical interfaces found on node {node_name}")
                self.logger.error("Full interface list:")
                self.logger.error(all_interfaces_output)
                raise AssertionError(
                    f"Cannot find physical interface on {node_name}, please check it manually"
                )

            return physical_interfaces

        finally:
            self.client.delete_pod(debug_pod)

    def up_interface_of_node(self, node_name, interface):
        """parse network interface from command 'ip addr show'"""
        self.logger.info(f"beginning to up {interface} on {node_name}")
        command = [
            "/bin/sh",
            "-c",
            f'chroot /host bash -c "ip link set {interface} up"',
        ]
        debug_pod = self.create_debug_pod(node_name)
        output, _ = self.client.exec_command_in_pod(debug_pod, command)
        time.sleep(5)
        command = [
            "/bin/sh",
            "-c",
            f'chroot /host bash -c "ip link show {interface}"',
        ]
        output, _ = self.client.exec_command_in_pod(debug_pod, command)
        self.logger.info(f"{output}")
        self.client.delete_pod(debug_pod)
        assert "mq state UP" in output

    def down_interface_of_node(self, node_name, interface):
        """parse network interface from command 'ip addr show'"""
        self.logger.info(f"beginning to down {interface} on {node_name}")
        command = [
            "/bin/sh",
            "-c",
            f'chroot /host bash -c "ip link set {interface} down"',
        ]
        debug_pod = self.create_debug_pod(node_name)
        output, _ = self.client.exec_command_in_pod(debug_pod, command)
        time.sleep(5)
        command = [
            "/bin/sh",
            "-c",
            f'chroot /host bash -c "ip link show {interface}"',
        ]
        output, _ = self.client.exec_command_in_pod(debug_pod, command)
        self.logger.info(f"{output}")
        self.client.delete_pod(debug_pod)
        assert "mq state DOWN" in output

    def down_up_interface_of_node_multi_times(self, node_name, interface, count=100):
        """Down and up the network interface on a node multiple times."""
        self.logger.info(
            f"Starting to down and up {interface} on {node_name} {count} times."
        )

        debug_pod = self.create_debug_pod(node_name)

        try:
            for i in range(count):
                self.logger.info(
                    f"Iteration {i + 1}/{count}: Downing {interface} on {node_name}"
                )
                # Down the interface
                command = [
                    "/bin/sh",
                    "-c",
                    f'chroot /host bash -c "ip link set {interface} down"',
                ]
                output, _ = self.client.exec_command_in_pod(debug_pod, command)
                time.sleep(5)
                command = [
                    "/bin/sh",
                    "-c",
                    f'chroot /host bash -c "ip link show {interface}"',
                ]
                output, _ = self.client.exec_command_in_pod(debug_pod, command)
                self.logger.info(f"Down output: {output}")
                assert "mq state DOWN" in output

                self.logger.info(
                    f"Iteration {i + 1}/{count}: Upping {interface} on {node_name}"
                )
                # Up the interface
                command = [
                    "/bin/sh",
                    "-c",
                    f'chroot /host bash -c "ip link set {interface} up"',
                ]
                output, _ = self.client.exec_command_in_pod(debug_pod, command)
                time.sleep(5)
                command = [
                    "/bin/sh",
                    "-c",
                    f'chroot /host bash -c "ip link show {interface}"',
                ]
                output, _ = self.client.exec_command_in_pod(debug_pod, command)
                self.logger.info(f"Up output: {output}")
                assert "mq state UP" in output

        finally:
            self.client.delete_pod(debug_pod)

        self.logger.info(f"Completed down and up {interface} on {node_name} {count} times.")

    def get_nodes_with_more_than_two_physical_interfaces(self):
        nodes, _ = self.client.get_nodes()
        node_names = [node.metadata.name for node in nodes]
        nodes_with_more_than_two_interfaces = {}
        for node in node_names:
            physical_interfaces = self.get_physical_interfaces_of_node(node)
            number = len(physical_interfaces)
            if number >= 2:
                nodes_with_more_than_two_interfaces[node] = physical_interfaces

        return nodes_with_more_than_two_interfaces

    def get_non_mgmt_ports_of_the_node(self, node_name):
        """get an non-mgmt ethernet interface of a node"""
        physical_interfaces = self.get_physical_interfaces_of_node(node_name)
        debug_pod = self.create_debug_pod(node_name)
        command = [
            "/bin/sh",
            "-c",
            'chroot /host bash -c "ip route get 8.8.8.8"',
        ]
        route_info, _ = self.client.exec_command_in_pod(debug_pod, command)
        match = re.search(r"dev (\w+)", route_info)
        assert match, "cannot find mgmt interface of the node"
        mgmt_interface = match.group(1)
        assert (
            mgmt_interface in physical_interfaces
        ), f"mgmt interface {mgmt_interface} is not in physical interfaces list:{physical_interfaces}. Pls check manually"
        self.logger.info(f"The mgmt port of the node {node_name} is {mgmt_interface}")
        non_mgmt_interfaces = [
            iface for iface in physical_interfaces if iface != mgmt_interface
        ]
        self.client.delete_pod(debug_pod)

        if non_mgmt_interfaces:
            self.logger.info(
                f"Got following non-mgmt ports on the node {node_name}: {non_mgmt_interfaces}"
            )
            non_mgmt_port = random.choice(non_mgmt_interfaces)
            self.logger.info(f"Pick up port {non_mgmt_port} on the node {node_name}")
            return non_mgmt_port
        return None

    def check_if_ib_interface(self, node_name):
        """Check if there is IB interface on the node"""
        self.logger.info(f"check if the node {node_name} has IB interface")
        command = [
            "/bin/sh",
            "-c",
            'chroot /host bash -c "ls /sys/class/infiniband"',
        ]
        debug_pod = self.create_debug_pod(node_name)
        output, _ = self.client.exec_command_in_pod(debug_pod, command)
        self.client.delete_pod(debug_pod)
        if not output or "No such file or directory" in output:
            return False
        return True

    def get_ib_interface_of_eth_interface(self, interface, node_name):
        """Get the IB interface of an ethernet interface"""
        command = [
            "/bin/sh",
            "-c",
            'chroot /host bash -c "ibdev2netdev"',
        ]
        debug_pod = self.create_debug_pod(node_name)
        output, _ = self.client.exec_command_in_pod(debug_pod, command)
        self.client.delete_pod(debug_pod)
        assert (
            "command not found" not in output
        ), "ibdev2netdev is not installed on the node, pls install it manually"
        lines = output.splitlines()
        for line in lines:
            parts = line.split("==>")
            if len(parts) == 2:
                ib_part, net_part = parts
                ib_part = ib_part.strip()
                net_part = net_part.strip()
                net_parts = net_part.split()
                if len(net_parts) == 2 and net_parts[0] == interface:
                    return ib_part.split()[0]
        return None

    def create_debug_pod(self, node_name, max_retries=5):
        yaml_file = os.path.join(os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "debug-pod.yaml")
        self.logger.info(f"yaml_file = {yaml_file}")
        with open(yaml_file, "r") as file:
            pod_body = yaml.safe_load(file)
            pod_body["spec"]["nodeName"] = node_name
        time.sleep(3)
        pods, _ = self.client.list_pods(
            pod_body["metadata"]["namespace"], name_pattern=pod_body["metadata"]["name"]
        )
        try:
            if pods:  # clean up existing debug pod before testing
                _, err_msg = self.client.delete_pod_by_name("debug-pod", "default")
                if err_msg:
                    self.logger.warning(
                        f"cannot delete debug pod with error message: {err_msg}"
                    )
        except Exception as error:
            self.logger.info(f"cannot delete debug pod with error {error}")

        retries = 0
        while retries < max_retries:
            try:
                debug_pod, _ = self.client.create_pod(pod_body=pod_body, wait=60)
                return debug_pod
            except AssertionError as e:
                self.logger.warning(f"Attempt {retries + 1} failed: {e}")
                retries += 1
                if retries >= max_retries:
                    raise Exception(
                        f"Failed to create debug pod after {max_retries} attempts"
                    )

        return None

    def verify_check_list(self, check_list, response):
        """
        Verify check list

        Args:
            check_list(list of string): the expected metrics list
            response(object): the response get from requests
        """
        result = True
        for check_item in check_list:
            if check_item in response.text:
                self.logger.info(f"Find {check_item}")
            else:
                self.logger.info(f"[FAIL] Cannot find {check_item}")
                result = False
        assert result, "[FAIL] NOT following metrics are included"
        self.logger.info("[PASS] All following metrics are included")

    def kubectl_port_forward_prometheus(self):
        """
        Kubectl port-forward prometheus 9090:9090
        - This is for new thread
        """
        # Check if a port-forward process is already running
        for proc in psutil.process_iter(['pid', 'name', 'cmdline']):
            if 'kubectl' in proc.info['name'] and 'port-forward' in proc.info['cmdline']:
                if 'service/prometheus-prometheus' in proc.info['cmdline'] and '-n' in proc.info['cmdline'] and 'prometheus' in proc.info['cmdline']:
                    print("Port forwarding is already running.")
                    return

        # If no existing process is found, start a new one
        try:
            result = subprocess.run(
                ["kubectl", "port-forward", "service/prometheus-prometheus", "-n", "prometheus", "9090:9090"],
                check=True,
                capture_output=True,
                text=True
            )
            print("Port forwarding established successfully.")
            print("Output:", result.stdout)
        except subprocess.CalledProcessError as e:
            print("Failed to establish port forwarding.")
            print("Error:", e.stderr)

    def start_prometheus_service(self):
        """
        Start prometheus service
        - This is a common test step
        """
        self.step_manager.print_header(
            "Get CRD podMonitor in nvsentinel, make sure it exists"
        )
        output_message, _ = self.client.get_crd(
            api_group="monitoring.coreos.com",
            api_version="v1",
            namespace="nvsentinel",
            resource_plural="podmonitors",
            resource_name="podmonitors"
        )

        self.logger.info(f"output_message = \n{output_message}")

        self.step_manager.print_header(
            "Get the svc cluster IP of prometheus-prometheus svc in prometheus namespace"
        )
        services, _ = self.client.list_services(
            namespace="prometheus",
            name_pattern="prometheus-prometheus"
        )
        self.logger.info(f"services = {services}")

        self.step_manager.print_header(
            "Download promtool in your local machine (Promtool is inside the prometheus tarballs)"
        )
        self.logger.info("skip this step for automation test")

        self.step_manager.print_header(
            "Untar the prometheus/promtool in your local machine"
        )
        self.logger.info("skip this step for automation test")

        self.step_manager.print_header(
            "Change into the prometheus folder and check promtool is working"
        )
        self.logger.info("skip this step for automation test")

        self.step_manager.print_header("Kubectl forward the prometheus svc")
        prometheus_thread = threading.Thread(
            target=self.kubectl_port_forward_prometheus, daemon=True
        )
        prometheus_thread.start()
        time.sleep(9)

    def query_metrics(
        self, query_params, url="http://localhost:9090/api/v1/query", fail_message=""
    ):
        """
        Query metrics

        Args:
            query_params(string): query params
            url(string): url for requests
            fail_message(string): message when requests fail

        Return:
            object: the response get from requests
                    we can use response.status_code / response.text / response.json() to read
        """
        self.logger.info(f"url = {url}")
        params = {"query": query_params}
        self.logger.info(f"params = {params}")
        response = requests.get(url, params=params)
        self.logger.info(f"response.status_code = {response.status_code}")
        if fail_message == "":
            fail_message = f"[FAIL] Cannot get {query_params}"
        assert response.status_code == 200, fail_message
        self.logger.info(f"response.json() = \n{response.json()}")
        return response

    def get_expected_value(self, query_params, value_before, expected_increase=1):
        """
        Get expected value
        - Using for-loop to check if the metrics value increase to our expected value

        Args:
            query_params(string): query params, the same for query_metrics()
            value_before(int): the value before test
            expected_increase(int): how many we expected the value increase

        Return:
            int: the latest value we from query metrics
        """
        self.logger.info(f"expected_increase = {expected_increase}")
        for _ in range(15):
            response = self.query_metrics(query_params=query_params)
            value = response.json()["data"]["result"][0]["value"]
            self.logger.info(f"[DEBUG] value = {value}")
            value_after = int(value[1])
            current_increase = value_after - value_before
            if current_increase >= expected_increase:
                break
            else:
                self.logger.info(f"current_increase = {current_increase}")
                time.sleep(10)
        return value_after

    def select_random_port_name(self, nic_info, exclude_nic):
        """select a port namae randomly"""

        # eth0 is not allowed to use
        exclude_nic.append("eth0")
        filtered_nics = {
            nic: info for nic, info in nic_info.items() if nic not in exclude_nic
        }
        sorted_nics = sorted(filtered_nics.keys())
        index = random.randint(0, len(sorted_nics) - 1)
        return sorted_nics[index]

    def parse_route(self, route_string):
        """parse route string from command 'ip route get 8.8.8.8' into a dictionary"""
        pattern = r"(\S+)\s+via\s+(\S+)\s+dev\s+(\S+)\s+src\s+(\S+)"
        match = re.search(pattern, route_string)
        route_info = None
        if match:
            route_info = {
                "destination": match.group(1),
                "gateway": match.group(2),
                "device": match.group(3),
                "source": match.group(4),
            }
        assert route_info, f"Route Info is not found: {route_string}"
        return route_info

    def get_fault_quarantine_pod(self):
        pods, _ = self.client.list_pods(
            "nvsentinel", name_pattern="nvsentinel-fault-quarantine*"
        )
        fault_quarantine_pod = pods[0]
        return fault_quarantine_pod

    def delete_fault_quarantine_pod(self):
        self.client.delete_pod(self.get_fault_quarantine_pod())
        time.sleep(10)

    def get_fault_quarantine_pod_log(self):
        fault_quarantine_pod = self.get_fault_quarantine_pod()
        logs, _ = self.client.get_pod_logs("nvsentinel", fault_quarantine_pod.metadata.name)
        return logs

    def get_node_drainer_pod(self):
        pods, _ = self.client.list_pods(
            "nvsentinel", name_pattern="nvsentinel-node-drainer*"
        )
        node_drainer_pod = pods[0]
        return node_drainer_pod

    def delete_node_drainer_pod(self):
        self.client.delete_pod(self.get_node_drainer_pod())
        time.sleep(10)

    def wait_for_node_drainer_pod_to_start(self):
        timeout = 300   # some times it takes longer than 120 seconds to start the pod
        start_time = time.time()
        while True:
            node_drainer_pod = self.get_node_drainer_pod()
            if node_drainer_pod.status.phase == "Running":
                break
            if time.time() - start_time > timeout:
                raise Exception("Timeout waiting for node drainer pod to start")
            time.sleep(10)
            self.logger.info(f"Node drainer pod {node_drainer_pod.metadata.name} is not running, waiting for it to start")
        self.logger.info(f"Node drainer pod {node_drainer_pod.metadata.name} is running")

    def get_node_drainer_pod_log(self):
        node_drainer_pod = self.get_node_drainer_pod()
        logs, _ = self.client.get_pod_logs(
            "nvsentinel", node_drainer_pod.metadata.name
        )
        return logs

    def verify_node_drainer_pod_log(self, expected_messages, timeout=60):
        # Create a while loop to check fault_logs every 5 seconds with a timeout
        start_time = time.time()
        found_all_messages = False
        while time.time() - start_time < timeout:
            node_drainer_logs = self.get_node_drainer_pod_log()
            self.logger.debug(f"Checking node_drainer logs...{node_drainer_logs}")
            # Check for both expected log messages
            print(node_drainer_logs, expected_messages)
            if all(message in node_drainer_logs for message in expected_messages):
                found_all_messages = True
                self.logger.info("Found all expected log messages")
            # If both conditions are met, break out of the loop
            if found_all_messages:
                break
            # Wait for 5 seconds before checking again
            self.logger.info("Waiting 5 seconds before checking logs again...")
            time.sleep(5)
        # Assert after the loop
        assert (
            found_all_messages
        ), f"The log does not contain the message of {expected_messages}"

    def get_error_details_info(self):
        pattern = re.compile(
            r"Handling event: version:(?P<version>\d+) +agent:\"(?P<agent>[^\"]+)\" +componentClass:\"(?P<componentClass>[^\"]+)\" +checkName:\"(?P<checkName>[^\"]+)\".*?entitiesImpacted:\{entityType:\"(?P<entityType>[^\"]+)\" +entityValue:\"(?P<entityValue>[^\"]+)\"\}",
            re.DOTALL,
        )
        log_content = self.get_fault_quarantine_pod_log()
        matches = pattern.findall(str(log_content))
        if matches:
            version, agent, componentClass, checkName, entityType, entityValue = matches[0]
            if all([version, agent, componentClass, checkName, entityType, entityValue]):
                return (
                    version,
                    agent,
                    componentClass,
                    checkName,
                    entityType,
                    entityValue,
                )
        return None

    def clear_gpu_fatal_error(self, node_name, condition):
        self.client.remove_annotation_on_node(
            node_name, "quarantineHealthEventAppliedTaints"
        )
        self.client.remove_annotation_on_node(node_name, "quarantineHealthEventIsCordoned")
        self.client.remove_annotation_on_node(node_name, "quarantineHealthEvent")
        self.client.remove_taint_on_node(node_name, "AggregatedNodeHealth")
        self.client.remove_taint_on_node(node_name, "GPUHealth")
        self.client.uncordon_node(node_name)
        self.client.remove_node_condition(node_name, condition)

    def clear_nvswitch_error(self, node_name):
        # get the clear command
        sh_file = os.path.join(
            os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "get-nvswitch-error-clear-cmd.sh"
        )
        ## run this from subprocess python standard library
        subprocess.run(["chmod", "+x", sh_file])
        clear_cmd = subprocess.run([sh_file, node_name], capture_output=True, text=True).stdout

        self.logger.info(f"clear_cmd = {clear_cmd}")
        print(f"clear_cmd = {clear_cmd}")
        yaml_file = os.path.join(
            os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "mangodb-client.yaml"
        )
        self.logger.info(f"yaml_file = {yaml_file}")
        with open(yaml_file, "r") as file:
            pod_body = yaml.safe_load(file)
        pods, _ = self.client.list_pods(
            pod_body["metadata"]["namespace"], name_pattern=pod_body["metadata"]["name"]
        )
        if pods:  # clean up existing debug pod before testing
            self.client.delete_pod(pod=pods[-1])
        mango_pod, _ = self.client.create_pod(pod_body=pod_body, wait=60)

        # write mongo_script.js to the mango-client pod
        cert_command = ["/bin/sh", "-c", "cat /certs/tls.crt /certs/tls.key > /tmp/tls.pem"]
        output, _ = self.client.exec_command_in_pod(mango_pod, cert_command)

        with open("/tmp/mongo_script.js", "w") as file:
            file.write(clear_cmd)
        copy_command = [
            "kubectl",
            "cp",
            "/tmp/mongo_script.js",
            f"{self.nv_namespace}/mongo-client-pod:/tmp",
        ]
        subprocess.run(copy_command)
        print(
            f"Copied /tmp/modified_mongo_script.js to {self.nv_namespace}/mongo-client-pod:/tmp/mongo_script.js in the pod."
        )
        mongo_command = [
            "mongosh",
            MONGO_URI,
            "--tls",
            "--tlsAllowInvalidCertificates",
            "--tlsCertificateKeyFile",
            "/tmp/tls.pem",
            "--file",
            "/tmp/mongo_script.js",
            "--verbose",
        ]
        output, _ = self.client.exec_command_in_pod(mango_pod, mongo_command)
        self.logger.info(output)

    def clear_all_pod_gpu_fatal_error(self):
        for healthy_pod in self.gpu_healthy_pods:
            self.clear_gpu_fatal_error(self.gpu_healthy_node, healthy_pod)

    def get_node_network_interfaces(self, node_name):
        """Get network interfaces on a node using ls /sys/class/net

        Args:
            node_name (str): Name of the node

        Returns:
            list: List of network interface names
        """
        command = ["/bin/sh", "-c", 'chroot /host bash -c "ls /sys/class/net"']
        debug_pod = self.create_debug_pod(node_name)
        interfaces_output, _ = self.client.exec_command_in_pod(debug_pod, command)

        # Split output into list of interfaces
        interfaces = interfaces_output.strip().split()
        assert (
            interfaces
        ), f"Cannot find network interfaces on {node_name}, please check manually"

        self.logger.info(f"Found network interfaces: {interfaces} on node {node_name}")
        self.client.delete_pod(debug_pod)
        return interfaces

    def ethernet_link_down_test(self, request, node_name, non_mgmt_interface):
        """Run ethernet link down test

        Args:
            request: pytest request fixture
            node_name: Name of the node to test
            non_mgmt_interface: Name of the network interface to test
        """
        try:
            self.logger.info(f"Down the port {non_mgmt_interface} on node {node_name}")
            self.down_interface_of_node(node_name, non_mgmt_interface)

            self.logger.info("Check error info From the pod log console")
            message_to_check = [
                'checkName:"EthernetErrorCheck"',
                'agent:"nic-health-monitor"',
                'componentClass:"NIC"',
                "state: down",
                f'entityType:"NIC"\\s+entityValue:"{non_mgmt_interface}"',
            ]
            find_match = all(
                re.search(message, self.pod_logs[-1]) for message in message_to_check
            )
            assert find_match, f"Find no expected message in console log:{self.pod_logs}"
            self.logger.debug(
                "SUCCESS: 'state: down' message is show when port up in pod console log"
            )

            self.logger.info("EthernetErrorCheck will change to True in node condition.")
            target_condition, _ = self.client.read_node_condition_by_type(
                node_name=node_name, condition_type="EthernetErrorCheck"
            )
            assert (
                target_condition.status == "True"
            ), f"Status of EthernetErrorCheck is still False: {target_condition}"
            self.logger.debug(
                "SUCCESS: EthernetErrorCheck status is flip to True when port up in node"
            )
        except:  # ensure the nic port has been turn up if any error condition occurred
            self.up_interface_of_node(node_name, non_mgmt_interface)
            raise

        self.logger.info(f"Up the port {non_mgmt_interface} on node {node_name}")
        self.up_interface_of_node(node_name, non_mgmt_interface)

        self.logger.info("Check The log when port up")
        message_to_check = [
            "EthernetErrorCheck",
            "Device is healthy",
            f'entityValue:"{non_mgmt_interface}"',
        ]
        find_match = all(
            message for message in message_to_check if message in self.pod_logs[-1]
        )
        assert find_match, f"Find no expected message in console log:{self.pod_logs}"
        self.logger.debug(
            "SUCCESS: 'Device is healthy' message is show when port up in pod console log"
        )

        self.logger.info("EthernetErrorCheck will change to False in node condition.")
        target_condition, _ = self.client.read_node_condition_by_type(
            node_name=node_name, condition_type="EthernetErrorCheck"
        )
        assert (
            target_condition.status == "False"
        ), f"Status of EthernetErrorCheck is still True after turn NIC port up: {target_condition}"
        self.logger.debug(
            "SUCCESS: EthernetErrorCheck status is flip back to False when port up in node"
        )

    def get_platform_connector_by_node_name(self, node_name):
        pods, _ = self.client.list_pods(
            "nvsentinel", name_pattern="nvsentinel-platform-connector*"
        )
        for pod in pods:
            if pod.spec.node_name == node_name:
                return pod
        return None

    def get_available_gpu_monitor_daemonset(self):
        self.logger.info("Entering get_available_gpu_monitor_daemonset")
        self.logger.info(f"self.client: {self.client}")
        self.logger.info(f"self.nv_namespace: {self.nv_namespace}")
        daemonsets, _ = self.client.list_daemonset(namespace=self.nv_namespace)
        for daemonset in daemonsets:
            daemonset_name = daemonset.metadata.name
            if "nvsentinel-gpu-health-monitor-dcgm" in daemonset_name:
                if daemonset.status.number_available:
                    self.logger.info(
                        f"Found available GPU health monitor daemonset: {daemonset_name}"
                    )
                    return daemonset_name
        return None

    def get_count_of_node_events(self, node_name, event_reason):
        events, _ = self.client.get_node_events(node_name=node_name)
        print(events)
        for event in events:
            if event.reason == event_reason:
                error_count = event.count
                print(event.count)
                self.logger.info(f"{event_reason} count: {error_count}")
                return error_count
        return 0

    def load_yaml(self, f, context):
        def string_constructor(loader, node):
            t = string.Template(node.value)
            value = t.substitute(context)
            return value

        safe_loader = yaml.SafeLoader
        safe_loader.add_constructor("tag:yaml.org,2002:str", string_constructor)

        token_re = string.Template.pattern
        safe_loader.add_implicit_resolver("tag:yaml.org,2002:str", token_re, None)

        x = yaml.load(f, Loader=safe_loader)
        return x

    def get_node_condition_by_type(self, node_name, condition_type, timeout=60):
        check_interval = 2
        start_time = time.time()
        conditions_found = False
        while (time.time() - start_time) < timeout:
            node_info, _ = self.client.get_node_by_name(
                node_name=node_name, node_type="gpu"
            )
            assert node_info is not None, "Find no node info by node name"
            conditions = [
                condition
                for condition in node_info.status.conditions
                if condition.reason == condition_type
            ]
            if conditions:
                conditions_found = True
                break
            self.logger.info(
                f"Waiting for {condition_type} condition on node {node_name}, checking again in {check_interval} seconds..."
            )
            time.sleep(check_interval)
        return conditions, conditions_found

    def verify_gpu_inforom_watch_condition(self, node_name):
        node_info, _ = self.client.get_node_by_name(node_name)
        expected_result = {
            "Condition Type": "GpuInforomWatch",
            "Condition Reason": "GpuInforomWatchIsNotHealthy",
            "Condition Message": "ErrorCode:DCGM_FR_CORRUPT_INFOROM GPU:0 A corrupt InfoROM has been detected in GPU 0. Flash the InfoROM to clear this corruption. Recommended Action=COMPONENT_RESET;",
        }
        self.verify_health_monitor_info(conditions=node_info.status.conditions, expected_result=expected_result)

    def inject_gpu_inforom_watch_error(self, pod):
        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 84 -v 0",
        ]
        output, _ = self.client.exec_command_in_pod(pod, command)
        assert "Successfully injected" in output
    
    def clear_gpu_inforom_watch_error(self, pod):
        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 84 -v 1",
        ]
        output, _ = self.client.exec_command_in_pod(pod, command)
        assert "Successfully injected" in output