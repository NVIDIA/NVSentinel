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
from math import ceil

class TestCircuitBreakerOvercordon(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine: Circuit Breaker Overcordon
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.faultquarantine
    def test_circuit_breaker_overcordon(self):
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

        nodes, _ = self.client.get_nodes(ready=False)
        cordoned_nodes_count_before = len([node for node in nodes if node.spec.unschedulable is True])
        cordoned_percentage = (cordoned_nodes_count_before / len(nodes)) * 100
        if cordoned_percentage > 50:
            pytest.fail(f"FAIL: More than 50% of the nodes are cordoned: {cordoned_percentage}%")
        else:
            self.logger.info(f"INFO: {cordoned_percentage}% of the nodes are cordoned")

        gpu_health_monitor_pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        self.step_manager.print_header("Inject a fatal error on all of the GPU nodes")
        self.inject_gpu_inforom_on_all_nodes(gpu_health_monitor_pods)
        time.sleep(30)

        self.step_manager.print_header("Check the node is cordoned")
        nodes, _ = self.client.get_nodes(ready=False)
        for node in nodes:
            self.logger.info(f"Node: {node.metadata.name} is cordoned: {node.spec.unschedulable}")

        cordoned_nodes_count_after = len([node for node in nodes if node.spec.unschedulable is True])
        cordoned_nodes_diff = cordoned_nodes_count_after - cordoned_nodes_count_before
        assert cordoned_nodes_diff <= ceil(0.5 * len(nodes)), "FAIL: Cordon nodes percentage difference should be less than 0.5"

        circuit_breaker_state = self.read_circuit_breaker_state()
        self.logger.info(f"Circuit breaker state: {circuit_breaker_state}")
        assert circuit_breaker_state == "TRIPPED", "FAIL: Circuit breaker state should be TRIPPED"

        self.clear_gpu_inforom_on_all_nodes(gpu_health_monitor_pods)
        time.sleep(30)

        self.change_circuit_breaker_state("CLOSED")
        self.delete_fault_quarantine_pod()
        time.sleep(40)

        self.step_manager.print_header("Check the node is uncordoned")
        nodes, _ = self.client.get_nodes(ready=False)
        cordoned_nodes_count_after  = len([node for node in nodes if node.spec.unschedulable is True])
        assert cordoned_nodes_count_before == cordoned_nodes_count_after, "FAIL: Cordon nodes count should be the same"


        self.step_manager.print_header("Check the circuit breaker state")
        circuit_breaker_state = self.read_circuit_breaker_state()
        self.logger.info(f"Circuit breaker state: {circuit_breaker_state}")
        assert circuit_breaker_state == "CLOSED", "FAIL: Circuit breaker state should be CLOSED"
