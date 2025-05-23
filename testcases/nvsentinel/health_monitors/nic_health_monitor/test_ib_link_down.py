# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import pytest
import threading
import random
import time
from functools import partial
import os
from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestInfiniBandLinkDown(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel NIC Health Monitor: InfiniBand link down
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.nichealthmonitor
    def test_infini_band_link_down(self, request):
        """
        Tests if the InfiniBandErrorCheck condition is set correctly when the InfiniBand link is down
        """
        if os.getenv("CLOUD_PROVIDER") == "aws":
            # Setting non-management interface down turn off kubernetes connectivity for some reason
            # Jira: https://jirasw.nvidia.com/browse/NGCC-25437
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
        # select 2 nodes to test
        nodes = random.sample(nodes_list, min(2, len(nodes_list)))
        self.logger.info(f"Will do inetrface up/down on below nodes:{nodes}")
        for node_name in nodes:
            if not self.check_if_ib_interface(node_name):
                pytest.skip("There are no IB interface on the node. Skip the test")
            self.step_manager.print_header(
                "Get the nvsentinel-nic-health-monitor pod of the node {node}"
            )
            pods, _ = self.client.list_pods(
                self.nv_namespace, name_pattern="nvsentinel-nic-health*"
            )
            pod_name = None
            for pod in pods:
                if pod.spec.node_name == node_name:
                    pod_name = pod.metadata.name
                    self.logger.info(f"POD   Name: {pod_name}")
                    self.logger.info(f"Node  Name: {node_name}")
                    break

            self.step_manager.print_header(
                "Open one console to check the logs from the pod, do not close this console"
            )
            self.pod_logs = []
            monitor_thread = threading.Thread(
                target=self.follow_pod_logs, args=(self.nv_namespace, pod_name), daemon=True
            )
            monitor_thread.start()

            self.step_manager.print_header(
                "Login the node where the monitor pod is running on"
            )

            non_mgmt_interface = self.get_non_mgmt_ports_of_the_node(node_name)
            assert (
                non_mgmt_interface
            ), "Cannot find non-mgmt port on the node {}. Pls check it manually"
            self.logger.info(f"TARGET PORT NAME: {non_mgmt_interface}")
            request.addfinalizer(
                partial(self.up_interface_of_node, node_name, non_mgmt_interface)
            )
            try:
                # get ib interface of the ethernet interface
                ib_interface = self.get_ib_interface_of_eth_interface(
                    non_mgmt_interface, node_name
                )
                self.step_manager.print_header(f"Down the port {non_mgmt_interface}")
                self.down_interface_of_node(node_name, non_mgmt_interface)

                self.step_manager.print_header("Check error info From the pod log console")
                self.logger.info("Checking the log of ethernet interface")
                message_to_check = [
                    'checkName:"EthernetErrorCheck"',
                    'agent:"nic-health-monitor"',
                    'componentClass:"NIC"',
                    "state: down",
                    f'entityType:"NIC" ' f'entityValue:"{non_mgmt_interface}"',
                ]
                find_match = all(
                    message for message in message_to_check if message in self.pod_logs[-1]
                )
                assert (
                    find_match
                ), f"Find no expected message in console log:{self.pod_logs}"
                self.logger.info(
                    "SUCCESS: 'state: down' message is show when ethernet port down in pod console log"
                )

                self.logger.info("Checking the log of ib interface")
                message_to_check = [
                    'checkName:"InfiniBandErrorCheck"',
                    'agent:"nic-health-monitor"',
                    'componentClass:"NIC"',
                    "state: 1: DOWN",
                    f'entityType:"NIC" ' f'entityValue:"{ib_interface}"',
                ]
                find_match = all(
                    message for message in message_to_check if message in self.pod_logs[-2]
                )
                assert (
                    find_match
                ), f"Find no expected message in console log:{self.pod_logs}"
                self.logger.info(
                    "SUCCESS: 'state: down' message is show when ib port down in pod console log"
                )

                self.step_manager.print_header(
                    "EthernetErrorCheck will change to True in node condition."
                )
                target_condition, _ = self.client.read_node_condition_by_type(
                    node_name=node_name, condition_type="EthernetErrorCheck"
                )
                assert (
                    target_condition.status == "True"
                ), f"Status of EthernetErrorCheck is still False: {target_condition}"
                self.logger.info(
                    "SUCCESS: EthernetErrorCheck status is flip to True when port up in node"
                )
            except:  # ensure the nic port has been trun up if any error condition occurred
                self.up_interface_of_node(node_name, non_mgmt_interface)
                raise

            self.step_manager.print_header("Up the port")
            self.up_interface_of_node(node_name, non_mgmt_interface)
            time.sleep(20)

            self.step_manager.print_header("Check The log when port up")
            message_to_check = [
                "InfiniBandErrorCheck",
                "Device is healthy",
                f'entityValue:"{ib_interface}"',
            ]
            find_match = all(
                message for message in message_to_check if message in self.pod_logs[-2]
            )
            assert find_match, f"Find no expected message in console log:{self.pod_logs}"
            self.logger.info(
                "SUCCESS: 'Device is healthy' message is show when ib port up in pod console log"
            )

            message_to_check = [
                "EthernetErrorCheck",
                "Device is healthy",
                f'entityValue:"{non_mgmt_interface}"',
            ]
            find_match = all(
                message for message in message_to_check if message in self.pod_logs[-1]
            )
            assert find_match, f"Find no expected message in console log:{self.pod_logs}"
            self.logger.info(
                "SUCCESS: 'Device is healthy' message is show when ethernet port up in pod console log"
            )

            self.step_manager.print_header(
                "EthernetErrorCheck will change to False in node condition."
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
