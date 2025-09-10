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

import os
import time
import pytest
from functools import partial
import pytest
from testcases.nvsentinel.base import TestNVSentinelCaseBase
import os

class TestNodeDrainerNodeLabeling(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Node Drainer: Node drainer node label
    """
    node_drain_label_key = "nvsentinel.dgxc.nvidia.com/node-drain-status"
    node_drain_label_value = "IN_PROGRESS"
    backup_cm_path = "node-drainer-config-backup.yaml"
    
    @pytest.fixture(autouse=True)
    def setup_node_drainer(self, setup_runai_test):
        self.logger.info("[Setup] Node Drainer Pod")
        try:
            yield
        finally:
            self.logger.info("[Teardown] Node Drainer Pod")
            self.logger.info("Cleaning up resources if any created during the test.")
            node_drainer_deployment = self.client.get_deployments(self.nv_namespace, "nvsentinel-node-drainer")
            if node_drainer_deployment:
                success, error = self.client.apply_configmap(self.backup_cm_path)
                if error:
                    self.logger.error(
                        f"Failed to restore node-drainer configmap: {error}"
                    )
            else:
                self.logger.warning("Node drainer deployment not found during cleanup")


    @pytest.mark.author(email="tanishag@nvidia.com")
    @pytest.mark.nodedrainer
    def test_node_drainer_node_label(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Tests if the node label is getting updated correctly
        """
        self.skip_if_node_drainer_deployment_not_found()
        self.step_manager.print_header("Check the node drainer pod is running")
        node_drainer_pod = self.get_node_drainer_pod()
        assert node_drainer_pod, "Node drainer pod is not present"

        self.step_manager.print_header("Backup default node-drainer configmap")
        self.client.backup_configmap(
            self.nv_namespace, "node-drainer-config", self.backup_cm_path
        )

        self.step_manager.print_header("Edit default node-drainer configmap")
        # Read the original configmap yaml
        cm_yaml = os.path.join(
            os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "allowcompletion-immediate-mode.yaml"
        )

        self.client.apply_configmap(cm_yaml)

        self.step_manager.print_header("Restart node-drainer pod so it picks the new namespace")
        self.delete_node_drainer_pod()
        self.wait_for_node_drainer_pod_to_start()
        time.sleep(10) # wait for the node drainer pod to connect to mongodb

        job_namespace = "runai-qa-automation-test"
        self.step_manager.print_header(
            "Submit a training job under the project and wait until it's running"
        )
        
        job_yaml_path = os.path.join(os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "gpu-job.yaml")
        namespace_obj, error = self.client.create_namespace(job_namespace)
        request.addfinalizer(partial(self.client.delete_namespace, job_namespace))
        assert namespace_obj, f"Failed to create namespace: {error}"
        
        job, error = self.client.create_job_from_yaml(job_yaml_path, job_namespace).values
        assert job, f"Failed to create job: {error}"
        self.job_name = job.metadata.name
        time.sleep(10)
        
        success, error = self.client.verify_job_is_running(self.job_name, job_namespace)
        assert success, f"Job {self.job_name} is not running: {error}"

        job_pod_name, _ = self.client.get_job_pod_name(self.job_name, job_namespace).values
        assert job_pod_name, f"Failed to get job pod name: {error}"

        self.node_name, _ = self.client.get_pod_running_node_name(
            job_pod_name, job_namespace
        ).values
        self.remove_managed_by_nvsentinel_label(self.node_name)
        request.addfinalizer(partial(self.restore_managed_by_nvsentinel_label, self.node_name))
        self.logger.info(f"Training job pod is running on node: {self.node_name}")

        self.step_manager.print_header("Get GPU nodes from cluster")
        
        # Get GPU health monitor pod running on the node
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
        
        self.logger.info(f"Injecting gpu fatal error on Node: {self.node_name}")
        
        # Inject GPU inforom watch error
        self.inject_gpu_inforom_watch_error(gpu_health_monitor_pod)
        request.addfinalizer(partial(self.clear_gpu_inforom_watch_error, gpu_health_monitor_pod))
        time.sleep(30)

        self.verify_gpu_inforom_watch_condition(self.node_name)

        self.step_manager.print_header("Check node-drainer pod logs for eviction monitoring")
        
        # Get node drainer pod logs
        node_drainer_logs = self.get_node_drainer_pod_log()
        
        # Split logs into lines and get last 5 lines
        log_lines = node_drainer_logs.splitlines()
        last_5_logs = log_lines[-5:] if len(log_lines) >= 5 else log_lines

        for log in last_5_logs:
            self.logger.info(log)

        self.step_manager.print_header("Verify node-drainer waiting for job pod")
        expected_msg = f'Still waiting for this pod to finish" node="{self.node_name}" name="{job_pod_name}" namespace="{job_namespace}"'
        self.verify_node_drainer_pod_log([expected_msg])

        result = self.client.get_node_labels(self.node_name)
        assert result, "Node labels not found"
        labels = result[0]  # extract the label dict from CommonResult
        assert labels.get(self.node_drain_label_key) == self.node_drain_label_value, "Node drain label not found"

        self.step_manager.print_header("Clear the gpu fatal error")
        self.clear_gpu_inforom_watch_error(gpu_health_monitor_pod)
        time.sleep(30)

        self.step_manager.print_header("Verify the node drain label is cleared")
        result = self.client.get_node_labels(self.node_name)
        assert result, "Node labels not found"
        labels = result[0]  # extract the label dict from CommonResult
        print(labels)
        assert labels.get(self.node_drain_label_key) is None, "Node drain label not cleared"
        self.logger.info("Node drain label cleared")