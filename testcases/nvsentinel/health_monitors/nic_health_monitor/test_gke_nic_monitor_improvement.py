# # SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# # SPDX-License-Identifier: LicenseRef-NvidiaProprietary
# #
# # NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# # property and proprietary rights in and to this material, related
# # documentation and any modifications thereto. Any use, reproduction,
# # disclosure or distribution of this material and related documentation
# # without an express license agreement from NVIDIA CORPORATION or
# # its affiliates is strictly prohibited.

# import time
# import pytest
# from functools import partial
# from testcases.nvsentinel.base import TestNVSentinelCaseBase

# from testcases.common.constants import SupportedCSP
# from testcases.common.constants import EnumJobStatus
# import subprocess
# import random
# import threading
# import re


# TODO: Fix this (ajmishra)

# def analyze_state_transitions(condition_history):
#     """Analyze the state transitions in condition history.

#     Args:
#         condition_history: List of tuples (timestamp, status)

#     Returns:
#         tuple: (initial_false, had_true_period, ended_false, transitions)
#         where transitions is a list of (timestamp, from_state, to_state)
#     """
#     if not condition_history:
#         return False, False, False, []

#     # Check initial state
#     initial_false = condition_history[0][1] == "False"

#     # Track transitions
#     transitions = []
#     last_status = condition_history[0][1]
#     had_true_period = False
#     true_count = 0

#     for timestamp, status in condition_history[1:]:
#         if status != last_status:
#             transitions.append((timestamp, last_status, status))
#             if status == "True":
#                 true_count = 1
#             elif last_status == "True":
#                 if true_count >= 2:  # At least 2 consecutive True readings
#                     had_true_period = True
#         else:
#             if status == "True":
#                 true_count += 1
#         last_status = status

#     # Check final state
#     ended_false = condition_history[-1][1] == "False"

#     return initial_false, had_true_period, ended_false, transitions


# def track_node_condition(client, node_name, condition_history, start_time, timeout, logger):
#     """Track node condition changes and record them in condition_history.

#     Args:
#         client: KubernetesClient instance
#         node_name: Name of the node to monitor
#         condition_history: List to store condition changes
#         start_time: Start time of monitoring
#         timeout: Maximum monitoring duration
#         logger: Logger instance for output
#     """
#     while time.time() - start_time < timeout:
#         condition, _ = client.read_node_condition_by_type(
#             node_name=node_name, condition_type="EthernetErrorCheck"
#         )
#         current_status = condition.status if condition else "Unknown"
#         timestamp = time.time() - start_time
#         condition_history.append((timestamp, current_status))
#         logger.info(f"[{timestamp:.1f}s] Node condition status: {current_status}")
#         time.sleep(5)


# class TestGKENICHealthMonitor(TestNVSentinelCaseBase):
#     """Test GKE NIC monitor improvement"""

#     @pytest.mark.healthmonitor
#     def test_gke_nic_monitor_improvement(self, request):
#         """
#         Test case of NVsentinel NIC Health Monitor: GKE NIC monitor improvement
#         """
        
#         pytest.skip(
#             "This test case is for GCP cluster", allow_module_level=True
#         ) if "GCP" != os.environ.get("CLOUD_PROVIDER") else None

#         self.step_manager.print_header(
#             "Filter out the nodes with more than 2 physical interface on the cluster"
#         )
#         nodes_interfaces_dict = self.get_nodes_with_more_than_two_physical_interfaces()
#         if not nodes_interfaces_dict:
#             pytest.skip(
#                 "Cannot find a node with more than 2 physical interfaces on the cluster. "
#                 "Please check all nodes manually."
#             )
#         self.logger.info(
#             f"Current interfaces info of all the nodes:\n {nodes_interfaces_dict}"
#         )
#         self.logger.info("Filter out ethx interface on the node")
#         has_eth_ports = False
#         for node, interfaces in nodes_interfaces_dict.items():
#             if interfaces[0].startswith("eth"):
#                 has_eth_ports = True
#                 break
#         assert (
#             has_eth_ports
#         ), "Cannot find a node with eth interface. Please check all nodes manually."

#         try:
#             self.step_manager.print_header(
#                 "Submit a distributed job which requires all (usually 8) the GPUs on one node "
#             )

#             self.device.runai.training.mpi.submit(
#                 name=self.job_name,
#                 image=self.runai_demo_dis_image,
#                 workers="1",
#                 gpu_devices_request="8",
#                 environment="RUNAI_SLEEP_SECS=3000",
#             )
#             self.step_manager.print_subheader("Verify that the job is running")
#             self.job_utility.info.verify_job_status(self.job_name, EnumJobStatus["RUNNING"])
#             job_pod_name = f"{self.job_name}-worker-0"
#             node_name, _ = self.client.get_pod_running_node_name(
#                 job_pod_name, self.default_namespace
#             )
#             initial_nics = nodes_interfaces_dict[node_name]
#             filtered_interfaces = [iface for iface in initial_nics if iface != "eth0"]
#             self.logger.info(f"Initial NICs on node {node_name}: {initial_nics}")
#             self.step_manager.print_header(
#                 "List the NIC again, verify ethX NICs (except eth0) are vanished"
#             )
#             nics_after = self.get_node_network_interfaces(node_name)
#             nics_after = [iface for iface in nics_after if iface.startswith("eth")]
#             self.logger.info(f"NICs after job start: {nics_after}")

#             # Verify eth interfaces (except eth0) are moved to pod
#             eth_interfaces_after = [
#                 nic for nic in nics_after if nic.startswith("eth") and nic != "eth0"
#             ]
#             assert not eth_interfaces_after, f"Expected all non-eth0 interfaces to be moved to pod, but found: {eth_interfaces_after}"
#             assert "eth0" in nics_after, "Management interface eth0 should remain on node"
#             self.logger.info("SUCCESS: All non-eth0 interfaces  are vanished")

#             self.step_manager.print_header(
#                 "Check the nic-health-monitor sidecar exists in the job pod"
#             )
#             job_pod, _ = self.client.read_pod(job_pod_name, self.default_namespace)
#             init_containers = job_pod.spec.init_containers
#             assert any(
#                 container.name == "nic-health-monitor" for container in init_containers
#             ), "nic-health-monitor sidecar not found in the job pod"
#             self.logger.info("SUCCESS: nic-health-monitor sidecar exists in the job pod")

#             self.step_manager.print_header(
#                 "Attach to the train job pod and execute network interface operations"
#             )

#             non_mgmt_interface = random.choice(filtered_interfaces)
#             request.addfinalizer(
#                 partial(self.up_interface_of_node, node_name, non_mgmt_interface)
#             )
#             pods, _ = self.client.list_pods(
#                 self.nv_namespace, name_pattern="nvsentinel-nic-health.*"
#             )
#             nic_pod_name = None

#             for pod in pods:
#                 if pod.spec.node_name == node_name:
#                     nic_pod_name = pod.metadata.name
#                     self.logger.info(f"POD   Name: {nic_pod_name}")
#                     self.logger.info(f"Node  Name: {node_name}")
#                     break
#             assert (
#                 nic_pod_name
#             ), f"Cannot find the nvsentinel-nic-health-monitor pod of the node {node_name}"

#             self.logger.info(
#                 f"SUCCESS: Found the nvsentinel-nic-health-monitor pod of the node {node_name}"
#             )

#             nic_pod_log_1, _ = self.client.get_pod_logs(self.nv_namespace, nic_pod_name)
#             self.step_manager.print_header(
#                 f"Down one port and up one port {non_mgmt_interface} on"
#             )
#             # Create debug command that executes all operations in sequence
#             debug_cmd = (
#                 f"kubectl debug {job_pod_name} -it "
#                 f"--image=busybox "
#                 f"--profile=netadmin "  # Use netadmin profile instead of privileged
#                 f"-c tcpx-daemon "  # Add container name
#                 f"-n {self.default_namespace} -- sh -c '"
#                 f'echo "Initial interface status:" && '
#                 f"ip link show && "
#                 f'echo "Setting {non_mgmt_interface} down..." && '
#                 f"ip link set dev {non_mgmt_interface} down && "
#                 f'echo "Interface status after down:" && '
#                 f"ip link show {non_mgmt_interface} && "
#                 f"sleep 30 && "  # Wait for condition to change
#                 f'echo "Setting {non_mgmt_interface} up..." && '
#                 f"ip link set dev {non_mgmt_interface} up && "
#                 f'echo "Interface status after up:" && '
#                 f"ip link show && "
#                 f"sleep 30'"  # Wait for condition to change back
#             )

#             # Track node condition changes
#             self.step_manager.print_header(
#                 "Track node condition changes when down one port and up one port"
#             )
#             condition_history = []
#             start_time = time.time()
#             timeout = 100  # Total timeout: initial check + 30s down + 30s up + buffer

#             # Start node condition tracking in a separate thread
#             tracking_thread = threading.Thread(
#                 target=track_node_condition,
#                 args=(
#                     self.client,
#                     node_name,
#                     condition_history,
#                     start_time,
#                     timeout,
#                     self.logger,
#                 ),
#                 daemon=False,  # Change to non-daemon thread
#             )

#             try:
#                 # Start both operations simultaneously
#                 self.logger.info(
#                     "Starting network interface operations and condition tracking..."
#                 )
#                 tracking_thread.start()

#                 # Execute debug command and capture output
#                 debug_process = subprocess.Popen(
#                     debug_cmd,
#                     shell=True,
#                     stdout=subprocess.PIPE,
#                     stderr=subprocess.PIPE,
#                     text=True,
#                 )

#                 # Wait for debug command to complete first
#                 stdout, stderr = debug_process.communicate()
#                 self.logger.info("Debug command completed")
#                 self.logger.info("DEBUG COMMAND OUTPUT:")
#                 self.logger.info(f"{stdout}")
#                 if stderr:
#                     self.logger.info("DEBUG COMMAND STDERR:")
#                     self.logger.info(f"{stderr}")

#                 # Now wait for the tracking thread to finish completely
#                 tracking_thread.join()  # Remove timeout to wait for full completion
#                 self.logger.info("Monitoring thread completed")

#                 # Now analyze the complete condition history
#                 self.logger.info("Analyzing node condition changes...")
#                 self.logger.info(f"Complete condition history: {condition_history}")
#                 initial_false, had_true_period, ended_false, transitions = (
#                     analyze_state_transitions(condition_history)
#                 )

#                 # Log transitions for debugging
#                 for timestamp, from_state, to_state in transitions:
#                     self.logger.info(
#                         f"State transition at {timestamp:.1f}s: {from_state} -> {to_state}"
#                     )

#                 # Verify the complete cycle
#                 assert initial_false, "Initial state should be False"
#                 assert had_true_period, "Should have a period of consecutive True states"
#                 assert ended_false, "Final state should be False"

#                 self.logger.info(
#                     "Network interface operations and condition monitoring completed successfully"
#                 )

#                 # Check pod logs and conditions after tracking is complete
#                 self.step_manager.print_header(
#                     "From the nvsentinel-nic-health-monitor pod running on the node log console, No error will be found"
#                 )
#                 # Verify no new log
#                 nic_pod_log_2, _ = self.client.get_pod_logs(self.nv_namespace, nic_pod_name)
#                 assert (
#                     nic_pod_log_1[-1] == nic_pod_log_2[-1]
#                 ), "The log of the nic-health-monitor pod should not be changed"

#                 self.step_manager.print_header(
#                     "Check from node Condition, EthernetErrorCheck will change to true when port is down"
#                 )
#                 message_down = [
#                     'checkName:"EthernetErrorCheck"',
#                     'agent:"nic-health-monitor"',
#                     'componentClass:"NIC"',
#                     "state: down",
#                     f'entityType:"NIC"\\s+entityValue:"{non_mgmt_interface}"',
#                 ]
#                 job_pod_log, _ = self.client.get_pod_logs(
#                     self.default_namespace, job_pod_name, "nic-health-monitor"
#                 )
#                 log_lines = [line for line in job_pod_log.split("\n") if line.strip()]
#                 find_match = all(
#                     re.search(message, log_lines[-2]) for message in message_down
#                 )
#                 assert find_match, f"Find no expected message in job pod log:{job_pod_log}"
#                 self.logger.info("SUCCESS: 'state: down' message is shown in job pod log")

#                 self.step_manager.print_header(
#                     "Check from node Condition, EthernetErrorCheck will change to False when port is up"
#                 )
#                 message_up = [
#                     "EthernetErrorCheck",
#                     "Device is healthy",
#                     f'entityType:"NIC"\\s+entityValue:"{non_mgmt_interface}"',
#                 ]
#                 job_pod_log, _ = self.client.get_pod_logs(
#                     self.default_namespace, job_pod_name, "nic-health-monitor"
#                 )
#                 find_match = all(
#                     re.search(message, log_lines[-1]) for message in message_up
#                 )
#                 assert find_match, f"Find no expected message in job pod log:{job_pod_log}"
#                 self.logger.info(
#                     "SUCCESS: 'Device is healthy' message is shown in job pod log"
#                 )

#             except Exception as e:
#                 self.logger.error(f"Error during execution: {str(e)}")
#                 if debug_process.poll() is None:
#                     debug_process.terminate()
#                 if tracking_thread.is_alive():
#                     # Set a flag or use an event to signal the thread to stop
#                     tracking_thread.join(timeout=5)  # Give it 5 seconds to finish
#                 raise

#         finally:
#             self.step_manager.print_header("Delete the submitted job")
#             # Delete job
#             self.device.runai.workload.delete(name=self.job_name)
#             self.job_utility.info.verify_jobs_were_deleted(job_names=[self.job_name])

#             self.step_manager.print_header(
#                 "Check that ethX interfaces are back in the Node"
#             )
#             # Verify NICs returned to node
#             final_nics = self.get_node_network_interfaces(node_name)
#             final_nics = [iface for iface in initial_nics if iface.startswith("eth")]
#             assert set(final_nics) == set(
#                 initial_nics
#             ), f"NICs not returned to node. Expected: {initial_nics}, Got: {final_nics}"

#             # Run Ethernet link down test case again
#             self.step_manager.print_header("Run Ethernet link down test case again")
#             time.sleep(30)
#             self.pod_logs = []
#             monitor_thread = threading.Thread(
#                 target=self.follow_pod_logs,
#                 args=(self.nv_namespace, nic_pod_name),
#                 daemon=True,
#             )
#             monitor_thread.start()
#             self.ethernet_link_down_test(request, node_name, non_mgmt_interface)
