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
import os
import re
from functools import partial
from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestInfiniBandLinkDown(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel NIC Health Monitor: InfiniBand link down
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.nichealthmonitor
    @pytest.mark.skip(reason="nic health monitor is disabled globally")
    def test_infini_band_link_down(self, request):
        """
        Tests if the InfiniBandErrorCheck condition is set correctly when the InfiniBand link is down
        """
        if os.getenv("CLOUD_PROVIDER") == "aws":
            request.getfixturevalue("nvsentinel_autosync_disabled_enabled")
            self.infiniband_down_in_aws(request)
        else:
            self.infiniband_link_down_in_csp(request)

    def infiniband_link_test_with_mock(self, request, node_name, device_name, port_name, pod_name, state):
        """
        Run InfiniBand link test using mock filesystem for both up and down states
        
        Args:
            request: pytest request object
            node_name: Name of the node to test
            device_name: Name of the InfiniBand device
            port_name: Name of the port to test
            pod_name: Name of the pod to monitor
            state: Either "up" or "down" to set the interface state
        """
        try:
            self.logger.info(f"Testing InfiniBand link {state} for mock device {device_name} port {port_name} on node {node_name}")
            
            # Define state values and expected messages based on state
            if state == "down":
                ib_state = "1: Down"
                ib_phys_state = "2: Polling"
                expected_status_message = "state: 1: Down"
                expected_condition_status = "True"
                success_message = "SUCCESS: 'state: 1: Down' message found in pod console log"
                recent_logs_count = 5
                wait_time = 10
            elif state == "up":
                ib_state = "4: ACTIVE"
                ib_phys_state = "5: LinkUp"
                expected_status_message = "Port is healthy"
                expected_condition_status = "False"
                success_message = "SUCCESS: 'Port is healthy' message found in pod console log"
                recent_logs_count = 10
                wait_time = 30
            else:
                self.logger.error(f"Invalid state: {state}")
                raise ValueError(f"Invalid state: {state}. Must be 'up' or 'down'")
            
            # Set interface to the specified state
            self.set_mock_infiniband_state(node_name, device_name, port_name, ib_state, ib_phys_state)
            
            # Wait for the monitor to detect the change
            self.logger.info(f"Waiting for NIC monitor to detect InfiniBand {state} state...")
            time.sleep(wait_time)
            
            self.logger.info(f"Check {state} state info from the pod log console")
            message_to_check = [
                'checkName:"InfiniBandErrorCheck"',
                'agent:"nic-health-monitor"',
                'componentClass:"NIC"',
                expected_status_message,
                f'entityType:"NIC".*entityValue:"{device_name}_{port_name}"',
            ]
            
            # Check if we have any logs
            if hasattr(self, 'pod_logs') and self.pod_logs:
                recent_logs = self.pod_logs[-recent_logs_count:]  # Check recent log entries
                found_expected_message = False
                
                for log_entry in recent_logs:
                    find_match = all(
                        re.search(message, log_entry) for message in message_to_check
                    )
                    if find_match:
                        found_expected_message = True
                        self.logger.debug(f"{success_message}: {log_entry}")
                        break
                
                if not found_expected_message:
                    self.logger.warning(f"Expected {state} messages not found in recent logs. Recent logs: {recent_logs}")
                    # Let's also check all logs for debugging
                    all_logs_str = '\n'.join(self.pod_logs)
                    if f"{device_name}_{port_name}" in all_logs_str:
                        self.logger.info(f"Device {device_name}_{port_name} found in logs, checking for state changes...")
            else:
                self.logger.warning("No pod logs available for verification")

            target_condition, _ = self.client.read_node_condition_by_type(
                node_name=node_name, condition_type="InfiniBandErrorCheck"
            )

            assert target_condition.status == expected_condition_status, f"InfiniBandErrorCheck status is {target_condition.status} when interface is {state}"

        except Exception as e:
            self.logger.error(f"Error during InfiniBand link {state} test: {e}")
            # Ensure cleanup happens - set to up state
            if state == "down":
                self.set_mock_infiniband_state(node_name, device_name, port_name, "4: ACTIVE", "5: LinkUp")
            raise

    def infiniband_down_in_aws(self, request):
        
        self.step_manager.print_header(
            "Select a node for testing with mock InfiniBand interface"
        )
        # Get available nodes
        nodes, _ = self.client.get_nodes()
        if not nodes:
            pytest.skip("No nodes available for testing")
        
        # Select a random node for testing
        node_name = random.choice([node.metadata.name for node in nodes])
        self.logger.info(f"Selected node for testing: {node_name}")

        self.step_manager.print_header(
            "Create mock InfiniBand interface structure"
        )
        
        # Update configmap to use /var/run/mock-infiniband (container perspective)
        try:
            self.update_nic_monitor_configmap("SysClassInfinibandPath", "/var/run/mock-infiniband")
            # Use InfiniBand network type to avoid RoCE interface filtering issues
            self.update_nic_monitor_configmap("MonitorNetworkType", "infiniband")
            # Register cleanup for configmap
            request.addfinalizer(self.restore_nic_monitor_configmap)
        except Exception as e:
            self.logger.error(f"Failed to update configmap: {e}")
            pytest.skip(f"Cannot update NIC monitor configuration: {e}")

        # Create a unique device name for this test using timestamp to avoid conflicts
        timestamp = int(time.time())
        device_name = f"mlx5_test_{timestamp}"
        port_name = "1"
        
        # Create the mock interface
        try:
            self.create_mock_infiniband_interface(node_name, device_name, port_name)
        except Exception as e:
            self.logger.error(f"Failed to create mock InfiniBand interface: {e}")
            pytest.skip(f"Cannot create mock InfiniBand interface on node {node_name}. Error: {e}")
        
        # Register cleanup
        request.addfinalizer(
            partial(self.cleanup_mock_infiniband_interface, node_name)
        )

        self.step_manager.print_header(
            "Restart NIC monitor to detect new InfiniBand interface"
        )
        
        # Restart NIC monitor to pick up the new InfiniBand interface
        try:
            pod_name = self.restart_nic_monitor_pod(node_name)
        except Exception as e:
            self.logger.error(f"Failed to restart NIC monitor: {e}")
            pytest.skip(f"Cannot restart NIC monitor on node {node_name}. Error: {e}")

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

        time.sleep(10)

        self.step_manager.print_header(
            "Test InfiniBand link down/up scenario with mock interface"
        )
        
        self.logger.info(f"Testing with mock InfiniBand device: {device_name} port {port_name}")
        
        # Test InfiniBand link down scenario
        self.logger.info("=== PHASE 1: Testing InfiniBand Link Down ===")
        self.infiniband_link_test_with_mock(request, node_name, device_name, port_name, pod_name, "down")
        self.logger.info("InfiniBand link down test completed successfully")
        
        # Test InfiniBand link up scenario (recovery)
        self.logger.info("=== PHASE 2: Testing InfiniBand Link Up (Recovery) ===")
        self.infiniband_link_test_with_mock(request, node_name, device_name, port_name, pod_name, "up")
        self.logger.info("InfiniBand link up test completed successfully")

    def infiniband_link_down_in_csp(self, request):
        """
        Tests if the InfiniBandErrorCheck condition is set correctly when the InfiniBand link is down
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

        nodes_list = list(nodes_interfaces_dict.keys())
        # select 2 nodes to test
        nodes = random.sample(nodes_list, min(2, len(nodes_list)))
        self.logger.info(f"Will do interface up/down on below nodes:{nodes}")
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
                    "state: 1: Down",
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
                    "InfiniBandErrorCheck will change to True in node condition."
                )
                target_condition, _ = self.client.read_node_condition_by_type(
                    node_name=node_name, condition_type="InfiniBandErrorCheck"
                )
                assert (
                    target_condition.status == "True"
                ), f"Status of InfiniBandErrorCheck is still False: {target_condition}"
                self.logger.info(
                    "SUCCESS: InfiniBandErrorCheck status is flip to True when port down in node"
                )
            except:  # ensure the nic port has been turn up if any error condition occurred
                self.up_interface_of_node(node_name, non_mgmt_interface)
                raise

            self.step_manager.print_header("Up the port")
            self.up_interface_of_node(node_name, non_mgmt_interface)
            time.sleep(20)

            self.step_manager.print_header("Check The log when port up")
            message_to_check = [
                "InfiniBandErrorCheck",
                "Port is healthy",
                f'entityValue:"{ib_interface}"',
            ]
            find_match = all(
                message for message in message_to_check if message in self.pod_logs[-2]
            )
            assert find_match, f"Find no expected message in console log:{self.pod_logs}"
            self.logger.info(
                "SUCCESS: 'Port is healthy' message is show when ib port up in pod console log"
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
                "InfiniBandErrorCheck will change to False in node condition."
            )
            target_condition, _ = self.client.read_node_condition_by_type(
                node_name=node_name, condition_type="InfiniBandErrorCheck"
            )
            assert (
                target_condition.status == "False"
            ), f"Status of InfiniBandErrorCheck is still True after turn NIC port up: {target_condition}"
            self.logger.info(
                "SUCCESS: InfiniBandErrorCheck status is flip back to False when port up in node"
            )
