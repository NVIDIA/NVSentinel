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


# class TestEvictionOfTrainingJobsUsingPVCWithImmediateMode(TestNVSentinelCaseBase):
#     """
#     Class for test case of NVsentinel Fault Notification: Eviction of training jobs using PVC with Immediate mode
#     """

#     template_id = "4998188"
#     backup_cm_path = "fault-notification-config-backup.yaml"

#     @pytest.fixture(autouse=True)
#     def setup_fault_notification(self, setup_runai_test):
#         self.logger.info("[Setup] Fault Notification Pod")
#         try:
#             yield
#         finally:
#             self.logger.info("[Teardown] Fault Notification Pod")
#             self.logger.info("Cleaning up resources if any created during the test.")
#             success, error = self.client.apply_configmap(self.backup_cm_path)
#             if error:
#                 self.logger.error(
#                     f"Failed to restore fault-notification configmap: {error}"
#                 )

#     @case_decorator(template_id)
#     def test_eviction_of_training_jobs_using_pvc_with_immediate_mode(
#         self, request, nvsentinel_autosync_disabled_enabled
#     ):
#         """
#         Test case of NVsentinel Fault Notification: Eviction of training jobs using PVC with Immediate mode
#         """
#         self.step_manager.print_header("Check the fault-notification pod is running")
#         fault_notification_pod = self.get_fault_notification_pod()
#         self.logger.info(
#             f"Fault Notification Pod Name: {fault_notification_pod.metadata.name}"
#         )
#         # Verify pod is running
#         self.pod_info.verify_pods_are_running_by_kubectl(
#             pod_names=[fault_notification_pod.metadata.name], namespace=self.nv_namespace
#         )

#         self.step_manager.print_header("Backup default fault-notification configmap")
#         self.client.backup_configmap(
#             self.nv_namespace, "fault-notification-config", self.backup_cm_path
#         )

#         self.step_manager.print_header("Edit default fault-notification configmap")
#         # Read the original configmap yaml
#         cm_yaml = os.path.join(
#             os.getcwd(), "testcases", "data", "cli", "nvsentinel", "immediate-fault-cm.yaml"
#         )
#         with open(cm_yaml, "r") as f:
#             cm_content = yaml.safe_load(f)
#         # Replace the namespace name
#         cm_content["data"]["config.toml"] = cm_content["data"]["config.toml"].replace(
#             "runai-qa-automation-test", self.default_namespace
#         )
#         # Save to a temporary file
#         temp_cm_path = "/tmp/immediate_fault_cm_temp.yaml"
#         with open(temp_cm_path, "w") as f:
#             yaml.dump(cm_content, f, default_flow_style=False)
#         self.logger.info(f"Modified configmap saved to {temp_cm_path}")
#         self.client.apply_configmap(temp_cm_path)

#         self.step_manager.print_header("Restart the fault-notification pod")
#         self.delete_fault_notification_pod()
#         time.sleep(15)

#         self.step_manager.print_header("Create PVC")
#         pvc_name = "test-pvc"
#         mpi_job_name = "mpi-training-job"
#         mount_path = "/home/local/data"
#         self.client.create_pvc(
#             name=pvc_name,
#             namespace=self.default_namespace,
#             accessmode=["ReadWriteMany"],
#             size="1200Gi",
#             storageclass="dgxc-enterprise-file",
#         )
#         request.addfinalizer(
#             lambda: self.client.delete_pvc(name=pvc_name, namespace=self.default_namespace)
#         )

#         self.step_manager.print_header(
#             "Submit a MPI training job using the PVC and wait until it's running"
#         )
#         self.device.runai.training.mpi.submit(
#             name=mpi_job_name,
#             image=self.runai_demo_dis_image,
#             gpu_devices_request=1,
#             existing_pvc=f"claimname={pvc_name},path={mount_path}",
#             workers="1",
#             environment="RUNAI_SLEEP_SECS=infinity",
#         )
#         self.job_utility.info.verify_job_status(mpi_job_name, EnumJobStatus["RUNNING"])

#         self.step_manager.print_header(
#             "EXEC to the job pod and create a test file and write some text in it"
#         )
#         pods, error = self.client.list_pods(
#             namespace=self.default_namespace, name_pattern=f"{mpi_job_name}*"
#         )
#         mpi_job_pod = pods[0]
#         self.logger.info(f"MPI training job pod name: {mpi_job_pod.metadata.name}")
#         test_file = os.path.join(mount_path, "test.txt")
#         command = ["/bin/bash", "-c", f"echo 'pvc training test 123456' > {test_file}"]
#         self.client.exec_command_in_pod(mpi_job_pod, command=command)
#         checksum_cmd = ["/bin/bash", "-c", f"sha512sum {test_file}"]
#         checksum, _ = self.client.exec_command_in_pod(mpi_job_pod, command=checksum_cmd)

#         job_pod_name = f"{mpi_job_name}-worker-0"
#         job_node_name, _ = self.client.get_pod_running_node_name(
#             job_pod_name, self.default_namespace
#         )
#         self.node_name = job_node_name
#         self.logger.info(f"MPI training job pod is running on node: {job_node_name}")

#         self.step_manager.print_header(
#             "Create busybox namespace and 3 busybox pods under it"
#         )
#         # Create busybox namespace
#         success, error = self.client.create_namespace("busybox")
#         request.addfinalizer(lambda: self.client.delete_namespace("busybox"))

#         # Get GPU nodes
#         self.step_manager.print_header("Get GPU nodes from cluster")
#         gpu_node_names, _ = self.client.get_node_names_by_label("nodeGroup=customer-gpu")
#         if len(gpu_node_names) < 2:
#             self.logger.error(
#                 f"Not enough GPU nodes found. Found {len(gpu_node_names)}, need at least 2"
#             )
#             assert False, "Not enough GPU nodes found"
#         self.logger.info(f"Found GPU nodes: {gpu_node_names}")

#         target_busybox_pod_name = "busybox-1"
#         # Find a GPU node that is different from job_node_name
#         other_gpu_node = next(
#             (node for node in gpu_node_names if node != self.node_name), None
#         )
#         if not other_gpu_node:
#             self.logger.error("Could not find a GPU node different from the job node")
#             assert False, "Could not find a GPU node different from the job node"

#         # Update busybox yaml with GPU node names
#         yamlfile_path = "data/cli/nvsentinel"
#         busybox_yaml = os.path.join(yamlfile_path, "busybox-pod-creation.yaml")
#         with open(busybox_yaml, "r", encoding="utf-8") as f:
#             yaml_content = self.load_yaml(
#                 f,
#                 {
#                     "NODE_NAME_1": job_node_name,
#                     "NODE_NAME_2": other_gpu_node,
#                     "NODE_NAME_3": other_gpu_node,
#                 },
#             )

#         self.logger.info(
#             f"Updated busybox yaml with nodes: job_node={job_node_name}, other_node={other_gpu_node}"
#         )
#         self.logger.info(f"Target busybox pod name: {target_busybox_pod_name}")
#         # Create a temporary file for the updated yaml
#         temp_yaml = "/tmp/busybox-pod-creation.yaml"
#         with open(temp_yaml, "w", encoding="utf-8") as tmp_file:
#             yaml.dump(yaml_content, tmp_file)
#         # Apply busybox pods using the temporary file
#         success, error = self.device.execute(f"kubectl apply -f {temp_yaml}")

#         self.step_manager.print_header("Verify busybox pods are running")
#         # Get all busybox pod names
#         pods, error = self.client.list_pods(namespace="busybox", name_pattern="busybox-*")
#         # Get pod names
#         busybox_pod_names = [pod.metadata.name for pod in pods]
#         self.logger.info(f"Found busybox pods: {busybox_pod_names}")
#         # Verify all busybox pods are running
#         self.pod_info.verify_pods_are_running_by_kubectl(
#             pod_names=busybox_pod_names, namespace="busybox", timeout=300
#         )

#         self.step_manager.print_header(
#             "Pick one node and inject a gpu fatal error on the node, eg node 10.0.43.235 , where are busybox-2 and mpi job are running"
#         )
#         pods, _ = self.client.list_pods(
#             self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor.*"
#         )
#         gpu_monitor_pod_name = None
#         for pod in pods:
#             if pod.spec.node_name == job_node_name:
#                 gpu_monitor_pod_name = pod.metadata.name
#                 self.logger.info(f"POD   Name: {gpu_monitor_pod_name}")
#                 self.logger.info(f"Node  Name: {job_node_name}")
#                 break
#         assert (
#             gpu_monitor_pod_name
#         ), f"Cannot find the nvsentinel-gpu-health-monitor pod of the node {job_node_name}"

#         self.step_manager.print_header("Inject a gpu fatal error on the node")
#         pods, _ = self.client.list_pods(
#             namespace=self.nv_namespace, name_pattern=gpu_monitor_pod_name
#         )
#         assert len(pods) > 0, "GPU health monitor pod not found"
#         gpu_health_monitor_pod = pods[0]
#         command = [
#             "/bin/sh",
#             "-c",
#             "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 84 -v 0",
#         ]
#         output, error = self.client.exec_command_in_pod(
#             gpu_health_monitor_pod, command=command
#         )
#         self.step_manager.print_header("Check the node is cordoned")
#         node_cordoned, _ = self.client.check_node_cordoned(job_node_name)
#         self.logger.info(f"Node {job_node_name} is cordoned.")
#         assert node_cordoned, f"Node {job_node_name} is not cordoned"
#         self.step_manager.print_header(
#             "Checking the fault-notification pod log. The log should contain the message of the node is cordoned"
#         )
#         self.verify_fault_notification_pod_log(
#             expected_messages=[
#                 "Evicting pods from namespace busybox in Immediate mode",
#                 f"Evicting pods from namespace {self.default_namespace} in Immediate mode",
#             ]
#         )

#         self.step_manager.print_header(
#             "Wait for 60s (evictionTimeoutInSeconds default value). Check the log again and the pods are force deletetd if they are evicted sucessfully"
#         )
#         time.sleep(60)
#         self.verify_fault_notification_pod_log(
#             expected_messages=[
#                 f"Force deleted pod {target_busybox_pod_name} in namespace busybox",
#                 f"Deleted all pods in namespace [busybox {self.default_namespace}] from node",
#             ]
#         )

#         self.step_manager.print_header("Check busybox running on job node is removed")
#         pods, error = self.client.list_pods(
#             namespace="busybox", name_pattern=target_busybox_pod_name
#         )
#         assert len(pods) == 0, f"{target_busybox_pod_name} is not removed"

#         self.step_manager.print_header(
#             "Check if the job pod is rescheuled to another node."
#         )
#         rescheduled_node_name = self.job_utility.info.get_job_located_node_name(
#             mpi_job_name
#         )
#         self.logger.info(
#             "Job %s is running on node %s" % (mpi_job_name, rescheduled_node_name)
#         )
#         assert (
#             rescheduled_node_name != job_node_name
#         ), "The job pod is not rescheduled to another node"

#         self.step_manager.print_header(
#             "Exec to the job and check the PVC is still mounted and the test file content keeps the same"
#         )
#         pods, error = self.client.list_pods(
#             namespace=self.default_namespace, name_pattern=f"{mpi_job_name}*"
#         )
#         mpi_job_pod1 = pods[0]
#         command = ["/bin/sh", "-c", f"ls {test_file} && cat {test_file}"]
#         output, error = self.client.exec_command_in_pod(pod=mpi_job_pod1, command=command)
#         assert "test.txt" in output, "Test file not found in the mounted PVC."
#         assert (
#             "pvc training test 123456" in output
#         ), "Test file content does not match expected value."
#         checksum1, _ = self.client.exec_command_in_pod(mpi_job_pod1, command=checksum_cmd)
#         assert checksum1 == checksum, "Test file content is changed"

#         self.step_manager.print_header(
#             "Clear the inject error in step 5 and check the node is uncordoned"
#         )
#         command = [
#             "/bin/sh",
#             "-c",
#             "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 84 -v 1",
#         ]
#         output, error = self.client.exec_command_in_pod(
#             gpu_health_monitor_pod, command=command
#         )

#         success, error = self.client.check_node_ready(job_node_name)
#         assert (
#             success
#         ), f"Node '{job_node_name}' is not ready after clearing injected error."

#         self.step_manager.print_header("Remove PVC")
#         success, error = self.client.delete_pvc(
#             name=pvc_name, namespace=self.default_namespace
#         )
#         self.logger.info(f"PVC '{pvc_name}' successfully deleted.")
#         success, error = self.client.verify_pvc_deleted(
#             name=pvc_name, namespace=self.default_namespace, timeout=120
#         )

#         self.step_manager.print_header("Remove all the running pods and jobs")
#         pods, error = self.client.list_pods(
#             namespace="runai-busybox", name_pattern="busybox-*"
#         )
#         for pod in pods:
#             success, error = self.client.delete_pod_by_name(
#                 pod.metadata.name, namespace="busybox"
#             )
#         self.device.runai.training.mpi.delete(name=mpi_job_name)

#         self.step_manager.print_header("Restore the change of fault-notification configmap")
#         success, error = self.client.apply_configmap(self.backup_cm_path)
#         self.logger.info("Fault-notification configmap successfully restored.")
#         fault_notification_pod = self.get_fault_notification_pod()
#         self.client.delete_pod_by_name(
#             fault_notification_pod.metadata.name, self.nv_namespace
#         )
