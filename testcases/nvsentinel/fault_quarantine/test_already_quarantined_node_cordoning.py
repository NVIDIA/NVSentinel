# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

from testcases.nvsentinel.base import TestNVSentinelCaseBase
import os
import time
import json
import pytest
from kubernetes import client as k8s_client
from functools import partial

class TestPreQuarantinedNodeCordoning(TestNVSentinelCaseBase):
    """Case: node is already quarantined manually; ensure FQ dry-run still adds taint / annotation but keeps cordon state."""

    original_deployment_data = None

    def _get_metric(self, metric_name):
        resp = self.query_metrics(query_params=f'sum({metric_name})')
        result = resp.json()["data"]["result"]
        if not result:
            return 0
        return int(float(result[0]["value"][1]))

    def _cordon_and_taint_node_manually(self, node_name):
        body = {"spec": {"unschedulable": True}}
        self.client.coreV1Api.patch_node(node_name, body)
        taint_key = "AggregatedNodeHealth"
        node = self.client.coreV1Api.read_node(node_name)
        taints = node.spec.taints or []
        if not any(t.key == taint_key and t.effect == "PreferNoSchedule" for t in taints):
            taints.append(k8s_client.V1Taint(key=taint_key, value="False", effect="PreferNoSchedule"))
            patch = {"spec": {"taints": [t.to_dict() for t in taints]}}
            self.client.coreV1Api.patch_node(node_name, patch)

    def _uncordon_node_manually(self, node_name):
        self.logger.info("Uncordoning the node: %s", node_name)
        try:
            # (1) Make the node schedulable again
            body = {"spec": {"unschedulable": False}}
            self.client.coreV1Api.patch_node(node_name, body)

            # (2) Remove the quarantine taint if it exists
            node = self.client.coreV1Api.read_node(node_name)
            taints = node.spec.taints or []
            filtered_taints = [
                t for t in taints if not (t.key == "AggregatedNodeHealth" and t.effect == "PreferNoSchedule")
            ]

            if len(filtered_taints) != len(taints):
                patch = {"spec": {"taints": [t.to_dict() for t in filtered_taints]}}
                self.client.coreV1Api.patch_node(node_name, patch)

            self.logger.info("Successfully uncordoned and untainted node %s", node_name)
        except Exception as exc:
            self.logger.error("Failed to uncordon node %s: %s", node_name, exc)

    # ---------------------------------------------------
    @pytest.mark.author(email="tanishag@nvidia.com")
    @pytest.mark.faultquarantine
    def test_already_quarantined_node_cordoning(self, request):
        self.skip_if_fault_quarantine_deployment_not_found()

        # Select gpu-health-monitor pod and node
        pods, _ = self.client.list_pods(self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*")
        assert pods, "No gpu-health-monitor pods found"
        gpu_pod = pods[-1]
        self.gpu_healthy_pod = gpu_pod
        self.node_name = gpu_pod.spec.node_name
        self.remove_managed_by_nvsentinel_label(self.node_name)
        
        request.addfinalizer(partial(self.restore_managed_by_nvsentinel_label, self.node_name))      

        self.step_manager.print_header(
            'Follow "Prometheus metrics for nvsentinel pods", make sure that promtool is installed and prometheus port 9090 is accessible.'
        )

        # Manually quarantine node
        self.step_manager.print_header(f"Manually cordoning the node: {self.node_name}")
        self._cordon_and_taint_node_manually(self.node_name)
        time.sleep(10)

        self.start_prometheus_service()
        request.addfinalizer(partial(self.stop_port_forward_prometheus))
        time.sleep(10)
        self.logger.info("Get the metric baseline")
        # Baseline metrics
        cur_before = self._get_metric("fault_quarantine_current_quarantined_nodes")
        total_before = self._get_metric("fault_quarantine_nodes_quarantined_total")
        
        self.step_manager.print_header(f"Inject GPU Inforom fatal error on {self.gpu_healthy_pod.metadata.name}")
        self.inject_gpu_inforom_watch_error(self.gpu_healthy_pod)

        self.logger.info(f"Waiting for 30 seconds to allow FQM to reconcile")
        time.sleep(30)

        # Assertions
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert node_info.spec.unschedulable is True, "Node lost cordon state"
        target_conditions = [
            {
                "key": "node.kubernetes.io/unschedulable",
                "value": None,
                "effect": "NoSchedule",
            },
        ]
        
        self.step_manager.print_header(f"Check the taints on the node")
        assert self.client.check_taints_on_node(
            self.node_name, conditions=target_conditions
        )
        self.step_manager.print_header("Check the annotations on the node")
        annotations, _ = self.client.get_annotation_on_node(
            self.node_name, "quarantineHealthEvent"
        )
        
        assert (
            '"agent":"gpu-health-monitor","componentClass":"GPU","checkName":"GpuInforomWatch","isFatal":true'
            in annotations
        )

        assert (
            self.client.get_annotation_on_node(
                self.node_name, "quarantineHealthEventIsCordoned"
            )[0]
            == "True"
        )

        self.step_manager.print_header("Check the metric after injecting the GPU Inforom fatal error")
        cur_after = self._get_metric("fault_quarantine_current_quarantined_nodes")
        total_after = self._get_metric("fault_quarantine_nodes_quarantined_total")
        
        assert cur_after >= cur_before + 1, "current_quarantined gauge not incremented"
        assert total_after >= total_before, "nodes_quarantined_total counter not incremented"

        self.clear_gpu_inforom_watch_error(self.gpu_healthy_pod)
        time.sleep(10)
        self._uncordon_node_manually(self.node_name)