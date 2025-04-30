# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import threading
import time
from functools import partial
import pytest

from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestXidEventOccurred(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Metrics: GPU monitor metrics: xid_event_occurred
    """

    template_title = "GPU monitor metrics: xid_event_occurred"

    def check_metric_increase(
        self, pod_name, job_pod, node_name, metric_name, label_selector=None, repeat_times=3
    ):
        """
        Helper function to check metric increase after XID error injection
        Args:
            pod_name: Name of the pod
            job_pod: The pod object to execute commands
            node_name: Name of the node
            metric_name: Name of the metric to check
            label_selector: Additional label selector for the metric query
            repeat_times: Number of times to repeat the check
        """
        for _ in range(repeat_times):
            self.step_manager.print_header(f"Check the value of metric {metric_name}:")

            # Construct query parameters
            query_params = f'{metric_name}{{pod="{pod_name}"'
            if label_selector:
                query_params += f", {label_selector}"
            query_params += "}"

            response = self.query_metrics(query_params)
            value = response.json()["data"]["result"][0]["value"]
            self.logger.info(f"[DEBUG] value = {value}")
            value_before = int(value[1])

            self.step_manager.print_header(
                "Execute the test case: 'DCGM Healthy check GpuXid non-fatal error' on the same node where the GPU monitor Pod is running."
            )
            command = [
                "/bin/sh",
                "-c",
                "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 230 -v 43",
            ]
            output, _ = self.client.exec_command_in_pod(job_pod, command)
            assert (
                "Successfully injected" in output
            ), f"Failed to inject GpuXid Error: {output}"
            time.sleep(15)
            events, _ = self.client.get_node_events(node_name=node_name)

            expected_result = {
                "Event Type": "GpuXidError",
                "Event Reason": "GpuXidErrorIsNotHealthy",
                "Event Message": "ErrorCode:43 GPU:0 XID error occured Recommended Action=NONE",
            }
            self.verfiy_health_monitor_info(
                conditions=events, expected_result=expected_result
            )

            self.step_manager.print_header(f"Check again the metric: {metric_name}")
            value_after = self.get_expected_value(
                query_params=query_params,
                value_before=value_before,
                expected_increase=1,
            )

            self.step_manager.print_header(
                "Double check that the value of the metric has increased by 1 exactly."
            )
            self.logger.info(f"value_before = {value_before}")
            self.logger.info(f"value_after = {value_after}")
            assert (
                value_after - value_before == 1
            ), f"[FAIL] value of the metric {metric_name} is NOT increased by 1"

            self.logger.info(f"[PASS] value of the metric {metric_name} is increased by 1")

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.metrics
    def test_xid_event_occurred(self, request):
        """
        Test case of NVsentinel Metrics: GPU monitor metrics: xid_event_occurred
        """
        self.step_manager.print_header(
            'Follow "Prometheus metrics for nvsentinel pods", make sure that promtool is installed and prometheus port 9090 is accessible.'
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
        request.addfinalizer(partial(self.clear_gpu_fatal_error, node_name, "GpuXidError"))
        self.gpu_healthy_node = node_name
        self.gpu_healthy_pods.append(job_pod)

        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 230 -v 43",
        ]
        output, _ = self.client.exec_command_in_pod(
            job_pod,
            command,
        )
        assert "Successfully injected" in output, f"Failed to inject GpuXid Error: {output}"
        time.sleep(30)

        # Check callback_success_total metric
        self.check_metric_increase(
            pod_name=pod_name,
            job_pod=job_pod,
            node_name=node_name,
            metric_name="callback_success_total",
            label_selector='func_name="xid_event_occurred"',
        )

        # Check health_events_insertion_to_uds_succeed_total metric
        self.check_metric_increase(
            pod_name=pod_name,
            job_pod=job_pod,
            node_name=node_name,
            metric_name="health_events_insertion_to_uds_succeed_total",
        )
