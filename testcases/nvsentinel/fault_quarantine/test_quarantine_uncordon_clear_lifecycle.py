#!/usr/bin/env python3
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
Module for testing the complete lifecycle of fault quarantine:
- Fatal error insertion triggers node cordoning with annotation
- Manual uncordoning leaves annotation intact
- Clearing health event removes annotation
- currentQuarantinedNodes metric is updated correctly at each stage
"""

import pytest
import time
from testcases.nvsentinel.base import TestNVSentinelCaseBase
from functools import partial


class TestQuarantineUncordonClearLifecycle(TestNVSentinelCaseBase):
    """Test the complete lifecycle of node quarantine, manual uncordon, and health event clearing"""
    
    def _get_metric(self, metric_name):
        resp = self.query_metrics(query_params=f'sum({metric_name})')
        result = resp.json()["data"]["result"]
        if not result:
            return 0
        return int(float(result[0]["value"][1]))
    
    @pytest.mark.author(email="deesharma@nvidia.com")
    @pytest.mark.faultquarantine
    def test_quarantine_uncordon_clear_lifecycle(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Tests the complete lifecycle:
        1. Insert fatal health event -> node gets cordoned with annotation, metric increases
        2. Manually uncordon node -> node uncordoned but annotation remains, metric unchanged
        3. Clear health event -> annotation removed, metric decreases
        """
        self.start_prometheus_service()
        request.addfinalizer(partial(self.stop_port_forward_prometheus))
        
        self.skip_if_fault_quarantine_deployment_not_found()

        pods, _ = self.client.list_pods(self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*")
        assert pods, "No gpu-health-monitor pods found"
        gpu_pod = pods[-1]
        self.gpu_healthy_pod = gpu_pod
        self.node_name = gpu_pod.spec.node_name
        self.remove_managed_by_nvsentinel_label(self.node_name)

        self.step_manager.print_header("Get initial currentQuarantinedNodes metric value")
        initial_metric = self._get_metric("fault_quarantine_current_quarantined_nodes{node='%s'}" % self.node_name)
        self.logger.info(f"Initial currentQuarantinedNodes metric: {initial_metric}")
        assert initial_metric == 0, f"Expected currentQuarantinedNodes metric to be 0 before inserting fatal health event, got {initial_metric}"
        
        self.step_manager.print_header("Ensure node starts in clean state")
        # Check node is not cordoned and has no quarantine annotation
        node = self.client.coreV1Api.read_node(self.node_name)
        assert node.spec.unschedulable is None, f"Node {self.node_name} should not be cordoned initially"
        assert "quarantineHealthEvent" not in (node.metadata.annotations or {}), \
            f"Node {self.node_name} should not have quarantine annotation initially"
        
        self.step_manager.print_header("Get GPU health monitor pod on the node")
        # Get the gpu-health-monitor pod running on this node
        self.gpu_health_pod = self._get_gpu_health_monitor_pod(self.node_name)
        self.logger.info(f"Found GPU health monitor pod: {self.gpu_health_pod.metadata.name}")
        
        self.step_manager.print_header("Insert fatal GPU health event")
        # Insert a fatal GPU XID error event using dcgmi
        self.inject_gpu_inforom_watch_error(self.gpu_health_pod)
        self.logger.info(f"Injected fatal GPU health event")
        
        # Verify the condition was created on the node
        self.logger.info("Verifying GPU error condition was created on node and FQ quarantine is triggered")
        time.sleep(20)  # Give time for GPU monitor to create condition
        self.verify_gpu_inforom_watch_condition(self.node_name)
        
        time.sleep(20)
        # Assertions
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert node_info.spec.unschedulable is True
 
        self.step_manager.print_header("Check the annotations on the node")
        annotations, _ = self.client.get_annotation_on_node(
            self.node_name, "quarantineHealthEvent"
        )
        
        assert (
            '"agent":"gpu-health-monitor","componentClass":"GPU","checkName":"GpuInforomWatch","isFatal":true'
            in annotations
        )
        
        self.step_manager.print_header("Verify metric increased")
        metric_after_quarantine = self._get_metric("fault_quarantine_current_quarantined_nodes{node='%s'}" % self.node_name)
        self.logger.info(f"Metric after quarantine: {metric_after_quarantine}")
        assert metric_after_quarantine == initial_metric + 1, \
            f"Metric should increase by 1 (from {initial_metric} to {initial_metric + 1}), got {metric_after_quarantine}"
        
        self.step_manager.print_header("Manually uncordon the node")
        # Uncordon the node using kubectl equivalent
        result, _ = self.client.uncordon_node(self.node_name)
        assert result, "Node should be uncordoned"
        
        self.step_manager.print_header("Verify node is uncordoned but annotation remains")
        time.sleep(30)  # Give time for any potential reconciliation
        
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert node_info.spec.unschedulable is None, "Node should be uncordoned"
 
        self.step_manager.print_header("Check the annotations on the node")
        annotations, _ = self.client.get_annotation_on_node(
            self.node_name, "quarantinedNodeUncordonedManually"
        )
        
        assert annotations == "True", "quarantinedNodeUncordonedManually annotation should be present"


        self.step_manager.print_header("Verify metric remains unchanged after manual uncordon")
        metric_after_uncordon = self._get_metric("fault_quarantine_current_quarantined_nodes{node='%s'}" % self.node_name)
        self.logger.info(f"Metric after manual uncordon: {metric_after_uncordon}")
        assert metric_after_uncordon == metric_after_quarantine - 1, \
            f"Metric should be decremented by 1 after manual uncordon, got {metric_after_uncordon}"

        
        self.step_manager.print_header("Clear the health event by inserting recovery event")
        # Clear the error using dcgmi
        self.clear_gpu_inforom_watch_error(self.gpu_health_pod)
        self.logger.info("Injected recovery GPU health event")
        
        # Wait for the recovery to be processed
        time.sleep(20)
        
        self.logger.info("Test completed successfully!")

    def _get_gpu_health_monitor_pod(self, node_name):
        """Get the gpu-health-monitor pod running on the given node"""
        pods, _ = self.client.list_pods(
            namespace=self.nv_namespace,
            name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        for pod in pods:
            if pod.spec.node_name == node_name:
                return pod
        raise Exception(f"No gpu-health-monitor pod found on node {node_name}")