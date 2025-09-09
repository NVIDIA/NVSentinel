# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import threading
import yaml
import time
import re
import pytest
import random
import tempfile
from functools import partial
from testcases.nvsentinel.base import TestNVSentinelCaseBase
import os

class TestNicExclusionRegex(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel NIC Health Monitor: Nic exclusion regex for NIC name
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.nichealthmonitor
    @pytest.mark.skip(reason="nic health monitor is disabled globally")
    def test_nic_exclusion_regex_for_nic_name(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Tests if the regex exclusion for NIC name is working correctly
        """
        # For AWS we use a mock filesystem (see nic_exclusion_regex_in_aws). For
        # all other providers keep the original flow that manipulates physical
        # interfaces.
        if os.getenv("CLOUD_PROVIDER") == "aws":
            self.logger.info("Running on AWS with mock filesystem")
            self.nic_exclusion_regex_in_aws(request)
            return
        else:
            self.logger.info("Running on CSP with original filesystem")
            self.nic_exclusion_regex_in_csp(request)
            return

    def nic_exclusion_regex_in_csp(self, request):
        """Run NicExclusionRegex test on CSPs using actual network interfaces."""

        self.step_manager.print_header(
            "Filter out the nodes with more than 2 physical interface on the node"
        )
        nodes_interfaces_dict = self.get_nodes_with_more_than_two_physical_interfaces()
        if not nodes_interfaces_dict:
            pytest.skip(
                "Cannot find a node with more than 2 physical interfaces on the cluster. "
                "Skipping this test case."
            )

        nodes_list = list(nodes_interfaces_dict.keys())
        # pickup one node to test
        node_name = nodes_list[0]
        self.logger.info(f"Will do inetrface up/down on below nodes:{node_name}")
        self.step_manager.print_header(
            "Get the nvsentinel-nic-health-monitor pod of the node {node}"
        )
        pod_name = None
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-nic-health*"
        )
        for pod in pods:
            if pod.spec.node_name == node_name:
                pod_name = pod.metadata.name
                self.logger.info(f"New POD   Name: {pod_name}")
                break

        self.step_manager.print_header("Login the node where the monitor pod is running on")
        non_mgmt_interface = self.get_non_mgmt_ports_of_the_node(node_name)
        assert (
            non_mgmt_interface
        ), "Cannot find non-mgmt port on the node {}. Pls check it manually"
        self.logger.info(f"TARGET PORT NAME: {non_mgmt_interface}")
        request.addfinalizer(
            partial(self.up_interface_of_node, node_name, non_mgmt_interface)
        )

        self.pod_logs = []
        monitor_thread = threading.Thread(
            target=self.follow_pod_logs, args=(self.nv_namespace, pod_name), daemon=True
        )
        monitor_thread.start()
        self.step_manager.print_header(
            "Down a port before the nic regex exculsion is updated"
        )
        self.down_interface_of_node(node_name, non_mgmt_interface)
        time.sleep(30)
        assert (
            "state: down" in self.pod_logs[-1]
        ), f"Should find nic down message in console log:{self.pod_logs}"
        assert (
            f'entityValue:"{non_mgmt_interface}"' in self.pod_logs[-1]
        ), f"Should find nic down message in console log:{self.pod_logs}"
        target_condition, _ = self.client.read_node_condition_by_type(
            node_name=node_name, condition_type="EthernetErrorCheck"
        )
        assert (
            target_condition.status == "True"
        ), f"Status of EthernetErrorCheck should be True: {target_condition}"
        self.up_interface_of_node(node_name, non_mgmt_interface)

        self.step_manager.print_header(
            "Update the NicExclusionRegex setting in configmap nvsentinel-nic-health-monitor to include the port"
        )
        ib_flag = self.check_if_ib_interface(node_name)
        ib_interface = None
        if ib_flag:
            ib_interface = self.get_ib_interface_of_eth_interface(
                non_mgmt_interface, node_name
            )
            regex = f"^({non_mgmt_interface}|{ib_interface})$"
        else:
            regex = f"^{non_mgmt_interface}$"

        backup_cm = "backup_nic_monitor_cm.yaml"
        modified_cm = "nic_regex_cm.yaml"
        self.client.backup_configmap(
            self.nv_namespace, "nvsentinel-nic-health-monitor", backup_cm
        )
        with open(backup_cm, "r") as f:
            config_data = yaml.safe_load(f)
        config_ini = config_data["data"]["config.ini"]
        line_pattern = r"NicExclusionRegex = (.*)\n"
        match = re.search(line_pattern, config_ini)
        if match:
            existing_regex = match.group(1).strip()
            if regex not in existing_regex.split(","):
                combined_regex = f"{existing_regex},{regex}"
            else:
                combined_regex = existing_regex
            new_config_ini = re.sub(line_pattern, f"NicExclusionRegex = {combined_regex}\n", config_ini)
        else:
            new_config_ini = config_ini.rstrip() + f"\nNicExclusionRegex = {regex}\n"
        config_data["data"]["config.ini"] = new_config_ini
        with open(modified_cm, "w") as f:
            yaml.safe_dump(config_data, f)
        self.logger.info(f"modified_cm: {config_data}")
        self.client.apply_configmap(modified_cm)
        request.addfinalizer(
            partial(
                self.client.rollout_daemonset,
                "nvsentinel-nic-health-monitor",
                self.nv_namespace,
            )
        )
        request.addfinalizer(partial(self.client.apply_configmap, backup_cm))
        self.step_manager.print_header(" Restart NIC monitor pods to make the change work.")
        self.client.rollout_daemonset("nvsentinel-nic-health-monitor", self.nv_namespace)
        time.sleep(30)
        # get new nic pod name
        pod_name_new = None
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-nic-health*"
        )
        for pod in pods:
            if pod.spec.node_name == node_name:
                pod_name_new = pod.metadata.name
                self.logger.info(f"New POD Name: {pod_name_new}")
                break

        self.step_manager.print_header(
            "Open one console to check the logs from the pod, do not close this console"
        )
        self.pod_logs = []
        monitor_thread = threading.Thread(
            target=self.follow_pod_logs, args=(self.nv_namespace, pod_name_new), daemon=True
        )
        monitor_thread.start()

        try:
            self.step_manager.print_header(f"Down the port {non_mgmt_interface}")
            self.down_interface_of_node(node_name, non_mgmt_interface)
            self.step_manager.print_header("Check error info From the pod log console")
            self.logger.info("Checking the log of ethernet interface")
            time.sleep(30)
            assert (
                "state: down" not in self.pod_logs[-1]
            ), f"Should not find nic down message in console log:{self.pod_logs}"
            assert (
                f'entityValue:"{non_mgmt_interface}"' not in self.pod_logs[-1]
            ), f"Should not find nic down message in console log:{self.pod_logs}"

            self.logger.info(
                "SUCCESS: 'state: down' message is show when ethernet port down in pod console log"
            )
            if ib_interface:
                self.logger.info("Checking the log of ib interface")
                assert (
                    "state: 1: DOWN" not in self.pod_logs[-2]
                ), f"Should not find nic down message in console log:{self.pod_logs}"
                assert (
                    f'entityValue"{ib_interface}"' not in self.pod_logs[-2]
                ), f"Should not find nic down message in console log:{self.pod_logs}"

            self.step_manager.print_header(
                "EthernetErrorCheck should be false in node condition."
            )
            target_condition, _ = self.client.read_node_condition_by_type(
                node_name=node_name, condition_type="EthernetErrorCheck"
            )
            assert (
                target_condition.status == "False"
            ), f"Status of EthernetErrorCheck should be False: {target_condition}"
            self.logger.info(
                "SUCCESS: EthernetErrorCheck status is flip to True when port up in node"
            )
        except:  # ensure the nic port has been trun up if any error condition occurred
            self.up_interface_of_node(node_name, non_mgmt_interface)
            raise

    def nic_exclusion_regex_in_aws(self, request):
        """Run NicExclusionRegex test using mock ethernet interface on AWS."""

        self.step_manager.print_header("Select a node for testing with mock ethernet interface")

        nodes, _ = self.client.get_nodes()
        if not nodes:
            pytest.skip("No nodes available for testing")

        node_name = random.choice([node.metadata.name for node in nodes])

        self.logger.info(f"Selected node for testing: {node_name}")

        # 1. Update SysClassNetPath in configmap so NIC monitor looks at mock path
        try:
            self.update_nic_monitor_configmap("SysClassNetPath", "/var/run/mock-net")
            request.addfinalizer(self.restore_nic_monitor_configmap)
        except Exception as e:
            self.logger.error(f"Failed to update SysClassNetPath in configmap: {e}")
            pytest.skip(f"Cannot update NIC monitor configuration: {e}")

        # 2. Create mock ethernet interface
        interface_name = f"eth{random.randint(1000, 9999)}"
        try:
            self.create_mock_ethernet_interface(node_name, interface_name)
            request.addfinalizer(partial(self.cleanup_mock_ethernet_interface, node_name))
        except Exception as e:
            self.logger.error(f"Failed to create mock interface: {e}")
            pytest.skip(f"Cannot create mock interface on node {node_name}. Error: {e}")

        # 3. Restart NIC monitor to pick up new configmap
        try:
            pod_name = self.restart_nic_monitor_pod(node_name)
        except Exception as e:
            self.logger.error(f"Failed to restart NIC monitor: {e}")
            pytest.skip(f"Cannot restart NIC monitor on node {node_name}. Error: {e}")

        # 4. Start log streaming
        self.pod_logs = []
        monitor_thread = threading.Thread(target=self.follow_pod_logs, args=(self.nv_namespace, pod_name), daemon=True)
        monitor_thread.start()

        time.sleep(10)

        # -------------------- Phase 1: Interface DOWN (should be detected) --------------------
        self.step_manager.print_header(f"[Phase-1] Set interface {interface_name} DOWN and expect NIC monitor to report error")
        self.set_mock_ethernet_state(node_name, interface_name, "down")

        time.sleep(10)
        expected_down_tokens = [
            'checkName:"EthernetErrorCheck"',
            'agent:"nic-health-monitor"',
            'componentClass:"NIC"',
            "state: down",
            f'entityValue:"{interface_name}"',
        ]
        down_found = any(all(re.search(tok, log) for tok in expected_down_tokens) for log in self.pod_logs[-10:])
        assert down_found, f"NIC down log not found for {interface_name}. Recent logs: {self.pod_logs[-10:]}"

        self.verify_ethernet_error_condition(node_name, "True")

        # Bring interface back up
        self.set_mock_ethernet_state(node_name, interface_name, "up")
        time.sleep(10)
        self.verify_ethernet_error_condition(node_name, "False")

        # -------------------- Phase 2: Update NicExclusionRegex --------------------
        self.step_manager.print_header("Update NicExclusionRegex to exclude the mock interface")

        try:
            cm, _ = self.client.get_configmap(self.nv_namespace, "nvsentinel-nic-health-monitor")
            config_ini = cm.data.get("config.ini", "")

            regex_line_pattern = r"NicExclusionRegex = (.*)\n"
            new_regex = f"^{interface_name}$"

            match = re.search(regex_line_pattern, config_ini)
            if match:
                existing_regex = match.group(1).strip()
                if new_regex not in existing_regex.split(","):
                    combined_regex = f"{existing_regex},{new_regex}"
                else:
                    combined_regex = existing_regex
                new_config_ini = re.sub(regex_line_pattern, f"NicExclusionRegex = {combined_regex}\n", config_ini)
            else:
                new_config_ini = config_ini.rstrip() + f"\nNicExclusionRegex = {new_regex}\n"

            with tempfile.NamedTemporaryFile(mode="w", suffix=".yaml", delete=False) as tmp:
                yaml.dump({
                    "apiVersion": "v1",
                    "kind": "ConfigMap",
                    "metadata": {"name": "nvsentinel-nic-health-monitor", "namespace": self.nv_namespace},
                    "data": {"config.ini": new_config_ini},
                }, tmp)
                temp_cm_path = tmp.name

            self.client.apply_configmap(temp_cm_path)
            os.unlink(temp_cm_path)
        except Exception as e:
            self.logger.error(f"Failed to update NicExclusionRegex: {e}")
            raise

        self.client.rollout_daemonset("nvsentinel-nic-health-monitor", self.nv_namespace)
        time.sleep(30)

        # Get new pod after restart
        pods, _ = self.client.list_pods(self.nv_namespace, name_pattern="nvsentinel-nic-health*")
        new_pod_name = None
        for pod in pods:
            if pod.spec.node_name == node_name:
                new_pod_name = pod.metadata.name
                break
        assert new_pod_name, f"Failed to find NIC monitor pod on node {node_name} after rollout"

        # Start new log thread
        self.pod_logs = []
        monitor_thread = threading.Thread(target=self.follow_pod_logs, args=(self.nv_namespace, new_pod_name), daemon=True)
        monitor_thread.start()

        time.sleep(10)

        # -------------------- Phase 3: Interface DOWN again (should be ignored) --------------------
        self.step_manager.print_header(f"[Phase-3] Set interface {interface_name} DOWN again – should be ignored due to regex")
        self.set_mock_ethernet_state(node_name, interface_name, "down")
        time.sleep(10)

        # Ensure no down message appears
        down_present_post_regex = any("state: down" in log and interface_name in log for log in self.pod_logs[-10:])
        assert not down_present_post_regex, f"Unexpected NIC down log after exclusion regex update: {self.pod_logs[-10:]}"

        self.verify_ethernet_error_condition(node_name, "False")

        self.logger.info("NicExclusionRegex mock test completed successfully on AWS")
