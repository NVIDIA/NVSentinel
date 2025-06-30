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
Module for class of NVsentinel NV Switch Health Monitor: Multi NVswitch errors
"""

import time
import threading
from functools import partial
from testcases.nvsentinel.health_monitors.nvswitch_health_monitor.base import (
    NVSwitchHealthMonitorBase,
)
import pytest


class TestMultiNvswitchErrors(NVSwitchHealthMonitorBase):
    """
    Class for test case of NVsentinel NV Switch Health Monitor: Multi NVswitch errors
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.healthmonitor
    def test_multi_nvswitch_errors(self, request):
        """
        Test case of NVsentinel NV Switch Health Monitor: Multi NVswitch errors
        """
        self.client.rollout_daemonset(
            "nvsentinel-nvswitch-health-monitor", namespace=self.nv_namespace
        )
        time.sleep(10)
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
        self.node_name = job_pod.spec.node_name
        self.set_managed_by_nvsentinel_label_to_false(self.node_name)
        self.logger.info(f"POD   Name: {pod_name}")
        self.logger.info(f"Node  Name: {self.node_name}")

        self.step_manager.print_header(
            "Open one console to check the logs from the pod, do not close this console"
        )
        monitor_thread = threading.Thread(
            target=self.follow_pod_logs, args=(self.nv_namespace, pod_name), daemon=True
        )
        monitor_thread.start()

        self.step_manager.print_header("Login the node where the monitor pod is running on")
        self.debug_pod = self.create_debug_pod(self.node_name)
        event_count_1 = self.get_count_of_node_events(
            self.node_name, "NvswitchErrorFromKmsgWatchIsNotHealthy"
        )

        self.step_manager.print_header(
            "Simulate SXID error by injecting 100 errors to dmsg"
        )
        self.step_manager.print_header("Check 100 error info From the pod log console")
        self.step_manager.print_header("Check 100 error info from node condition")

        count = 100
        for i in range(count):
            index = i + 1
            self.logger.info(f"Send SXID error: {index}")
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

        self.step_manager.print_header(
            "From the pod log console, will get all 100 error logs indicating error found"
        )
        message_to_check = 'errorCode:"19077"'
        # Count occurrences of this specific error code in the logs
        message_count = 0
        for log_entry in self.pod_logs:
            if message_to_check in log_entry:
                message_count += 1

        self.logger.info(
            f"Found {message_count} occurrences of '{message_to_check}' in logs"
        )
        assert (
            message_count == count
        ), f"Expected at least {count} occurrences of '{message_to_check}', but found {message_count}"

        self.step_manager.print_header(
            "Check from node event, should also get all 100 related errors info"
        )
        events, _ = self.client.get_node_events(node_name=self.node_name)
        expected_result = {
            "Condition Type": "NvswitchErrorFromKmsgWatch",
            "Condition Reason": "NvswitchErrorFromKmsgWatchIsNotHealthy",
            "Condition Message": ".*19077.*Nvltlc Tx Lnk An1 Timeout Vc5.*Recommended Action=NONE",
        }
        self.verify_health_monitor_info(conditions=events, expected_result=expected_result)
        event_count_2 = self.get_count_of_node_events(
            self.node_name, "NvswitchErrorFromKmsgWatchIsNotHealthy"
        )
        self.logger.info(
            f"Event count before: {event_count_1}, Event count after: {event_count_2}"
        )
        if event_count_1 is None or event_count_1 == 0:
            self.logger.warning(
                "Initial event count was 0, validating only that current count is at least 'count'"
            )
            assert (
                event_count_2 == count
            ), f"Expected count to be at least {count}, got {event_count_2}"
        else:
            assert (
                event_count_2 == event_count_1 + count
            ), f"Expected count to be {event_count_1 + count}, got {event_count_2}"
        self.restore_managed_by_nvsentinel_label(self.node_name)