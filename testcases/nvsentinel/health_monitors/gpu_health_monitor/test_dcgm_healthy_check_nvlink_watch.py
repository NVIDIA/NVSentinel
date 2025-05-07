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
Module for class of NVsentinel GPU Health Monitor: DCGM Healthy check NvlinkWatch
"""

import time
import threading
import random
from testcases.nvsentinel.health_monitors.gpu_health_monitor.base import (
    GPUHealthMonitorBase,
)
import pytest


class TestDGCMHealthyCheckNvlinkWatch(GPUHealthMonitorBase):
    """
    Class for test case of NVsentinel GPU Health Monitor: DCGM Healthy check NvlinkWatch
    """

    conditions = []

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.gpuhealthmonitor
    def test_dcgm_healthy_check_nvlink_watch(self, request):
        """
        Tests if the node condition GpuNvlinkWatch is updated correctly when a NvlinkWatch error is injected from GPU health monitor
        """
        self.step_manager.print_header("Get gpu health monitor pod name")
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        job_pod = pods[-1]
        self.node_name = job_pod.spec.node_name
        self.logger.info(f"POD  Name: {job_pod.metadata.name}")
        self.logger.info(f"Node Name: {self.node_name}")

        self.step_manager.print_header("Trigger NvlinkWatch error multiple times")
        thread1 = threading.Thread(target=self.inject_errors, args=(job_pod,))
        thread2 = threading.Thread(target=self.check_conditions, args=(self.node_name,))

        # Start both threads
        thread1.start()
        thread2.start()

        # Wait for both threads to complete
        thread1.join()
        thread2.join()

        self.logger.info(f"Find conditions: {self.conditions}")
        self.step_manager.print_header("Check events on the corresponding GPU node")
        self.conditions = [
            condition
            for condition in self.conditions
            if condition.reason == "GpuNvlinkWatchIsNotHealthy"
        ]
        self.logger.info(f"Find GpuNvlinkWatchIsNotHealthy conditions: {self.conditions}")
        expected_result = {
            "Condition Type": "GpuNvlinkWatch",
            "Condition Reason": "GpuNvlinkWatchIsNotHealthy",
            "Condition Message": "ErrorCode:DCGM_FR_NVLINK_CRC_ERROR_THRESHOLD GPU:0.*nvlink_flit_crc_error_count_total.*Recommended Action=RUN_FIELDDIAG;",
        }
        self.verify_health_monitor_info(
            conditions=self.conditions, expected_result=expected_result
        )

    def inject_errors(self, job_pod):
        for i in range(100):
            value = 1 if i == 0 else random.randint(1000, 1000000)
            command = [
                "/bin/sh",
                "-c",
                f"dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 409 -v {value}",
            ]
            self.logger.info(f"Run CMD: {command}")
            output, _ = self.client.exec_command_in_pod(job_pod, command)
            self.logger.info(f"Output: {output}")
            time.sleep(1)
            assert "Successfully injected" in output, f"Failed to inject Error: {output}"

    def check_conditions(self, node_name):
        self.logger.info("check_conditions started")  # Debugging line

        for i in range(100):
            try:
                time.sleep(1)
                node_info, _ = self.client.get_node_by_name(
                    node_name=node_name, node_type="gpu"
                )
                condition = next(
                    (
                        condition
                        for condition in node_info.status.conditions
                        if condition.type == "GpuNvlinkWatch"
                    ),
                    None,
                )
                if condition:
                    self.conditions.append(condition)
                    self.logger.info(f"Condition found: {condition}")  # Debugging line
                else:
                    self.logger.info(
                        "FAIL: No condition which type is GpuNvlinkWatch, try again"
                    )  # Debugging line

            except Exception as e:
                self.logger.info(f"Exception in check_conditions: {e}")  # Debugging line
