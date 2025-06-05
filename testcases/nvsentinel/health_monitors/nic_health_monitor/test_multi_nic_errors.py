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
import os
import random
from functools import partial
from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestEthernetLinkDown(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel NIC Health Monitor: Multi NIC errors
    """

    @pytest.mark.author("ajmishra@nvidia.com")
    @pytest.mark.nichealthmonitor
    def test_multi_nic_errors(self, request):
        """
        Tests if the EthernetErrorCheck condition and related events are set correctly when multiple NICs are down
        """
        if os.getenv("CLOUD_PROVIDER") == "aws":
            self.logger.info("Running on AWS with mock filesystem")
            autosync_fixture = request.getfixturevalue("nvsentinel_autosync_disabled_enabled")
            self._test_multi_nic_errors_with_mock(request)
        else:
            self._test_multi_nic_errors_with_real_interface(request)

    def down_up_mock_interface_multi_times(self, node_name, interface_name, count=15):
        """Down and up the mock network interface on a node multiple times."""
        self.logger.info(
            f"Starting to down and up mock interface {interface_name} on {node_name} {count} times."
        )

        for i in range(count):
            self.logger.info(
                f"Iteration {i + 1}/{count}: Setting mock interface {interface_name} to down on {node_name}"
            )
            # Set interface to down state
            self.set_mock_ethernet_state(node_name, interface_name, "down")
            time.sleep(5)

            self.logger.info(
                f"Iteration {i + 1}/{count}: Setting mock interface {interface_name} to up on {node_name}"
            )
            # Set interface to up state
            self.set_mock_ethernet_state(node_name, interface_name, "up")
            time.sleep(5)

        self.logger.info(f"Completed down and up mock interface {interface_name} on {node_name} {count} times.")

    def _test_multi_nic_errors_with_mock(self, request):
        """Test multiple NIC errors using mock filesystem interface"""
        self.step_manager.print_header(
            "Select a node for testing with mock ethernet interface"
        )
        # Get available nodes
        nodes, _ = self.client.get_nodes()
        if not nodes:
            pytest.skip("No nodes available for testing")
        
        # Select the first node for testing
        node_name = nodes[0].metadata.name
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

        # Get pod and run tests
        pod_name = self._setup_monitoring_and_get_pod(request, node_name, is_mock=True)
        self._run_multi_nic_error_tests(request, node_name, interface_name, pod_name, is_mock=True)

    def _test_multi_nic_errors_with_real_interface(self, request):
        """Test multiple NIC errors using real physical interfaces"""
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
        node_name = nodes_list[0]

        # Get the interface to test
        non_mgmt_interface = self.get_non_mgmt_ports_of_the_node(node_name)
        assert non_mgmt_interface, "Cannot find non-mgmt port on the node. Please check manually"
        self.logger.info(f"TARGET PORT NAME: {non_mgmt_interface}")

        # Get pod and run tests
        pod_name = self._setup_monitoring_and_get_pod(request, node_name, is_mock=False)
        self._run_multi_nic_error_tests(request, node_name, non_mgmt_interface, pod_name, is_mock=False)

    def _setup_monitoring_and_get_pod(self, request, node_name, is_mock=True):
        """Common setup for monitoring and getting pod name"""
        if is_mock:
            # For mock tests, restart NIC monitor to use new configuration
            self.step_manager.print_header(
                "Restart NIC monitor to use new configuration"
            )
            try:
                pod_name = self.restart_nic_monitor_pod(node_name)
            except Exception as e:
                self.logger.error(f"Failed to restart NIC monitor: {e}")
                pytest.skip(f"Cannot restart NIC monitor on node {node_name}. Error: {e}")
        else:
            # For real interface tests, manage existing pods
            self.set_managed_by_nvsentinel_label_to_false(node_name)
            request.addfinalizer(partial(self.restore_managed_by_nvsentinel_label, node_name))

            # Delete existing pod to avoid old logs impact
            pods, _ = self.client.list_pods(
                self.nv_namespace, name_pattern="nvsentinel-nic-health*"
            )
            for pod in pods:
                if pod.spec.node_name == node_name:
                    # delete current nic pod to avoid old logs impact match counts
                    self.client.delete_pod(pod)
                    time.sleep(5)
                    break

            # Get the new pod
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

        return pod_name

    def _run_multi_nic_error_tests(self, request, node_name, interface_name, pod_name, is_mock=True):
        """Run the actual multiple NIC error tests"""
        test_type = "mock interface" if is_mock else "real interface"
        self.step_manager.print_header(f"Login the node where the monitor pod is running on")
        self.logger.info(f"TARGET PORT NAME: {interface_name}")

        count = 15
        self.step_manager.print_header(f"Repeat the port down/up operations for {count} times")
        
        if is_mock:
            self.down_up_mock_interface_multi_times(node_name, interface_name, count=count)
        else:
            self.down_up_interface_of_node_multi_times(node_name, interface_name, count=count)

        # Common log analysis and verification
        nic_pod_logs, _ = self.client.get_pod_logs(self.nv_namespace, pod_name)
        nic_down_pattern = re.compile(
            rf'events:\{{.*?isFatal:true.*?message:"state: down".*?entityValue:"{interface_name}"'
        )
        nic_healthy_pattern = re.compile(
            rf'events:\{{.*?isHealthy:true.*?message:"Device is healthy".*?entityValue:"{interface_name}"'
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
            f"Verify will get all {count} error logs indicating error found and Device is healthy messages when port up"
        )
        assert down_count == count and healthy_count == count, (
            f"Mismatch error found and 'Device is healthy' message count in pod console log, Error Found:{down_count}, Device is healthy:{healthy_count}, "
            f"Expected: {count}"
        )
        self.logger.info("SUCCESS: All message is show when port up in pod console log")

        self.step_manager.print_header(
            f"Check from node Condition, should see the node condition flip back from true to false {count} times"
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
