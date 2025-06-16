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
Shared utility module for syslog health monitor tests
"""

import os
import time
import tempfile
import yaml
import pytest
from testcases.nvsentinel.base import TestNVSentinelCaseBase


class SyslogMockTestBase(TestNVSentinelCaseBase):
    """
    Base class for syslog health monitor mock tests with shared functionality
    """

    def setup_method(self):
        """Setup method to initialize test variables"""
        self.backup_files = []
        self.test_node_name = None
        self.debug_pod = None
        self.node_conditions_to_cleanup = []
        self.original_daemonset_data = None

    def teardown_method(self):
        """Teardown method to clean up all changes"""
        self.logger.info("Starting test cleanup...")
        
        # Clean up node conditions for test idempotency
        if self.test_node_name and self.node_conditions_to_cleanup:
            self.logger.info(f"Attempting to remove node conditions: {self.node_conditions_to_cleanup}")
            
            # First, check what conditions currently exist
            node_info, err_msg = self.client.get_node_by_name(
                node_name=self.test_node_name, node_type="gpu"
            )
            if node_info and node_info.status.conditions:
                self.logger.info("Current node conditions before cleanup:")
                for condition in node_info.status.conditions:
                    self.logger.info(f"  - {condition.type}: {condition.status} ({condition.reason})")
            
            # Attempt to remove each condition
            for condition_type in self.node_conditions_to_cleanup:
                try:
                    self.logger.info(f"Attempting to remove condition: {condition_type}")
                    success, err_msg = self.client.remove_node_condition(self.test_node_name, condition_type)
                    if success:
                        self.logger.info(f"✅ Successfully removed node condition: {condition_type}")
                    else:
                        self.logger.warning(f"❌ Failed to remove node condition {condition_type}: {err_msg}")
                except Exception as e:
                    self.logger.warning(f"❌ Exception while removing node condition {condition_type}: {e}")
            
            # Verify conditions were removed
            node_info, err_msg = self.client.get_node_by_name(
                node_name=self.test_node_name, node_type="gpu"
            )
            if node_info and node_info.status.conditions:
                remaining_conditions = [c.type for c in node_info.status.conditions if c.type in self.node_conditions_to_cleanup]
                if remaining_conditions:
                    self.logger.warning(f"⚠️ Some conditions still remain: {remaining_conditions}")
                else:
                    self.logger.info("✅ All target conditions successfully removed")
        
        # Restore original DaemonSet if it was modified
        if self.original_daemonset_data:
            self._restore_daemonset_polling_interval()
        
        # Restore node label after test
        if hasattr(self, 'test_node_name') and self.test_node_name:
            self.restore_managed_by_nvsentinel_label(self.test_node_name)
        
        # Clean up debug pod (base class uses fixed name "debug-pod" in "default" namespace)
        if self.debug_pod:
            self.client.delete_pod_by_name("debug-pod", "default")
        
        # Remove backup files (no longer needed since we don't create temp files)
        for backup_file in self.backup_files:
            if os.path.exists(backup_file):
                os.remove(backup_file)
        
        self.logger.info("Test cleanup completed")

    def _pre_test_verification(self):
        """Pre-test verification: Check syslog-health-monitor pods"""
        self.step_manager.print_header("Pre-test verification: Check syslog-health-monitor pods")
        pods, err_msg = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-syslog-health*"
        )
        if err_msg or not pods:
            pytest.skip(f"No nvsentinel-syslog-health-monitor pod found: {err_msg}")
        
        # Select a target node
        self.step_manager.print_header("Select target node for testing")
        job_pod = pods[0]
        self.test_node_name = job_pod.spec.node_name
        self.logger.info(f"Selected node: {self.test_node_name}")
        self.logger.info(f"Syslog health monitor pod: {job_pod.metadata.name}")

    def _create_debug_pod_on_node(self):
        """Create debug pod on target node using base class create_debug_pod method"""
        # Use the base class method to create debug pod on the target node
        debug_pod = self.create_debug_pod(node_name=self.test_node_name)
        if not debug_pod:
            pytest.fail("Failed to create debug pod using base class method")
        
        self.logger.info(f"Debug pod {debug_pod.metadata.name} created and ready on node {self.test_node_name}")
        
        # Return the pod name for exec commands
        return debug_pod

    def _inject_error_messages_with_logger(self, error_messages, repeat_count=1):
        """Inject error messages using logger command"""
        
        for message in error_messages:
            for i in range(repeat_count):
                # Use logger to inject messages into the system journal
                logger_command = [
                    "/bin/sh", "-c",
                    f'chroot /host logger -p daemon.err "{message}"'
                ]
                
                output, err_msg = self.client.exec_command_in_pod(self.debug_pod, logger_command)
                if err_msg:
                    self.logger.warning(f"Logger command had stderr (may be normal): {err_msg}")
                
                self.logger.info(f"Injected error message: {message}")

    def _verify_node_condition(self, condition_type, expected_reason="ErrorDetected"):
        """Verify that node condition is updated"""
        
        # Get node info to access conditions
        node_info, err_msg = self.client.get_node_by_name(
            node_name=self.test_node_name, node_type="gpu"
        )
        if err_msg or node_info is None:
            pytest.fail(f"Failed to get node info: {err_msg}")
        
        # Check for the specific condition
        condition_found = False
        for condition in node_info.status.conditions:
            if condition.type == condition_type:
                condition_found = True
                self.logger.info(f"Found condition: {condition.type} - {condition.status} - {condition.reason}")
                assert condition.status == "True", f"Expected condition status to be True, got {condition.status}"
                assert condition.reason == expected_reason, f"Expected reason to be {expected_reason}, got {condition.reason}"
                break
        
        assert condition_found, f"Condition {condition_type} not found in node conditions"
        self.logger.info(f"Successfully verified node condition: {condition_type}")

    def _update_daemonset_polling_interval(self, interval="1m"):
        """Update the DaemonSet to poll at specified interval (default 1 minute for tests)"""
        
        # Get current DaemonSet
        daemonsets, err_msg = self.client.list_daemonset(
            namespace=self.nv_namespace,
            name_pattern="nvsentinel-syslog-health-monitor"
        )
        
        if err_msg or not daemonsets:
            pytest.fail(f"Failed to get DaemonSet: {err_msg}")
        
        daemonset = daemonsets[0]
        
        # Backup original data only if not already backed up
        if not self.original_daemonset_data:
            self.original_daemonset_data = daemonset.to_dict()
        
        # Update polling interval argument
        containers = daemonset.spec.template.spec.containers
        for container in containers:
            if container.name == "syslog-health-monitor":
                # Find and update the polling-interval argument
                args = container.args or []
                updated = False
                for i, arg in enumerate(args):
                    if arg == "--polling-interval" and i + 1 < len(args):
                        original_interval = args[i + 1]
                        args[i + 1] = interval
                        self.logger.info(f"Updated polling interval from {original_interval} to {interval}")
                        updated = True
                        break
                
                if not updated:
                    # If polling-interval not found, add it
                    args.extend(["--polling-interval", interval])
                    self.logger.info(f"Added polling interval argument: {interval}")
                
                container.args = args
                break
        
        # Apply the updated DaemonSet
        try:
            self.client.appsV1Api.replace_namespaced_daemon_set(
                name=daemonset.metadata.name,
                namespace=self.nv_namespace,
                body=daemonset
            )
            self.logger.info(f"Successfully updated DaemonSet polling interval to {interval}")
            
            # Wait for DaemonSet to rollout
            time.sleep(10)
            
        except Exception as e:
            pytest.fail(f"Failed to update DaemonSet: {e}")

    def _restore_daemonset_polling_interval(self):
        """Restore the original DaemonSet configuration"""
        
        if not self.original_daemonset_data:
            self.logger.warning("No original DaemonSet data to restore")
            return
        
        try:
            # Get current DaemonSet
            daemonsets, err_msg = self.client.list_daemonset(
                namespace=self.nv_namespace,
                name_pattern="nvsentinel-syslog-health-monitor"
            )
            
            if err_msg or not daemonsets:
                self.logger.warning(f"Failed to get DaemonSet for restoration: {err_msg}")
                return
            
            daemonset = daemonsets[0]
            
            # Restore original args
            original_containers = self.original_daemonset_data['spec']['template']['spec']['containers']
            current_containers = daemonset.spec.template.spec.containers
            
            for i, container in enumerate(current_containers):
                if container.name == "syslog-health-monitor" and i < len(original_containers):
                    container.args = original_containers[i].get('args', [])
                    break
            
            # Apply the restored DaemonSet
            self.client.appsV1Api.replace_namespaced_daemon_set(
                name=daemonset.metadata.name,
                namespace=self.nv_namespace,
                body=daemonset
            )
            self.logger.info("Successfully restored original DaemonSet configuration")
            
            # Wait for DaemonSet to rollout
            time.sleep(30)
            
        except Exception as e:
            self.logger.warning(f"Failed to restore DaemonSet: {e}")

    def run_logger_error_injection_test(self, error_messages, condition_type, expected_reason="ErrorDetected", repeat_count=1):
        """
        Generic method to run logger-based error injection test
        
        Args:
            error_messages: List of error messages to inject
            condition_type: Expected node condition type
            repeat_count: Number of times to repeat each message
        """
        
        # Pre-test verification
        self._pre_test_verification()
        
        # Set node label to prevent FQ module handling during test
        self.set_managed_by_nvsentinel_label_to_false(self.test_node_name)
        
        # Track condition type for cleanup
        self.node_conditions_to_cleanup.append(condition_type)
        
        # Step 1: Update DaemonSet to poll every 1 minute (for faster test execution)
        self.step_manager.print_header("Step 1: Update DaemonSet polling interval to 1 minute")
        self._update_daemonset_polling_interval()
        
        # Step 2: Create debug pod
        self.step_manager.print_header("Step 2: Create debug pod on target node")
        self.debug_pod = self._create_debug_pod_on_node()
        
        # Step 3: Inject error messages using logger
        self.step_manager.print_header("Step 3: Inject error messages using logger")
        self._inject_error_messages_with_logger(error_messages, repeat_count)
        
        # Step 4: Wait for monitoring cycle
        self.step_manager.print_header("Step 4: Wait for monitoring cycle to detect errors")
        self.logger.info("Waiting for 60 seconds for the syslog health monitor to poll and detect errors...")
        time.sleep(100)
        
        # Step 5: Check if node conditions are updated
        self.step_manager.print_header(f"Step 5: Verify node condition: {condition_type}")
        self._verify_node_condition(condition_type, expected_reason)
        
        # Step 6: Cleanup is handled by teardown_method
        self.step_manager.print_header("Step 6: Cleanup will be handled by teardown")
        self.logger.info("All test steps completed successfully") 