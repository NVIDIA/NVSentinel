# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

"""
Module for test case of NVsentinel GPU Health Monitor: DCGM Connection Error
"""

import time
from functools import partial
from testcases.nvsentinel.health_monitors.gpu_health_monitor.base import (
    GPUHealthMonitorBase,
)
import pytest


class TestDCGMHealthyCheckDCGMConnectionError(GPUHealthMonitorBase):
    """
    Class for test case of NVsentinel GPU Health Monitor: DCGM Connection Error
    Tests the node condition GpuDcgmConnectivityFailure when DCGM communication is broken
    """

    @pytest.mark.author(email="deesharma@nvidia.com")
    @pytest.mark.gpuhealthmonitor
    def test_dcgm_healthy_check_dcgm_connection_error(self, request):
        """
        Tests if the node condition GpuDcgmConnectivityFailure is published when DCGM communication
        is broken and becomes healthy when communication is restored
        """
        
        self.step_manager.print_header(
            "Get the gpu-health-monitor pods and ensure they are healthy and running"
        )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        assert pods, f"No gpu-health-monitor pods found in {self.nv_namespace} namespace"
        
        # Select the first pod for testing
        test_pod = pods[0]
        self.node_name = test_pod.spec.node_name
        self.logger.info(f"Selected Pod   Name: {test_pod.metadata.name}")
        self.logger.info(f"Selected Node  Name: {self.node_name}")
        
        # Verify the pod is healthy before starting
        health_status, _ = self.client.is_pod_healthy(pod=test_pod)
        assert health_status, f"POD: {test_pod.metadata.name} is not healthy before test"
        
        self.step_manager.print_header(
            "Break DCGM communication by changing the DCGM service port"
        )
        
        # Store the original port for cleanup
        svc_yaml, _ = self.client.get_service_yaml(self.gpu_operator_namespace, "nvidia-dcgm")
        original_port = svc_yaml.get("spec", {}).get("ports", [{}])[0].get("targetPort", 5555)
        
        # Change the DCGM service port to break communication
        self.client.patch_custom_resource(
            "svc",
            "nvidia-dcgm",
            self.gpu_operator_namespace,
            [{"op": "replace", "path": "/spec/ports/0/targetPort", "value": 1555}],
        )
        
        # Add finalizer to restore the port
        request.addfinalizer(partial(self.restore_dcgm_port, original_port))
        
        # Verify the port was changed successfully
        svc_yaml, _ = self.client.get_service_yaml(self.gpu_operator_namespace, "nvidia-dcgm")
        ports = svc_yaml.get("spec", {}).get("ports", [])
        for port in ports:
            if port.get("targetPort"):
                assert port.get("targetPort") == 1555, "Failed to change DCGM service port"
        
        self.logger.info("Successfully changed DCGM service port to 1555")
        
        self.step_manager.print_header(
            "Wait for GpuDcgmConnectivityFailure node condition to be published"
        )
        
        # Wait for the condition to appear (polling for up to 2 minutes)
        condition_found = False
        max_wait_time = 120  # 2 minutes
        check_interval = 10
        start_time = time.time()
        
        while (time.time() - start_time) < max_wait_time:
            node_info, _ = self.client.get_node_by_name(
                node_name=self.node_name, node_type="gpu"
            )
            
            if node_info and node_info.status.conditions:
                for condition in node_info.status.conditions:
                    if condition.type == "GpuDcgmConnectivityFailure":
                        self.logger.info(f"Found GpuDcgmConnectivityFailure condition:")
                        self.logger.info(f"  Status: {condition.status}")
                        self.logger.info(f"  Reason: {condition.reason}")
                        self.logger.info(f"  Message: {condition.message}")
                        
                        # Verify the condition indicates failure
                        if (condition.status == "True" and 
                            "GpuDcgmConnectivityFailureIsNotHealthy" in condition.reason):
                            condition_found = True
                            break
            
            if condition_found:
                break
                
            self.logger.info(
                f"Waiting for GpuDcgmConnectivityFailure condition, "
                f"checking again in {check_interval} seconds..."
            )
            time.sleep(check_interval)
        
        assert condition_found, (
            f"GpuDcgmConnectivityFailure condition not found after {max_wait_time} seconds"
        )
        
        self.logger.info("SUCCESS: GpuDcgmConnectivityFailure condition published as unhealthy")
        
        # Verify the condition details
        expected_result = {
            "Condition Type": "GpuDcgmConnectivityFailure",
            "Condition Reason": "GpuDcgmConnectivityFailureIsNotHealthy",
            "Condition Message": ".*Failed to connect to DCGM.*|.*DCGM connectivity.*",
        }
        self.verify_health_monitor_info(
            conditions=node_info.status.conditions, expected_result=expected_result
        )
        
        self.step_manager.print_header(
            "Restore DCGM communication by restoring the original service port"
        )
        
        self.restore_dcgm_port(original_port)
        
        self.step_manager.print_header(
            "Wait for GpuDcgmConnectivityFailure node condition to become healthy"
        )
        
        # Wait for the condition to become healthy (polling for up to 2 minutes)
        condition_healthy = False
        start_time = time.time()
        
        while (time.time() - start_time) < max_wait_time:
            node_info, _ = self.client.get_node_by_name(
                node_name=self.node_name, node_type="gpu"
            )
            
            if node_info and node_info.status.conditions:
                for condition in node_info.status.conditions:
                    if condition.type == "GpuDcgmConnectivityFailure":
                        self.logger.info(f"Checking GpuDcgmConnectivityFailure condition:")
                        self.logger.info(f"  Status: {condition.status}")
                        self.logger.info(f"  Reason: {condition.reason}")
                        self.logger.info(f"  Message: {condition.message}")
                        
                        # Check if the condition is now healthy
                        if (condition.status == "False" and 
                            "GpuDcgmConnectivityFailureIsHealthy" in condition.reason):
                            condition_healthy = True
                            break
            
            if condition_healthy:
                break
                
            self.logger.info(
                f"Waiting for GpuDcgmConnectivityFailure to become healthy, "
                f"checking again in {check_interval} seconds..."
            )
            time.sleep(check_interval)
        
        assert condition_healthy, (
            f"GpuDcgmConnectivityFailure condition did not become healthy after {max_wait_time} seconds"
        )
        
        self.logger.info("SUCCESS: GpuDcgmConnectivityFailure condition is now healthy")
        
        # Verify the healthy condition details
        expected_healthy_result = {
            "Condition Type": "GpuDcgmConnectivityFailure",
            "Condition Reason": "GpuDcgmConnectivityFailureIsHealthy",
            "Condition Message": "No Health Failure",
        }
        self.verify_health_monitor_info(
            conditions=node_info.status.conditions, expected_result=expected_healthy_result
        )
        
        self.step_manager.print_header(
            "Verify gpu-health-monitor pods are still running and healthy"
        )
        
        # Check that the gpu-health-monitor pods recovered and are healthy
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        
        for pod in pods:
            if pod.spec.node_name == self.node_name:
                health_status, _ = self.client.is_pod_healthy(pod=pod)
                assert health_status, (
                    f"POD: {pod.metadata.name} is not healthy after DCGM connection restoration"
                )
                self.logger.info(f"Pod {pod.metadata.name} is healthy after recovery")
        
        self.logger.info("Test completed successfully: DCGM connectivity failure and recovery verified")
    
    def restore_dcgm_port(self, original_port=5555):
        """
        Helper method to restore the DCGM service port to its original value
        """
        self.logger.info(f"Restoring DCGM service port to {original_port}")
        
        self.client.patch_custom_resource(
            "svc",
            "nvidia-dcgm",
            self.gpu_operator_namespace,
            [{"op": "replace", "path": "/spec/ports/0/targetPort", "value": original_port}],
        )
        
        # Verify the port was restored successfully
        svc_yaml, _ = self.client.get_service_yaml(self.gpu_operator_namespace, "nvidia-dcgm")
        ports = svc_yaml.get("spec", {}).get("ports", [])
        for port in ports:
            if port.get("targetPort"):
                if port.get("targetPort") == original_port:
                    self.logger.info(f"Successfully restored DCGM service port to {original_port}")
                    return True
        
        self.logger.warning(f"Failed to verify DCGM service port restoration to {original_port}")
        return False
