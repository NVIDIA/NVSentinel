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
import string
import os
import subprocess
import yaml
import threading
import time
import pytest
import requests
import psutil
import signal
from testcases.common.base import Base
from kubernetes import client
from testcases.utils.kubernetes_utils import KubernetesClient
from datetime import datetime, timezone
import tempfile


class TestNVSentinelCaseBase(Base):
    daemonset_name = ""
    node_name = ""
    MONGO_URI = "mongodb://CN%3Dmongo-user-client%2COU%3DDGXC%2CO%3DNvidia%2CL%3DSantaClara%2CST%3DCalifornia%2CC%3DUS@nvsentinel-mongodb-0.nvsentinel-mongodb-headless.nvsentinel.svc.cluster.local:27017/HealthEventsDatabase?authMechanism=MONGODB-X509&authSource=$external&tls=true"
    nmc_context = os.getenv("NMC_KUBE_CONTEXT")
    if nmc_context:
        nv_namespace = "dgxc-system"
        gpu_operator_namespace = "dgxc-system"
        prometheus_namespace = "dgxc-system"
    else:
        nv_namespace = "nvsentinel"
        gpu_operator_namespace = "gpu-operator"
        prometheus_namespace = "prometheus"

    @pytest.fixture(autouse=True)
    def setup_runai_test(self):
        time.sleep(10)
        self.default_namespace = "runai-" + self.project

        self.client = KubernetesClient()
        self.pod_logs = []
        self.debug_pod = None
        self.gpu_healthy_node = None
        self.gpu_healthy_pods = []
        self.node_name = None
        pods, _ = self.client.list_pods(self.nv_namespace, name_pattern="mongo-client-pod")
        if pods:  # clean up existing debug pod before testing
            self.client.delete_pod(pod=pods[-1])
        try:
            yield
        finally:
            # Equivalent to addCleanup in unittest
            if self.debug_pod:
                self.client.delete_pod(pod=self.debug_pod)
            if self.node_name:
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
        self.logger.info(f"debug_pod = {debug_pod}")
        output, _ = self.client.exec_command_in_pod(debug_pod, command)
        time.sleep(10)
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
                assert output and "mq state UP" in output

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
                time.sleep(10)
                if err_msg:
                    self.logger.warning(
                        f"cannot delete debug pod with error message: {err_msg}"
                    )
        except Exception as error:
            self.logger.info(f"cannot delete debug pod with error {error}")

        retries = 0
        while retries < max_retries:
            try:
                debug_pod, err_msg = self.client.create_pod(pod_body=pod_body, wait=60)
                if err_msg:
                    self.logger.info(f"Attempt {retries + 1} failed: {err_msg}")
                    retries += 1
                    continue
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
                if 'service/prometheus-prometheus' in proc.info['cmdline'] and '-n' in proc.info['cmdline'] and self.prometheus_namespace in proc.info['cmdline']:
                    print("Port forwarding is already running.")
                    return

        # If no existing process is found, start a new one
        try:
            result = subprocess.run(
                ["kubectl", "port-forward", "service/prometheus-prometheus", "-n", self.prometheus_namespace, "9090:9090"],
                check=True,
                capture_output=True,
                text=True
            )
            print("Port forwarding established successfully.")
            print(f"Output: {result.stdout}")
        except subprocess.CalledProcessError as e:
            print("Failed to establish port forwarding.")
            print("Error:", e.stderr)

    def stop_port_forward_prometheus(self):
        """
        Find and terminate any kubectl port-forward processes that point at
        prometheus-prometheus in the prometheus namespace.
        """
        self.step_manager.print_header("Stopping port-forward prometheus")
        for proc in psutil.process_iter(['pid', 'name', 'cmdline']):
            if 'kubectl' in proc.info['name'] and 'port-forward' in proc.info['cmdline']:
                if 'service/prometheus-prometheus' in proc.info['cmdline'] and '-n' in proc.info['cmdline'] and 'prometheus' in proc.info['cmdline']:
                    print(f"Stopping port-forward PID {proc.pid}")
                    proc.send_signal(signal.SIGTERM)
                    proc.wait(timeout=5)

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
            namespace=self.nv_namespace,
            resource_plural="podmonitors",
            resource_name="podmonitors"
        )

        self.logger.info(f"output_message = \n{output_message}")

        self.step_manager.print_header(
            "Get the svc cluster IP of prometheus-prometheus svc in prometheus namespace"
        )
        services, _ = self.client.list_services(
            namespace=self.prometheus_namespace,
            name_pattern="prometheus-prometheus"
        )
        
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
        for _ in range(5):
            try:
                response = requests.get(url, params=params)
                self.logger.info(f"response.status_code = {response.status_code}")
                if fail_message == "":
                    fail_message = f"[FAIL] Cannot get {query_params}"
                assert response.status_code == 200, fail_message
                self.logger.info(f"response.json() = \n{response.json()}")
                return response
            except Exception as e:
                self.logger.info(f"Error: {e}")
                time.sleep(10)
        raise Exception(f"Failed to get {query_params} after 5 times")

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
            self.nv_namespace, name_pattern="nvsentinel-fault-quarantine*"
        )
        fault_quarantine_pod = pods[0]
        return fault_quarantine_pod

    def delete_fault_quarantine_pod(self):
        self.client.delete_pod(self.get_fault_quarantine_pod())
        time.sleep(10)

    def get_fault_quarantine_pod_log(self):
        fault_quarantine_pod = self.get_fault_quarantine_pod()
        logs, _ = self.client.get_pod_logs(self.nv_namespace, fault_quarantine_pod.metadata.name)
        return logs

    def get_node_drainer_pod(self):
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-node-drainer*"
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
            self.nv_namespace, node_drainer_pod.metadata.name
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
            self.MONGO_URI,
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
            self.nv_namespace, name_pattern="nvsentinel-platform-connector*"
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
            f"dcgmi test --host nvidia-dcgm.{self.gpu_operator_namespace}.svc:5555 --inject --gpuid 0 -f 84 -v 0",
        ]
        output, _ = self.client.exec_command_in_pod(pod, command)
        assert "Successfully injected" in output
    def clear_gpu_inforom_watch_error(self, pod):
        command = [
            "/bin/sh",
            "-c",
            f"dcgmi test --host nvidia-dcgm.{self.gpu_operator_namespace}.svc:5555 --inject --gpuid 0 -f 84 -v 1",
        ]
        output, _ = self.client.exec_command_in_pod(pod, command)
        assert "Successfully injected" in output

    def set_managed_by_nvsentinel_label_to_false(self, node_name):
        self.backup_label_value, _ = self.client.get_label_on_node(node_name, "k8saas.nvidia.com/ManagedByNVSentinel")
        success, err = self.client.add_label_to_node(node_name, "k8saas.nvidia.com/ManagedByNVSentinel", "false")
        assert success, f"Failed to set the label k8saas.nvidia.com/ManagedByNVSentinel to false on the node: {err}"

    def remove_managed_by_nvsentinel_label(self, node_name):
        self.logger.info(f"Remove the label k8saas.nvidia.com/ManagedByNVSentinel from the node: {node_name}")
        self.backup_label_value, _ = self.client.get_label_on_node(node_name, "k8saas.nvidia.com/ManagedByNVSentinel")
        self.client.remove_label_from_node(node_name, "k8saas.nvidia.com/ManagedByNVSentinel")
        self.logger.info(f"Backup label value: {self.backup_label_value}")

    def restore_managed_by_nvsentinel_label(self, node_name):
        if hasattr(self, "backup_label_value") and self.backup_label_value:
            self.client.add_label_to_node(node_name, "k8saas.nvidia.com/ManagedByNVSentinel", self.backup_label_value)
        else:
            self.client.remove_label_from_node(node_name, "k8saas.nvidia.com/ManagedByNVSentinel")


    def skip_if_node_drainer_deployment_not_found(self):
        node_drainer_deployment = self.client.get_deployments(self.nv_namespace, "nvsentinel-node-drainer")
        if not node_drainer_deployment:
            self.logger.error("Node drainer deployment not found")
            pytest.skip("Node drainer deployment not found")

    def skip_if_fault_quarantine_deployment_not_found(self):
        fault_quarantine_deployment = self.client.get_deployments(self.nv_namespace, "nvsentinel-fault-quarantine")
        if not fault_quarantine_deployment:
            self.logger.error("Fault quarantine deployment not found")
            pytest.skip("Fault quarantine deployment not found")
            
    def skip_if_fault_remediation_deployment_not_found(self):
        fault_remediation_deployment = self.client.get_deployments(self.nv_namespace, "nvsentinel-fault-remediation")
        if not fault_remediation_deployment:
            self.logger.error("Fault remediation deployment not found")
            pytest.skip("Fault remediation deployment not found")

    def skip_if_csp_health_monitor_deployment_not_found(self):
        deployment = self.client.get_deployments(self.nv_namespace, "nvsentinel-csp-health-monitor")
        if not deployment:
            self.logger.error("CSP health monitor deployment not found")
            pytest.skip("CSP health monitor deployment not found")

    def cleanup_mock_ethernet_interface(self, node_name):
        """
        Clean up the mock ethernet interface structure
        """
        self.logger.info(f"Cleaning up mock ethernet interface on node {node_name}")
        
        debug_pod = self.create_debug_pod(node_name)
        try:
            command = ["/bin/sh", "-c", 'chroot /host bash -c "rm -rf /var/run/nvsentinel/mock-net"']
            output, _ = self.client.exec_command_in_pod(debug_pod, command)
            self.logger.debug(f"Cleanup output: {output}")
        finally:
            self.client.delete_pod(debug_pod)

    def cleanup_mock_infiniband_interface(self, node_name):
        """
        Clean up the mock InfiniBand interface structure
        """
        self.logger.info(f"Cleaning up mock InfiniBand interface on node {node_name}")
        
        debug_pod = self.create_debug_pod(node_name)
        try:
            command = ["/bin/sh", "-c", 'chroot /host bash -c "rm -rf /var/run/nvsentinel/mock-infiniband"']
            output, _ = self.client.exec_command_in_pod(debug_pod, command)
            self.logger.debug(f"Cleanup output: {output}")
        finally:
            self.client.delete_pod(debug_pod)

    def set_mock_ethernet_state(self, node_name, interface_name, state):
        """
        Set the operstate of the mock ethernet interface
        """
        
        debug_pod = self.create_debug_pod(node_name)
        try:
            # Update the file in the mock location (host path)
            mock_path = f"/var/run/nvsentinel/mock-net/{interface_name}/operstate"
            command = ["/bin/sh", "-c", f'chroot /host bash -c "echo \'{state}\' > {mock_path}"']
            output, _ = self.client.exec_command_in_pod(debug_pod, command)
            self.logger.debug(f"Set state output: {output}")
            
            # Verify the state was set
            verify_command = ["/bin/sh", "-c", f'chroot /host bash -c "cat {mock_path}"']
            output, _ = self.client.exec_command_in_pod(debug_pod, verify_command)
            self.logger.info(f"Verified state: {output.strip()}")
            
        finally:
            self.client.delete_pod(debug_pod)

    def set_mock_infiniband_state(self, node_name, device_name, port_name, state, phys_state):
        """
        Set the state and phys_state of the mock InfiniBand interface
        """
        self.logger.info(f"Setting {device_name} port {port_name} state to {state}, phys_state to {phys_state} on node {node_name}")
        
        debug_pod = self.create_debug_pod(node_name)
        try:
            # Update the state file in the mock location (host path)
            mock_state_path = f"/var/run/nvsentinel/mock-infiniband/{device_name}/ports/{port_name}/state"
            mock_phys_state_path = f"/var/run/nvsentinel/mock-infiniband/{device_name}/ports/{port_name}/phys_state"
            
            # Set state
            command = ["/bin/sh", "-c", f'chroot /host bash -c "echo \'{state}\' > {mock_state_path}"']
            output, _ = self.client.exec_command_in_pod(debug_pod, command)
            self.logger.debug(f"Set state output: {output}")
            
            # Set phys_state
            command = ["/bin/sh", "-c", f'chroot /host bash -c "echo \'{phys_state}\' > {mock_phys_state_path}"']
            output, _ = self.client.exec_command_in_pod(debug_pod, command)
            self.logger.debug(f"Set phys_state output: {output}")
            
        finally:
            self.client.delete_pod(debug_pod)
            
    def create_mock_ethernet_interface(self, node_name, interface_name="dummy_test"):
        """
        Create a mock ethernet interface in a location accessible to both test and container
        """
        self.logger.info(f"Creating mock ethernet interface {interface_name} on node {node_name}")
        
        debug_pod = self.create_debug_pod(node_name)
        try:
            # Create the mock filesystem structure in /var/run/nvsentinel (host path)
            # This will be accessible as /var/run/mock-net inside the container
            mock_base_path = f"/var/run/nvsentinel/mock-net"
            mock_interface_path = f"{mock_base_path}/{interface_name}"
            
            commands = [
                # Create the base directory structure
                f'mkdir -p {mock_interface_path}',
                
                # Create device directory (indicates physical device)
                f'mkdir -p {mock_interface_path}/device',
                
                # Create type file with ethernet type (1)
                f'echo "1" > {mock_interface_path}/type',
                
                # Create operstate file with initial "up" state
                f'echo "up" > {mock_interface_path}/operstate',
                
                # Set proper permissions
                f'chmod 644 {mock_interface_path}/type',
                f'chmod 644 {mock_interface_path}/operstate',
                
                # Verify the structure was created
                f'ls -la {mock_interface_path}/',
                f'cat {mock_interface_path}/type',
                f'cat {mock_interface_path}/operstate',
            ]
            
            for command in commands:
                exec_command = ["/bin/sh", "-c", f'chroot /host bash -c "{command}"']
                output, _ = self.client.exec_command_in_pod(debug_pod, exec_command)
                self.logger.debug(f"Command: {command}, Output: {output}")
                
        finally:
            self.client.delete_pod(debug_pod)
        
        return interface_name

    def create_mock_infiniband_interface(self, node_name, device_name="mlx5_test", port_name="1"):
        """
        Create a mock InfiniBand interface in a location accessible to both test and container
        """
        self.logger.info(f"Creating mock InfiniBand interface {device_name} port {port_name} on node {node_name}")
        
        debug_pod = self.create_debug_pod(node_name)
        try:
            # Create the mock filesystem structure in /var/run/nvsentinel (host path)
            # This will be accessible as /var/run/mock-infiniband inside the container
            mock_base_path = f"/var/run/nvsentinel/mock-infiniband"
            mock_device_path = f"{mock_base_path}/{device_name}"
            mock_port_path = f"{mock_device_path}/ports/{port_name}"
            mock_net_path = f"{mock_device_path}/device/net"
            
            commands = [
                # Create the base directory structure
                f'mkdir -p {mock_port_path}',
                
                # Create the device/net directory structure for RoCE interface filtering
                f'mkdir -p {mock_net_path}',
                
                # Create a mock network interface that matches typical InfiniBand naming
                f'mkdir -p {mock_net_path}/{device_name.replace("rdmap", "rdma")}',
                
                # Create state file with initial "4: ACTIVE" state (healthy)
                f'echo "4: ACTIVE" > {mock_port_path}/state',
                
                # Create phys_state file with initial "5: LinkUp" state (healthy)
                f'echo "5: LinkUp" > {mock_port_path}/phys_state',
                
                # Set link_layer to "InfiniBand" instead of "unknown" for proper filtering
                f'echo "InfiniBand" > {mock_port_path}/link_layer',

                # Set proper permissions
                f'chmod 644 {mock_port_path}/state',
                f'chmod 644 {mock_port_path}/phys_state',
                f'chmod 644 {mock_port_path}/link_layer',
                
                # Verify the structure was created
                f'ls -la {mock_port_path}/',
                f'cat {mock_port_path}/state',
                f'cat {mock_port_path}/phys_state',
                f'cat {mock_port_path}/link_layer',
                f'ls -la {mock_net_path}/',
            ]
            
            for command in commands:
                exec_command = ["/bin/sh", "-c", f'chroot /host bash -c "{command}"']
                output, _ = self.client.exec_command_in_pod(debug_pod, exec_command)
                self.logger.info(f"Command: {command}, Output: {output}")
                
        finally:
            self.client.delete_pod(debug_pod)
        
        return device_name, port_name

    def restart_nic_monitor_pod(self, node_name):
        """
        Restart the NIC monitor pod to pick up new configmap configuration
        """
        self.logger.info(f"Restarting NIC monitor pod on node {node_name} to pick up new configuration")
        
        # Get the current pod
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-nic-health*"
        )
        
        target_pod = None
        for pod in pods:
            if pod.spec.node_name == node_name:
                target_pod = pod
                break
        
        if not target_pod:
            raise Exception(f"No NIC health monitor pod found on node {node_name}")
        
        # Delete the current pod so it gets recreated with new configmap
        self.logger.info(f"Deleting NIC monitor pod {target_pod.metadata.name} to restart with new configmap")
        self.client.delete_pod(target_pod)
        
        # Wait for the pod to be recreated
        time.sleep(10)
        
        # Find the new pod
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-nic-health*"
        )
        
        new_pod = None
        for pod in pods:
            if pod.spec.node_name == node_name:
                new_pod = pod
                break
        
        if not new_pod:
            raise Exception(f"New NIC health monitor pod not found on node {node_name}")
        
        self.logger.info(f"New NIC monitor pod: {new_pod.metadata.name}")
        return new_pod.metadata.name

    def update_nic_monitor_configmap(self, field, custom_path):
        """
        Update the NIC monitor configmap to include the custom path (container perspective)
        """
        self.logger.info(f"Updating NIC monitor configmap with custom path: {custom_path}")
        
        # Backup the original configmap first
        self.backup_cm = "backup_nic_monitor_cm.yaml"
        self.client.backup_configmap(
            self.nv_namespace, "nvsentinel-nic-health-monitor", self.backup_cm
        )
        
        try:
            # Get current configmap
            cm, _ = self.client.get_configmap(self.nv_namespace, "nvsentinel-nic-health-monitor")
            
            # Get current config
            current_config = cm.data.get("config.ini", "")
            
            # Create new config with custom path
            new_config = current_config
            if field not in new_config:
                new_config += f"\n{field} = {custom_path}\n"
            else:
                # Replace existing path
                lines = new_config.split('\n')
                for i, line in enumerate(lines):
                    if line.strip().startswith(field):
                        lines[i] = f"{field} = {custom_path}"
                        break
                new_config = '\n'.join(lines)
            
            # Create a temporary configmap file with the new config
            with tempfile.NamedTemporaryFile(mode='w', suffix='.yaml', delete=False) as temp_file:
                configmap_data = {
                    'apiVersion': 'v1',
                    'kind': 'ConfigMap',
                    'metadata': {
                        'name': 'nvsentinel-nic-health-monitor',
                        'namespace': self.nv_namespace
                    },
                    'data': {
                        'config.ini': new_config
                    }
                }
                yaml.dump(configmap_data, temp_file)
                temp_cm_file = temp_file.name
            # Apply the new configmap
            self.client.apply_configmap(temp_cm_file)
            
            # Clean up temp file
            os.unlink(temp_cm_file)
            
            self.logger.info("Updated configmap successfully")
            
        except Exception as e:
            self.logger.error(f"Failed to update configmap: {e}")
            raise

    def restore_nic_monitor_configmap(self):
        """
        Restore the original NIC monitor configmap
        """
        if hasattr(self, 'backup_cm'):
            self.logger.info("Restoring original NIC monitor configmap")
            try:
                self.client.apply_configmap(self.backup_cm)
                self.logger.info("Restored configmap successfully")
                # Clean up backup file
                if os.path.exists(self.backup_cm):
                    os.unlink(self.backup_cm)
            except Exception as e:
                self.logger.error(f"Failed to restore configmap: {e}")
    def verify_ethernet_error_condition(self, node_name, expected_status):
        """Utility function to verify EthernetErrorCheck condition"""
        self.step_manager.print_header(
            f"EthernetErrorCheck will change to {expected_status} in node condition."
        )
        target_condition, _ = self.client.read_node_condition_by_type(
            node_name=node_name, condition_type="EthernetErrorCheck"
        )
        assert (
            target_condition.status == expected_status
        ), f"Status of EthernetErrorCheck is not {expected_status}: {target_condition}"
        self.logger.info(
            f"SUCCESS: EthernetErrorCheck status is {expected_status} when interface state changed"
        )

    def get_metric_value(self, pod_name):
        """Utility function to get current metric value"""
        self.step_manager.print_header(
            "Get the current value of metric nic_monitor_health_events_published_total"
        )
        max_retry = 5
        retry_delay = 10  # seconds
        for attempt in range(max_retry):
            response = self.query_metrics(
                query_params=f'nic_monitor_health_events_published_total{{pod="{pod_name}"}}'
            )
            data = response.json().get("data", {})
            results = data.get("result", []) if isinstance(data, dict) else []

            if results:
                value = results[0]["value"]
                self.logger.info(f"[DEBUG] value = {value}")
                return int(value[1])

            self.logger.info(
                "Metric nic_monitor_health_events_published_total returned no data for pod %s (attempt %d/%d); retrying after %ds",
                pod_name,
                attempt + 1,
                max_retry,
                retry_delay,
            )
            time.sleep(retry_delay)

        # If still no data, return 0 and log warning
        self.logger.warning(
            "Metric nic_monitor_health_events_published_total returned no data for pod %s after %d attempts; assuming zero",
            pod_name,
            max_retry,
        )
        return 0

    def validate_metric_changes(self, value_before, value_after_down, value_after_up, expected_down_count):
        """Utility function to validate metric changes"""
        self.step_manager.print_header(
            "check value of the metric is increased when the NIC is set down and when the NIC is set up"
        )
        self.logger.info(f"value_before = {value_before}")
        self.logger.info(f"value_after_down = {value_after_down}")
        self.logger.info(f"value_after_up = {value_after_up}")

        assert (
            value_after_down - value_before == expected_down_count
        ), f"[FAIL] value of the metric is NOT increased by {expected_down_count} when the NIC is set down"
        assert (
            value_after_up - value_after_down == 1
        ), "[FAIL] value of the metric is NOT increased by 1 when the NIC is set up"
        self.logger.info(
            f"[PASS] value of the metric is increased by {expected_down_count} when the NIC is set down and when the NIC is set up"
        )

    def _enable_dry_run_mode(self):
        """Put the Fault-Quarantine deployment into dry-run mode.

        * Stores the original container arguments so they can be restored later
          (saved only the first time it is invoked).
        * Ensures there is **exactly one** `--dry-run true` flag in the container
          arguments, regardless of the original flag style.
        """
        deployments = self.client.get_deployments(
            self.nv_namespace, "nvsentinel-fault-quarantine"
        )
        if not deployments:
            pytest.fail("Fault-quarantine deployment not found – cannot enable dry-run mode")

        deployment = deployments[0]

        # Locate the single container we care about
        fq_container = None
        for c in deployment.spec.template.spec.containers:
            if c.name == "fault-quarantine":
                fq_container = c
                break
        if fq_container is None:
            pytest.fail("fault-quarantine container not found in deployment")

        # Cache the ORIGINAL args once so we can restore them later
        if not hasattr(self, "_fq_original_args"):
            # Make a **deep** copy so later edits do not mutate the saved list
            self._fq_original_args = list(fq_container.args or [])
            self.logger.info(f"Cached original fault-quarantine args: {self._fq_original_args}")

        # ------------------------------------------------------------------
        # Build new args list with *no* existing dry-run flags, then append ours
        # ------------------------------------------------------------------
        new_args = []
        skip_next = False
        for arg in fq_container.args or []:
            if skip_next:
                skip_next = False
                continue

            # Handle split form: --dry-run true/false
            if arg == "--dry-run":
                skip_next = True
                continue
            # Handle combined form: --dry-run=true / --dry-run=false
            if arg.startswith("--dry-run="):
                continue

            new_args.append(arg)

        # Add the canonical form we want
        new_args.extend(["--dry-run", "true"])
        fq_container.args = new_args
        self.logger.info(f"Applying dry-run args: {new_args}")

        # Update the deployment (replace is fine – we kept the current resourceVersion)
        self.client.appsV1Api.replace_namespaced_deployment(
            name=deployment.metadata.name,
            namespace=self.nv_namespace,
            body=deployment,
        )
        # Allow some time for rollout to start
        time.sleep(10)

    def _restore_dry_run_mode(self):
        """Restore the Fault-Quarantine deployment to its original non-dry-run state."""
        deployments = self.client.get_deployments(
            self.nv_namespace, "nvsentinel-fault-quarantine"
        )
        if not deployments:
            self.logger.error("Fault-quarantine deployment not found – cannot restore dry-run mode")
            return

        deployment = deployments[0]

        # Locate container
        fq_container = None
        for c in deployment.spec.template.spec.containers:
            if c.name == "fault-quarantine":
                fq_container = c
                break
        if fq_container is None:
            self.logger.error("fault-quarantine container not found in deployment – cannot restore")
            return

        # If we cached the original args, restore them; otherwise just strip the flag
        if hasattr(self, "_fq_original_args"):
            fq_container.args = list(self._fq_original_args)
            self.logger.info("Restored original fault-quarantine args from cache")
        else:
            # No cache – remove any dry-run flags that may exist
            new_args = []
            skip_next = False
            for arg in fq_container.args or []:
                if skip_next:
                    skip_next = False
                    continue
                if arg == "--dry-run":
                    skip_next = True
                    continue
                if arg.startswith("--dry-run="):
                    continue
                new_args.append(arg)
            fq_container.args = new_args
            self.logger.info("Removed --dry-run flags from current deployment args")

        # Replace deployment with cleaned args
        self.client.appsV1Api.replace_namespaced_deployment(
            name=deployment.metadata.name,
            namespace=self.nv_namespace,
            body=deployment,
        )
        # Wait for rollout to complete
        time.sleep(10)

    def skip_if_kata_mode_disabled(self):
        """Skip the test if kata mode is disabled"""
        if not self.client.get_deployments(self.nv_namespace, "nvsentinel-platform-connector"):
            pytest.skip("Kata mode is disabled")

    def reboot_forge_node(self, node_name, wait_second = 1800):
        """Reboot the node by setting nke.nvidia.com/reboot=nvsentinel-integration-test-reboot to the node"""

        value, err_msg = self.client.get_label_on_node(node_name, "nke.nvidia.com/reboot")
        if err_msg:
            pytest.fail(f"Failed to get label: {err_msg}")
        current_time = datetime.now(timezone.utc)
        if value:
            self.logger.info(f"Reboot already in progress for node {node_name}")
        else:
            _, err_msg = self.client.add_label_to_node(node_name, "nke.nvidia.com/reboot", "nvsentinel-integration-test-reboot")
            if err_msg:
                pytest.fail(f"Failed to add label: {err_msg}")

        for _ in range(0, wait_second, 50):
            time.sleep(50)
            value, err_msg = self.client.get_label_on_node(node_name, "nke.nvidia.com/last-completed-reboot")
            if err_msg:
                pytest.fail(f"Failed to get label: {err_msg}")
            if value:
                timestamp = datetime.strptime(value, "%Y-%m-%dT%H-%M-%SZ").replace(tzinfo=timezone.utc)
                if timestamp > current_time:
                    self.logger.info(f"Reboot completed for node {node_name} at {timestamp}")
                    return

            reboot_attempt, err_msg = self.client.get_label_on_node(node_name, "nke.nvidia.com/reboot-attempt")
            if err_msg:
                pytest.fail(f"Failed to get label: {err_msg}")
            reboot_started, err_msg = self.client.get_label_on_node(node_name, "nke.nvidia.com/reboot-started")
            if err_msg:
                pytest.fail(f"Failed to get label: {err_msg}")
            self.logger.info(f"Reboot attempt {reboot_attempt} for node {node_name} at {reboot_started} UTC")

        self.logger.info(f"Reboot did not complete for node {node_name} after {wait_second} seconds")
        pytest.fail(f"Reboot did not complete for node {node_name} after {wait_second} seconds")

    def read_circuit_breaker_state(self):
        """Read the circuit breaker state"""
        cm, err = self.client.get_configmap(self.nv_namespace, "fault-quarantine-circuit-breaker")
        if err:
            pytest.fail(f"Failed to get circuit breaker state: {err}")
        return cm.data["status"]

    def change_circuit_breaker_state(self, state):
        """Change the circuit breaker state"""
        cm, err = self.client.get_configmap(self.nv_namespace, "fault-quarantine-circuit-breaker")
        if err:
            pytest.fail(f"Failed to get circuit breaker state: {err}")

        cm.data["status"] = state
        configmap_yaml = {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {
                "name": "fault-quarantine-circuit-breaker",
                "namespace": self.nv_namespace
            },
            "data": cm.data
        }
        with tempfile.NamedTemporaryFile(mode='w', suffix='.yaml', delete=False) as temp_configmap_file:
            yaml.dump(configmap_yaml, temp_configmap_file, default_flow_style=False)
            temp_configmap_file_name = temp_configmap_file.name

        success, err_msg = self.client.apply_configmap(temp_configmap_file_name)
        if not success:
            pytest.fail(f"Failed to apply updated ConfigMap: {err_msg}")
        os.unlink(temp_configmap_file_name)

    def inject_gpu_inforom_on_all_nodes(self, gpu_health_monitor_pods):
        for pod in gpu_health_monitor_pods:
            assert pod.status.phase == "Running", f"FAIL: Pod {pod.metadata.name} is not running"
            self.inject_gpu_inforom_watch_error(pod)

    def clear_gpu_inforom_on_all_nodes(self, gpu_health_monitor_pods):
        for pod in gpu_health_monitor_pods:
            assert pod.status.phase == "Running", f"FAIL: Pod {pod.metadata.name} is not running"

            self.clear_gpu_inforom_watch_error(pod)

    def copy_file_to_pod(self, source, destination):
        copy_command = [
            "kubectl",
            "cp",
            source,
            destination,
        ]
        try:
            result = subprocess.run(copy_command, check=True, capture_output=True, text=True)
            self.logger.info(f"Successfully copied {source} to {destination}")
            return result
        except subprocess.CalledProcessError as e:
            self.logger.error(f"Failed to copy {source} to {destination}: {e.stderr}")
            raise

    def skip_if_circuit_breaker_disabled(self):
        """Skip the test if circuit breaker is disabled"""
        if not self.client.get_configmap(self.nv_namespace, "fault-quarantine-circuit-breaker"):
            pytest.skip("Circuit breaker is disabled")

    def set_managed_by_nvsentinel_label_to_false_for_all_nodes(self):
        """
        Set label k8saas.nvidia.com/ManagedByNVSentinel=false on all GPU-present nodes
        (nodes with label nvidia.com/gpu.present=true), backing up original values
        for later restoration.
        """
        target_node_names, _ = self.client.get_node_names_by_label(label_selector="nvidia.com/gpu.present=true")

        if not hasattr(self, "_managed_by_nv_backup"):
            self._managed_by_nv_backup = {}

        for node_name in target_node_names:
            prev_val, _ = self.client.get_label_on_node(node_name, "k8saas.nvidia.com/ManagedByNVSentinel")
            self._managed_by_nv_backup[node_name] = prev_val
            success, err = self.client.add_label_to_node(
                node_name,
                "k8saas.nvidia.com/ManagedByNVSentinel",
                "false",
            )
            assert success, (
                f"Failed to set k8saas.nvidia.com/ManagedByNVSentinel=false on node {node_name}: {err}"
            )

    def restore_managed_by_nvsentinel_label_for_all_nodes(self):
        """
        Restore the label k8saas.nvidia.com/ManagedByNVSentinel on all nodes that were

        modified by set_managed_by_nvsentinel_label_to_false_for_all_nodes().

        """
        if not hasattr(self, "_managed_by_nv_backup") or not self._managed_by_nv_backup:
            return

        for node_name, prev_val in self._managed_by_nv_backup.items():
            if prev_val:
                self.client.add_label_to_node(
                    node_name,
                    "k8saas.nvidia.com/ManagedByNVSentinel",
                    prev_val,
                )
            else:
                self.client.remove_label_from_node(
                    node_name, "k8saas.nvidia.com/ManagedByNVSentinel"
                )

        self._managed_by_nv_backup = {}
