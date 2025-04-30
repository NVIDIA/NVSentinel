# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import pytest

from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestNVSentinelMetricsPrometheus(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Metrics: Prometheus metrics for nvsentinel pods
    """

    template_title = "Prometheus metrics for nvsentinel pods"

    def verify_lines_and_pods(self, response, pod_regex):
        number_of_lines = len(response.json()["data"]["result"])
        self.logger.info(f"number_of_lines = {number_of_lines}")
        pods, _ = self.client.list_pods(self.nv_namespace, name_pattern=pod_regex)
        number_of_pods = len(pods)
        self.logger.info(f"number_of_pods = {number_of_pods}")
        assert number_of_pods > 0, f"[FAIL] Cannot find any {pod_regex} pod"
        for job_pod in pods:
            pod_name = job_pod.metadata.name
            self.logger.info(f"POD   Name: {pod_name}")
            node_name = job_pod.spec.node_name
            self.logger.info(f"Node  Name: {node_name}")
        assert number_of_lines == number_of_pods, "[FAIL] number_of_lines != number_of_pods"

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.metrics
    def test_prometheus_metrics_for_nvsentinel_pods(self, request):
        """
        Test case of NVsentinel Metrics: Prometheus metrics for nvsentinel pods
        """
        self.step_manager.print_header(
            "Follow “Prometheus metrics for nvsentinel pods” , make sure that promtool is installed and prometheus port 9090 is accessible."
        )
        self.start_prometheus_service()

        self.step_manager.print_header(
            "Open a new terminal, use promtool to query the metrics of nvsentinel, make sure the metrics exists"
        )
        response = self.query_metrics(query_params="number_of_health_watches")

        self.step_manager.print_header(
            "Make sure that the number of lines matches the number of gpu monitor pods."
        )
        self.verify_lines_and_pods(
            response=response, pod_regex="nvsentinel-gpu-health-monitor-dcgm.*"
        )

        self.step_manager.print_header("Check the NIC monitor metrics")
        response = self.query_metrics(
            query_params="nic_monitor_health_events_published_total"
        )

        self.step_manager.print_header(
            "Make sure that the number of lines matches the number of nic monitor pods."
        )
        self.verify_lines_and_pods(
            response=response, pod_regex="nvsentinel-nic-health-monitor.*"
        )

        self.step_manager.print_header("Check nvswitch monitor metrics")
        response = self.query_metrics(
            query_params="nvswitch_monitor_health_events_published_total"
        )

        self.step_manager.print_header(
            "Make sure that the number of lines matches the number of nvswitch monitor pods."
        )
        self.verify_lines_and_pods(
            response=response, pod_regex="nvsentinel-nvswitch-health-monitor.*"
        )

        self.step_manager.print_header("Check platform monitor metric")
        response = self.query_metrics(
            query_params="platform_connector_health_events_received_total"
        )

        self.step_manager.print_header(
            "Make sure that the number of lines matches the number of platform connector pods."
        )
        self.verify_lines_and_pods(
            response=response, pod_regex="nvsentinel-platform-connector.*"
        )
