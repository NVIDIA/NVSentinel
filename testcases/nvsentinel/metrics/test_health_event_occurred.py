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
Module for class of NVsentinel Metrics: GPU monitor metrics: health_event_occurred
"""

import threading
import time
import pytest

from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestHealthEventOccurredMetric(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Metrics: GPU monitor metrics: health_event_occurred
    """

    template_title = "GPU monitor metrics: health_event_occurred"

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.metrics
    def test_health_event_occurred_metric(self, request):
        """
        Test case of NVsentinel Metrics: GPU monitor metrics: health_event_occurred
        """
        self.step_manager.print_header(
            "Follow “Prometheus metrics for nvsentinel pods” , make sure that promtool is installed and prometheus port 9090 is accessible."
        )
        self.start_prometheus_service()

        self.step_manager.print_header(
            "Select one of the GPU monitor pod in nvsentinel pod, take a note of the node where it is running"
        )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor*"
        )
        assert len(pods) > 0, "[FAIL] Cannot find any nvsentinel-gpu-health-monitor pod"
        job_pod = pods[0]
        pod_name = job_pod.metadata.name
        node_name = job_pod.spec.node_name
        self.logger.info(f"POD   Name: {pod_name}")
        self.logger.info(f"Node  Name: {node_name}")

        self.step_manager.print_header(
            "Open another terminal to continuously monitor the GPU monitor log"
        )
        self.pod_logs = []
        monitor_thread = threading.Thread(
            target=self.follow_pod_logs, args=(self.nv_namespace, pod_name), daemon=True
        )
        monitor_thread.start()

        self.step_manager.print_header("Check the value of metric callbck_success_total:")
        response = self.query_metrics(
            query_params=f'callback_success_total{{pod="{pod_name}", func_name="health_event_occurred"}}'
        )
        value = response.json()["data"]["result"][0]["value"]
        self.logger.info(f"[DEBUG] value = {value}")
        value_before = int(value[1])

        self.step_manager.print_header(
            "Wait for 180s, the callback will be sent every 15s."
        )
        time.sleep(180)

        self.step_manager.print_header("Check again the metric:")
        value_after = self.get_expected_value(
            query_params=f'callback_success_total{{pod="{pod_name}", func_name="health_event_occurred"}}',
            value_before=value_before,
            expected_increase=12,
        )

        self.step_manager.print_header(
            "Double check that the value of the metric has increased by 12."
        )
        self.logger.info(f"value_before = {value_before}")
        self.logger.info(f"value_after = {value_after}")
        assert (
            value_after - value_before == 12
        ), "[FAIL] value of the metric is NOT increased by 12"
        self.logger.info("[PASS] value of the metric is increased by 12")
