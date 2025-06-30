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
import os
import yaml
from functools import partial
import re
import threading
from testcases.nvsentinel.health_monitors.gpu_health_monitor.base import (
    GPUHealthMonitorBase,
)
import pytest


class TestNVsentinelGPUHealthMonitorGpuOperatorIntegration(GPUHealthMonitorBase):

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.gpuhealthmonitor
    def test_gpu_operator_integration(self, request):
        """
        Tests if the gpu-health-monitor pod is correctly integrated with the gpu-operator and the node-health-events-uds-client pod is correctly deployed
        """
        self.step_manager.print_header(
            "Check daemonset exist and is running and healthy on all GPU nodes."
        )
        daemonset_to_check = ["nvsentinel-platform-connector"]
        target_ademonset = None
        daemonsets, _ = self.client.list_daemonset(namespace=self.nv_namespace)
        assert daemonsets, f"ERROR: No resources found in {self.nv_namespace} namespace."
        for daemonset in daemonsets:
            daemonset_name = daemonset.metadata.name
            self.logger.info(f"Daemonset Name:{daemonset_name}")
            if daemonset_name in daemonset_to_check:
                target_ademonset = daemonset
        assert (
            target_ademonset is not None
        ), f"Find no target daemonset:{daemonset_to_check}"
        desired = target_ademonset.status.desired_number_scheduled
        current = target_ademonset.status.current_number_scheduled
        ready = target_ademonset.status.number_ready
        available = target_ademonset.status.number_available
        self.logger.info(
            f"Desired: {desired}, Current: {current}, Ready: {ready}, Available: {available}"
        )
        is_fully_running = desired == current == ready == available
        assert is_fully_running, "DaemonSet is not fully running"
        self.step_manager.print_header(
            "Choose one nvsentinel node, and record the node name"
        )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        gpu_monitor_pod = pods[-1]
        pod_name = gpu_monitor_pod.metadata.name
        self.node_name = gpu_monitor_pod.spec.node_name
        self.logger.info(f"Selected Pod   Name: {pod_name}")
        self.logger.info(f"Selected Node  Name: {self.node_name}")

        self.step_manager.print_header("Change the “minikube” to one of the GPU node names")
        yaml_file = os.path.join(
            os.getcwd(),
            "nvsentinel",
            "testcases",
            "data",
            "cli",
            "nvsentinel",
            "node-health-events-uds-client.yaml",
        )
        self.logger.info(f"yaml_file = {yaml_file}")
        with open(yaml_file, "r") as file:
            pod_body = yaml.safe_load(file)
            node_selector_terms = pod_body["spec"]["affinity"]["nodeAffinity"][
                "requiredDuringSchedulingIgnoredDuringExecution"
            ]["nodeSelectorTerms"]
            pod_body["metadata"]["namespace"] = self.nv_namespace
            for term in node_selector_terms:
                for match_field in term["matchFields"]:
                    if (
                        match_field["key"] == "metadata.name"
                        and "minikube" in match_field["values"]
                    ):
                        match_field["values"] = [self.node_name]

        self.step_manager.print_header(
            "Deploy node-health-events-uds-client.yaml and Check the sample client is up and running"
        )
        pods, _ = self.client.list_pods(
            pod_body["metadata"]["namespace"], name_pattern=pod_body["metadata"]["name"]
        )
        if pods:  # clean up existing debug pod before testing
            self.client.delete_pod(pod=pods[-1])
        self.debug_pod, _ = self.client.create_pod(pod_body=pod_body)
        assert self.client.wait_for_pod_healthy(
            self.debug_pod, timeout=60
        ), f"Client {pod_body['metadata']['name']} is in unhealthy state until timeout"

        self.step_manager.print_header(
            "Open one console to check the logs from the pod, do not close this console"
        )
        monitor_thread = threading.Thread(
            target=self.follow_pod_logs,
            args=(self.nv_namespace, self.debug_pod.metadata.name),
            daemon=True,
        )
        monitor_thread.start()
        time.sleep(30)
        console_logs = [
            log_info
            for log_info in self.pod_logs
            if "New Message from Nvsentinel" not in log_info
        ]
        expected_kw = 'events:{checkName:"GpuDriverWatch"  isHealthy:true'
        assert (
            expected_kw in console_logs[-1]
        ), f"Error: job pod console log is not correct: {self.pod_logs}"
        self.logger.info("SUCCESS: job pod console log is shown correctly.")

        self.step_manager.print_header(
            "Inject a GPU inforom error by following the test case “DCGM Healthy check InForom”"
        )
        command = [
            "/bin/sh",
            "-c",
             f"dcgmi test --host nvidia-dcgm.{self.gpu_operator_namespace}.svc:5555 --inject --gpuid 0 -f 84 -v 0",
        ]
        output, _ = self.client.exec_command_in_pod(
            gpu_monitor_pod,
            command=command,
        )
        assert "Successfully injected" in output, f"Failed to inject Error: {output}"
        # add cleanup of dcgm error
        command = [
            "/bin/sh",
            "-c",
             f"dcgmi test --host nvidia-dcgm.{self.gpu_operator_namespace}.svc:5555 --inject --gpuid 0 -f 84 -v 1",
        ]
        request.addfinalizer(
            partial(self.client.exec_command_in_pod, gpu_monitor_pod, command)
        )
        time.sleep(30)
        console_logs = [
            log_info
            for log_info in self.pod_logs
            if "New Message from Nvsentinel" not in log_info
        ]
        expected_kw = [
            'checkName:"GpuInforomWatch"',
            "A corrupt InfoROM has been detected in GPU",
            "Flash the InfoROM to clear this corruption",
            "recommendedAction:COMPONENT_RESET",
        ]
        find_match = all(
            re.search(keyword, console_logs[-1], re.IGNORECASE) for keyword in expected_kw
        )

        assert (
            find_match
        ), f"GPU inforom error event is not found in console log: {console_logs}"
        self.logger.info("SUCCESS: GPU inforom error event is found in console log.")
