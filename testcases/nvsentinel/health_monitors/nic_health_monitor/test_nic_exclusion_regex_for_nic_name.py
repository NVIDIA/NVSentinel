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
from functools import partial
from testcases.nvsentinel.base import TestNVSentinelCaseBase
import os

class TestNicExclusionRegex(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel NIC Health Monitor: Nic exclusion regex for NIC name
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.nichealthmonitor
    def test_nic_exclusion_regex_for_nic_name(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Tests if the regex exclusion for NIC name is working correctly
        """
        if os.getenv("CLOUD_PROVIDER") == "aws":
            # Jira: https://jirasw.nvidia.com/browse/NGCC-25436
            # ibdev2netdev not found on nodes, need to figure out how to map ib interface to nic interface
            pytest.skip("This test case is not supported on AWS. Skipping this test case.")
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
            "nvsentinel", "nvsentinel-nic-health-monitor", backup_cm
        )
        with open(backup_cm, "r") as f:
            config_data = yaml.safe_load(f)
        config_ini = config_data["data"]["config.ini"]
        pattern = r"NicExclusionRegex = .*?\n"
        replacement = f"NicExclusionRegex = {regex}\n"
        new_config_ini = re.sub(pattern, replacement, config_ini)
        config_data["data"]["config.ini"] = new_config_ini
        with open(modified_cm, "w") as f:
            yaml.safe_dump(config_data, f)
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
