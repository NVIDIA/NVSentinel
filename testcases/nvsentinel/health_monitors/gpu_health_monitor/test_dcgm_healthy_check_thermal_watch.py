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
Module for class of NVsentinel GPU Health Monitor: DCGM Healthy check ThermalWatch
"""

import time
from functools import partial
from testcases.nvsentinel.health_monitors.gpu_health_monitor.base import (
    GPUHealthMonitorBase,
)
import pytest


class TestDGCMHealthyCheckThermalWatch(GPUHealthMonitorBase):
    """
    Class for test case of NVsentinel GPU Health Monitor: DCGM Healthy check ThermalWatch
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.gpuhealthmonitor
    def test_dcgm_healthy_check_thermal_watch(self, request):
        """
        Tests if the node condition GpuThermalWatch is updated correctly when a ThermalWatch error is injected from GPU health monitor
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
            "Exec into one gpu health monitor pod and inject a ThermalWatch error"
        )
        for _ in range(
            2
        ):  # retry once, sometimes error injection is not successful the first time
            command = [
                "/bin/sh",
                "-c",
                "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 241 -v 30000",
            ]
            output, _ = self.client.exec_command_in_pod(job_pod, command)
            if "Successfully injected" in output:
                break
        assert "Successfully injected" in output, f"Failed to inject Error: {output}"

        request.addfinalizer(
            partial(self.clear_gpu_fatal_error, self.node_name, "GpuXidError")
        )

        self.step_manager.print_header("Check events on the corresponding GPU node")
        time.sleep(30)
        node_info, _ = self.client.get_node_by_name(
            node_name=self.node_name, node_type="gpu"
        )
        assert node_info is not None, "Find no node info by node name"

        expected_result = {
            "Condition Type": "GpuThermalWatch",
            "Condition Reason": "GpuThermalWatchIsNotHealthy",
            "Condition Message": "errorCode:DCGM_FR_CLOCK_THROTTLE_THERMAL GPU:0.*thermal violation.*Recommended Action=NONE",
        }
        self.verify_health_monitor_info(
            conditions=node_info.status.conditions, expected_result=expected_result
        )
