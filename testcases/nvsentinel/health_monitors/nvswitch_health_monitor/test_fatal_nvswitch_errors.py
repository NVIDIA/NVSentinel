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
Module for class of NVsentinel NV Switch Health Monitor: Fatal NVswitch errors
"""

import time
from functools import partial
import threading
from testcases.nvsentinel.health_monitors.nvswitch_health_monitor.base import (
    NVSwitchHealthMonitorBase,
)
import pytest


class TestFatalNvswitchErrors(NVSwitchHealthMonitorBase):
    """
    Class for test case of NVsentinel NV Switch Health Monitor: Fatal NVswitch errors
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.nvswitchhealthmonitor
    def test_fatal_nvswitch_errors(self, request):
        """
        Tests if the node condition NvswitchErrorFromKmsgWatch is updated correctly when a fatal NVswitch error is injected from debug pod
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
        self.node_name = job_pod.spec.node_name
        self.logger.info(f"POD   Name: {pod_name}")
        self.logger.info(f"Node  Name: {self.node_name}")
        request.addfinalizer(partial(self.clear_nvswitch_error, self.node_name))

        self.step_manager.print_header(
            "Open one console to check the logs from the pod, do not close this console"
        )
        monitor_thread = threading.Thread(
            target=self.follow_pod_logs, args=(self.nv_namespace, pod_name), daemon=True
        )
        monitor_thread.start()

        self.step_manager.print_header("Login the node where the monitor pod is running on")
        self.debug_pod = self.create_debug_pod(self.node_name)

        self.step_manager.print_header(
            "Simulate SXID fatal error by injecting the error info to /dev/kmsg"
        )
        self.client.remove_node_condition(self.node_name, "NvswitchErrorFromKmsgWatch")
        command = [
            "/bin/sh",
            "-c",
            'echo "nvidia-nvswitch3: SXid (PCI:0000:cd:00.0): 20034, Fatal, Link 24 LTSSM Fault Up" | tee -a /host/dev/kmsg',
        ]
        output, _ = self.client.exec_command_in_pod(self.debug_pod, command)
        self.logger.info(f"OUTPUT: {output}")
        assert (
            "20034, Fatal, Link 24 LTSSM Fault Up" in output
        ), f"Failed to inject Error: {output}"
        time.sleep(10)
        self.step_manager.print_header("Check error info From the pod log console")
        message_to_check = "LTSSM Fault Up"
        assert (
            message_to_check in self.pod_logs[-1]
        ), f"Find no expected message in console log:{self.pod_logs}"

        self.step_manager.print_header("Check error info from node condition")
        node_info, _ = self.client.get_node_by_name(
            node_name=self.node_name, node_type="gpu"
        )
        assert node_info is not None, f"Find no node info by node name:{self.node_name}"
        expected_result = {
            "Condition Type": "NvswitchErrorFromKmsgWatch",
            "Condition Reason": "NvswitchErrorFromKmsgWatchIsNotHealthy",
            "Condition Message": "20034.*LTSSM Fault Up.*Recommended Action=RESET_GPU",
        }

        self.verfiy_health_monitor_info(
            conditions=node_info.status.conditions, expected_result=expected_result
        )
        self.client.remove_node_condition(self.node_name, "NvswitchErrorFromKmsgWatch")
        # Check unhandled interrupt error
        self.step_manager.print_header(
            "Simulate unhandled interrupt error by injecting the error info to /dev/kmsg"
        )
        command = [
            "/bin/sh",
            "-c",
            'echo "nvidia-nvswitch0: SXid (PCI:0000:ca:00.0): 10003, Fatal, Link 20 unhandled interrupt" | tee -a /host/dev/kmsg',
        ]
        output, _ = self.client.exec_command_in_pod(self.debug_pod, command)
        self.logger.info(f"OUTPUT: {output}")
        time.sleep(30)
        self.step_manager.print_header("Check error info From the pod log console")
        message_to_check = "unhandled interrupt"
        assert (
            message_to_check in self.pod_logs[-1]
        ), f"Find no expected message in console log:{self.pod_logs}"

        self.step_manager.print_header("Check error info from node condition")
        node_info, _ = self.client.get_node_by_name(
            node_name=self.node_name, node_type="gpu"
        )
        assert node_info is not None, f"Find no node info by node name:{self.node_name}"
        expected_result = {
            "Condition Type": "NvswitchErrorFromKmsgWatch",
            "Condition Reason": "NvswitchErrorFromKmsgWatchIsNotHealthy",
            "Condition Message": "10003.*unhandled interrupt.*Recommended Action=RESET_FABRIC",
        }
        self.verfiy_health_monitor_info(
            conditions=node_info.status.conditions, expected_result=expected_result
        )
        self.client.remove_node_condition(self.node_name, "NvswitchErrorFromKmsgWatch")
        # Check NVSwitch Seq ID error
        self.step_manager.print_header(
            "Simulate NVSwitch Seq ID error by injecting the error info to /dev/kmsg"
        )
        command = [
            "/bin/sh",
            "-c",
            'echo "nvidia-nvswitch0: SXid (PCI:0000:ca:00.0): 12020, Fatal, Link 20 Seq ID error" | tee -a /host/dev/kmsg',
        ]
        output, _ = self.client.exec_command_in_pod(self.debug_pod, command)
        self.logger.info(f"OUTPUT: {output}")
        assert (
            "12020, Fatal, Link 20 Seq ID error" in output
        ), f"Failed to inject Error: {output}"
        time.sleep(30)
        self.step_manager.print_header("Check error info From the pod log console")
        message_to_check = "Seq ID error"
        assert (
            message_to_check in self.pod_logs[-1]
        ), f"Find no expected message in console log:{self.pod_logs}"

        self.step_manager.print_header("Check error info from node condition")
        node_info, _ = self.client.get_node_by_name(
            node_name=self.node_name, node_type="gpu"
        )
        assert node_info is not None, f"Find no node info by node name:{self.node_name}"
        expected_result = {
            "Condition Type": "NvswitchErrorFromKmsgWatch",
            "Condition Reason": "NvswitchErrorFromKmsgWatchIsNotHealthy",
            "Condition Message": "12020.*Seq ID error.*Recommended Action=RESET_FABRIC",
        }
        self.verfiy_health_monitor_info(
            conditions=node_info.status.conditions, expected_result=expected_result
        )
        self.client.remove_node_condition(self.node_name, "NvswitchErrorFromKmsgWatch")
        # Check NVSwitch Egress CDT Parity error
        self.step_manager.print_header(
            "Simulate NVSwitch Egress CDT Parity error by injecting the error info to /dev/kmsg"
        )
        command = [
            "/bin/sh",
            "-c",
            'echo "nvidia-nvswitch0: SXid (PCI:0000:ca:00.0): 23017, Fatal, Link 20 Egress CDT Parity error" | tee -a /host/dev/kmsg',
        ]
        output, _ = self.client.exec_command_in_pod(self.debug_pod, command)
        self.logger.info(f"OUTPUT: {output}")
        assert (
            "23017, Fatal, Link 20 Egress CDT Parity error" in output
        ), f"Failed to inject Error: {output}"
        time.sleep(30)
        self.step_manager.print_header("Check error info From the pod log console")
        message_to_check = "Egress CDT Parity error"
        assert (
            message_to_check in self.pod_logs[-1]
        ), f"Find no expected message in console log:{self.pod_logs}"

        self.step_manager.print_header("Check error info from node condition")
        node_info, _ = self.client.get_node_by_name(
            node_name=self.node_name, node_type="gpu"
        )
        assert node_info is not None, f"Find no node info by node name:{self.node_name}"
        expected_result = {
            "Condition Type": "NvswitchErrorFromKmsgWatch",
            "Condition Reason": "NvswitchErrorFromKmsgWatchIsNotHealthy",
            "Condition Message": "23017.*Egress CDT Parity error.*Recommended Action=WORKFLOW_NVLINK_POTENTIALY_FATAL_ERR",
        }
        self.verfiy_health_monitor_info(
            conditions=node_info.status.conditions, expected_result=expected_result
        )
        self.logger.info(f"Clear nvswitch error on {self.node_name}")
        self.client.remove_node_condition(self.node_name, "NvswitchErrorFromKmsgWatch")
        self.clear_nvswitch_error(self.node_name)
