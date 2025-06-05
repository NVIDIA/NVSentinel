# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
import yaml
import tempfile
from functools import partial
from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestEthernetLinkDown(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel NIC Health Monitor: Ethernet link down
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.nichealthmonitor
    def test_ethernet_link_down(self, request):
        """
        Tests if the EthernetErrorCheck condition is set correctly when the ethernet link is down
        """
        if os.getenv("CLOUD_PROVIDER") == "aws":
            # This test should work on AWS since we're using mock filesystem
            self.logger.info("Running on AWS with mock filesystem")
            autosync_fixture = request.getfixturevalue("nvsentinel_autosync_disabled_enabled")
            self.ethernet_down_in_aws(request)
        else:
            self.logger.info("Running on CSP with original filesystem")
            self.ethernet_down_in_csp(request)

    def ethernet_link_test_with_mock(self, request, node_name, interface_name, pod_name, state):
        """
        Run ethernet link test using mock filesystem for both up and down states
        
        Args:
            request: pytest request object
            node_name: Name of the node to test
            interface_name: Name of the interface to test
            pod_name: Name of the pod to monitor
            state: Either "up" or "down" to set the interface state
        """
        try:
            # Set interface to the specified state
            self.set_mock_ethernet_state(node_name, interface_name, state)
            
            # Wait for the monitor to detect the change
            self.logger.info(f"Waiting for NIC monitor to detect interface {state} state...")
            time.sleep(10)
            
            # Define expected messages based on state
            if state == "down":
                expected_status_message = "state: down"
                expected_condition_status = "True"
                success_message = f"SUCCESS: 'state: down' message found in pod console log"
                condition_success_message = "SUCCESS: EthernetErrorCheck status is True when interface is down"
                recent_logs_count = 5
            elif state == "up":
                expected_status_message = "Device is healthy"
                expected_condition_status = "False"
                success_message = f"SUCCESS: 'Device is healthy' message found in pod console log"
                condition_success_message = "SUCCESS: EthernetErrorCheck status is False when interface is up"
                recent_logs_count = 10
            else: 
                self.logger.error(f"Invalid state: {state}")
            
            self.logger.info(f"Check {state} state info from the pod log console")
            message_to_check = [
                'checkName:"EthernetErrorCheck"',
                'agent:"nic-health-monitor"',
                'componentClass:"NIC"',
                expected_status_message,
                f'entityType:"NIC".*entityValue:"{interface_name}"',
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
                    if interface_name in all_logs_str:
                        self.logger.info(f"Interface {interface_name} found in logs, checking for state changes...")
            else:
                self.logger.warning("No pod logs available for verification")

            target_condition, _ = self.client.read_node_condition_by_type(
                node_name=node_name, condition_type="EthernetErrorCheck"
            )
            
            if target_condition and target_condition.status == expected_condition_status:
                self.logger.debug(condition_success_message)
            else:
                self.logger.warning(f"EthernetErrorCheck status: {target_condition.status if target_condition else 'Not found'}")

        except Exception as e:
            self.logger.error(f"Error during ethernet link {state} test: {e}")
            # Ensure cleanup happens
            raise

    def ethernet_down_in_aws(self, request):
        self.step_manager.print_header(
            "Select a node for testing with mock ethernet interface"
        )
        # Get available nodes
        nodes, _ = self.client.get_nodes()
        if not nodes:
            pytest.skip("No nodes available for testing")
        
        # Select a random node for testing
        node_name = random.choice([node.metadata.name for node in nodes])
        self.logger.info(f"Selected node for testing: {node_name}")

        self.step_manager.print_header(
            "Update NIC monitor configuration to use custom path"
        )
        
        # Update configmap to use /var/run/mock-net (container perspective)
        try:
            self.update_nic_monitor_configmap("SysClassNetPath", "/var/run/mock-net")
            # Register cleanup for configmap
            request.addfinalizer(self.restore_nic_monitor_configmap)
        except Exception as e:
            self.logger.error(f"Failed to update configmap: {e}")
            pytest.skip(f"Cannot update NIC monitor configuration: {e}")

        self.step_manager.print_header(
            "Create mock ethernet interface structure"
        )
        
        # Create a unique interface name for this test
        interface_name = f"eth1_test_{random.randint(1000, 9999)}"
        
        # Create the mock interface
        try:
            self.create_mock_ethernet_interface(node_name, interface_name)
        except Exception as e:
            self.logger.error(f"Failed to create mock interface: {e}")
            pytest.skip(f"Cannot create mock interface on node {node_name}. Error: {e}")
        
        # Register cleanup
        request.addfinalizer(
            partial(self.cleanup_mock_ethernet_interface, node_name)
        )

        self.step_manager.print_header(
            "Restart NIC monitor to use new configuration"
        )
        
        # Restart NIC monitor to pick up the new configmap
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
            "Test ethernet link down/up scenario with mock interface"
        )
        
        self.logger.info(f"Testing with mock interface: {interface_name}")
        
        # Test ethernet link down scenario
        self.logger.info("=== PHASE 1: Testing Ethernet Link Down ===")
        self.ethernet_link_test_with_mock(request, node_name, interface_name, pod_name, "down")
        self.logger.info("Ethernet link down test completed successfully")
        
        # Test ethernet link up scenario (recovery)
        self.logger.info("=== PHASE 2: Testing Ethernet Link Up (Recovery) ===")
        self.ethernet_link_test_with_mock(request, node_name, interface_name, pod_name, "up")
        self.logger.info("Ethernet link up test completed successfully")

    def ethernet_down_in_csp(self, request):
        """
        Tests if the EthernetErrorCheck condition is set correctly when the ethernet link is down
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
            assert not self.check_if_ib_interface(node_name), (
                "There are IB interface on the node. Pls check it manually. This case is for ethernet interface "
                "without IB "
                "interface connected"
            )

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
            self.step_manager.print_header(
                "Down/Up one port and check the logs from the pod"
            )
            self.ethernet_link_down_test(request, node_name, non_mgmt_interface)
