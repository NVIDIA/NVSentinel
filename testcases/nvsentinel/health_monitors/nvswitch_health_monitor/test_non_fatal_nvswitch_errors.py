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
Module for class of NVsentinel NV Switch Health Monitor: Non-fatal NVswitch errors
"""

import time
import threading
from testcases.nvsentinel.health_monitors.nvswitch_health_monitor.base import (
    NVSwitchHealthMonitorBase,
)
import pytest


class TestNonFatalNvswitchErrors(NVSwitchHealthMonitorBase):
    """
    Class for test case of NVsentinel NV Switch Health Monitor: Non-fatal NVswitch errors
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.smoke
    @pytest.mark.nvswitchhealthmonitor
    def test_non_fatal_nvswitch_errors(self, request):
        """
        Tests if the node condition NvswitchErrorFromKmsgWatch is updated correctly when a non-fatal NVswitch error is injected from debug pod
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
        self.debug_pod = self.create_debug_pod(node_name)

        self.step_manager.print_header(
            "Simulate SXID non-fatal error by injecting the error info to /dev/kmsg"
        )
        command = [
            "/bin/sh",
            "-c",
            'echo "nvidia-nvswitch0: SXid (PCI:0000:ca:00.0): 28002, Non-fatal, Link 20 Therm Warn Deactivated" | tee -a /host/dev/kmsg',
        ]
        output, _ = self.client.exec_command_in_pod(self.debug_pod, command)
        self.logger.info(f"OUTPUT: {output}")
        assert (
            "Non-fatal, Link 20 Therm Warn Deactivated" in output
        ), f"Failed to inject Error: {output}"
        time.sleep(30)
        self.step_manager.print_header("Check error info From the pod log console")
        message_to_check = 'errorCode:"28002"'
        assert (
            message_to_check in self.pod_logs[-1]
        ), f"Find no expected message in console log:{self.pod_logs}"

        self.step_manager.print_header("Check error info from node condition")
        events, _ = self.client.get_node_events(node_name=node_name)
        expected_result = {
            "Condition Type": "NvswitchErrorFromKmsgWatch",
            "Condition Reason": "NvswitchErrorFromKmsgWatchIsNotHealthy",
            "Condition Message": ".*28002.*Therm Warn Deactivated.*Recommended Action=NONE",
        }
        self.verify_health_monitor_info(conditions=events, expected_result=expected_result)

        self.step_manager.print_header(
            "Simulate another SXID non-fatal error by injecting the error info to /dev/kmsg"
        )
        command = [
            "/bin/sh",
            "-c",
            'echo "nvidia-nvswitch0: SXid (PCI:0000:ca:00.0): 30005, Non-fatal, Link 20 Crumbstore Buf Overwrite" | tee -a /host/dev/kmsg',
        ]
        output, _ = self.client.exec_command_in_pod(self.debug_pod, command)
        self.logger.info(f"OUTPUT: {output}")
        assert (
            "Non-fatal, Link 20 Crumbstore Buf Overwrite" in output
        ), f"Failed to inject Error: {output}"
        time.sleep(30)
        self.step_manager.print_header("Check error info From the pod log console")
        message_to_check = 'errorCode:"30005"'
        assert (
            message_to_check in self.pod_logs[-1]
        ), f"Find no expected message in console log:{self.pod_logs}"
        self.logger.info(self.pod_logs)

        self.step_manager.print_header("Check error info from node condition")
        events, _ = self.client.get_node_events(node_name=node_name)
        expected_result = {
            "Condition Type": "NvswitchErrorFromKmsgWatch",
            "Condition Reason": "NvswitchErrorFromKmsgWatchIsNotHealthy",
            "Condition Message": ".*30005.*Crumbstore Buf Overwrite.*Recommended Action=NONE",
        }
        self.verify_health_monitor_info(conditions=events, expected_result=expected_result)

        self.step_manager.print_header(
            "Simulate third SXID non-fatal error by injecting the error info to /dev/kmsg"
        )
        command = [
            "/bin/sh",
            "-c",
            'echo "nvidia-nvswitch0: SXid (PCI:0000:ca:00.0): 19077, Non-fatal, Link 20 Nvltlc Tx Lnk An1 Timeout Vc5" | tee -a /host/dev/kmsg',
        ]
        output, _ = self.client.exec_command_in_pod(self.debug_pod, command)
        self.logger.info(f"OUTPUT: {output}")
        assert (
            "Non-fatal, Link 20 Nvltlc Tx Lnk An1 Timeout Vc5" in output
        ), f"Failed to inject Error: {output}"
        time.sleep(30)
        self.step_manager.print_header("Check error info From the pod log console")
        message_to_check = 'errorCode:"19077"'
        assert (
            message_to_check in self.pod_logs[-1]
        ), f"Find no expected message in console log:{self.pod_logs}"

        self.step_manager.print_header("Check error info from node condition")
        events, _ = self.client.get_node_events(node_name=node_name)
        expected_result = {
            "Condition Type": "NvswitchErrorFromKmsgWatch",
            "Condition Reason": "NvswitchErrorFromKmsgWatchIsNotHealthy",
            "Condition Message": ".*19077.*Nvltlc Tx Lnk An1 Timeout Vc5.*Recommended Action=NONE",
        }
        self.verify_health_monitor_info(conditions=events, expected_result=expected_result)
