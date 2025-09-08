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

class TestCircuitBreakerForceOvercordon(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine: Circuit Breaker Force Overcordon
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.faultquarantine
    def test_circuit_breaker_force_overcordon(self):
        """
        Tests:
        """
        self.skip_if_fault_quarantine_deployment_not_found()
        self.skip_if_circuit_breaker_disabled()
        self.step_manager.print_header("Read the circuit breaker state")
        circuit_breaker_state = self.read_circuit_breaker_state()
        self.logger.info(f"Circuit breaker state: {circuit_breaker_state}")

        if circuit_breaker_state == "TRIPPED":
            self.step_manager.print_header("Change the circuit breaker state to CLOSED")
            self.change_circuit_breaker_state("CLOSED")
            self.logger.info("Circuit breaker state changed to CLOSED")
            self.delete_fault_quarantine_pod()
            time.sleep(10)

        self.step_manager.print_header("Check the node is not cordoned")
        nodes, _ = self.client.get_nodes(ready=False)
        cordoned_nodes_count_before = len([node for node in nodes if node.spec.unschedulable is True])
        cordoned_percentage = (cordoned_nodes_count_before / len(nodes)) * 100

        if cordoned_percentage == 100:
            pytest.fail(f"FAIL: All nodes are already cordoned: {cordoned_percentage}%")
        else:
            self.logger.info(f"INFO: {cordoned_percentage}% of the nodes are cordoned")

        gpu_health_monitor_pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )

        self.step_manager.print_header("Copy the send-fatal-health-event-through-uds.py to all the GPU health monitor pods")
        self.copy_file_to_all_pods(os.path.join(os.getcwd(), 
                                    "nvsentinel", "testcases", "data", "cli", "nvsentinel", "send-health-event-through-uds.py"), 
                                    "/send-health-event-through-uds.py",
                                    gpu_health_monitor_pods)
        
        self.step_manager.print_header("Inject a fatal health event on all the GPU nodes")
        self.inject_health_event_on_all_nodes_through_uds(gpu_health_monitor_pods, healthy=False)

        self.step_manager.print_header("Sleep for 30 seconds")
        time.sleep(30)

        self.step_manager.print_header("Check the circuit breaker state after 30 seconds")
        circuit_breaker_state = self.read_circuit_breaker_state()
        self.logger.info(f"Circuit breaker state: {circuit_breaker_state}")
        assert circuit_breaker_state == "CLOSED", "FAIL: Circuit breaker state should be CLOSED"

        self.step_manager.print_header("Check the node is cordoned after 30 seconds")
        nodes, _ = self.client.get_nodes(ready=False)
        for node in nodes:
            self.logger.info(f"Node: {node.metadata.name} is cordoned: {node.spec.unschedulable}")

        cordoned_nodes_count_after = len([node for node in nodes if node.spec.unschedulable is True])
        assert len(nodes) == cordoned_nodes_count_after, "FAIL: All nodes should be cordoned"

        self.step_manager.print_header("Inject a healthy health event on all the GPU nodes")
        self.inject_health_event_on_all_nodes_through_uds(gpu_health_monitor_pods, healthy=True)
        time.sleep(30)

    def inject_health_event_on_all_nodes_through_uds(self, gpu_health_monitor_pods, healthy):
        for pod in gpu_health_monitor_pods:
            self.logger.info(f"Injecting health event on node: {pod.metadata.name}")
            self.client.exec_command_in_pod(pod, ["/bin/sh", "-c", f"python3 send-health-event-through-uds.py --healthy {healthy}"])

    def copy_file_to_all_pods(self, source_path, destination_path, pods):
        for pod in pods:
            self.logger.info(f"Copying {source_path} to {destination_path} on node: {pod.metadata.name}")
            self.copy_file_to_pod(source_path, f"{self.nv_namespace}/{pod.metadata.name}:{destination_path}")