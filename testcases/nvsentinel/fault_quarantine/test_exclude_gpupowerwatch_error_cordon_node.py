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

from testcases.nvsentinel.base import TestNVSentinelCaseBase
import pytest


class TestExcludeGPUPowerWatchError(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine Exclude GPUPowerWatch Error to Cordon the Node
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.smoke
    @pytest.mark.faultquarantine
    def test_exclude_gpupowerwatch_error_cordon_node(self, request):
        """
        Tests if the GPUPowerWatch error is excluded from the node quarantine rule and the node is not cordoned
        """
        self.step_manager.print_header("Inject a GPUPowerWatch error on a GPU node")
        pods, _ = self.client.list_pods(
            namespace=self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor*"
        )
        gpu_health_monitor_pod = pods[0]
        self.node_name = gpu_health_monitor_pod.spec.node_name
        self.logger.info(f"POD Name: {gpu_health_monitor_pod.metadata.name}")
        self.logger.info(f"Node Name: {self.node_name}")
        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 1 -f 240 -v 1000",
        ]
        output, _ = self.client.exec_command_in_pod(
            pod=gpu_health_monitor_pod, command=command
        )
        assert "Successfully injected" in output

        self.step_manager.print_header(
            "Check the node condition. The GpuPowerWatch condition is set to be True."
        )
        self.conditions, get_expected_result = self.get_node_condition_by_type(
            self.node_name, "GpuPowerWatchIsNotHealthy"
        )
        assert (
            get_expected_result
        ), f"GpuPowerWatch condition should be True on node {self.node_name}"

        expected_result = {
            "Condition Type": "GpuPowerWatch",
            "Condition Reason": "GpuPowerWatchIsNotHealthy",
            "Condition Message": "ErrorCode:DCGM_FR_CLOCK_THROTTLE_POWER GPU.* This GPU can still perform workload. Recommended Action=NONE;",
        }
        self.verify_health_monitor_info(
            conditions=self.conditions, expected_result=expected_result
        )

        self.step_manager.print_header("Check the node  is not cordoned")
        success, _ = self.client.check_node_ready(self.node_name)
        assert (
            success
        ), f"Node '{self.node_name}' is not healthy after clearing injected error."

        self.step_manager.print_header("Clear the error using script")
        command = [
            "/bin/sh",
            "-c",
            "python3 clear_xid_error_health_event.py",
        ]
        output, _ = self.client.exec_command_in_pod(
            pod=gpu_health_monitor_pod, command=command
        )
