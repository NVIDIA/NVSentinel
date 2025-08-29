# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import os
import yaml
import random
import pytest
import time
import re
import tempfile
from functools import partial
from testcases.nvsentinel.health_monitors.syslog_health_monitor.syslog_mock_test_utils import SyslogMockTestBase


class TestKataModeXIDMonitoring(SyslogMockTestBase):
    """
    Test class for XID monitoring in kata mode
    """
    
    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_kata_mode_xid_monitoring(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Test XID monitoring in kata mode
        """
        
        self.skip_if_kata_mode_disabled()

        nodes, err_msg = self.client.get_nodes()
        if err_msg:
            pytest.fail(f"Failed to get nodes: {err_msg}")
        
        self.test_node = random.choice(nodes)
        self.logger.info(f"Selected node: {self.test_node.metadata.name}")
        self.set_managed_by_nvsentinel_label_to_false(self.test_node.metadata.name)
        syslog_hm_pods = self.client.list_pods(self.nv_namespace, name_pattern="nvsentinel-syslog-health*")
        if err_msg or not syslog_hm_pods:
            pytest.skip(f"No nvsentinel-syslog-health-monitor pod found: {err_msg}")
        
        condition_type = "SysLogsXIDError"
        self.step_manager.print_header("Step 1: Make XID 13 Fatal")
        self.make_xid_13_fatal()

        self.step_manager.print_header("Step 2: Update DaemonSet polling interval to 1 minute")
        self._update_daemonset_polling_interval("30s")

        gpu_sleep_pod = self.create_gpu_sleep_pod(self.test_node.metadata.name)
        request.addfinalizer(partial(self.client.delete_pod, gpu_sleep_pod, 60))
    
        self.inject_xid_error_in_pod(gpu_sleep_pod)

        self.step_manager.print_header("Step 3: Wait for 80 seconds to let the syslog health monitor detect the XID error")
        self.node_conditions_to_cleanup.append(condition_type)
        time.sleep(80)
        node_info, err_msg = self.client.get_node_by_name(node_name=self.test_node.metadata.name, node_type="gpu")
        if err_msg:
            pytest.fail(f"Failed to get node info: {err_msg}")
        
        self.step_manager.print_header("Step 4: Verify node condition after XID error")
        expected_node_condition = {
            "Condition Type": condition_type,
            "Condition Reason": condition_type + "IsNotHealthy",
            "Condition Message": ".*NVRM: Xid \\(PCI:.*\\): 13.*"
        }
        self.verify_health_monitor_info(node_info.status.conditions, expected_node_condition)

        self.step_manager.print_header("Step 5: Verify node events")
        events, _ = self.client.get_node_events(node_name=self.test_node.metadata.name)

        expected_node_event = {
            "Event Type": condition_type,
            "Event Reason": condition_type + "IsNotHealthy",
            "Event Message": ".*NVRM: Xid \\(PCI:.*\\): 31.*"
        }
        self.verify_health_monitor_info(conditions=events, expected_result=expected_node_event)

        self.step_manager.print_header("Step 6: Reboot the node")
        self.reboot_forge_node(self.test_node.metadata.name, wait_second=1800)

        time.sleep(50) # wait for syslog health monitor to restart

        node_info, err_msg = self.client.get_node_by_name(node_name=self.test_node.metadata.name, node_type="gpu")
        if err_msg:
            pytest.fail(f"Failed to get node info: {err_msg}")

        self.step_manager.print_header("Step 7: Verify node condition after reboot")

        assert not self.verify_health_monitor_info(node_info.status.conditions, expected_node_condition, assert_on_fail=False), "Node condition should be cleared after reboot"

        self.step_manager.print_header("Step 8: Restore managed by nvsentinel label")
        self.restore_managed_by_nvsentinel_label(self.test_node.metadata.name)

    def create_gpu_sleep_pod(self, node_name):
        """Create a GPU sleep pod on the given node"""
        yaml_file = os.path.join(os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "kata-sleep-pod.yaml")
        self.logger.info(f"yaml_file = {yaml_file}")
        with open(yaml_file, "r") as file:
            pod_body = yaml.safe_load(file)
            pod_body["spec"]["nodeName"] = node_name
        pod, err_msg = self.client.create_pod(pod_body=pod_body, wait=100)
        if err_msg:
            pytest.fail(f"Failed to create GPU sleep pod: {err_msg}")
        
        self.logger.info(f"GPU sleep pod created: {pod.metadata.name}")
        return pod
    
    def inject_xid_error_in_pod(self, pod):
        """Inject an XID error into the given pod using CUDA code"""

        cuda_code = '''#include <cuda_runtime.h>
#include <stdio.h>

__global__ void trigger_xid() {
    // Intentionally access invalid memory to trigger XID 13
    int* invalid_ptr = (int*)0xDEADBEEF;
    *invalid_ptr = 42;
}

int main() {
    trigger_xid<<<1, 1>>>();
    cudaDeviceSynchronize();
    return 0;
}'''
        
        output, err_msg = self.client.exec_command_in_pod(pod, ["/bin/sh", "-c", f"cat > xid_trigger.cu << 'EOF'\n{cuda_code}\nEOF"])
        if err_msg:
            pytest.fail(f"Failed to create CUDA code file: {err_msg}")
        
        output, err_msg = self.client.exec_command_in_pod(pod, ["/bin/sh", "-c", "nvcc -o xid_trigger xid_trigger.cu && ./xid_trigger"])
        if err_msg:
            pytest.fail(f"Failed to compile and run CUDA code: {err_msg}")
        
        self.logger.info(f"XID error injected into pod: {pod.metadata.name} using CUDA code")


    def make_xid_13_fatal(self):
        configmap, err_msg = self.client.get_configmap(self.nv_namespace, "nvsentinel-syslog-health-monitor")
        if err_msg:
            pytest.fail(f"Failed to get configmap: {err_msg}")

        configmap_data = configmap.data
        configmap_data["xiderrorsmapping.csv"] = re.sub(r"13,.*", "13,ROBUST_CHANNEL_GR_EXCEPTION / ROBUST_CHANNEL_GR_ERROR_SW_NOTIFY,RESTART_APP,FATAL", configmap_data["xiderrorsmapping.csv"])
        configmap_yaml = {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {
                "name": "nvsentinel-syslog-health-monitor",
                "namespace": self.nv_namespace
            },
            "data": configmap_data
        }
        with tempfile.NamedTemporaryFile(mode='w', suffix='.yaml', delete=False) as temp_configmap_file:
            yaml.dump(configmap_yaml, temp_configmap_file, default_flow_style=False)
            temp_configmap_file_name = temp_configmap_file.name

        success, err_msg = self.client.apply_configmap(temp_configmap_file_name)
        if not success:
            pytest.fail(f"Failed to apply updated ConfigMap: {err_msg}")
        
        self.logger.info("Successfully updated ConfigMap with XID 13 set to fatal")