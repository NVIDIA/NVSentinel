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
import random



class TestCircuitBreakerBasicTest(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine: Circuit Breaker Basic Test
    """


    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.faultquarantine
    def test_circuit_breaker_basic_test(self):
        """
        Tests:
         1. When the circuit breaker is in the CLOSED state, the node should be cordoned
        """
        self.skip_if_fault_quarantine_deployment_not_found()
        self.skip_if_circuit_breaker_disabled()

        self.step_manager.print_header("Read the circuit breaker state")
        circuit_breaker_state = self.read_circuit_breaker_state()
        self.logger.info(f"Circuit breaker state: {circuit_breaker_state}")

        self.step_manager.print_header("Change the circuit breaker state to TRIPPED")
        self.change_circuit_breaker_state("TRIPPED")
        self.logger.info("Circuit breaker state changed to TRIPPED")

        self.delete_fault_quarantine_pod()
        time.sleep(10)

        gpu_health_monitor_pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        self.gpu_healthy_pod = random.choice(gpu_health_monitor_pods)
        self.node_name = self.gpu_healthy_pod.spec.node_name
        self.remove_managed_by_nvsentinel_label(self.node_name)
        self.logger.info(f"Node Name: {self.node_name}")
        self.logger.info(f"GPU Healthy Pod Name: {self.gpu_healthy_pod.metadata.name}")
        command = [
            "/bin/sh",
            "-c",
            f"dcgmi test --host nvidia-dcgm.{self.gpu_operator_namespace}.svc:5555 --inject --gpuid 0 -f 84 -v 0",
        ]
        output, _ = self.client.exec_command_in_pod(self.gpu_healthy_pod, command)
        assert "Successfully injected" in output

        time.sleep(30)

        self.step_manager.print_header("Check the node is cordoned")
        success, _ = self.client.check_node_cordoned(self.node_name, timeout=30)
        assert not success, f"FAIL: Node {self.node_name} is not cordoned"

        self.step_manager.print_header("Clear the inject error in step 5 and check the node is uncordoned")
        command = [
            "/bin/sh",
            "-c",
            f"dcgmi test --host nvidia-dcgm.{self.gpu_operator_namespace}.svc:5555 --inject --gpuid 0 -f 84 -v 0",
        ]
        output, _ = self.client.exec_command_in_pod(self.gpu_healthy_pod, command)
        assert "Successfully injected" in output

        self.step_manager.print_header("Change the circuit breaker state to CLOSED")
        self.change_circuit_breaker_state("CLOSED")
        self.logger.info("Circuit breaker state changed to CLOSED")


