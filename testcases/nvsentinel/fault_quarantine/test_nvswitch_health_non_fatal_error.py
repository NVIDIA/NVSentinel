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
Module for class of NVsentinel Fault Quarantine:NVSwitch health non-fatal error
"""

import os
import yaml
import time
from testcases.nvsentinel.base import TestNVSentinelCaseBase
import pytest

class TestNVSwitchHealthNonFatalError(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine NVSwitch Health Non Fatal Error
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.smoke
    @pytest.mark.faultquarantine
    def test_nvswitch_health_non_fatal_error(self, request):
        """
        Tests if the node is not cordoned and not tainted when a non-fatal error is injected from NVSwitch health monitor that does not matches any ruleset
        """
        self.logger.info("Restart the fault-quarantine pod")
        self.delete_fault_quarantine_pod()

        nodes, _ = self.client.get_nodes()
        self.node_name = nodes[0].metadata.name
        self.logger.info(f"Node Name: {self.node_name}")

        self.step_manager.print_header("Login the node where the monitor pod is running on")
        yaml_file = os.path.join(os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "debug-pod.yaml")
        self.logger.info(f"yaml_file = {yaml_file}")
        with open(yaml_file, "r") as file:
            pod_body = yaml.safe_load(file)
            pod_body["spec"]["nodeName"] = self.node_name

        pods, _ = self.client.list_pods(
            pod_body["metadata"]["namespace"], name_pattern=pod_body["metadata"]["name"]
        )
        if pods:  # clean up existing debug pod before testing
            self.client.delete_pod(pod=pods[-1])
        self.debug_pod, _ = self.client.create_pod(pod_body=pod_body, wait=60)

        self.step_manager.print_header(
            "Simulate non fatal of NVSiwtch by injecting the error info to /dev/kmsg"
        )
        command = [
            "/bin/sh",
            "-c",
            'echo "nvidia-nvswitch0 SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04 RX Short Error Rate" | tee /dev/kmsg',
        ]
        output, _ = self.client.exec_command_in_pod(self.debug_pod, command)
        self.logger.info(f"OUTPUT: {output}")
        assert "Non-fatal, Link 04 RX Short Error Rate" in output
        time.sleep(20)

        self.step_manager.print_header("Check the node status")
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert "AggregatedNodeHealth" not in str(node_info.spec.taints)
        assert "node.kubernetes.io/unschedulable" not in str(node_info.spec.taints)
        assert (
            self.client.get_annotation_on_node(self.node_name, "quarantineHealthEvent")[0]
            is None
        )
        assert (
            self.client.get_annotation_on_node(
                self.node_name, "quarantineHealthEventAppliedTaints"
            )[0]
            is None
        )
        assert (
            self.client.get_annotation_on_node(
                self.node_name, "quarantineHealthEventIsCordoned"
            )[0]
            is None
        )
        assert node_info.spec.unschedulable is None
