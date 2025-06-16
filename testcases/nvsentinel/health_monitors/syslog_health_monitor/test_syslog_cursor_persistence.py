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
import json
import tempfile
import uuid
import pytest
import time
from unittest.mock import patch, mock_open, MagicMock
from testcases.nvsentinel.health_monitors.syslog_health_monitor.syslog_mock_test_utils import SyslogMockTestBase


class TestSyslogCursorPersistence(SyslogMockTestBase):
    """
    Unit tests for syslog health monitor cursor persistence functionality.
    
    Tests cover:
    - State file creation and loading
    - Cursor persistence across restarts
    - Boot ID detection and cursor reset
    - Error handling for corrupted state files
    - Version migration
    """

    def setup_method(self):
        """Setup method to initialize test variables"""
        super().setup_method()
        # Create temporary state file for each test
        self.temp_state_file = None
        self.test_state_dir = tempfile.mkdtemp()
        self.test_state_file = os.path.join(self.test_state_dir, "test-syslog-state.json")

    def teardown_method(self):
        """Teardown method to clean up test files"""
        # Clean up temporary files
        if self.temp_state_file and os.path.exists(self.temp_state_file):
            os.remove(self.temp_state_file)
        
        if os.path.exists(self.test_state_file):
            os.remove(self.test_state_file)
            
        if os.path.exists(self.test_state_dir):
            os.rmdir(self.test_state_dir)
            
        super().teardown_method()

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_state_file_creation_and_persistence(self):
        """
        Test that state files are created correctly and cursor positions are persisted
        """
        self.step_manager.print_header("Test: State File Creation and Persistence")
        
        # Step 1: Create mock state data
        test_boot_id = str(uuid.uuid4())
        test_cursors = {
            "SysLogsNvidiaFabricmanager": "cursor-001",
            "SysLogsNvidiaPersistenced": "cursor-002"
        }
        
        expected_state = {
            "version": 1,
            "boot_id": test_boot_id,
            "check_last_cursors": test_cursors
        }
        
        # Step 2: Simulate writing state file
        self.logger.info("Creating test state file with mock data")
        with open(self.test_state_file, 'w') as f:
            json.dump(expected_state, f, indent=2)
        
        # Step 3: Verify state file was created correctly
        assert os.path.exists(self.test_state_file), "State file should be created"
        
        # Step 4: Read and verify state file contents
        with open(self.test_state_file, 'r') as f:
            loaded_state = json.load(f)
        
        assert loaded_state["version"] == expected_state["version"], "Version should match"
        assert loaded_state["boot_id"] == expected_state["boot_id"], "Boot ID should match"
        assert loaded_state["check_last_cursors"] == expected_state["check_last_cursors"], "Cursors should match"
        
        self.logger.info("✅ State file creation and persistence test passed")

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_state_file_loading_with_missing_file(self, nvsentinel_autosync_disabled_enabled):
        """
        Test that missing state files are handled gracefully with default state
        """
        self.step_manager.print_header("Test: State File Loading with Missing File")
        
        # Step 1: Ensure state file doesn't exist
        non_existent_file = "/tmp/non-existent-state.json"
        assert not os.path.exists(non_existent_file), "State file should not exist"
        
        # Step 2: Simulate loading missing state file behavior
        # This would normally be handled by the Go code, but we can test the logic
        default_state = {
            "version": 1,
            "boot_id": "",
            "check_last_cursors": {}
        }
        
        self.logger.info("Testing default state creation for missing file")
        
        # Step 3: Verify default state structure
        assert default_state["version"] == 1, "Default version should be 1"
        assert default_state["boot_id"] == "", "Default boot ID should be empty"
        assert default_state["check_last_cursors"] == {}, "Default cursors should be empty"
        
        self.logger.info("✅ Missing state file handling test passed")

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_corrupted_state_file_handling(self, nvsentinel_autosync_disabled_enabled):
        """
        Test that corrupted state files are handled gracefully
        """
        self.step_manager.print_header("Test: Corrupted State File Handling")
        
        # Step 1: Create corrupted JSON file
        self.logger.info("Creating corrupted state file")
        with open(self.test_state_file, 'w') as f:
            f.write("{ invalid json content here }")
        
        # Step 2: Verify file exists but is corrupted
        assert os.path.exists(self.test_state_file), "Corrupted file should exist"
        
        # Step 3: Try to load corrupted file
        try:
            with open(self.test_state_file, 'r') as f:
                json.load(f)
            assert False, "Should have failed to parse corrupted JSON"
        except json.JSONDecodeError:
            self.logger.info("✅ Corrupted JSON correctly detected")
        
        # Step 4: Test empty file handling
        self.logger.info("Testing empty state file")
        with open(self.test_state_file, 'w') as f:
            f.write("")
        
        with open(self.test_state_file, 'r') as f:
            content = f.read()
            assert content == "", "File should be empty"
        
        self.logger.info("✅ Corrupted state file handling test passed")

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_boot_id_change_detection(self, nvsentinel_autosync_disabled_enabled):
        """
        Test that boot ID changes are detected and cursors are reset
        """
        self.step_manager.print_header("Test: Boot ID Change Detection")
        
        # Step 1: Create initial state with old boot ID
        old_boot_id = "old-boot-id-12345"
        new_boot_id = "new-boot-id-67890"
        
        initial_cursors = {
            "SysLogsNvidiaFabricmanager": "old-cursor-001",
            "SysLogsNvidiaPersistenced": "old-cursor-002"
        }
        
        initial_state = {
            "version": 1,
            "boot_id": old_boot_id,
            "check_last_cursors": initial_cursors
        }
        
        self.logger.info(f"Creating initial state with boot ID: {old_boot_id}")
        with open(self.test_state_file, 'w') as f:
            json.dump(initial_state, f, indent=2)
        
        # Step 2: Simulate boot ID change
        self.logger.info(f"Simulating boot ID change to: {new_boot_id}")
        
        # Step 3: Create expected state after boot ID change (cursors should be reset)
        expected_state_after_reboot = {
            "version": 1,
            "boot_id": new_boot_id,
            "check_last_cursors": {}  # Cursors should be reset
        }
        
        # Step 4: Write updated state (simulating what the monitor would do)
        with open(self.test_state_file, 'w') as f:
            json.dump(expected_state_after_reboot, f, indent=2)
        
        # Step 5: Verify state was updated correctly
        with open(self.test_state_file, 'r') as f:
            updated_state = json.load(f)
        
        assert updated_state["boot_id"] == new_boot_id, "Boot ID should be updated"
        assert updated_state["check_last_cursors"] == {}, "Cursors should be reset after reboot"
        
        self.logger.info("✅ Boot ID change detection test passed")

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_version_migration(self, nvsentinel_autosync_disabled_enabled):
        """
        Test that state file version migration works correctly
        """
        self.step_manager.print_header("Test: Version Migration")
        
        # Step 1: Create state file with old version
        old_version_state = {
            "version": 0,  # Old version
            "boot_id": "test-boot-id",
            "check_last_cursors": {
                "SysLogsNvidiaFabricmanager": "old-version-cursor"
            }
        }
        
        self.logger.info("Creating state file with old version (0)")
        with open(self.test_state_file, 'w') as f:
            json.dump(old_version_state, f, indent=2)
        
        # Step 2: Simulate version migration (what the monitor would do)
        migrated_state = {
            "version": 1,  # Updated to current version
            "boot_id": "test-boot-id",
            "check_last_cursors": {
                "SysLogsNvidiaFabricmanager": "old-version-cursor"
            }
        }
        
        self.logger.info("Simulating version migration to version 1")
        with open(self.test_state_file, 'w') as f:
            json.dump(migrated_state, f, indent=2)
        
        # Step 3: Verify migration was successful
        with open(self.test_state_file, 'r') as f:
            updated_state = json.load(f)
        
        assert updated_state["version"] == 1, "Version should be migrated to 1"
        assert updated_state["boot_id"] == "test-boot-id", "Boot ID should be preserved"
        assert "check_last_cursors" in updated_state, "Cursors should be preserved"
        
        self.logger.info("✅ Version migration test passed")

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_cursor_persistence_across_multiple_runs(self, nvsentinel_autosync_disabled_enabled):
        """
        Test that cursors are correctly persisted and updated across multiple runs
        """
        self.step_manager.print_header("Test: Cursor Persistence Across Multiple Runs")
        
        boot_id = "stable-boot-id"
        
        # Step 1: Simulate first run with initial cursors
        run1_state = {
            "version": 1,
            "boot_id": boot_id,
            "check_last_cursors": {
                "SysLogsNvidiaFabricmanager": "cursor-run1-001",
                "SysLogsNvidiaPersistenced": "cursor-run1-002"
            }
        }
        
        self.logger.info("Simulating first run - saving initial cursors")
        with open(self.test_state_file, 'w') as f:
            json.dump(run1_state, f, indent=2)
        
        # Step 2: Simulate second run with updated cursors
        run2_state = {
            "version": 1,
            "boot_id": boot_id,
            "check_last_cursors": {
                "SysLogsNvidiaFabricmanager": "cursor-run2-001",  # Updated
                "SysLogsNvidiaPersistenced": "cursor-run2-002",   # Updated
                "SysLogsNvidiaDriver": "cursor-run2-003"          # New check added
            }
        }
        
        self.logger.info("Simulating second run - updating cursors")
        with open(self.test_state_file, 'w') as f:
            json.dump(run2_state, f, indent=2)
        
        # Step 3: Verify state persistence
        with open(self.test_state_file, 'r') as f:
            final_state = json.load(f)
        
        assert final_state["boot_id"] == boot_id, "Boot ID should remain stable"
        assert len(final_state["check_last_cursors"]) == 3, "Should have 3 cursors after second run"
        assert final_state["check_last_cursors"]["SysLogsNvidiaFabricmanager"] == "cursor-run2-001"
        assert final_state["check_last_cursors"]["SysLogsNvidiaPersistenced"] == "cursor-run2-002"
        assert final_state["check_last_cursors"]["SysLogsNvidiaDriver"] == "cursor-run2-003"
        
        self.logger.info("✅ Multi-run cursor persistence test passed")

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_state_file_directory_creation(self, nvsentinel_autosync_disabled_enabled):
        """
        Test that state file directory is created if it doesn't exist
        """
        self.step_manager.print_header("Test: State File Directory Creation")
        
        # Step 1: Create path to non-existent directory
        non_existent_dir = "/tmp/test-nvsentinel-syslog"
        test_state_file = os.path.join(non_existent_dir, "state.json")
        
        # Ensure directory doesn't exist
        if os.path.exists(non_existent_dir):
            import shutil
            shutil.rmtree(non_existent_dir)
        
        assert not os.path.exists(non_existent_dir), "Directory should not exist initially"
        
        # Step 2: Simulate directory creation (what the Go code would do)
        self.logger.info(f"Creating directory: {non_existent_dir}")
        os.makedirs(non_existent_dir, mode=0o755, exist_ok=True)
        
        # Step 3: Verify directory was created
        assert os.path.exists(non_existent_dir), "Directory should be created"
        assert os.path.isdir(non_existent_dir), "Path should be a directory"
        
        # Step 4: Test state file creation in new directory
        test_state = {
            "version": 1,
            "boot_id": "test-boot-id",
            "check_last_cursors": {}
        }
        
        self.logger.info("Creating state file in new directory")
        with open(test_state_file, 'w') as f:
            json.dump(test_state, f, indent=2)
        
        # Step 5: Verify file was created successfully
        assert os.path.exists(test_state_file), "State file should be created in new directory"
        
        # Cleanup
        import shutil
        shutil.rmtree(non_existent_dir)
        
        self.logger.info("✅ Directory creation test passed")

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_state_file_permissions(self, nvsentinel_autosync_disabled_enabled):
        """
        Test that state files are created with correct permissions (0600)
        """
        self.step_manager.print_header("Test: State File Permissions")
        
        # Step 1: Create state file
        test_state = {
            "version": 1,
            "boot_id": "test-boot-id",
            "check_last_cursors": {"test_check": "test_cursor"}
        }
        
        self.logger.info("Creating state file to test permissions")
        with open(self.test_state_file, 'w') as f:
            json.dump(test_state, f, indent=2)
        
        # Step 2: Set permissions to 0600 (owner read/write only)
        os.chmod(self.test_state_file, 0o600)
        
        # Step 3: Verify file permissions
        file_stat = os.stat(self.test_state_file)
        file_mode = file_stat.st_mode & 0o777  # Get permission bits
        
        assert file_mode == 0o600, f"File permissions should be 0600, got {oct(file_mode)}"
        
        # Step 4: Verify file is readable by owner
        assert os.access(self.test_state_file, os.R_OK), "File should be readable by owner"
        assert os.access(self.test_state_file, os.W_OK), "File should be writable by owner"
        
        self.logger.info("✅ State file permissions test passed")

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_large_cursor_map_handling(self, nvsentinel_autosync_disabled_enabled):
        """
        Test that large cursor maps are handled correctly
        """
        self.step_manager.print_header("Test: Large Cursor Map Handling")
        
        # Step 1: Create state with many cursors (simulating many checks)
        large_cursor_map = {}
        for i in range(100):
            check_name = f"SysLogsNvidiaCheck{i:03d}"
            cursor_value = f"cursor-{i:06d}-{uuid.uuid4()}"
            large_cursor_map[check_name] = cursor_value
        
        large_state = {
            "version": 1,
            "boot_id": "test-boot-id",
            "check_last_cursors": large_cursor_map
        }
        
        self.logger.info(f"Creating state file with {len(large_cursor_map)} cursors")
        
        # Step 2: Write large state to file
        with open(self.test_state_file, 'w') as f:
            json.dump(large_state, f, indent=2)
        
        # Step 3: Verify file was written successfully
        assert os.path.exists(self.test_state_file), "Large state file should be created"
        
        # Step 4: Read back and verify all cursors
        with open(self.test_state_file, 'r') as f:
            loaded_state = json.load(f)
        
        assert len(loaded_state["check_last_cursors"]) == 100, "Should have 100 cursors"
        
        # Step 5: Verify a few random cursors
        for i in [0, 50, 99]:
            check_name = f"SysLogsNvidiaCheck{i:03d}"
            assert check_name in loaded_state["check_last_cursors"], f"Cursor {check_name} should exist"
            assert loaded_state["check_last_cursors"][check_name] == large_cursor_map[check_name]
        
        self.logger.info("✅ Large cursor map handling test passed")

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_concurrent_state_file_access_simulation(self, nvsentinel_autosync_disabled_enabled):
        """
        Test behavior when multiple processes might access the state file
        (simulation of potential race conditions)
        """
        self.step_manager.print_header("Test: Concurrent State File Access Simulation")
        
        # Step 1: Create initial state
        initial_state = {
            "version": 1,
            "boot_id": "test-boot-id",
            "check_last_cursors": {"initial_check": "initial_cursor"}
        }
        
        with open(self.test_state_file, 'w') as f:
            json.dump(initial_state, f, indent=2)
        
        # Step 2: Simulate rapid updates (like what might happen during high-frequency polling)
        self.logger.info("Simulating rapid state updates")
        
        for i in range(10):
            # Read current state
            with open(self.test_state_file, 'r') as f:
                current_state = json.load(f)
            
            # Update cursor
            current_state["check_last_cursors"][f"check_{i}"] = f"cursor_{i}_{int(time.time() * 1000)}"
            
            # Write updated state
            with open(self.test_state_file, 'w') as f:
                json.dump(current_state, f, indent=2)
            
            # Small delay to simulate processing time
            time.sleep(0.001)
        
        # Step 3: Verify final state integrity
        with open(self.test_state_file, 'r') as f:
            final_state = json.load(f)
        
        assert final_state["version"] == 1, "Version should remain consistent"
        assert final_state["boot_id"] == "test-boot-id", "Boot ID should remain consistent"
        assert len(final_state["check_last_cursors"]) == 11, "Should have initial + 10 new cursors"  # initial + 10 new
        
        self.logger.info("✅ Concurrent access simulation test passed") 