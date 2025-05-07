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
Module for class of NVsentinel Fault Quarantine:Ruleset priority
"""

import os
import time
import pytest
from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestRulesetPriority(TestNVSentinelCaseBase):
    """Class for test case of Fault Quarantine:Ruleset priority"""

    backup_cm = "backup_fault_quarantine_cm.yaml"

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
            command = [
                "/bin/sh",
                "-c",
                "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 84 -v 1",
            ]
            output, _ = self.client.exec_command_in_pod(self.gpu_healthy_pod, command)
            assert "Successfully injected" in output
            self.client.apply_configmap(self.backup_cm)
            self.delete_fault_quarantine_pod()

    @pytest.mark.author(email="ajmishra@nvidia.com")
    #@pytest.mark.faultquarantine
    def test_ruleset_priority(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Tests if the node is cordoned and tainted when the node is matched with the rule, and the node is recovered when the error is cleared
        """
        self.step_manager.print_header("Backup the default fault quarantine config map")
        self.client.backup_configmap(
            "nvsentinel", "fault-quarantine-config", self.backup_cm
        )

        self.step_manager.print_header(
            "Redeploy fault quarantine with a new config map of ruleset priority"
        )
        cm_yaml = os.path.join(
            os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "ruleset_priority.yaml"
        )
        self.client.apply_configmap(cm_yaml)
        self.logger.info("Restart the fault-quarantine pod")
        self.delete_fault_quarantine_pod()
        time.sleep(15)
        cm, _ = self.client.get_configmap("nvsentinel", "fault-quarantine-config")
        assert "Higher priority than Rule 1" in str(cm.data)
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        self.gpu_healthy_pod = pods[-1]
        self.node_name = self.gpu_healthy_pod.spec.node_name
        self.logger.info(f"POD  Name: {self.gpu_healthy_pod.metadata.name}")
        self.logger.info(f"Node Name: {self.node_name}")

        self.step_manager.print_header("Inject a fatal error on a GPU node")
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )

        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 84 -v 0",
        ]
        output, _ = self.client.exec_command_in_pod(self.gpu_healthy_pod, command)
        assert "Successfully injected" in output

        time.sleep(20)

        self.step_manager.print_header("Check the node taints and annotations")
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert node_info.spec.unschedulable is True
        self.logger.info("Check the taints on the node")
        target_conditions = [
            {
                "key": "AggregatedNodeHealth",
                "value": "False",
                "effect": "PreferNoSchedule",
            },
            {
                "key": "node.kubernetes.io/unschedulable",
                "value": None,
                "effect": "NoSchedule",
            },
        ]
        assert self.client.check_taints_on_node(
            self.node_name, conditions=target_conditions
        )

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
                self.node_name, "quarantineHealthEventAppliedTaints"
            )[0]
            == '[{"Key":"AggregatedNodeHealth","Value":"False","Effect":"PreferNoSchedule"}]'
        )
        assert (
            self.client.get_annotation_on_node(
                self.node_name, "quarantineHealthEventIsCordoned"
            )[0]
            == "True"
        )
