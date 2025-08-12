# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import time
import pytest
from testcases.nvsentinel.health_monitors.syslog_health_monitor.syslog_mock_test_utils import SyslogMockTestBase


class TestKernelMessageFiltering(SyslogMockTestBase):
    """
    Class for testing kernel message filtering in NVsentinel Syslog Health Monitor:
    Tests the -k tag functionality which filters for kernel messages (SYSLOG_FACILITY=0)
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_kernel_message_filtering(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Test for kernel message filtering using the -k tag.
        
        Test steps:
        1. Update DaemonSet polling interval to 1 minute
        2. Create debug pod
        3. Write kernel message using logger
        4. Wait for 1 minute
        5. Check if node conditions are updated
        """
        
        # Pre-test verification
        self._pre_test_verification()
        
        # Track condition types for cleanup
        self.node_conditions_to_cleanup.extend(["SysLogsDriverVersionMismatch"])
        
        # Step 1: Update DaemonSet polling interval to 1 minute
        self.step_manager.print_header("Step 1: Update DaemonSet polling interval to 1 minute")
        self._update_daemonset_polling_interval("1m")
        
        # Step 2: Create debug pod
        self.step_manager.print_header("Step 2: Create debug pod on target node")
        self.debug_pod = self._create_debug_pod_on_node()
        
        # Step 3: Write kernel message
        self.step_manager.print_header("Step 3: Write kernel message using logger")
        self._write_kernel_message()
        
        # Step 3.5: Verify message was written and check initial syslog monitor logs
        self.step_manager.print_header("Step 3.5: Verify message was written and check syslog monitor logs")
        self._debug_journal_entries()
        self._check_syslog_monitor_logs()
        
        # Step 4: Wait for 1 minute
        self.step_manager.print_header("Step 4: Wait for 1 minute for monitoring to detect errors")
        self.logger.info("Waiting for 100 seconds for the syslog health monitor to poll and detect errors...")
        time.sleep(100)
        
        # Step 5: Check if node conditions are updated
        self.step_manager.print_header("Step 5: Verify node conditions are updated")
        self._debug_journal_entries()
        self._verify_kernel_condition()
        
        self.logger.info("All test steps completed successfully")

    def _verify_kernel_condition(self):
        """Verify that node conditions are updated after kernel message detection"""
        
        # Use base class method to verify SysLogsDriverVersionMismatch condition
        self._verify_node_condition("SysLogsDriverVersionMismatch", expected_reason="SysLogsDriverVersionMismatchIsNotHealthy")
        self.logger.info("✅ Kernel message condition verified successfully")
        
        # Additional debugging if needed
        self._check_syslog_monitor_logs()

    def _write_kernel_message(self):
        """Write a kernel message using direct write to /dev/kmsg"""
        
        # Write kernel message directly to /dev/kmsg to create a real kernel message
        cmd = ["/bin/sh", "-c", 'chroot /host sh -c \'echo "<6>Failed to initialize NVML: Driver/library version mismatch" > /dev/kmsg\'']
        output, err_msg = self.client.exec_command_in_pod(self.debug_pod, cmd, timeout=30)
        
        if err_msg:
            self.logger.warning(f"Failed to write kernel message: {err_msg}")
        else:
            self.logger.info("Successfully wrote kernel message to /dev/kmsg")


    def _debug_journal_entries(self):
        """Debug what patterns are actually being generated in the journal"""
        if not self.debug_pod:
            return
            
        self.logger.info("=== DEBUGGING KERNEL JOURNAL PATTERNS ===")
        
        debug_commands = [
            # Test pattern matching manually against expected patterns
            ["/bin/bash", "-c", "chroot /host journalctl -k --no-pager -n 10 | grep -E 'Version mismatch'"],
            
            # Show recent kernel messages to see what was actually logged
            ["/bin/bash", "-c", "chroot /host journalctl -k --no-pager -n 10"],
            
            # Show all recent journal entries (not just kernel) to see if message went elsewhere
            ["/bin/bash", "-c", "chroot /host journalctl --no-pager -n 10"],
        ]
        
        for cmd in debug_commands:
            try:
                output, err_msg = self.client.exec_command_in_pod(self.debug_pod, cmd, timeout=30)
                cmd_str = " ".join(cmd)
                self.logger.info(f"Debug command: {cmd_str}")
                self.logger.info(f"Output: {output}")
                if err_msg:
                    self.logger.info(f"Stderr: {err_msg}")
                self.logger.info("---")
            except Exception as e:
                self.logger.warning(f"Debug command failed: {e}")
        
        self.logger.info("=== END KERNEL JOURNAL PATTERN DEBUG ===")

    def _check_syslog_monitor_logs(self):
        """Check syslog monitor pod logs for debugging"""
        
        pods, err_msg = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-syslog-health*"
        )
        
        if not err_msg and pods:
            for pod in pods:
                if pod.spec.node_name == self.test_node_name:
                    logs, log_err = self.client.get_pod_logs(
                        namespace=self.nv_namespace,
                        pod_name=pod.metadata.name,
                        container_name="syslog-health-monitor"
                    )
                    
                    if not log_err:
                        self.logger.info(f"Syslog monitor logs from {pod.metadata.name}:")
                        # Show last 30 lines
                        log_lines = logs.split('\n')[-30:]
                        for line in log_lines:
                            if line.strip():
                                self.logger.info(f"  {line}")
                    else:
                        self.logger.warning(f"Failed to get logs from {pod.metadata.name}: {log_err}")
                    break
        else:
            self.logger.warning(f"Failed to find syslog monitor pods: {err_msg}") 