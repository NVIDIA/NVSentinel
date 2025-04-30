# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.
"""
Module for class of NVsentinel NIC Health Monitor: Multi NIC errors
"""

import pytest
import re
import time
from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestEthernetLinkDown(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel NIC Health Monitor: Multi NIC errors
    """

    @pytest.mark.nichealthmonitor
    def test_multi_nic_errors(self, request):
        """
        Tests if the EthernetErrorCheck condition and related events are set correctly when multiple NICs are down
        """
        self.step_manager.print_header(
            "Filter out the nodes with more than 2 physical interface on the node"
        )
        nodes_interfaces_dict = self.get_nodes_with_more_than_two_physical_interfaces()
        if not nodes_interfaces_dict:
            pytest.skip(
                "Cannot find a node with more than 2 physical interfaces on the cluster. "
                "Skipping this test case."
            )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-nic-health*"
        )
        pod_name = None
        nodes_list = list(nodes_interfaces_dict.keys())
        node_name = nodes_list[0]
        for pod in pods:
            if pod.spec.node_name == node_name:
                # delete current nic pod to aviod old logs impact match counts
                self.client.delete_pod(pod)
                time.sleep(5)
                break

        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-nic-health*"
        )
        for pod in pods:
            if pod.spec.node_name == node_name:
                pod_name = pod.metadata.name
                self.logger.info(f"POD   Name: {pod_name}")
                self.logger.info(f"Node  Name: {node_name}")
                break

        self.step_manager.print_header("Login the node where the monitor pod is running on")
        non_mgmt_interface = self.get_non_mgmt_ports_of_the_node(node_name)
        self.logger.info(f"TARGET PORT NAME: {non_mgmt_interface}")

        self.step_manager.print_header("Repeat the port down/up operations for 100 times")
        count = 100
        self.down_up_interface_of_node_multi_times(
            node_name, non_mgmt_interface, count=count
        )

        nic_pod_logs, _ = self.client.get_pod_logs(self.nv_namespace, pod_name)
        nic_down_pattern = re.compile(
            rf'events:\{{.*?isFatal:true.*?message:"state: down".*?entityValue:"{non_mgmt_interface}"'
        )
        nic_healthy_pattern = re.compile(
            rf'events:\{{.*?isHealthy:true.*?message:"Device is healthy".*?entityValue:"{non_mgmt_interface}"'
        )
        log_lines = nic_pod_logs.splitlines()
        down_count = 0
        healthy_count = 0
        start_counting = False
        for line in log_lines:
            # Start counting from first down event
            if not start_counting and nic_down_pattern.search(line):
                start_counting = True
                self.logger.info(f"Start counting from line: {line}")

            if start_counting:
                if nic_down_pattern.search(line):
                    down_count += 1
                    self.logger.info(f"Found down event ({down_count}): {line}")
                if nic_healthy_pattern.search(line):
                    healthy_count += 1
                    self.logger.info(f"Found healthy event ({healthy_count}): {line}")

        self.step_manager.print_header(
            "Verify will get all 100 error logs indicating error found and Device is healthy messages when port up"
        )
        assert down_count == count and healthy_count == count, (
            f"Mismatch error found and 'Device is healthy' message count in pod console log, Error Found:{down_count}, Device is healthy:{healthy_count}, "
            f"Expected: {count}"
        )
        self.logger.info("SUCCESS: All message is show when port up in pod console log")

        self.step_manager.print_header(
            "Check from node Condition, should see the node condition flip back from true to false 100 times"
        )
        target_condition, _ = self.client.read_node_condition_by_type(
            node_name=node_name, condition_type="EthernetErrorCheck"
        )
        assert (
            target_condition.status == "False"
        ), f"Status of EthernetErrorCheck is still True after turn NIC port up: {target_condition}"
        self.logger.info(
            "SUCCESS: EthernetErrorCheck status is flip back to False when port up in node"
        )
