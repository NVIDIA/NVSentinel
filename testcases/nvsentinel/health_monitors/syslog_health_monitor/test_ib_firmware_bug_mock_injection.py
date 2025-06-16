# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

import pytest
from testcases.nvsentinel.health_monitors.syslog_health_monitor.syslog_mock_test_utils import SyslogMockTestBase


class TestIBFirmwareBugMockErrorInjection(SyslogMockTestBase):
    """
    Test class for IB Firmware Bug error injection using logger command
    
    Tests the syslog health monitor's ability to detect IB firmware bug errors:
    - Pattern: 'Skipping wait for vf pages stage'
    - Count: 0 (triggers immediately on first occurrence)
    - Case sensitive matching
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_ib_firmware_bug_mock_error_injection(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Test IB Firmware Bug error injection using logger command.
        
        Test steps:
        1. Create debug pod
        2. Inject error messages using logger command
        3. Wait for monitoring cycle to detect errors
        4. Check if node conditions are updated
        5. Cleanup
        """
        
        # Define the error messages to inject
        # Note: This pattern is case-sensitive
        error_messages = [
            "mlx5_core 0000:3b:00.0: Skipping wait for vf pages stage due to firmware bug",
            "IB driver: Skipping wait for vf pages stage - firmware initialization issue"
        ]
        
        condition_type = "SysLogsIbFirmwareBug"
        
        # Count is 0, so any occurrence triggers the alert
        self.run_logger_error_injection_test(
            error_messages=error_messages,
            condition_type=condition_type,
            expected_reason=condition_type + "IsNotHealthy",
            repeat_count=1
        ) 