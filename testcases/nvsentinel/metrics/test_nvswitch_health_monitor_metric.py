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
Module for class of NVsentinel Metrics: NVSwitch Health Monitor
"""

import pytest

from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestNVSwitchHealthMonitorMetric(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Metrics: NVSwitch Health Monitor
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.metrics
    def test_nvswitch_health_monitor_metric(self, request):
        """
        Test case of NVsentinel Metrics: NVSwitch Health Monitor
        """
        self.step_manager.print_header(
            "Follow “Prometheus metrics for nvsentinel pods” , make sure that promtool is installed and prometheus port 9090 is accessible."
        )
        self.start_prometheus_service()

        self.step_manager.print_header(
            "Select one of the NVSwitch health monitor pod in nvsentinel pod, take a note of the node where it is running"
        )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-nvswitch-health*"
        )
        assert len(pods) > 0, "[FAIL] Cannot find any nvsentinel-nvswitch-health pod"
        job_pod = pods[0]
        pod_name = job_pod.metadata.name
        node_name = job_pod.spec.node_name
        self.logger.info(f"POD   Name: {pod_name}")
        self.logger.info(f"Node  Name: {node_name}")

        self.step_manager.print_header("Check all the metrics from this pod")
        response = self.query_metrics(
            query_params=f'group({{pod="{pod_name}"}}) by (__name__)'
        )

        self.step_manager.print_header("Make sure following metrics are included")
        check_list = [
            "nvswitch_monitor_health_event_publish_duration_milliseconds_bucket",
            "nvswitch_monitor_health_event_publish_duration_milliseconds_sum",
            "nvswitch_monitor_health_event_publish_duration_milliseconds_count",
            "nvswitch_monitor_health_events_published_total",
            "nvswitch_monitor_kernel_logs_processed_total",
            "nvswitch_monitor_polling_loop_processing_duration_milliseconds_bucket",
            "nvswitch_monitor_polling_loop_processing_duration_milliseconds_sum",
            "nvswitch_monitor_polling_loop_processing_duration_milliseconds_count",
            "nvswitch_monitor_sxid_logs_failed_total",
            "nvswitch_monitor_sxid_logs_succeeded_total",
            "nvswitch_monitor_syslog_read_calls_failed_total",
            "nvswitch_monitor_syslog_read_calls_succeeded_total",
        ]
        self.verify_check_list(check_list=check_list, response=response)
