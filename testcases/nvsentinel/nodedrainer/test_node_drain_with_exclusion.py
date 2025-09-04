# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from functools import partial
import pytest
from testcases.nvsentinel.base import TestNVSentinelCaseBase
import time
import os
import yaml

class TestNodeDrainWithExclusion(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Node Drainer: Node drainer with exclusion should not drain namespaces matching the exclusion pattern
    """

    @pytest.mark.author(email="tanishag@nvidia.com")
    @pytest.mark.nodedrainer
    def test_node_drain_with_exclusion(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Test case for node drainer with exclusion to exclude monitoring pods in system namespaces
        """

        self.skip_if_node_drainer_deployment_not_found()
        self.step_manager.print_header("Check the node drainer pod is running")

        node_drainer_pod = self.get_node_drainer_pod()
        assert node_drainer_pod, "Node drainer pod is not present"
        
        self.step_manager.print_header("Create pod in kube-system namespace")
        # create pod in kube-system namespace
        yamlfile_path = os.path.join(os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel")
        kube_system_yaml = os.path.join(yamlfile_path, "kube-system-pod-creation.yaml")
        
        # Select a target node to run the pods (pick the first GPU node, or any node if none)
        gpu_node_names, _ = self.client.get_node_names_by_label("nvidia.com/gpu.present=true")
        if gpu_node_names:
            self.node_name = gpu_node_names[0]
        else:
            # Fall back to any schedulable node in the cluster
            node_names, _ = self.client.get_node_names_by_label("kubernetes.io/hostname")
            assert node_names, "Could not find any node to schedule test pods"
            self.node_name = node_names[0]

        # ------------------------------------------------------------------
        # 1. Create pod(s) in kube-system (should be EXCLUDED by node-drainer)
        # ------------------------------------------------------------------
        with open(kube_system_yaml, "r", encoding="utf-8") as f:
            kube_sys_yaml_content = self.load_yaml(f, {"NODE_NAME_1": self.node_name})
        for pod_spec in kube_sys_yaml_content["items"]:
            success, err = self.client.create_pod(pod_spec, wait=60)
            assert success, f"Failed to create kube-system pod: {err}"
            # Ensure cleanup afterwards
            request.addfinalizer(partial(self.client.delete_pod, pod_spec))

        self.logger.info("Successfully created test pods in kube-system namespaces")
        # Additional verification steps (eviction / exclusion) will follow in subsequent steps of the test
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        gpu_monitor_pod_name = None
        for pod in pods:
            if pod.spec.node_name == self.node_name:
                gpu_monitor_pod_name = pod.metadata.name
                self.logger.info(f"GPU Monitor Pod Name: {gpu_monitor_pod_name}")
                break
        assert gpu_monitor_pod_name, f"Cannot find the nvsentinel-gpu-health-monitor pod of the node {self.node_name}"

        self.step_manager.print_header("Inject a gpu fatal error on the node")
        pods, _ = self.client.list_pods(
            namespace=self.nv_namespace, name_pattern=gpu_monitor_pod_name
        )
        assert len(pods) > 0, "GPU health monitor pod not found"
        gpu_health_monitor_pod = pods[0]
        
        # Inject GPU inforom watch error
        self.inject_gpu_inforom_watch_error(gpu_health_monitor_pod)
        time.sleep(30)

        self.verify_gpu_inforom_watch_condition(self.node_name)

        self.step_manager.print_header("Check node-drainer pod logs for eviction monitoring")
        
        # Get node drainer pod logs
        node_drainer_logs = self.get_node_drainer_pod_log()
        
        # Split logs into lines and get last 15 lines
        log_lines = node_drainer_logs.splitlines()
        last_15_logs = log_lines[-15:] if len(log_lines) >= 15 else log_lines

        for log in last_15_logs:
            self.logger.info(log)
        
        unexpected_messages = [
            f'Still waiting for this pod to finish" node="{self.node_name}" name="node-drainer-test-pod" namespace="kube-system"'
        ]
        assert not any(message in node_drainer_logs for message in unexpected_messages), "Node drainer is evicting pods from excluded namespace"

        self.clear_gpu_inforom_watch_error(gpu_health_monitor_pod)
        time.sleep(10)