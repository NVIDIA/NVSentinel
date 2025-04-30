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
            "Follow “Prometheus metrics for nvsentinel pods” , make sure that promtool is installed and prometheus port 9090 is accessible."
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
        response = self.query_metrics(
            query_params=f'nic_monitor_health_events_published_total{{pod="{pod_name}"}}'
        )
        value = response.json()["data"]["result"][0]["value"]
        self.logger.info(f"[DEBUG] value = {value}")
        value_before = int(value[1])

        self.step_manager.print_header(
            "Execute test case “Ethernet link down”, check value of the metric is increased by 1 or 2 when the NIC is set down and when the NIC is set UP."
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
            target_condition, _ = self.client.read_node_condition_by_type(
                node_name=node_name, condition_type="EthernetErrorCheck"
            )
            assert (
                target_condition.status == "True"
            ), f"Status of EthernetErrorCheck is still False: {target_condition}"
            self.logger.info(
                "SUCCESS: EthernetErrorCheck status is flip to True when port down in node"
            )
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
        self.logger.info(f"value_before = {value_before}")
        self.logger.info(f"value_after_down = {value_after_down}")
        self.logger.info(f"value_after_up = {value_after_up}")
        ib_interface_flag = self.check_if_ib_interface(node_name)
        if ib_interface_flag:
            # if the node has IB interface, then bring down 1 interface, 2 interface will be down
            count = 2
        else:
            count = 1

        assert (
            value_after_down - value_before == count
        ), "[FAIL] value of the metric is NOT increased by {count} when the NIC is set down"
        assert (
            value_after_up - value_after_down == 1
        ), "[FAIL] value of the metric is NOT increased by {count} when the NIC is set up"
        self.logger.info(
            "[PASS] value of the metric is increased by {count} when the NIC is set down and when the NIC is set up"
        )
