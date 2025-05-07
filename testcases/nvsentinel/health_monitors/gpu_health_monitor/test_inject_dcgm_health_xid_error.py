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

import time
import os
import re
from testcases.nvsentinel.health_monitors.gpu_health_monitor.base import (
    GPUHealthMonitorBase,
)


# class TestInjectDCGMHealthXIDError(GPUHealthMonitorBase):
#     """
#     Class for test case of NVsentinel GPU Health Monitor: Inject a DCGM health/XID Error from one of the gpu-health-monitor pod
#     """

#     template_id = "5013128"

#     @case_decorator(template_id)
#     def test_inject_dcgm_health_xid_error(self, request):
#         """
#         Test case of NVsentinel GPU Health Monitor: Inject a DCGM health/XID Error from one of the gpu-health-monitor pod
#         """
#         self.step_manager.print_header("Get gpu health monitor pod name")
#         pods, _ = self.client.list_pods(
#             namespace=self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
#         )
#         assert len(pods) >= 2, "Test requires 2 gpu-health-monitor pods in the cluster"
#         x_gpu_monitor_pod = pods[1]
#         y_gpu_monitor_pod = pods[0]
#         self.node_name = y_gpu_monitor_pod.spec.node_name
#         self.logger.info(f"POD Name: {x_gpu_monitor_pod.metadata.name}")
#         self.logger.info(f"Node Name: {self.node_name}")

#         self.step_manager.print_header("Get 2 GPU nodes in the cluster")
#         x_gpu_node_name = x_gpu_monitor_pod.spec.node_name
#         y_gpu_node_name = y_gpu_monitor_pod.spec.node_name
#         self.logger.info(f"X GPU Node: {x_gpu_node_name}")
#         self.logger.info(f"Y GPU Node: {y_gpu_node_name}")

#         self.step_manager.print_header("Update the script to inject XID error on node X")
#         # Read the local script
#         script_path = os.path.join(
#             os.getcwd(), "data", "cli", "nvsentinel", "send_healthy_event.py"
#         )
#         with open(script_path, "r") as f:
#             script_content = f.read()
#         # Replace timestamp placeholders with current time
#         current_time = time.time()
#         current_secs = int(current_time)
#         current_nanos = int((current_time - current_secs) * 1e9)

#         # Replace timestamp placeholders
#         script_content = re.sub(
#             r"health_event\.generatedTimestamp\.seconds\s*=\s*\${[^}]+}",
#             f"health_event.generatedTimestamp.seconds = {current_secs}",
#             script_content,
#         )
#         script_content = re.sub(
#             r"health_event\.generatedTimestamp\.nanos\s*=\s*\${[^}]+}",
#             f"health_event.generatedTimestamp.nanos = {current_nanos}",
#             script_content,
#         )

#         # Replace node name placeholders with the second GPU node name
#         script_content = re.sub(
#             r"health_event\.nodeName\s*=\s*\${[^}]+}",
#             f'health_event.nodeName = "{y_gpu_node_name}"',
#             script_content,
#         )
#         script_content = re.sub(
#             r"health_event\.nodeName\s*=\s*[^$\n]+",
#             f'health_event.nodeName = "{y_gpu_node_name}"',
#             script_content,
#         )
#         # Replace entitiesImpacted with proper Entity objects
#         script_content = re.sub(
#             r'health_event\.entitiesImpacted\.extend\(\["1"\]\)',
#             'health_event.entitiesImpacted.extend([platformconnector_pb2.Entity(entityType="GPU", entityValue=str("1"))])',
#             script_content,
#         )
#         # Ensure checkName is set to GpuXidError
#         script_content = re.sub(
#             r'health_event\.checkName\s*=\s*[^"\n]*"[^"\n]+"',
#             'health_event.checkName = "GpuXidError"',
#             script_content,
#         )
#         # Write the modified script to local file system
#         local_tmp_path = "/tmp/send_health_event.py"
#         with open(local_tmp_path, "w") as f:
#             f.write(script_content)
#         self.logger.info(f"Modified script written to local file: {local_tmp_path}")

#         self.step_manager.print_header("Run the script on node X")
#         copy_command = f"kubectl cp {local_tmp_path} {self.nv_namespace}/{x_gpu_monitor_pod.metadata.name}:/"
#         self.device.execute(copy_command)
#         inject_command = [
#             "/bin/sh",
#             "-c",
#             "python3 send_health_event.py",
#         ]
#         inject_output, _ = self.client.exec_command_in_pod(
#             pod=x_gpu_monitor_pod, command=inject_command
#         )

#         self.step_manager.print_header(
#             "Check the node condition of node X and no XID error condition is updated."
#         )
#         x_node_info, _ = self.client.get_node_by_name(
#             node_name=x_gpu_node_name, node_type="gpu"
#         )
#         # Check if any GpuXidError conditions exist in the node X conditions
#         xid_error_conditions = [
#             condition
#             for condition in x_node_info.status.conditions
#             if (
#                 condition.type == "GpuXidError"
#                 and condition.reason == "GpuXidErrorIsNotHealthy"
#             )
#         ]

#         # Assert that no XID error conditions are present in node X
#         assert (
#             not xid_error_conditions
#         ), "GpuXidError condition should not be present on node X"

#         self.step_manager.print_header(
#             "Check the node condition of node Y and the GpuInforomWatch in NodeCondition should be True"
#         )
#         y_node_conditions, get_expected_result = self.get_node_condition_by_type(
#             y_gpu_node_name, "GpuXidErrorIsNotHealthy"
#         )
#         assert (
#             get_expected_result
#         ), f"GpuXidError condition should be True on node {y_gpu_node_name}"

#         expected_result = {
#             "Condition Type": "GpuXidError",
#             "Condition Reason": "GpuXidErrorIsNotHealthy",
#             "Condition Message": "ErrorCode:46 GPU:1 XID error "
#             "occured Recommended Action=REPORT_ISSUE",
#         }
#         self.verify_health_monitor_info(
#             conditions=y_node_conditions, expected_result=expected_result
#         )
#         request.addfinalizer(
#             lambda: self.client.remove_node_condition(self.node_name, "GpuXidError")
#         )

#         self.step_manager.print_header("Clear the injected error")
#         clear_error_script = """
# import socket
# import grpc
# from google.protobuf.timestamp_pb2 import Timestamp
# from protos import platformconnector_pb2, platformconnector_pb2_grpc

# def send_health_event(health_event: platformconnector_pb2.HealthEvent):
#     unix_socket_path = "/var/run/nvsentinel.sock"
#     with grpc.insecure_channel("unix://" + unix_socket_path) as chan:
#         stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
#         events = [health_event]
#         stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=events, version=1))

# timestamp = Timestamp()
# timestamp.GetCurrentTime()

# # Create health event object
# health_event = platformconnector_pb2.HealthEvent(
#     version=1,
#     agent="gpu-health-monitor",
#     componentClass="GPU",
#     checkName="GpuXidError",
#     isFatal=False,
#     isHealthy=True,
#     message="XID error occurred",
#     recommendedAction=platformconnector_pb2.REPORT_ISSUE,
#     entitiesImpacted=[
#         platformconnector_pb2.Entity(
#             entityType="GPU",
#             entityValue="1"
#         )
#     ],
#     generatedTimestamp=timestamp,
#     nodeName="{node_name}"
# )

# # Add metadata separately to avoid parsing issues
# health_event.metadata["SerialNumber"] = "123456789"

# send_health_event(health_event)
# """.format(node_name=y_gpu_node_name)

#         # Write the clear script to a local file first
#         local_clear_script_path = "/tmp/clear_error.py"
#         with open(local_clear_script_path, "w") as f:
#             f.write(clear_error_script)
#         self.logger.info(f"Clear script written to local file: {local_clear_script_path}")

#         self.logger.info("Copy the clear script to the pod")
#         copy_command = f"kubectl cp {local_clear_script_path} {self.nv_namespace}/{x_gpu_monitor_pod.metadata.name}:/"
#         output, _ = self.device.execute(copy_command)
#         clear_command = [
#             "/bin/sh",
#             "-c",
#             "python3 clear_error.py",
#         ]
#         clear_output, _ = self.client.exec_command_in_pod(
#             pod=y_gpu_monitor_pod, command=clear_command
#         )
#         assert (
#             "XID error occurred" not in clear_output
#         ), f"Failed to clear error: {clear_output}"
