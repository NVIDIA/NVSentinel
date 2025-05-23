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

import pytest
from testcases.nvsentinel.base import TestNVSentinelCaseBase
import time
import os
import yaml

default_namespace = "default"
class TestNodeRecoveryDuringEvictionProcessAfterPodShifted(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Notification: Node recovery during pod eviction process after the pod is shifted to another node
    """

    backup_cm_path = "node-drainer-config-backup.yaml"

    @pytest.fixture(autouse=True)
    def setup_node_drainer(self, setup_runai_test):
        self.logger.info("[Setup] Node Drainer Pod")
        try:
            yield
        finally:
            self.logger.info("[Teardown] Node drainer Pod")
            self.logger.info("Cleaning up resources if any created during the test.")
            node_drainer_deployment = self.client.get_deployments(self.nv_namespace, "nvsentinel-node-drainer")
            if not node_drainer_deployment:
                return
            success, error = self.client.apply_configmap(self.backup_cm_path)
            if error:
                self.logger.error(
                    f"Failed to restore node-drainer configmap: {error}"
                )

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.nodedrainer
    def test_node_recovery_during_eviction_process_after_pod_shifted(
        self, request, nvsentinel_autosync_disabled_enabled
    ):
        """
        Tests immediate mode: verifies that:
            1. The node is cordoned on injection of gpu fatal error
            2. Job pod is evicted and shifted to another node
            3. The node is recovered when the error is cleared
        """
        self.skip_if_node_drainer_deployment_not_found()
        self.step_manager.print_header("Check the node-drainer pod is running")
        # Get node drainer pod
        node_drainer_pod = self.get_node_drainer_pod()
        self.logger.info(
            f"Node Drainer Pod Name: {node_drainer_pod.metadata.name}"
        )
        # Verify pod is running
        assert node_drainer_pod, "Node drainer pod is not present"

        self.step_manager.print_header("Backup default node-drainer configmap")
        self.client.backup_configmap(
            self.nv_namespace, "node-drainer-config", self.backup_cm_path
        )

        self.step_manager.print_header("Edit default node-drainer configmap")
        # Read the original configmap yaml
        cm_yaml = os.path.join(
            os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "immediate-fault-cm.yaml"
        )
        with open(cm_yaml, "r") as f:
            cm_content = yaml.safe_load(f)
        # Replace the namespace name
        cm_content["data"]["config.toml"] = cm_content["data"]["config.toml"].replace(
            "runai-qa-automation-test", default_namespace
        )
        # Save to a temporary file
        temp_cm_path = "/tmp/immediate_fault_cm_temp.yaml"
        with open(temp_cm_path, "w") as f:
            yaml.dump(cm_content, f, default_flow_style=False)
        self.logger.info(f"Modified configmap saved to {temp_cm_path}")
        self.client.apply_configmap(temp_cm_path)

        self.step_manager.print_header("Restart the node-drainer pod")
        self.delete_node_drainer_pod()
        self.wait_for_node_drainer_pod_to_start()
        time.sleep(60) # wait for the node drainer pod to connect to mongodb

        self.step_manager.print_header(
            "Submit a training job under the project and wait unitil it's running"
        )

        job_yaml_path = os.path.join(os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "gpu-job.yaml")
        job, error = self.client.create_job_from_yaml(job_yaml_path, default_namespace).values
        assert job, f"Failed to create job: {error}"

        self.job_name = job.metadata.name
        time.sleep(10)
        success, error = self.client.verify_job_is_running(self.job_name, default_namespace)
        assert success, f"Job {self.job_name} is not running: {error}"

        job_pod_name, _ = self.client.get_job_pod_name(self.job_name, default_namespace).values
        assert job_pod_name, f"Failed to get job pod name: {error}"

        self.node_name, _ = self.client.get_pod_running_node_name(
            job_pod_name, default_namespace
        ).values
        self.logger.info(f"Training job pod is running on node: {self.node_name}")
        self.remove_managed_by_nvsentinel_label(self.node_name)
        self.step_manager.print_header(
            "Running 3 busybox pods under namespace busybox on 2 nodes, set one of the nodes to be the nodename  where the job pod is running on"
        )
        success, error = self.client.create_namespace("busybox")
        request.addfinalizer(lambda: self.client.delete_namespace("busybox"))

        # Get GPU nodes
        self.step_manager.print_header("Get GPU nodes from cluster")
        gpu_node_names, _ = self.client.get_node_names_by_label("nvidia.com/gpu.present=true")
        if len(gpu_node_names) < 2:
            self.logger.error(
                f"Not enough GPU nodes found. Found {len(gpu_node_names)}, need at least 2"
            )
            assert False, "Not enough GPU nodes found"
        self.logger.info(f"Found GPU nodes: {gpu_node_names}")

        target_busybox_pod_name = "busybox-1"
        # Find a GPU node that is different from job_node_name
        other_gpu_node = next(
            (node for node in gpu_node_names if node != self.node_name), None
        )
        if not other_gpu_node:
            self.logger.error("Could not find a GPU node different from the job node")
            assert False, "Could not find a GPU node different from the job node"

        # Update busybox yaml with GPU node names
        yamlfile_path = "nvsentinel/testcases/data/cli/nvsentinel"
        busybox_yaml = os.path.join(yamlfile_path, "busybox-pod-creation.yaml")
        with open(busybox_yaml, "r", encoding="utf-8") as f:
            yaml_content = self.load_yaml(
                f,
                {
                    "NODE_NAME_1": self.node_name,
                    "NODE_NAME_2": other_gpu_node,
                    "NODE_NAME_3": other_gpu_node,
                },
            )

        self.logger.info(
            f"Updated busybox yaml with nodes: job_node={self.node_name}, other_node={other_gpu_node}"
        )
        self.logger.info(f"Target busybox pod name: {target_busybox_pod_name}")
        # Create a temporary file for the updated yaml
        temp_yaml = "/tmp/busybox-pod-creation.yaml"
        with open(temp_yaml, "w", encoding="utf-8") as tmp_file:
            yaml.dump(yaml_content, tmp_file)

        # Apply busybox pods using the temporary file
        with open(temp_yaml, "r") as f:
            pod_body = yaml.safe_load(f)
            for pod in pod_body["items"]:
                success, error = self.client.create_pod(pod, wait=60)
                if error:
                    self.logger.error(f"Failed to create pod: {error}")
                    assert False, f"Failed to create pod: {error}"

        self.step_manager.print_header("Verify busybox pods are running")
        # Get all busybox pod names
        pods, error = self.client.list_pods(namespace="busybox", name_pattern="busybox-*")
        # Get pod names
        busybox_pod_names = [pod.metadata.name for pod in pods]
        self.logger.info(f"Found busybox pods: {busybox_pod_names}")
        # Verify all busybox pods are running
        assert self.client.verify_pod_are_running(busybox_pod_names, "busybox"), "Busybox pods are not running"

        self.step_manager.print_header(
            "Pick one node and inject a gpu fatal error on the node, eg node 10.0.43.235 , where are busybox-2 and mpi job are running"
        )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor.*"
        )
        gpu_monitor_pod_name = None
        for pod in pods:
            if pod.spec.node_name == self.node_name:
                gpu_monitor_pod_name = pod.metadata.name
                self.logger.info(f"POD   Name: {gpu_monitor_pod_name}")
                self.logger.info(f"Node  Name: {self.node_name}")
                break
        assert (
            gpu_monitor_pod_name
        ), f"Cannot find the nvsentinel-gpu-health-monitor pod of the node {self.node_name}"

        self.step_manager.print_header("Inject a gpu fatal error on the node")
        pods, _ = self.client.list_pods(
            namespace=self.nv_namespace, name_pattern=gpu_monitor_pod_name
        )
        assert len(pods) > 0, "GPU health monitor pod not found"
        gpu_health_monitor_pod = pods[0]
        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 84 -v 0",
        ]
        output, error = self.client.exec_command_in_pod(
            gpu_health_monitor_pod, command=command
        )
        assert "Successfully injected" in output, "Failed to inject a gpu fatal error on the node"

        self.step_manager.print_header("Check the node is cordoned")
        success, error = self.client.check_node_cordoned(self.node_name)
        assert success, f"FAIL: Node {self.node_name} is not cordoned"

        self.step_manager.print_header("Check all pods in busybox are running")
        filtered_pod_names = [
            name for name in busybox_pod_names if name != target_busybox_pod_name
        ]
        self.logger.info(f"Filtered busybox pods: {filtered_pod_names}")
        # Verify all busybox pods are running
        assert self.client.verify_pod_are_running(filtered_pod_names, "busybox"), "Busybox pods are not running"

        self.step_manager.print_header(
            "Check the job pod, it will be shifted to another node"
        )
        time.sleep(90)
        job_pod_name, _ = self.client.get_job_pod_name(self.job_name, default_namespace).values
        job_node_name1, _ = self.client.get_pod_running_node_name(
            job_pod_name, default_namespace
        ).values
        self.logger.info(f"Training job pod is running on node: {job_node_name1}")
        assert (
            job_node_name1 != self.node_name
        ), "The job pod is not running on the original node"

        self.step_manager.print_header(
            "Clear the inject error in step 5 and check the node is uncordoned"
        )
        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 84 -v 1",
        ]
        self.client.exec_command_in_pod(gpu_health_monitor_pod, command=command)
        success, error = self.client.check_node_ready(self.node_name)
        assert (
            success
        ), f"Node '{self.node_name}' is not ready after clearing injected error."


        self.step_manager.print_header(
            "Check the pods again, pod running node was terminated"
        )
        assert self.client.verify_pod_are_running(filtered_pod_names, "busybox"), "Busybox pods are not running"
        pods, _ = self.client.list_pods(
            namespace="busybox", name_pattern=target_busybox_pod_name
        )
        assert len(pods) == 0, "The target busybox pod is not terminated"

        self.step_manager.print_header("Check the job pod, it still running on second node")
        job_pod_name, _ = self.client.get_job_pod_name(self.job_name, default_namespace).values
        job_node_name2, _ = self.client.get_pod_running_node_name(
            job_pod_name, default_namespace
        ).values
        self.logger.info(f"Training job pod is running on node: {job_node_name2}")
        assert (
            job_node_name2 == job_node_name1
        ), "The job pod is not running on the second node"

        self.step_manager.print_header("Delete all the running pods and jobs")
        for pod in busybox_pod_names:
            self.client.delete_pod_by_name(pod, "busybox")
        self.client.delete_job(self.job_name, default_namespace)
        pods, _ = self.client.list_pods(default_namespace, name_pattern=self.job_name + ".*")
        for pod in pods:
            self.client.delete_pod_by_name(pod.metadata.name, default_namespace)
        self.step_manager.print_header("Restore the change of node-drainer configmap")
        node_drainer_pod = self.get_node_drainer_pod()
        self.client.delete_pod_by_name(
            node_drainer_pod.metadata.name, self.nv_namespace
        )
        self.restore_managed_by_nvsentinel_label(self.node_name)