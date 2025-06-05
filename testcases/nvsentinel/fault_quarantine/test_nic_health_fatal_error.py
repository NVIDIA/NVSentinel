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
Module for class of NVsentinel Fault Quarantine:NIC health fatal error/recover
"""

from functools import partial
import time
import os
import random
import threading
import yaml
from testcases.nvsentinel.base import TestNVSentinelCaseBase
import pytest

class TestNICHealthFatalError(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine NIC Health Fatal Error
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.faultquarantine
    def test_nic_health_fatal_error(self, request):
        """NIC fatal error test supporting both AWS (mock) and CSP workflows."""

        self.skip_if_fault_quarantine_deployment_not_found()

        # AWS – use mock filesystem
        if os.getenv("CLOUD_PROVIDER") == "aws":
            self.logger.info("Running on AWS with mock filesystem")
            autosync_fixture = request.getfixturevalue("nvsentinel_autosync_disabled_enabled")
            self.nic_health_fatal_error_in_aws(request)
        else:
            self.logger.info("Running on CSP with original filesystem")
            self.nic_health_fatal_error_in_csp(request)

    def nic_health_fatal_error_in_csp(self, request):
        """Original fatal-error flow using real interfaces."""

        self.step_manager.print_header("Filter out the nodes with more than 2 physical interface on the node")
        nodes_interfaces_dict = self.get_nodes_with_more_than_two_physical_interfaces()
        if not nodes_interfaces_dict:
            pytest.skip("Cannot find a node with more than 2 physical interfaces on the cluster. Skipping this test case.")

        self.node_name = list(nodes_interfaces_dict.keys())[0]
        self.logger.info(f"Selected node for testing: {self.node_name}")

        self._run_fatal_error_flow(request, self.node_name, use_mock=False)

    def nic_health_fatal_error_in_aws(self, request):
        """AWS-specific flow using mock ethernet interface & path."""

        # Choose any node
        nodes, _ = self.client.get_nodes()
        if not nodes:
            pytest.skip("No nodes available for testing")

        self.node_name = random.choice([n.metadata.name for n in nodes])
        self.logger.info(f"Selected node for testing: {self.node_name}")

        # Update SysClassNetPath to mock location and ensure cleanup
        try:
            self.update_nic_monitor_configmap("SysClassNetPath", "/var/run/mock-net")
            request.addfinalizer(self.restore_nic_monitor_configmap)
        except Exception as e:
            pytest.skip(f"Failed to update NIC monitor configmap: {e}")

        # Create mock ethernet interface
        self.mock_interface = f"eth1_test_{random.randint(1000,9999)}"
        self.create_mock_ethernet_interface(self.node_name, self.mock_interface)
        request.addfinalizer(partial(self.cleanup_mock_ethernet_interface, self.node_name))

        # Restart NIC monitor on this node
        self.restart_nic_monitor_pod(self.node_name)

        # Continue with fatal-error flow using mock
        self._run_fatal_error_flow(request, self.node_name, use_mock=True, interface_name=self.mock_interface)

    # ------------------------------------------------------------------
    # Common routine used by both flows
    # ------------------------------------------------------------------

    def _run_fatal_error_flow(self, request, node_name, use_mock=False, interface_name=None):
        """Shared fatal error test logic.

        Args:
            node_name (str): node to test
            use_mock (bool): whether to manipulate mock interface files instead of real NIC
            interface_name (str|None): name of the interface (only for mock path)
        """

        # When real NIC, pick a non-mgmt interface if not provided
        if not use_mock:
            interface_name = self.get_non_mgmt_ports_of_the_node(node_name)
            assert interface_name, f"Cannot find non-mgmt port on node {node_name}"

        # Remove label to allow fault quarantine
        self.remove_managed_by_nvsentinel_label(node_name)
        request.addfinalizer(partial(self.restore_managed_by_nvsentinel_label, node_name))

        # Restart fault-quarantine pod to ensure fresh state
        self.logger.info("Restart the fault-quarantine pod")
        self.delete_fault_quarantine_pod()

        # Simulate NIC DOWN
        self.step_manager.print_header("Simulate fatal error of NIC: down a port")
        if use_mock:
            self.set_mock_ethernet_state(node_name, interface_name, "down")
        else:
            request.addfinalizer(partial(self.up_interface_of_node, node_name, interface_name))
            self.down_interface_of_node(node_name, interface_name)

        # Wait a bit for events to propagate
        time.sleep(20)

        # Validate taints and annotations
        self.step_manager.print_header("Check the node taints and annotations")
        node_info, _ = self.client.get_node_by_name(node_name)
        assert node_info.spec.unschedulable is True
        target_conditions = [{"key": "node.kubernetes.io/unschedulable", "value": None, "effect": "NoSchedule"}]
        assert self.client.check_taints_on_node(node_name, conditions=target_conditions)

        annotations, _ = self.client.get_annotation_on_node(node_name, "quarantineHealthEvent")
        check_name = "EthernetErrorCheck"
        if self.check_if_ib_interface(node_name) and use_mock == False:
            check_name = "InfiniBandErrorCheck"
        assert f'"agent":"nic-health-monitor","componentClass":"NIC","checkName":"{check_name}","isFatal":true' in annotations
        assert self.client.get_annotation_on_node(node_name, "quarantineHealthEventIsCordoned")[0] == "True"

        # Recover NIC
        self.step_manager.print_header("Recover fatal error of NIC: up the port")
        if use_mock:
            self.set_mock_ethernet_state(node_name, interface_name, "up")
        else:
            self.up_interface_of_node(node_name, interface_name)

        time.sleep(20)

        # Validate recovery (annotations and taints cleared)
        self.step_manager.print_header("Check the node status, taints and annotations are removed")
        node_info, _ = self.client.get_node_by_name(node_name)
        assert "node.kubernetes.io/unschedulable" not in str(node_info.spec.taints)
        assert self.client.get_annotation_on_node(node_name, "quarantineHealthEvent")[0] is None
        assert self.client.get_annotation_on_node(node_name, "quarantineHealthEventIsCordoned")[0] is None
        assert node_info.spec.unschedulable is None
