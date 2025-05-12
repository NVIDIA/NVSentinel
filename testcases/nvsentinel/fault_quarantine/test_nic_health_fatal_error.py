# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

"""
Module for class of NVsentinel Fault Quarantine:NIC health fatal error/recover
"""

from functools import partial
import time
from testcases.nvsentinel.base import TestNVSentinelCaseBase
import pytest

class TestNICHealthFatalError(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel Fault Quarantine NIC Health Fatal Error
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.faultquarantine
    def test_nic_health_fatal_error(self, request):
        """
        Tests if appropriate node annotations and node condition are set when NIC interface is set down
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
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-nic-health.*"
        )
        pod_name = None
        nodes_list = list(nodes_interfaces_dict.keys())
        self.node_name = nodes_list[0]
        self.remove_managed_by_nvsentinel_label(self.node_name)
        for pod in pods:
            if pod.spec.node_name == self.node_name:
                pod_name = pod.metadata.name
                self.logger.info(f"POD   Name: {pod_name}")
                self.logger.info(f"Node  Name: {self.node_name}")
                break
        self.logger.info("Restart the fault-quarantine pod")
        self.delete_fault_quarantine_pod()

        self.step_manager.print_header("Simulate fatal error of NIC: down a port")
        non_mgmt_interface = self.get_non_mgmt_ports_of_the_node(self.node_name)
        self.logger.info(f"TARGET PORT NAME: {non_mgmt_interface}")
        request.addfinalizer(
            partial(self.up_interface_of_node, self.node_name, non_mgmt_interface)
        )
        self.down_interface_of_node(self.node_name, non_mgmt_interface)

        self.step_manager.print_header("Check the node taints and annotations")
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert node_info.spec.unschedulable is True
        self.logger.info("Check the taints on the node")
        target_conditions = [
            {
                "key": "node.kubernetes.io/unschedulable",
                "value": None,
                "effect": "NoSchedule",
            },
        ]
        assert self.client.check_taints_on_node(
            self.node_name, conditions=target_conditions
        )

        self.logger.info("Check the annotations on the node")
        annotations, _ = self.client.get_annotation_on_node(
            self.node_name, "quarantineHealthEvent"
        )
        check_name = "EthernetErrorCheck"
        if self.check_if_ib_interface(self.node_name):
            check_name = "InfiniBandErrorCheck"
        assert (
            f'"agent":"nic-health-monitor","componentClass":"NIC","checkName":"{check_name}","isFatal":true'
            in annotations
        )
        assert (
            self.client.get_annotation_on_node(
                self.node_name, "quarantineHealthEventIsCordoned"
            )[0]
            == "True"
        )

        self.step_manager.print_header("Recover fatal error of NIC: up the port")
        self.up_interface_of_node(self.node_name, non_mgmt_interface)
        time.sleep(20)

        self.step_manager.print_header(
            "Check the node status, taints and annotations are removed"
        )
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert "node.kubernetes.io/unschedulable" not in str(node_info.spec.taints)
        assert (
            self.client.get_annotation_on_node(self.node_name, "quarantineHealthEvent")[0]
            is None
        )
        assert (
            self.client.get_annotation_on_node(
                self.node_name, "quarantineHealthEventIsCordoned"
            )[0]
            is None
        )
        assert node_info.spec.unschedulable is None
        self.restore_managed_by_nvsentinel_label(self.node_name)
