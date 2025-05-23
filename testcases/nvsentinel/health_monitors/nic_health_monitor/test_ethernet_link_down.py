# SPDX-FileCopyrightText: Copyright (c) 2024 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import pytest
import threading
import random
from functools import partial
import os
from testcases.nvsentinel.base import TestNVSentinelCaseBase


class TestEthernetLinkDown(TestNVSentinelCaseBase):
    """
    Class for test case of NVsentinel NIC Health Monitor: Ethernet link down
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.nichealthmonitor
    def test_ethernet_link_down(self, request):
        """
        Tests if the EthernetErrorCheck condition is set correctly when the ethernet link is down
        """
        if os.getenv("CLOUD_PROVIDER") == "aws":
            # Setting non-management interface down turn off kubernetes connectivity for some reason
            # Jira: https://jirasw.nvidia.com/browse/NGCC-25441
            pytest.skip("This test case is not supported on AWS. Skipping this test case.")

        self.step_manager.print_header(
            "Filter out the nodes with more than 2 physical interface on the node"
        )
        nodes_interfaces_dict = self.get_nodes_with_more_than_two_physical_interfaces()
        if not nodes_interfaces_dict:
            pytest.skip(
                "Cannot find a node with more than 2 physical interfaces on the cluster. "
                "Skipping this test case."
            )

        nodes_list = list(nodes_interfaces_dict.keys())
        # select 2 nodes to test
        nodes = random.sample(nodes_list, min(2, len(nodes_list)))
        self.logger.info(f"Will do interface up/down on below nodes:{nodes}")
        for node_name in nodes:
            assert not self.check_if_ib_interface(node_name), (
                "There are IB interface on the node. Pls check it manually. This case is for ethernet interface "
                "without IB "
                "interface connected"
            )

            self.step_manager.print_header(
                "Get the nvsentinel-nic-health-monitor pod of the node {node}"
            )
            pods, _ = self.client.list_pods(
                self.nv_namespace, name_pattern="nvsentinel-nic-health*"
            )
            pod_name = None
            for pod in pods:
                if pod.spec.node_name == node_name:
                    pod_name = pod.metadata.name
                    self.logger.info(f"POD   Name: {pod_name}")
                    self.logger.info(f"Node  Name: {node_name}")
                    break

            self.step_manager.print_header(
                "Open one console to check the logs from the pod, do not close this console"
            )
            self.pod_logs = []
            monitor_thread = threading.Thread(
                target=self.follow_pod_logs, args=(self.nv_namespace, pod_name), daemon=True
            )
            monitor_thread.start()

            self.step_manager.print_header(
                "Login the node where the monitor pod is running on"
            )

            non_mgmt_interface = self.get_non_mgmt_ports_of_the_node(node_name)
            assert (
                non_mgmt_interface
            ), "Cannot find non-mgmt port on the node {}. Pls check it manually"
            self.logger.info(f"TARGET PORT NAME: {non_mgmt_interface}")
            request.addfinalizer(
                partial(self.up_interface_of_node, node_name, non_mgmt_interface)
            )
            self.step_manager.print_header(
                "Down/Up one port and check the logs from the pod"
            )
            self.ethernet_link_down_test(request, node_name, non_mgmt_interface)
