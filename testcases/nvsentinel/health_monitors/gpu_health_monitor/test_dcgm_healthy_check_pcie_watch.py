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
Module for class of NVsentinel GPU Health Monitor: DCGM Healthy check PcieWatch
"""

import time
from functools import partial
from testcases.nvsentinel.health_monitors.gpu_health_monitor.base import (
    GPUHealthMonitorBase,
)
import pytest


class TestDGCMHealthyCheckPcieWatch(GPUHealthMonitorBase):
    """
    Class for test case of NVsentinel GPU Health Monitor: DCGM Healthy check PcieWatch
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.gpuhealthmonitor
    def test_dcgm_healthy_check_pcie_watch(self, request):
        """
        Tests if the node condition GpuPcieWatch is updated correctly when a PcieWatch error is injected from GPU health monitor
        """
        self.step_manager.print_header("Get gpu health monitor pod name")
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        job_pod = pods[-1]
        self.node_name = job_pod.spec.node_name
        self.logger.info(f"POD  Name: {job_pod.metadata.name}")
        self.logger.info(f"Node Name: {self.node_name}")

        self.step_manager.print_header(
            "Exec into one gpu health monitor pod and inject a PcieWatch error"
        )
        for _ in range(5):
            command = [
                "/bin/sh",
                "-c",
                "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 1 -f 202 -v 99999",
            ]
            output, _ = self.client.exec_command_in_pod(job_pod, command)
            assert output and "Successfully injected" in output, f"Failed to inject Error: {output}"
            time.sleep(3)
        time.sleep(1)

        request.addfinalizer(
            partial(self.clear_gpu_fatal_error, self.node_name, "GpuXidError")
        )

        self.step_manager.print_header("Check events on the corresponding GPU node")
        time.sleep(5)
        node_info, _ = self.client.get_node_by_name(
            node_name=self.node_name, node_type="gpu"
        )
        assert node_info is not None, "Find no node info by node name"
        expected_result = {
            "Condition Type": "GpuPcieWatch",
            "Condition Reason": "GpuPcieWatchIsNotHealthy",
            "Condition Message": ".*DCGM_FR_PCI_REPLAY_RATE.*GPU:1.*PCIe replays.*Recommended Action=REPORT_ISSUE",
        }
        print(node_info.status.conditions)
        self.verify_health_monitor_info(
            conditions=node_info.status.conditions, expected_result=expected_result
        )
