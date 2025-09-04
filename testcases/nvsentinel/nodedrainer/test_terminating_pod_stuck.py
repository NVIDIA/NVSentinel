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
import os
import yaml
import time

class TestTerminatingPodStuck(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Node drainer
    """

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
            if not node_drainer_deployment:
                self.logger.info("Node drainer deployment not found, skipping configmap restoration")
            else:
                success, error = self.client.apply_configmap(self.backup_cm_path)
                if error:
                    self.logger.error(
                        f"Failed to restore node-drainer configmap: {error}"
                    )
    
    @pytest.mark.author(email="tanishag@nvidia.com")
    @pytest.mark.nodedrainer
    def test_terminating_pod_stuck(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Test case for node drainer handling stuck terminating pod: the pod should not be monitored if its stuck in terminating state
        """
        self.skip_if_node_drainer_deployment_not_found()
        self.step_manager.print_header("Check the node-drainer pod is running")
        self.wait_for_node_drainer_pod_to_start()

        test_ns = "runai-qa-automation-test"
        pod_name = "busybox-1"

        self.step_manager.print_header(f"Create namespace {test_ns} and a terminating pod with dummy finalizer")
        ns_obj, err = self.client.create_namespace(test_ns)
        assert ns_obj, f"Failed to create namespace: {err}"
        request.addfinalizer(lambda: self.client.delete_namespace(test_ns))

        self.step_manager.print_header("Backup and patch node-drainer configmap to include the test namespace")
        self.client.backup_configmap(self.nv_namespace, "node-drainer-config", self.backup_cm_path)

        cm_yaml_path = os.path.join(
            os.getcwd(),
            "nvsentinel",
            "testcases",
            "data",
            "cli",
            "nvsentinel",
            "allowcompletion-immediate-mode.yaml",
        )
        with open(cm_yaml_path, "r") as f:
            cm_content = yaml.safe_load(f)

        temp_cm = "/tmp/node_drainer_cm_terminating.yaml"
        with open(temp_cm, "w") as f:
            yaml.dump(cm_content, f)
        # Apply patched configmap
        self.client.apply_configmap(temp_cm)

        self.step_manager.print_header("Restart node-drainer pod so it picks the new namespace")
        self.delete_node_drainer_pod()
        self.wait_for_node_drainer_pod_to_start()
        time.sleep(30)  # give it some time to reconnect to MongoDB

        # Get GPU nodes
        self.step_manager.print_header("Get GPU nodes from cluster")
        gpu_node_names, _ = self.client.get_node_names_by_label("nvidia.com/gpu.present=true")
        self.logger.info(f"Found GPU nodes: {gpu_node_names}")
        self.node_name = gpu_node_names[0]


        # Update busybox yaml with GPU node names
        yamlfile_path = os.path.join(os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel")
        busybox_yaml = os.path.join(yamlfile_path, "busybox-pod-creation-with-finalizer.yaml")
        with open(busybox_yaml, "r", encoding="utf-8") as f:
            yaml_content = self.load_yaml(
                f,
                {
                    "NODE_NAME_1": self.node_name,
                },
            )

        self.logger.info(
            f"Created testing pod on node: {self.node_name}"
        )
        temp_yaml = "/tmp/busybox-pod-creation-with-finalizer.yaml"
        with open(temp_yaml, "w", encoding="utf-8") as tmp_file:
            yaml.dump(yaml_content, tmp_file)

        # Apply busybox pods using the Python SDK
        with open(temp_yaml, "r") as f:
            pod_body = yaml.safe_load(f)
            for pod in pod_body["items"]:
                success, error = self.client.create_pod(pod, wait=60)
                request.addfinalizer(lambda: self.client.force_delete_pod(test_ns, pod_name))
                if error:
                    self.logger.error(f"Failed to create pod: {error}")
                    assert False, f"Failed to create pod: {error}"

        self.step_manager.print_header("Verify busybox pods are running")
        # Get all busybox pod names
        pods, error = self.client.list_pods(namespace=test_ns, name_pattern="busybox-*")
        # Get pod names
        busybox_pod_names = [pod.metadata.name for pod in pods]
        self.logger.info(f"Found busybox pods: {busybox_pod_names}")
        # Verify all busybox pods are running
        assert self.client.verify_pod_are_running(busybox_pod_names, test_ns), "Busybox pods are not running"

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

        self.inject_gpu_inforom_watch_error(gpu_health_monitor_pod)
        request.addfinalizer(lambda: self.clear_gpu_inforom_watch_error(gpu_health_monitor_pod))
        time.sleep(10)  # allow health event propagation

        # Verify condition shows up on node
        self.verify_gpu_inforom_watch_condition(self.node_name)

        log_lines = self.get_node_drainer_pod_log().splitlines()
        last_5_logs = log_lines[-5:] if len(log_lines) >= 5 else log_lines
        self.logger.info("Last 5 logs from node-drainer pod:")
        for log in last_5_logs:
            self.logger.info(log)

        self.step_manager.print_header("Verify node-drainer logs show wait message for stuck pod")
        expected_msg = f'Still waiting for this pod to finish" node="{self.node_name}" name="{pod_name}" namespace="{test_ns}"'
        self.verify_node_drainer_pod_log([expected_msg])

        # ------------------------------------------------------------------
        # Step-5 – Delete the pod – because of the finalizer it will be stuck in Terminating
        # ------------------------------------------------------------------
        self.step_manager.print_header("Delete the pod so it gets stuck in Terminating (finalizer blocks it)")
        _, err = self.client.delete_pod_by_name(pod_name, test_ns)
        assert not err, f"Failed to delete pod: {err}"
        self.step_manager.print_header("Wait for node drainer to exit the monitoring")
        time.sleep(40)

        final_expected_msg = f'Ignoring completed pod {pod_name} in namespace {test_ns} on node {self.node_name} (status: Failed) during eviction check'

        final_logs = self.get_node_drainer_pod_log().splitlines()
        final_5_logs = final_logs[-5:] if len(final_logs) >= 5 else final_logs
        self.logger.info("Last 5 logs from node-drainer pod:")
        for log in final_5_logs:
            self.logger.info(log)

        self.step_manager.print_header("Verify node-drainer logs no longer contain wait message")
        self.verify_node_drainer_pod_log([final_expected_msg])

        self.logger.info("[PASS] Node-drainer handled stuck terminating pod correctly")
