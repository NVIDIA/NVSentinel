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
Module for class of Deployment: NVsentinel Pods are correctly deployed
"""

from testcases.nvsentinel.base import TestNVSentinelCaseBase
import pytest


class TestPodsAreCorrectlyDeployed(TestNVSentinelCaseBase):
    """
    Class for test case of Deployment: NVsentinel Pods are correctly deployed
    """
    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.smoke
    @pytest.mark.healthcheck
    def test_pods_are_correctly_deployed(self, request):
        """
        Tests if all NVsentinel pods are correctly deployed and running
        """
        self.logger.print_header("Check namespace nvsentinel exist")
        namespaces, _ = self.client.list_namespaces()
        namespace_names = [namespace.metadata.name for namespace in namespaces]
        self.logger.info(namespace_names)
        assert (
            self.nv_namespace in namespace_names
        ), f"Namespace - {self.nv_namespace} does not exist"

        self.logger.print_header("Check daemonsets exist.")
        daemonset_to_check = [
            "nvsentinel-nic-health-monitor",
            "nvsentinel-nvswitch-health-monitor",
            "nvsentinel-platform-connector",
        ]

        # Get the available GPU health monitor daemonset name
        self.logger.info("Starting to get available GPU monitor daemonset...")

        gpu_monitor_name = self.get_available_gpu_monitor_daemonset()

        self.logger.info(f"Available GPU monitor daemonset: {gpu_monitor_name}")
        assert gpu_monitor_name, "No available GPU monitor daemonset found"
        daemonset_to_check.append(gpu_monitor_name)

        daemonsets, _ = self.client.list_daemonset(namespace=self.nv_namespace)
        gpu_nodes, _ = self.client.get_nodes(node_type="gpu")
        cpu_nodes, _ = self.client.get_nodes(node_type="cpu")
        sys_nodes, _ = self.client.get_nodes(node_type="system")
        gpu_nodes_number = len(gpu_nodes)
        total_nodes_number = len(gpu_nodes) + len(cpu_nodes) + len(sys_nodes)
        self.logger.info(f"gpu_nodes_number : {gpu_nodes_number}")
        self.logger.info(f"total_nodes_number : {total_nodes_number}")

        assert daemonsets, f"ERROR: No resources found in {self.nv_namespace} namespace."

        total_pods_number = 0
        actual_daemonset_names = [daemonset.metadata.name for daemonset in daemonsets]

        # Check if all required daemonsets exist
        for required_daemonset in daemonset_to_check:
            assert (
                required_daemonset in actual_daemonset_names
            ), f"Required daemonset {required_daemonset} not found. Available daemonsets: {actual_daemonset_names}"

        # Check each daemonset's pod count
        for daemonset in daemonsets:
            daemonset_name = daemonset.metadata.name
            # Skip if daemonset is not in our check list
            if daemonset_name not in daemonset_to_check:
                continue

            current_nodes_number = daemonset.status.current_number_scheduled
            total_pods_number += int(current_nodes_number)
            self.logger.info(f"Daemonset Name:{daemonset_name}")
            self.logger.info(f"current_number_scheduled: {current_nodes_number}")

            target_nodes_number = (
                total_nodes_number
                if "platform-connector" in daemonset_name
                else gpu_nodes_number
            )

            err_msg = f"Mismatch nodes number found, current nodes number: {current_nodes_number}, expected: {target_nodes_number}"
            assert current_nodes_number == target_nodes_number, err_msg

        self.logger.print_header(
            "Check pods are in running state and pods number for each daemonset should match expected number."
        )
        target_pod_state = "Running"
        pods, _ = self.client.list_pods(namespace=self.nv_namespace)
        assert pods, f"ERROR: No resources found in {self.nv_namespace} namespace."
        number = 0
        for pod in pods:
            pod_state = pod.status.phase
            self.logger.info(f"pod name: {pod.metadata.name}")
            self.logger.info(f"Status: {pod_state}")
            err_msg = f"Invalid Pod state found: current state:{pod_state}, expected state:{target_pod_state}"
            if "mongodb" not in pod.metadata.name:
                assert pod_state == target_pod_state, err_msg
            if (
                "mongo" not in pod.metadata.name
                and "event" not in pod.metadata.name
                and "node-labeler" not in pod.metadata.name
                and "fault" not in pod.metadata.name
                and "drainer" not in pod.metadata.name
            ):
                number += 1
        err_msg = f"Mismatch nodes number found, current pods number:{number}, expected:{total_pods_number}"
        assert number == total_pods_number, err_msg
