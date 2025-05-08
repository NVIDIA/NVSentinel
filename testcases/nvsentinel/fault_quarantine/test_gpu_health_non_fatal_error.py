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
Module for class of NVsentinel Fault Quarantine:GPU health non-fatal error
"""

import time
from testcases.nvsentinel.base import TestNVSentinelCaseBase
import pytest


class TestGPUHealthNonFatalError(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine GPU Health Non Fatal Error
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.faultquarantine
    def test_gpu_health_non_fatal_error(self, request):
        """
        Tests if the node is not cordoned and not tainted when a non-fatal error is injected from GPU health monitor that does not matches any ruleset
        """
        self.logger.info("Restart the fault-quarantine pod")
        self.delete_fault_quarantine_pod()

        self.step_manager.print_header("Inject a non-fatal error on a GPU node")
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        gpu_healthy_pod = pods[-1]
        self.node_name = gpu_healthy_pod.spec.node_name
        self.logger.info(f"POD  Name: {gpu_healthy_pod.metadata.name}")
        self.logger.info(f"Node Name: {self.node_name}")
        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 230 -v 43",
        ]
        output, _ = self.client.exec_command_in_pod(gpu_healthy_pod, command)
        assert "Successfully injected" in output

        self.step_manager.print_header("Check the fault-quarantine pod log")
        time.sleep(20)
        log_content = self.get_fault_quarantine_pod_log()
        assert (
            "Tainting node {} with taint config:".format(self.node_name) not in log_content
        )
        assert f"Cordoning node {self.node_name}" not in log_content

        self.step_manager.print_header("Check the node status")
        node_info, _ = self.client.get_node_by_name(self.node_name)
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

        self.step_manager.print_header("Clear the injected error")
        command = ["python3 clear_xid_error_health_event.py"]
        self.client.exec_command_in_pod(gpu_healthy_pod, command)
