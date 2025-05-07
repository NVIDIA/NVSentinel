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
Module for class of NVsentinel GPU Health Monitor: DCGM Healthy check GpuXid non-fatal error
"""

import time

# import pytest
from testcases.nvsentinel.health_monitors.gpu_health_monitor.base import (
    GPUHealthMonitorBase,
)
import pytest


class TestDGCMHealthyCheckGpuXidNonFatalError(GPUHealthMonitorBase):
    """
    Class for test case of NVsentinel GPU Health Monitor: DCGM Healthy check GpuXid non-fatal error
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.smoke
    @pytest.mark.gpuhealthmonitor
    def test_dcgm_healthy_check_gpuxid_non_fatal_error(self, request):
        """
        Tests if node event is reported when a non-fatal XID error is injected from GPU health monitor of the node
        """
        self.logger.print_header("Get gpu health monitor pod name")
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        job_pod = pods[-1]
        node_name = job_pod.spec.node_name
        self.logger.info(f"POD  Name: {job_pod.metadata.name}")
        self.logger.info(f"Node Name: {node_name}")
        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 230 -v 43",
        ]
        output, _ = self.client.exec_command_in_pod(job_pod, command)
        assert "Successfully injected" in output, f"Failed to inject GpuXid Error: {output}"
        time.sleep(5)
        events, _ = self.client.get_node_events(node_name=node_name)

        expected_result = {
            "Event Type": "GpuXidError",
            "Event Reason": "GpuXidErrorIsNotHealthy",
            "Event Message": "ErrorCode:43 GPU:0 XID error occured Recommended Action=NONE",
        }
        self.verify_health_monitor_info(conditions=events, expected_result=expected_result)
