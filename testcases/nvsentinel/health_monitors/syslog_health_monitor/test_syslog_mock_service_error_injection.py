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
import time
import pytest
import tempfile
import yaml
from kubernetes.client import V1DaemonSet
import json
from testcases.nvsentinel.health_monitors.syslog_health_monitor.syslog_mock_test_utils import SyslogMockTestBase


class TestFabricmanagerMockErrorInjection(SyslogMockTestBase):
    """
    Class for comprehensive test case of NVsentinel Syslog Health Monitor: 
    Mock Fabricmanager errors injection with full setup and teardown
    """

    def setup_method(self):
        """Setup method to initialize test variables"""
        super().setup_method()  # Call parent setup
        self.created_services = []
        self.original_configmap_data = None

    def teardown_method(self):
        """Teardown method to clean up all changes"""
        self.logger.info("Starting service-specific test cleanup...")
        
        # Clean up mock services (service-specific cleanup)
        if self.debug_pod and self.test_node_name:
            self._cleanup_mock_services()
        
        # Restore node label after test
        if hasattr(self, 'test_node_name') and self.test_node_name:
            self.restore_managed_by_nvsentinel_label(self.test_node_name)
        
        # Call parent teardown (handles node conditions, debug pod, and backup files)
        super().teardown_method()

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_fabricmanager_mock_error_injection(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Comprehensive test for fabricmanager error injection using mock services.
        
        Test steps:
        1. Apply new configmap with mock service name
        2. Apply DS changes to poll every 1m
        3. Create debug pod
        4. Create mock service inside debug pod after chroot /host
        5. Start service using systemctl
        6. Wait for 1 minute
        7. Check if node conditions are updated
        8. Undo all changes
        """
        
        # Pre-test verification
        self._pre_test_verification()
        
        # Set node label to prevent FQ module handling during test
        self.set_managed_by_nvsentinel_label_to_false(self.test_node_name)
        
        # Track condition types for cleanup
        self.node_conditions_to_cleanup.extend(["SysLogsNvidiaFabricmanager", "SysLogsNvidiaPersistenced"])
        
        # Step 1: Apply new configmap with mock service name
        self.step_manager.print_header("Step 1: Backup and update ConfigMap with mock services")
        self._update_configmap_with_mock_services()
        
        # Step 2: Apply DS changes to poll every 1m
        self.step_manager.print_header("Step 2: Update DaemonSet polling interval to 1 minute")
        self._update_daemonset_polling_interval()
        
        # Step 3: Create debug pod
        self.step_manager.print_header("Step 3: Create debug pod on target node")
        self.debug_pod = self._create_debug_pod_on_node()
        
        # Step 4: Create mock service inside debug pod after chroot /host
        self.step_manager.print_header("Step 4: Create mock systemd services in debug pod")
        self._create_mock_services_in_debug_pod()
        
        # Step 5: Start service using systemctl
        self.step_manager.print_header("Step 5: Start mock services using systemctl")
        self._start_mock_services()
        
        # Step 6: Wait for 1 minute
        self.step_manager.print_header("Step 6: Wait for 1 minute for monitoring to detect errors")
        self.logger.info("Waiting for 60 seconds for the syslog health monitor to poll and detect errors...")
        time.sleep(60)
        
        # Step 7: Check if node conditions are updated
        self.step_manager.print_header("Step 7: Verify node conditions are updated")
        self._debug_journal_entries()
        self._verify_service_specific_conditions()
        
        # Step 8: Cleanup is handled by teardown_method
        self.step_manager.print_header("Step 8: Cleanup will be handled by teardown")
        self.logger.info("All test steps completed successfully")

    def _verify_service_specific_conditions(self):
        """Verify that node conditions are updated after error detection"""
        
        # Use base class method to verify SysLogsNvidiaFabricmanager condition
        self._verify_node_condition("SysLogsNvidiaFabricmanager", expected_reason="SysLogsNvidiaFabricmanagerIsNotHealthy")
        self.logger.info("✅ Fabricmanager error condition verified successfully")
        
        # Use base class method to verify SysLogsNvidiaPersistenced condition  
        self._verify_node_condition("SysLogsNvidiaPersistenced", expected_reason="SysLogsNvidiaPersistencedIsNotHealthy")
        self.logger.info("✅ Persistenced error condition verified successfully")
        
        # Additional debugging if needed
        self._check_syslog_monitor_logs()

    def _update_configmap_with_mock_services(self):
        """Update the syslog-health-monitor ConfigMap to use mock services"""
        
        # Get current ConfigMap
        configmap, err_msg = self.client.get_configmap(
            namespace=self.nv_namespace,
            configmap_name="nvsentinel-syslog-health-monitor"
        )
        
        if err_msg or not configmap:
            pytest.fail(f"Failed to get ConfigMap: {err_msg}")
        
        # Backup original data
        self.original_configmap_data = configmap.data.copy()
        
        # Parse the current YAML configuration
        current_config = yaml.safe_load(configmap.data["log_check_definitions.yaml"])
        
        # Update the service names to mock services
        for check in current_config["checks"]:
            if check["name"] == "SysLogsNvidiaFabricmanager":
                # Update tags to point to mock service
                check["tags"] = ["-b", "-u mock-nv-fabricmanager.service"]
                self.logger.info("Updated fabricmanager check to use mock-nv-fabricmanager.service")
            elif check["name"] == "SysLogsNvidiaPersistenced":
                # Update tags to point to mock service
                check["tags"] = ["-b", "-u mock-nvidia-persistenced.service"]
                self.logger.info("Updated persistenced check to use mock-nvidia-persistenced.service")
        
        # Create temporary ConfigMap file
        temp_configmap_file = tempfile.NamedTemporaryFile(mode='w', suffix='.yaml', delete=False)
        self.backup_files.append(temp_configmap_file.name)
        
        configmap_yaml = {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {
                "name": "nvsentinel-syslog-health-monitor",
                "namespace": self.nv_namespace
            },
            "data": {
                "log_check_definitions.yaml": yaml.dump(current_config, default_flow_style=False)
            }
        }
        
        yaml.dump(configmap_yaml, temp_configmap_file, default_flow_style=False)
        temp_configmap_file.close()
        
        # Apply the updated ConfigMap
        success, err_msg = self.client.apply_configmap(temp_configmap_file.name)
        if not success:
            pytest.fail(f"Failed to apply updated ConfigMap: {err_msg}")
        
        self.logger.info("Successfully updated ConfigMap with mock service names")

    def _create_mock_services_in_debug_pod(self):
        """Create mock systemd services in the debug pod"""
        
        # Create service files locally first
        fabricmanager_service_content = """[Unit]
Description=Mock NVIDIA Fabric Manager Service for Testing
After=multi-user.target

[Service]
Type=oneshot
ExecStart=/bin/bash -c 'echo "Failed to find GPU" >&2; echo "nv-fabricmanager fatal: Unable to initialize" >&2; exit 1'
RemainAfterExit=no

[Install]
WantedBy=multi-user.target
"""

        persistenced_service_content = """[Unit]
Description=Mock NVIDIA Persistence Daemon for Testing
After=multi-user.target

[Service]
Type=oneshot
ExecStart=/bin/bash -c 'echo "nvidia-persistenced: failed to open /dev/nvidiactl" >&2; exit 1'
RemainAfterExit=no

[Install]
WantedBy=multi-user.target
"""

        # Create temporary files
        import tempfile
        fabricmanager_file = tempfile.NamedTemporaryFile(mode='w', suffix='.service', delete=False)
        persistenced_file = tempfile.NamedTemporaryFile(mode='w', suffix='.service', delete=False)
        
        try:
            # Write service files
            fabricmanager_file.write(fabricmanager_service_content)
            fabricmanager_file.close()
            
            persistenced_file.write(persistenced_service_content)
            persistenced_file.close()
            
            # Add to backup files for cleanup
            self.backup_files.extend([fabricmanager_file.name, persistenced_file.name])
            
            # Copy files to debug pod using kubectl cp
            copy_commands = [
                f"kubectl cp {fabricmanager_file.name} default/{self.debug_pod.metadata.name}:/tmp/mock-nv-fabricmanager.service",
                f"kubectl cp {persistenced_file.name} default/{self.debug_pod.metadata.name}:/tmp/mock-nvidia-persistenced.service"
            ]
            
            for cmd in copy_commands:
                result = os.system(cmd)
                if result != 0:
                    self.logger.warning(f"Copy command failed: {cmd}")
                else:
                    self.logger.info(f"Successfully copied file: {cmd}")
            
            # Verify files were copied to pod and then move to host systemd directory
            setup_commands = [
                # First verify the files exist in the pod
                ["/bin/bash", "-c", "ls -la /tmp/mock-*.service"],
                # Also check what's in /tmp
                ["/bin/bash", "-c", "ls -la /tmp/"],
                # Copy files directly to host systemd directory (bypassing the intermediate copy)
                ["/bin/bash", "-c", "cp /tmp/mock-nv-fabricmanager.service /host/etc/systemd/system/ || echo 'Failed to copy fabricmanager service'"],
                ["/bin/bash", "-c", "cp /tmp/mock-nvidia-persistenced.service /host/etc/systemd/system/ || echo 'Failed to copy persistenced service'"],
                ["/bin/bash", "-c", "chroot /host systemctl daemon-reload"]
            ]
            
            for cmd in setup_commands:
                output, err_msg = self.client.exec_command_in_pod(self.debug_pod, cmd, timeout=30)
                if err_msg:
                    self.logger.warning(f"Setup command warning: {err_msg}")
                self.logger.info(f"Setup command output: {output}")
            
            # Verify services were created
            verify_commands = [
                ["/bin/bash", "-c", "chroot /host cat /etc/systemd/system/mock-nv-fabricmanager.service"],
                ["/bin/bash", "-c", "chroot /host cat /etc/systemd/system/mock-nvidia-persistenced.service"]
            ]
            
            for cmd in verify_commands:
                output, err_msg = self.client.exec_command_in_pod(self.debug_pod, cmd, timeout=30)
                self.logger.info(f"Verification output: {output}")
                if err_msg:
                    self.logger.warning(f"Verification warning: {err_msg}")
            
            self.created_services = ["mock-nv-fabricmanager.service", "mock-nvidia-persistenced.service"]
            self.logger.info("Successfully created mock systemd services")
            
        except Exception as e:
            self.logger.error(f"Failed to create mock services: {e}")
            raise

    def _start_mock_services(self):
        """Start the mock services to generate error logs"""
        
        start_commands = [
            # Start mock fabricmanager service (will fail intentionally)
            ["/bin/bash", "-c", "chroot /host systemctl start mock-nv-fabricmanager.service || true"],
            
            # Start mock persistenced service (will fail intentionally)  
            ["/bin/bash", "-c", "chroot /host systemctl start mock-nvidia-persistenced.service || true"],
            
            # Check service status
            ["/bin/bash", "-c", "chroot /host systemctl status mock-nv-fabricmanager.service || true"],
            ["/bin/bash", "-c", "chroot /host systemctl status mock-nvidia-persistenced.service || true"]
        ]
        
        for cmd in start_commands:
            output, err_msg = self.client.exec_command_in_pod(self.debug_pod, cmd, timeout=30)
            self.logger.info(f"Service command output: {output}")
            if err_msg:
                self.logger.info(f"Service command stderr: {err_msg}")
        
        # Verify error logs are in journal
        verify_logs_cmd = ["/bin/bash", "-c", 
            "chroot /host journalctl -u mock-nv-fabricmanager.service -u mock-nvidia-persistenced.service --no-pager -n 20"]
        output, err_msg = self.client.exec_command_in_pod(self.debug_pod, verify_logs_cmd, timeout=30)
        self.logger.info(f"Journal logs verification: {output}")
        
        self.logger.info("Successfully started mock services and generated error logs")

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
                        # Show last 20 lines
                        log_lines = logs.split('\n')[-20:]
                        for line in log_lines:
                            if line.strip():
                                self.logger.info(f"  {line}")
                    break

    def _debug_journal_entries(self):
        """Debug what patterns are actually being generated in the journal"""
        if not self.debug_pod:
            return
            
        self.logger.info("=== DEBUGGING JOURNAL PATTERNS ===")
        
        debug_commands = [
            # Show raw journal entries for our mock services
            ["/bin/bash", "-c", "chroot /host journalctl -u mock-nv-fabricmanager.service --no-pager -n 5 --output=cat"],
            ["/bin/bash", "-c", "chroot /host journalctl -u mock-nvidia-persistenced.service --no-pager -n 5 --output=cat"],
            
            # Test pattern matching manually against expected patterns
            ["/bin/bash", "-c", "chroot /host journalctl -u mock-nv-fabricmanager.service --no-pager -n 10 | grep -E 'Failed to find GPU'"],
            ["/bin/bash", "-c", "chroot /host journalctl -u mock-nv-fabricmanager.service --no-pager -n 10 | grep -E ' fatal'"],
            ["/bin/bash", "-c", "chroot /host journalctl -u mock-nvidia-persistenced.service --no-pager -n 10 | grep -E 'failed to open'"],
            
            # Show the actual journalctl command the syslog monitor would use (with boot flag)
            ["/bin/bash", "-c", "chroot /host journalctl -b -u mock-nv-fabricmanager.service --no-pager -n 5"],
            ["/bin/bash", "-c", "chroot /host journalctl -b -u mock-nvidia-persistenced.service --no-pager -n 5"]
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
        
        self.logger.info("=== END JOURNAL PATTERN DEBUG ===")

    def _cleanup_mock_services(self):
        """Clean up mock services from the host"""
        
        # First check if the debug pod still exists and is running
        try:
            pod_info = self.client.coreV1Api.read_namespaced_pod(
                name=self.debug_pod,
                namespace=self.nv_namespace
            )
            
            if pod_info.status.phase != "Running":
                self.logger.warning(f"Debug pod {self.debug_pod} is not running (status: {pod_info.status.phase}). Skipping mock service cleanup.")
                return
                
        except Exception as e:
            self.logger.warning(f"Debug pod {self.debug_pod} no longer exists or is not accessible: {e}. Skipping mock service cleanup.")
            return
        
        cleanup_commands = [
            # Stop services
            ["/bin/bash", "-c", "chroot /host systemctl stop mock-nv-fabricmanager.service || true"],
            ["/bin/bash", "-c", "chroot /host systemctl stop mock-nvidia-persistenced.service || true"],
            
            # Disable services
            ["/bin/bash", "-c", "chroot /host systemctl disable mock-nv-fabricmanager.service || true"],
            ["/bin/bash", "-c", "chroot /host systemctl disable mock-nvidia-persistenced.service || true"],
            
            # Remove service files
            ["/bin/bash", "-c", "chroot /host rm -f /etc/systemd/system/mock-nv-fabricmanager.service"],
            ["/bin/bash", "-c", "chroot /host rm -f /etc/systemd/system/mock-nvidia-persistenced.service"],
            
            # Remove temporary files from pod
            ["/bin/bash", "-c", "rm -f /tmp/mock-nv-fabricmanager.service"],
            ["/bin/bash", "-c", "rm -f /tmp/mock-nvidia-persistenced.service"],
            
            # Reload systemd daemon
            ["/bin/bash", "-c", "chroot /host systemctl daemon-reload"]
        ]
        
        for cmd in cleanup_commands:
            try:
                output, err_msg = self.client.exec_command_in_pod(self.debug_pod, cmd, timeout=30)
                if err_msg:
                    self.logger.warning(f"Cleanup command warning: {err_msg}")
            except Exception as e:
                self.logger.warning(f"Failed to execute cleanup command in debug pod: {e}")
                # If pod becomes unavailable during cleanup, break out of the loop
                if "404" in str(e) or "Not Found" in str(e):
                    self.logger.warning("Debug pod became unavailable during cleanup. Stopping cleanup commands.")
                    break
        
        self.logger.info("Cleaned up mock services from host")
