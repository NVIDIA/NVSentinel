# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import os
import time
import yaml
from functools import partial
import pytest

from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestNvswitchMonitorSxidLogsSucceededTotal(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Metrics: NVSwitch Health Monitor: nvswitch_monitor_sxid_logs_succeeded_total
    """

    template_title = "NVSwitch Health Monitor: nvswitch_monitor_sxid_logs_succeeded_total"

    nodes_name = []

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.metrics
    def test_nvswitch_monitor_sxid_logs_succeeded_total(self, request):
        """
        Test case of NVsentinel Metrics: NVSwitch Health Monitor: nvswitch_monitor_sxid_logs_succeeded_total
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

        self.step_manager.print_header(
            "Get the current value of metric nvswitch_monitor_sxid_logs_succeeded_total"
        )
        response = self.query_metrics(
            query_params=f'nvswitch_monitor_sxid_logs_succeeded_total{{pod="{pod_name}"}}'
        )
        value = response.json()["data"]["result"][0]["value"]
        self.logger.info(f"[DEBUG] value = {value}")
        value_before = int(value[1])

        self.step_manager.print_header(
            "Execute test case “Fatal NVswitch errors”, check value of the metric is increased by 1."
        )
        self.step_manager.print_header("Login the node where the monitor pod is running on")
        yaml_file = os.path.join(os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "debug-pod.yaml")
        self.logger.info(f"yaml_file = {yaml_file}")
        with open(yaml_file, "r") as file:
            pod_body = yaml.safe_load(file)
            pod_body["spec"]["nodeName"] = node_name

        self.nodes_name.append(node_name)

        pods, _ = self.client.list_pods(
            pod_body["metadata"]["namespace"], name_pattern=pod_body["metadata"]["name"]
        )
        if pods:  # clean up existing debug pod before testing
            self.client.delete_pod_by_name(
                pods[-1].metadata.name, pods[-1].metadata.namespace
            )
        self.debug_pod, _ = self.client.create_pod(pod_body=pod_body)

        self.step_manager.print_header(
            "Simulate  SXID fatal error by injecting the error info to /dev/kmsg"
        )
        node, _ = self.client.get_node_by_name(node_name)
        if "H100" in node.metadata.labels.get("nvidia.com/gpu.product"):
            command = [
                "/bin/sh",
                "-c",
                'echo "nvidia-nvswitch1: SXid (PCI:0000:cd:00.0): 20034, Fatal, Link 2 LTSSM Fault Up" | tee -a /host/dev/kmsg',
            ]
        else:
            command = [
                "/bin/sh",
                "-c",
                'echo "nvidia-nvswitch1: SXid (PCI:0000:cd:00.0): 20034, Fatal, Link 24 LTSSM Fault Up" | tee -a /dev/kmsg',
            ]
        # command = ['/bin/sh', '-c', 'echo "nvidia-nvswitch0: SXid (PCI:0000:ca:00.0): 10003, Fatal, unhandled interrupt" | tee -a /host/dev/kmsg']
        output, _ = self.client.exec_command_in_pod(self.debug_pod, command)
        self.logger.info(f"OUTPUT: {output}")
        time.sleep(30)
        self.step_manager.print_header("Check error info from node condition")
        node_info, _ = self.client.get_node_by_name(node_name=node_name, node_type="gpu")
        assert node_info is not None, f"Find no node info by node name:{node_name}"
        expected_result = {
            "Condition Type": "NvswitchErrorFromKmsgWatch",
            "Condition Reason": "NvswitchErrorFromKmsgWatchIsNotHealthy",
            "Condition Message": "LTSSM Fault Up",
        }
        self.verify_health_monitor_info(
            conditions=node_info.status.conditions, expected_result=expected_result
        )

        self.step_manager.print_header(
            "Get the current value of metric nvswitch_monitor_sxid_logs_succeeded_total"
        )
        value_after = self.get_expected_value(
            query_params=f'nvswitch_monitor_sxid_logs_succeeded_total{{pod="{pod_name}"}}',
            value_before=value_before,
            expected_increase=1,
        )

        self.step_manager.print_header("check value of the metric is increased by 1")
        self.logger.info(f"value_before = {value_before}")
        self.logger.info(f"value_after = {value_after}")
        assert (
            value_after - value_before == 1
        ), "[FAIL] value of the metric is NOT increased by 1"
        self.logger.info("[PASS] value of the metric is increased by 1")
        self.client.remove_node_condition(node_name, "NvswitchErrorFromKmsgWatch")

