# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import time
from functools import partial
from testcases.nvsentinel.health_monitors.gpu_health_monitor.base import (
    GPUHealthMonitorBase,
)
import pytest


class TestGPUHealthMonitorWaitingForDCGM(GPUHealthMonitorBase):
    """
    Class for test case of NVsentinel GPU Health Monitor: Gpu-health-monitor waiting for DCGM
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.gpuhealthmonitor
    def test_gpu_healthy_monitor_waiting_for_dcgm(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Tests if the gpu-health-monitor pod is in 'Init' status when DCGM is blocked, and works correctly when DCGM is restored
        """
        self.step_manager.print_header(
            "Check daemonset exist and is running and healthy on all GPU nodes."
        )
        daemonset_to_check = ["nvidia-dcgm-exporter", "nvidia-dcgm"]
        daemonsets, _ = self.client.list_daemonset(namespace="gpu-operator")
        assert daemonsets, "ERROR: No resources found in gpu-operator namespace."
        daemonset_list = [daemonset.metadata.name for daemonset in daemonsets]
        for target_ademonset in daemonset_to_check:
            self.logger.info(f"Daemonset Name:{target_ademonset}")
            find_match = any(
                target_ademonset == daemonset_name for daemonset_name in daemonset_list
            )
            assert find_match, f"Mismatch daemonset found - Current:{daemonset_list}, Expected:{daemonset_to_check}"

        self.step_manager.print_header(
            "Get the gpu-health-monitor in the nvsentinel, make sure they are healthy and up"
        )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        for pod in pods:
            err_msg = (
                f"POD: {pod.metadata.name} is not healthy, current state:{pod.status.phase}"
            )
            health_status, _ = self.client.is_pod_healthy(pod=pod)
            assert health_status, err_msg

        self.step_manager.print_header("Scale deployment gpu-operator to be 0")

        self.client.scale_deployment("gpu-operator", 0, namespace="gpu-operator")
        request.addfinalizer(
            partial(self.client.scale_deployment, "gpu-operator", 1, "gpu-operator")
        )

        self.step_manager.print_header("Change the port of the DCGM service")
        self.client.patch_custom_resource(
            "svc",
            "nvidia-dcgm",
            "gpu-operator",
            [{"op": "replace", "path": "/spec/ports/0/targetPort", "value": 1555}],
        )

        self.logger.info("Check the port is updated succesfully")
        svc_yaml, _ = self.client.get_service_yaml("gpu-operator", "nvidia-dcgm")
        ports = svc_yaml.get("spec", {}).get("ports", [])
        for port in ports:
            if port.get("targetPort"):
                assert port.get("targetPort") == 1555
        request.addfinalizer(self.recover_dgcm_port)
        self.step_manager.print_header(
            "Choose one nvsentinel node, and record the node name"
        )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        gpu_monitor_pod = pods[-1]
        pod_name = gpu_monitor_pod.metadata.name
        node_name = gpu_monitor_pod.spec.node_name
        self.logger.info(f"Selected Pod   Name: {pod_name}")
        self.logger.info(f"Selected Node  Name: {node_name}")

        self.step_manager.print_header(
            "Login into one nvidia-dcgm pod of a gpu node and try to inject error"
        )
        pods, _ = self.client.list_pods("gpu-operator", name_pattern="nvidia-dcgm-.*")
        dcgm_pod = next(
            (
                pod
                for pod in pods
                if pod.spec.node_name == node_name
                and "nvidia-dcgm-exporter" not in pod.metadata.name
            ),
            None,
        )
        assert dcgm_pod
        self.logger.info(f"Selected Pod Name: {dcgm_pod.metadata.name}")
        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 230 -v 43",
        ]
        output, _ = self.client.exec_command_in_pod(dcgm_pod, command)
        keywords = [
            "unable to establish a connection",
            "Host engine connection invalid/disconnected",
        ]

        assert any(
            kw in output for kw in keywords
        ), f"Expected Error message not found: {output}"
        self.logger.info("SUCCESS to get expected error output after injecting an error")

        self.step_manager.print_header(
            "Restart one of the gpu-health-monitor pods and take a note of the node."
        )
        self.client.delete_pod(gpu_monitor_pod)
        time.sleep(10)
        self.step_manager.print_header(
            "Check the status of the new gpu-health-monitor pod, it should be in INIT state without restarts."
        )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        new_pod = next(pod for pod in pods if pod.spec.node_name == node_name)
        assert self.is_wait_for_dcgm(
            pod=new_pod
        ), "Pod Status is not in 'Init' status after block dcgm"
        self.logger.info(
            "SUCCESS: Pod Status is in Init State after apply block dcgm network policy"
        )

        self.step_manager.print_header(
            "Wait for 5 min, and check again the gpu-health-monitor pod, it should remain in the INIT status without any restart."
        )
        time.sleep(60 * 5)
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        new_pod = next(pod for pod in pods if pod.spec.node_name == node_name)
        assert self.is_wait_for_dcgm(
            pod=new_pod
        ), "Pod Status is not in 'Init' status after block dcgm"
        self.logger.info(
            "SUCCESS: Pod Status is still in Init State after waiting for 5 minutes"
        )

        self.step_manager.print_header(" Restore the DCGM access")
        self.recover_dgcm_port()

        self.step_manager.print_header(
            "Wait for 10s, and check the status of the gpu-health-monitor pods, they should be up and running."
        )
        for _ in range(15):
            pods, _ = self.client.list_pods(
                self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
            )
            count = 0
            for pod in pods:
                healthy_status, _ = self.client.is_pod_healthy(pod=pod)
                if healthy_status:
                    count += 1
            if count == len(pods):
                break
            else:
                time.sleep(30)

        self.step_manager.print_header("Recover gpu-operator deployment")
        self.client.scale_deployment("gpu-operator", 1, namespace="gpu-operator")

        self.step_manager.print_header(
            "Inject a GpuXid non-fatal error on the node where the gpu monitor restarted"
        )
        command = [
            "/bin/sh",
            "-c",
            "dcgmi test --host nvidia-dcgm.gpu-operator.svc:5555 --inject --gpuid 0 -f 230 -v 43",
        ]
        output, _ = self.client.exec_command_in_pod(
            new_pod,
            command=command,
        )
        assert "Successfully injected" in output, f"Failed to inject GpuXid Error: {output}"

        time.sleep(20)
        self.step_manager.print_header(
            "Make sure that the health event is successfully sent after DCGM access is restored"
        )
        events, _ = self.client.get_node_events(node_name=node_name)
        expected_result = {
            "Event Type": "GpuXidError",
            "Event Reason": "GpuXidErrorIsNotHealthy",
            "Event Message": "ErrorCode:43 GPU:0 XID error occured Recommended Action=NONE",
        }
        self.verfiy_health_monitor_info(conditions=events, expected_result=expected_result)

    def is_wait_for_dcgm(self, pod):
        """Pod Init Status is not supported in Kubernetes API, need to check container status instead"""
        in_init_state = False
        self.logger.info(f"Pod Name: {pod.metadata.name}")
        container_condition = next(
            condition
            for condition in pod.status.conditions
            if condition.type == "Initialized"
        )
        in_init_state = pod.status.phase == "Pending" and "wait-for-dcgm-pod-to-run" in str(
            container_condition.message
        )
        return in_init_state

    def recover_dgcm_port(self):
        self.client.patch_custom_resource(
            "svc",
            "nvidia-dcgm",
            "gpu-operator",
            '[{"op": "replace", "path": "/spec/ports/0/targetPort", "value":5555}]',
        )
        self.logger.info("Check the port is updated succesfully")
        svc_yaml, _ = self.client.get_service_yaml("gpu-operator", "nvidia-dcgm")
        ports = svc_yaml.get("spec", {}).get("ports", [])
        for port in ports:
            if port.get("targetPort"):
                return port.get("targetPort") == 5555
