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
Module for class of NVsentinel Metrics: NIC health monitor metric: nic_monitor_health_events_published_total
"""

import pytest
import time
from functools import partial
import os
import random
import threading

from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestNicMonitorHealthEventsPublishedTotal(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Metrics: NIC health monitor metric: nic_monitor_health_events_published_total
    """

    template_title = "NIC health monitor metric: nic_monitor_health_events_published_total"

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.metrics
    def test_nic_monitor_health_events_published_total(self, request):
        """
        Test case of NVsentinel Metrics: NIC health monitor metric: nic_monitor_health_events_published_total
        """
        if os.getenv("CLOUD_PROVIDER") == "aws":
            self.logger.info("Running on AWS with mock filesystem")
            autosync_fixture = request.getfixturevalue("nvsentinel_autosync_disabled_enabled")
            self.nic_monitor_health_events_published_total_with_mock(request)
        else:
            self.logger.info("Running on CSP with original filesystem")
            self.nic_monitor_health_events_published_total_with_real_interface(request)

    def nic_monitor_health_events_published_total_with_mock(self, request):
        """
        Test NIC monitor health events published total metric using mock filesystem
        """
        self.step_manager.print_header(
            "Select a node for testing with mock ethernet interface"
        )
        nodes, _ = self.client.get_nodes()
        if not nodes:
            pytest.skip("No nodes available for testing")
        
        node_name = random.choice([node.metadata.name for node in nodes])
        self.logger.info(f"Selected node for testing: {node_name}")

        self.step_manager.print_header(
            'Follow "Prometheus metrics for nvsentinel pods" , make sure that promtool is installed and prometheus port 9090 is accessible.'
        )
        self.start_prometheus_service()

        self.step_manager.print_header(
            "Update NIC monitor configuration to use custom path"
        )
        
        try:
            self.update_nic_monitor_configmap("SysClassNetPath", "/var/run/mock-net")
            request.addfinalizer(self.restore_nic_monitor_configmap)
        except Exception as e:
            self.logger.error(f"Failed to update configmap: {e}")
            pytest.skip(f"Cannot update NIC monitor configuration: {e}")

        
        interface_name = f"eth1_test_{random.randint(1000, 9999)}"
        
        try:
            self.create_mock_ethernet_interface(node_name, interface_name)
        except Exception as e:
            self.logger.error(f"Failed to create mock interface: {e}")
            pytest.skip(f"Cannot create mock interface on node {node_name}. Error: {e}")
        
        # Register cleanup
        request.addfinalizer(
            partial(self.cleanup_mock_ethernet_interface, node_name)
        )

        
        try:
            pod_name = self.restart_nic_monitor_pod(node_name)
        except Exception as e:
            self.logger.error(f"Failed to restart NIC monitor: {e}")
            pytest.skip(f"Cannot restart NIC monitor on node {node_name}. Error: {e}")

        self.step_manager.print_header(
            "Open one console to check the logs from the pod, do not close this console"
        )
        self.pod_logs = []
        monitor_thread = threading.Thread(
            target=self.follow_pod_logs, args=(self.nv_namespace, pod_name), daemon=True
        )
        monitor_thread.start()

        time.sleep(10)

        value_before = self.get_metric_value(pod_name)

        self.step_manager.print_header(
            'Execute test case "Ethernet link down", check value of the metric is increased by 1 when the NIC is set down and when the NIC is set UP.'
        )
        self.logger.info(f"TARGET INTERFACE NAME: {interface_name}")

        self.step_manager.print_header(f"Down the interface {interface_name}")
        self.set_mock_ethernet_state(node_name, interface_name, "down")
        time.sleep(20)

        self.step_manager.print_header(
            "EthernetErrorCheck will change to True in node condition."
        )
        self.verify_ethernet_error_condition(node_name, "True")

        value_after_down = self.get_expected_value(
            query_params=f'nic_monitor_health_events_published_total{{pod="{pod_name}"}}',
            value_before=value_before,
            expected_increase=1,
        )

        self.step_manager.print_header("Up the interface")
        self.set_mock_ethernet_state(node_name, interface_name, "up")
        time.sleep(25)

        value_after_up = self.get_expected_value(
            query_params=f'nic_monitor_health_events_published_total{{pod="{pod_name}"}}',
            value_before=value_after_down,
            expected_increase=1,
        )

        self.validate_metric_changes(value_before, value_after_down, value_after_up, 1)

    def nic_monitor_health_events_published_total_with_real_interface(self, request):
        """
        Test NIC monitor health events published total metric using real physical interfaces
        """
        self.step_manager.print_header(
            "Filter out the nodes with more than 2 physical interface on the node"
        )
        nodes_interfaces_dict = self.get_nodes_with_more_than_two_physical_interfaces()
        if not nodes_interfaces_dict:
            pytest.skip(
                "Cannot find a node with more than 2 physical interfaces on the cluster. "
                "Skipping this test case."
            )

        self.step_manager.print_header(
            'Follow "Prometheus metrics for nvsentinel pods" , make sure that promtool is installed and prometheus port 9090 is accessible.'
        )
        self.start_prometheus_service()

        self.step_manager.print_header(
            "Choose one pod of nic-health-monitor, and take note of the node where it is running"
        )
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-nic-health*"
        )
        pod_name = None
        nodes_list = list(nodes_interfaces_dict.keys())
        node_name = nodes_list[0]
        for pod in pods:
            if pod.spec.node_name == node_name:
                pod_name = pod.metadata.name
                self.logger.info(f"POD   Name: {pod_name}")
                self.logger.info(f"Node  Name: {node_name}")
                break

        self.step_manager.print_header(
            "Get the current value of metric nic_monitor_health_events_published_total"
        )
        value_before = self.get_metric_value(pod_name)

        self.step_manager.print_header(
            'Execute test case "Ethernet link down", check value of the metric is increased by 1 or 2 when the NIC is set down and when the NIC is set UP.'
        )
        non_mgmt_interface = self.get_non_mgmt_ports_of_the_node(node_name)
        self.logger.info(f"TARGET PORT NAME: {non_mgmt_interface}")
        request.addfinalizer(
            partial(self.up_interface_of_node, node_name, non_mgmt_interface)
        )
        try:
            self.step_manager.print_header(f"Down the port {non_mgmt_interface}")
            self.down_interface_of_node(node_name, non_mgmt_interface)
            time.sleep(20)

            self.step_manager.print_header(
                "EthernetErrorCheck will change to True in node condition."
            )
            self.verify_ethernet_error_condition(node_name, "True")
        except:
            self.up_interface_of_node(node_name, non_mgmt_interface)
            raise

        self.step_manager.print_header(
            "Get the current value of metric nic_monitor_health_events_published_total"
        )
        value_after_down = self.get_expected_value(
            query_params=f'nic_monitor_health_events_published_total{{pod="{pod_name}"}}',
            value_before=value_before,
            expected_increase=1,
        )

        self.step_manager.print_header("Up the port")
        self.up_interface_of_node(node_name, non_mgmt_interface)
        time.sleep(25)

        self.step_manager.print_header(
            "Get the current value of metric nic_monitor_health_events_published_total"
        )
        value_after_up = self.get_expected_value(
            query_params=f'nic_monitor_health_events_published_total{{pod="{pod_name}"}}',
            value_before=value_after_down,
            expected_increase=1,
        )

        self.step_manager.print_header(
            "check value of the metric is increased by 1 when the NIC is set down and when the NIC is set up"
        )
        ib_interface_flag = self.check_if_ib_interface(node_name)
        if ib_interface_flag:
            # if the node has IB interface, then bring down 1 interface, 2 interface will be down
            count = 2
        else:
            count = 1

        self.validate_metric_changes(value_before, value_after_down, value_after_up, count)
