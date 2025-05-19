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
Module for class of NVsentinel Fault Quarantine:NVswitch health fatal error
"""

import pytest
import time
from testcases.nvsentinel.base import TestNVSentinelCaseBase

MONGO_URI = "mongodb://CN%3Dmongo-user-client%2COU%3DDGXC%2CO%3DNvidia%2CL%3DSantaClara%2CST%3DCalifornia%2CC%3DUS@nvsentinel-mongodb-headless.nvsentinel.svc.cluster.local:27017/HealthEventsDatabase?authMechanism=MONGODB-X509&authSource=$external&tls=true"


class TestNVSwitchHealthFatalErrorRecover(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine NVSwitch Health Fatal Error Recover
    """

    reboot = False

    @pytest.fixture(autouse=True)
    def setup_nvswitch_fatal_error(self, setup_runai_test):
        # Equivalent to setUp in unittest
        self.logger.info("[Setup] nvswitch_fatal_error")
        try:
            yield
        finally:
            # Equivalent to addCleanup in unittest
            self.logger.info("[Teardown] nvswitch_fatal_error")
            platform_connector = self.get_platform_connector_by_node_name(self.node_name)
            assert (
                platform_connector
            ), f"cannot find the platform connector of {self.node_name}"
            self.client.delete_pod(platform_connector)

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.faultquarantine
    def test_nvswitch_health_fatal_error_recover(self, request):
        """
        Tests if the node is cordoned and tainted when the node is matched with the rule, and the node is recovered when the error is cleared
        """
        self.skip_if_fault_quarantine_deployment_not_found()
        self.logger.info("Restart the fault-quarantine pod")
        self.delete_fault_quarantine_pod()
        nodes, _ = self.client.get_nodes()
        self.node_name = nodes[0].metadata.name
        self.remove_managed_by_nvsentinel_label(self.node_name)
        self.logger.info(f"Node Name: {self.node_name}")

        self.step_manager.print_header("Login the node where the monitor pod is running on")

        self.debug_pod = self.create_debug_pod(self.node_name)
        self.step_manager.print_header(
            "Simulate SXID fatal error by injecting the error info to /dev/kmsg"
        )

        node, _ = self.client.get_node_by_name(self.node_name)
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
        output, _ = self.client.exec_command_in_pod(self.debug_pod, command)
        self.logger.info(f"OUTPUT: {output}")
        assert "20034, Fatal" in output
        time.sleep(20)

        self.step_manager.print_header("Check the node taints and annotations")
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert node_info.spec.unschedulable is True
        self.logger.info("Check the taints on the node")
        target_conditions = [
            {
                "key": "node.kubernetes.io/unschedulable",
                "value": None,
                "effect": "NoSchedule",
            },
        ]
        assert self.client.check_taints_on_node(
            self.node_name, conditions=target_conditions
        )

        self.logger.info("Check the annotations on the node")
        annotations, _ = self.client.get_annotation_on_node(
            self.node_name, "quarantineHealthEvent"
        )
        assert (
            '"agent":"nvswitch-health-monitor","componentClass":"nvswitch","checkName":"NvswitchErrorFromKmsgWatch","isFatal":true'
            in annotations
        )
        assert (
            self.client.get_annotation_on_node(
                self.node_name, "quarantineHealthEventIsCordoned"
            )[0]
            == "True"
        )

        self.step_manager.print_header(
            "Run clear error java script recover nvswitch fatal error"
        )
        self.clear_nvswitch_error(self.node_name)

        # delete platform connector pod
        platform_connector = self.get_platform_connector_by_node_name(self.node_name)
        assert platform_connector, f"cannot find the platform connector of {self.node_name}"
        self.client.delete_pod(platform_connector)

        time.sleep(20)


        self.step_manager.print_header(
            "Check the node status, taints and annotations are removed"
        )
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert "node.kubernetes.io/unschedulable" not in str(node_info.spec.taints)
        assert (
            self.client.get_annotation_on_node(self.node_name, "quarantineHealthEvent")[0]
            is None
        )
        assert (
            self.client.get_annotation_on_node(
                self.node_name, "quarantineHealthEventIsCordoned"
            )[0]
            is None
        )
        assert node_info.spec.unschedulable is None
        self.client.remove_node_condition(self.node_name, "NvswitchErrorFromKmsgWatch")
        self.restore_managed_by_nvsentinel_label(self.node_name)