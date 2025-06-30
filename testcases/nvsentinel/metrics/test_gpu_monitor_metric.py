# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import time
import pytest

from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestGPUMonitorMetric(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Metrics: GPU monitor metrics
    """

    template_title = "GPU monitor metrics"

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.metrics
    def test_gpu_monitor_metric(self, request):
        """
        Test case of NVsentinel Metrics: GPU monitor metrics
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
        command = [
            "/bin/sh",
            "-c",
             f"dcgmi test --host nvidia-dcgm.{self.gpu_operator_namespace}.svc:5555 --inject --gpuid 0 -f 230 -v 43",
        ]
        output, _ = self.client.exec_command_in_pod(job_pod, command)
        assert "Successfully injected" in output, f"Failed to inject GpuXid Error: {output}"
        time.sleep(30)
        self.step_manager.print_header("Check all the metrics from this pod")
        response = self.query_metrics(
            query_params=f'group({{pod="{pod_name}"}}) by (__name__)'
        )

        self.step_manager.print_header("Make sure following metrics are included")
        check_list = [
            "xid_events_publish_time_to_grpc_channel_bucket",
            "xid_events_publish_time_to_grpc_channel_count",
            "xid_events_publish_time_to_grpc_channel_sum",
            "xid_events_publish_time_to_grpc_channel_created",
            "xid_errors_batch_processing_reconcile_time_bucket",
            "xid_errors_batch_processing_reconcile_time_count",
            "xid_errors_batch_processing_reconcile_time_sum",
            "xid_errors_batch_processing_reconcile_time_created",
            "dcgm_health_events_publish_time_to_grpc_channel_bucket",
            "dcgm_health_events_publish_time_to_grpc_channel_count",
            "dcgm_health_events_publish_time_to_grpc_channel_sum",
            "dcgm_health_events_publish_time_to_grpc_channel_created",
            "callback_success_total",
            "number_of_health_watches",
            "dcgm_reconcile_time_bucket",
            "dcgm_reconcile_time_count",
            "dcgm_reconcile_time_sum",
            "dcgm_reconcile_time_created",
            "dcgm_api_latency_bucket",
            "dcgm_api_latency_count",
            "dcgm_api_latency_sum",
            "dcgm_api_latency_created",
        ]
        self.verify_check_list(check_list=check_list, response=response)
