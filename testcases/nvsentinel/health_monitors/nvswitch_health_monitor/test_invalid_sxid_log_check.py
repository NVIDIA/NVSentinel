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
Module for class of NVsentinel NV Switch Health Monitor: Invalid SXID log check
"""

import os
import yaml
import time
import threading
from testcases.nvsentinel.health_monitors.nvswitch_health_monitor.base import (
    NVSwitchHealthMonitorBase,
)
import pytest


class TestInvalidSxidLogCheck(NVSwitchHealthMonitorBase):
    """
    Class for test case of NVsentinel NV Switch Health Monitor: Invalid SXID log check
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.nvswitchhealthmonitor
    def test_invalid_sxid_log_check(self, request):
        """
        Tests if the invalid SXID is detected from NVSwitch logs
        """
        self.step_manager.print_header(
            "Check nvsentinel-nvswitch-health-monitor pod in the cluster"
        )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-nvswitch-health*"
        )
        self.step_manager.print_header(
            "Choose one nvsentinel-nvswitch-health-monitor pod, and record the node name of this pod"
        )
        job_pod = pods[-1]
        pod_name = job_pod.metadata.name
        node_name = job_pod.spec.node_name
        self.set_managed_by_nvsentinel_label_to_false(node_name)
        self.logger.info(f"POD   Name: {pod_name}")
        self.logger.info(f"Node  Name: {node_name}")

        self.step_manager.print_header(
            "Open one console to check the logs from the pod, do not close this console"
        )
        monitor_thread = threading.Thread(
            target=self.follow_pod_logs, args=(self.nv_namespace, pod_name), daemon=True
        )
        monitor_thread.start()

        self.step_manager.print_header("Login the node where the monitor pod is running on")
        yaml_file = os.path.join(os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "debug-pod.yaml")
        self.logger.info(f"yaml_file = {yaml_file}")
        with open(yaml_file, "r") as file:
            pod_body = yaml.safe_load(file)
            pod_body["spec"]["nodeName"] = node_name

        pods, _ = self.client.list_pods(
            pod_body["metadata"]["namespace"], name_pattern=pod_body["metadata"]["name"]
        )
        if pods:  # clean up existing debug pod before testing
            self.client.delete_pod(pod=pods[-1])
        self.debug_pod, _ = self.client.create_pod(pod_body=pod_body, wait=60)

        self.step_manager.print_header(
            "Simulate SXID fatal error by injecting the error info to /dev/kmsg"
        )
        command = [
            "/bin/sh",
            "-c",
            'echo "nvidia-nvswitch3: SXid (PCI:0000:cd:00.0): 20034, Fatal, Link 24 LTSSM Fault Up" | tee /dev/kmsg',
        ]
        output, _ = self.client.exec_command_in_pod(self.debug_pod, command)
        self.logger.info(f"OUTPUT: {output}")
        assert (
            "Fatal, Link 24 LTSSM Fault Up" in output
        ), f"Failed to inject Error: {output}"
        time.sleep(5)
        self.step_manager.print_header("Check error info From the pod log console")
        message_to_check = 'errorCode:"20034"'
        assert (
            message_to_check in self.pod_logs[-1]
        ), f"Find no expected message in console log:{self.pod_logs}"
        self.logger.info(self.pod_logs)

        
        self.client.remove_node_condition(
            node_name, "NvswitchErrorFromKmsgWatch"
        )
        self.restore_managed_by_nvsentinel_label(node_name)