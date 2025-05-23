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
Module for class of NVsentinel Fault Quarantine:complex ruleset
"""

import os
import pytest
import time
from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestComplexRuleset(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine complex ruleset
    """

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
            fault_quarantine_deployment = self.client.get_deployments(self.nv_namespace, "nvsentinel-fault-quarantine")
            if not fault_quarantine_deployment:
                return
            self.clear_gpu_fatal_error(self.node_name, "GpuInforomWatch")
            self.client.apply_configmap(self.backup_cm)
            self.delete_fault_quarantine_pod()

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.faultquarantine
    def test_complex_ruleset(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Tests if fault quarantine pod is correctly deployed and handles the complex ruleset correctly with respect to the priority of the ruleset
        Also tests if the node is cordoned and tainted when the node is matched with the rule
        """
        self.skip_if_fault_quarantine_deployment_not_found()
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
        time.sleep(30)
        cm, _ = self.client.get_configmap("nvsentinel", "fault-quarantine-config")
        assert "Higher priority than Rule 1" in str(cm.data)

        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        self.gpu_healthy_pod = pods[-1]
        self.node_name = self.gpu_healthy_pod.spec.node_name
        self.remove_managed_by_nvsentinel_label(self.node_name)
        self.logger.info(f"POD  Name: {self.gpu_healthy_pod.metadata.name}")
        self.logger.info(f"Node Name: {self.node_name}")

        for errorcode in ["143", "152"]:
            self.delete_fault_quarantine_pod()
            time.sleep(20)
            self.step_manager.print_header(
                f"Inject a fatal error of {errorcode} on a GPU node matched with rule 2"
            )
            command = [
                "/bin/sh",
                "-c",
                f"dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 1 -f 230 -v {errorcode}",
            ]
            output, _ = self.client.exec_command_in_pod(self.gpu_healthy_pod, command)
            assert "Successfully injected" in output

            time.sleep(30)

            self.step_manager.print_header("Check the node taints and annotations")
            node_info, _ = self.client.get_node_by_name(self.node_name)
            assert node_info.spec.unschedulable is None
            self.logger.info("Check the taints on the node")
            target_conditions = [
                {"key": "GPUHealth", "value": "False", "effect": "PreferNoSchedule"}
            ]
            assert self.client.check_taints_on_node(
                self.node_name, conditions=target_conditions
            )

            self.step_manager.print_header("Clear the injected error")
            command = ["/bin/sh", "-c", "python3 clear_xid_error_health_event.py"]
            result = self.client.exec_command_in_pod(self.gpu_healthy_pod, command)

            
        self.step_manager.print_header(
            f"Inject a fatal error of {errorcode} on a GPU node which doesn't match with any rule"
        )

        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 1 -f 230 -v 153",
        ]
        output, _ = self.client.exec_command_in_pod(self.gpu_healthy_pod, command)
        assert "Successfully injected" in output

        time.sleep(50)
        self.step_manager.print_header("Check the node status")
        node_info, _ = self.client.get_node_by_name(self.node_name)

        assert "GPUHealth" not in str(node_info.spec.taints)
        assert "AggregatedNodeHealth" not in str(node_info.spec.taints)
        assert "node.kubernetes.io/unschedulable" not in str(node_info.spec.taints)
        assert (
            self.client.get_annotation_on_node(self.node_name, "quarantineHealthEvent")[0]
            is None
        )
        assert (
            self.client.get_annotation_on_node(
                self.node_name, "quarantineHealthEventAppliedTaints"
            )[0]
            is None
        )
        assert (
            self.client.get_annotation_on_node(
                self.node_name, "quarantineHealthEventIsCordoned"
            )[0]
            is None
        )
        assert node_info.spec.unschedulable is None

        self.restore_managed_by_nvsentinel_label(self.node_name)
