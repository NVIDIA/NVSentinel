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
Module for class of NVsentinel Fault Quarantine: NodeManagedByLlabel
"""

import time
import pytest
from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestNodeManagedByLabel(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine: NodeManagedByLabel
    """

    @pytest.fixture(autouse=True)
    def setup_gpu_monitor_fatal_error(self, setup_runai_test):
        # Equivalent to setUp in unittest
        self.logger.info("[Setup] gpu_monitor_fatal_error")
        self.gpu_healthy_pod = ""
        try:
            yield
        finally:
            # Equivalent to addCleanup in unittest
            self.logger.info("[Teardown] gpu_monitor_fatal_error")
            self.clear_gpu_fatal_error(self.node_name, "GpuXidError")
            self.delete_fault_quarantine_pod()
    
    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.faultquarantine
    def test_node_managed_by_label(self, request):
        """
        Tests if the node that do not have the label "k8saas.nvidia.com/ManagedByNVSentinel" set to false are not cordoned
        """
        
        self.logger.info("Restart the fault-quarantine pod")
        self.delete_fault_quarantine_pod()

        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        self.gpu_healthy_pod = pods[-1]
        self.node_name = self.gpu_healthy_pod.spec.node_name
        self.logger.info(f"POD  Name: {self.gpu_healthy_pod.metadata.name}")
        self.logger.info(f"Node Name: {self.node_name}")
        backup_label_value, _ = self.client.get_label_on_node(self.node_name, "k8saas.nvidia.com/ManagedByNVSentinel")
        self.logger.info(f"Backup label value: {backup_label_value}")

        # Inject a fatal error on a GPU node with label set to false
        self.step_manager.print_header("Set the label k8saas.nvidia.com/ManagedByNVSentinel to false on the node")
        success, _ = self.client.add_label_to_node(self.node_name, "k8saas.nvidia.com/ManagedByNVSentinel", "false")
        assert success, f"Failed to set the label k8saas.nvidia.com/ManagedByNVSentinel to false on the node: {err}"
        time.sleep(30)

        self.step_manager.print_header("Inject a fatal error on a GPU node")
        self.inject_gpu_inforom_watch_error(self.gpu_healthy_pod)

        time.sleep(30)
        self.step_manager.print_header("Verify that node condition GPUInforomWatch is True")
        self.verify_gpu_inforom_watch_condition(self.node_name)

        self.step_manager.print_header("Check if the node is not cordoned")
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert node_info.spec.unschedulable is None, f"FAIL: Node {self.node_name} is cordoned"

        self.step_manager.print_header("Clear the fatal error")
        self.clear_gpu_inforom_watch_error(self.gpu_healthy_pod)
        time.sleep(30)


        # Inject a fatal error on a GPU node with label set to true
        self.step_manager.print_header("Add the label k8saas.nvidia.com/ManagedByNVSentinel to the node")
        self.client.add_label_to_node(self.node_name, "k8saas.nvidia.com/ManagedByNVSentinel", "true")

        self.step_manager.print_header("Inject a fatal error on a GPU node")
        self.inject_gpu_inforom_watch_error(self.gpu_healthy_pod)

        time.sleep(30)
        self.verify_gpu_inforom_watch_condition(self.node_name)

        self.step_manager.print_header("Check if the node is cordoned")
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert node_info.spec.unschedulable is True, f"FAIL: Node {self.node_name} is not cordoned"
        self.clear_gpu_inforom_watch_error(self.gpu_healthy_pod)

        # Inject a fatal error on a GPU node with label removed
        self.step_manager.print_header("Remove the label k8saas.nvidia.com/ManagedByNVSentinel from the node")
        self.client.remove_label_from_node(self.node_name, "k8saas.nvidia.com/ManagedByNVSentinel")

        self.step_manager.print_header("Inject a fatal error on a GPU node")
        self.inject_gpu_inforom_watch_error(self.gpu_healthy_pod)
        
        time.sleep(30)
        self.verify_gpu_inforom_watch_condition(self.node_name)

        self.step_manager.print_header("Check if the node is cordoned")
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert node_info.spec.unschedulable is True, f"FAIL: Node {self.node_name} is not cordoned"

        self.step_manager.print_header("Clear the fatal error")
        self.clear_gpu_inforom_watch_error(self.gpu_healthy_pod)

        if backup_label_value:
            self.client.add_label_to_node(self.node_name, "k8saas.nvidia.com/ManagedByNVSentinel", backup_label_value)
        else:
            self.client.remove_label_from_node(self.node_name, "k8saas.nvidia.com/ManagedByNVSentinel")

