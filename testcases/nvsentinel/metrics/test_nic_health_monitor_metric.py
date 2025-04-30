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
Module for class of NVsentinel Metrics: NIC health monitor metric
"""

import pytest

from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestNICHealthMonitorMetric(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Metrics: NIC health monitor metric
    """

    template_title = "NIC health monitor metric"

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.metrics
    def test_nic_health_monitor_metric(self, request):
        """
        Test case of NVsentinel Metrics: NIC health monitor metric
        """
        self.step_manager.print_header(
            "Follow “Prometheus metrics for nvsentinel pods” , make sure that promtool is installed and prometheus port 9090 is accessible."
        )
        self.start_prometheus_service()

        self.step_manager.print_header(
            "Select one of the NIC health monitor pod in nvsentinel pod, take a note of the node where it is running"
        )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-nic-health*"
        )
        assert len(pods) > 0, "[FAIL] Cannot find any nvsentinel-nic-health pod"
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
            "nic_monitor_health_event_publish_duration_milliseconds_bucket",
            "nic_monitor_health_event_publish_duration_milliseconds_sum",
            "nic_monitor_health_event_publish_duration_milliseconds_count",
            "nic_monitor_health_events_published_total",
            "nic_monitor_polling_loop_processing_duration_milliseconds_bucket",
            "nic_monitor_polling_loop_processing_duration_milliseconds_sum",
            "nic_monitor_polling_loop_processing_duration_milliseconds_count",
        ]
        self.verify_check_list(check_list=check_list, response=response)
