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
Module for class of NVsentinel Fault Quarantine: MaxCordon
"""

import yaml
import os
import time
import pytest
from testcases.nvsentinel.base import TestNVSentinelCaseBase

class TestMaxCordon(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine: MaxCordon
    """

    backup_cm = "backup_fault_quarantine_cm.yaml"

    @pytest.fixture(autouse=True)
    def setup_max_cordon(self, setup_runai_test):
        # Equivalent to setUp in unittest
        self.logger.info("[Setup] max_cordon")
        try:
            yield
        finally:
            # Equivalent to addCleanup in unittest
            fault_quarantine_deployment = self.client.get_deployments(self.nv_namespace, "nvsentinel-fault-quarantine")
            if not fault_quarantine_deployment:
                return
            self.logger.info("[Teardown] max_cordon")
            self.client.apply_configmap(self.backup_cm)
            self.delete_fault_quarantine_pod()


    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.faultquarantine
    def test_max_cordon(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Tests:
         1. When MaxPercentageOfNodesToCordon is 100, then all the nodes are cordoned
         2. When MaxPercentageOfNodesToCordon is 50, then 50% of the nodes are cordoned
            - Upon clearing the fatal error from one of the cordoned node, the node is uncordoned,
              and the other node which was not cordoned before but has the same fatal error, is cordoned
        """
        self.skip_if_fault_quarantine_deployment_not_found()
        self.step_manager.print_header("Backup the default fault quarantine config map")
        self.client.backup_configmap(
            "nvsentinel", "fault-quarantine-config", self.backup_cm
        )

        self.step_manager.print_header("Redeploy fault quarantine with a new config map of max cordon")
        cm_yaml = os.path.join(
            os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "max-cordon.yaml"
        )

        self.step_manager.print_header("Apply the new config map with maxPercentageOfNodesToCordon <= 100")
        self.client.apply_configmap(cm_yaml)
        self.logger.info("Restart the fault-quarantine pod")
        self.delete_fault_quarantine_pod()
        time.sleep(30)

        gpu_health_monitor_pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )

        self.step_manager.print_header("Check if the node is uncordoned")
        nodes, _ = self.client.get_nodes(ready=False)
        self.remove_managed_by_nvsentinel_label_from_all_nodes(nodes)
        assert all(node.spec.unschedulable is None for node in nodes), f"FAIL: Node is cordoned"

        self.step_manager.print_header("Inject a fatal error on all of the GPU nodes")
        self.inject_gpu_inforom_on_all_nodes(gpu_health_monitor_pods)
        time.sleep(30)


        self.step_manager.print_header("Check if the node is cordoned")
        nodes, _ = self.client.get_nodes(ready=False)
        assert all(node.spec.unschedulable is True for node in nodes), f"FAIL: Node is not cordoned"

        self.step_manager.print_header("Clear the fatal error")
        self.clear_gpu_inforom_on_all_nodes(gpu_health_monitor_pods)

        time.sleep(30)
        self.step_manager.print_header("Check if the node is uncordoned")
        nodes, _ = self.client.get_nodes(ready=False)
        assert all(node.spec.unschedulable is None for node in nodes), f"FAIL: Node is cordoned"


        self.step_manager.print_header("Apply the new config map with maxPercentageOfNodesToCordon <= 50")
        self.apply_50_percent_cordon_configmap(cm_yaml)

        self.step_manager.print_header("Restart the fault-quarantine pod")
        self.delete_fault_quarantine_pod()
        time.sleep(30)

        self.step_manager.print_header("Inject a fatal error on all of the GPU nodes")
        self.inject_gpu_inforom_on_all_nodes(gpu_health_monitor_pods)
        time.sleep(30)

        self.step_manager.print_header("Check if the 50% of the nodes are cordoned")
        nodes, _ = self.client.get_nodes(ready=False)
        cordoned_nodes = [node.metadata.name for node in nodes if node.spec.unschedulable is True]
        print(f"cordoned_nodes: {cordoned_nodes}")
        assert 0 < len(cordoned_nodes) / len(nodes) <= 0.5, f"FAIL: More than 50% of the nodes are cordoned"

        cordoned_node = cordoned_nodes[0]
        gpu_health_monitor_pod = [pod for pod in gpu_health_monitor_pods if pod.spec.node_name == cordoned_node][0]
        self.step_manager.print_header("Clear the fatal error on one of the cordoned nodes")
        self.clear_gpu_inforom_watch_error(gpu_health_monitor_pod)
        time.sleep(30)

        self.step_manager.print_header("Check if the node is uncordoned")
        nodes, _ = self.client.get_nodes(ready=False)
        new_cordoned_nodes = [node.metadata.name for node in nodes if node.spec.unschedulable is True]

        assert len(new_cordoned_nodes) == len(cordoned_nodes), f"FAIL: The number of cordoned nodes is not same"
        assert cordoned_node not in new_cordoned_nodes, f"FAIL: The cordoned node is not changed"

        self.step_manager.print_header("Clear the fatal error")
        self.clear_gpu_inforom_on_all_nodes(gpu_health_monitor_pods)

        time.sleep(30)
        self.restore_managed_by_nvsentinel_label_to_all_nodes(nodes)



    def apply_50_percent_cordon_configmap(self, cm_yaml):
        with open(cm_yaml, "r") as f:
            cm = yaml.safe_load(f)
        cm["data"]["config.toml"] = cm["data"]["config.toml"].replace(
            "maxPercentageOfNodesToCordon <= 100", "maxPercentageOfNodesToCordon <= 50"
        )

        temp_cm_path = "/tmp/immediate_fault_cm_temp.yaml"
        with open(temp_cm_path, "w") as f:
            yaml.dump(cm, f, default_flow_style=False)
        self.client.apply_configmap(temp_cm_path)

        

    def inject_gpu_inforom_on_all_nodes(self, gpu_health_monitor_pods):
        for pod in gpu_health_monitor_pods:
            assert pod.status.phase == "Running", f"FAIL: Pod {pod.metadata.name} is not running"

            self.inject_gpu_inforom_watch_error(pod)

    def clear_gpu_inforom_on_all_nodes(self, gpu_health_monitor_pods):
        for pod in gpu_health_monitor_pods:
            assert pod.status.phase == "Running", f"FAIL: Pod {pod.metadata.name} is not running"

            self.clear_gpu_inforom_watch_error(pod)

    def remove_managed_by_nvsentinel_label_from_all_nodes(self, nodes):
        self.node_to_label_map = {}
        for node in nodes:
            self.node_to_label_map[node.metadata.name] = self.client.get_label_on_node(node.metadata.name, "k8saas.nvidia.com/ManagedByNVSentinel")
            self.remove_managed_by_nvsentinel_label(node.metadata.name)

    def restore_managed_by_nvsentinel_label_to_all_nodes(self, nodes):
        for node in nodes:
            self.backup_label_value = self.node_to_label_map[node.metadata.name]
            self.restore_managed_by_nvsentinel_label(node.metadata.name)