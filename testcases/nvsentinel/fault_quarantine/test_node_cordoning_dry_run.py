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
Module for class of NVsentinel Fault Quarantine:dry run mode 
"""

import os
import pytest
import time
import json
from testcases.nvsentinel.base import TestNVSentinelCaseBase
from kubernetes import client as k8s_client
from functools import partial

class TestNodeCordoningInDryRun(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine dry run mode 
    """

    # Hold original deployment spec so we can restore after test
    original_deployment_data = None

    # -------------------- Helper utilities --------------------

    def _get_metric(self, metric_name):
        """Return integer value of a Prometheus metric (sum over all pods)."""
        resp = self.query_metrics(query_params=f'sum({metric_name})')
        result = resp.json()["data"]["result"]
        if not result:
            return 0
        return int(float(result[0]["value"][1]))

    @pytest.mark.author(email="tanishag@nvidia.com")
    @pytest.mark.faultquarantine
    #def test_node_cordoning_dry_run(self, request, nvsentinel_autosync_disabled_enabled):
    def test_node_cordoning_dry_run(self, request):
        """
        Integration validation for Fault-Quarantine dry-run mode.

        FQM must apply taints / annotations while not cordoning the node due to dry-run.
        """
        self.skip_if_fault_quarantine_deployment_not_found()

        # Enable dry-run mode via deployment patch before redeploying
        self.step_manager.print_header("Enable dry-run mode in fault-quarantine deployment")
        self._enable_dry_run_mode()
        request.addfinalizer(partial(self._restore_dry_run_mode))
         # Allow some time to update the cache and metrics
        time.sleep(10)


        self.step_manager.print_header("Get a GPU health-monitor pod (used for DCGM injection)")
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        assert pods, "No gpu-health-monitor pods found"
        gpu_pod = pods[-1]
        self.gpu_healthy_pod = gpu_pod
        self.node_name = gpu_pod.spec.node_name

        self.logger.info(f"GPU monitor pod: {gpu_pod.metadata.name} on node {self.node_name}")

        self.step_manager.print_header(f"Remove managed-by-nvsentinel label from node {self.node_name}")
        self.remove_managed_by_nvsentinel_label(self.node_name)
        request.addfinalizer(partial(self.restore_managed_by_nvsentinel_label, self.node_name))
        time.sleep(10)

        self.start_prometheus_service()
        request.addfinalizer(partial(self.stop_port_forward_prometheus))
        # Metric baseline
        self.logger.info("Get the metric baseline")
        q_before = self._get_metric("fault_quarantine_current_quarantined_nodes")

        self.step_manager.print_header("inject GPU Inforom fatal error on clean node")
        self.inject_gpu_inforom_watch_error(self.gpu_healthy_pod)
        request.addfinalizer(partial(self.clear_gpu_inforom_watch_error, self.gpu_healthy_pod))

        self.logger.info(f"Waiting for 30 seconds to allow FQM to reconcile")
        time.sleep(30)
        
        self.step_manager.print_header("Checking the node status, taints and annotations")
        # Node should NOT be cordoned in dry-run
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert node_info.spec.unschedulable is None

        self.logger.info("Check the annotations on the node")
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

        q_after = self._get_metric("fault_quarantine_current_quarantined_nodes")

        assert q_after >= q_before + 1, "current quarantined gauge did not increase as expected"