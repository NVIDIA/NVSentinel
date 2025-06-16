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


class TestMCEErrorsMockErrorInjection(SyslogMockTestBase):
    """
    Test class for MCE Errors injection using logger command
    
    Tests the syslog health monitor's ability to detect Machine Check Exception errors:
    - Pattern: 'Machine check events logged'
    - Count: 20 (requires 20 occurrences to trigger)
    - Case insensitive matching
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_mce_errors_mock_error_injection(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Test MCE Errors injection using logger command.
        
        Test steps:
        1. Create debug pod
        2. Inject error messages using logger command
        3. Wait for monitoring cycle to detect errors
        4. Check if node conditions are updated
        5. Cleanup
        """
        
        # Define the error messages to inject
        error_messages = [
            "kernel: mce: Machine check events logged - CPU 0 Bank 0 Error 0x0001",
            "kernel: mce: Machine check events logged - CPU 1 Bank 1 Error 0x0002", 
            "kernel: mce: Machine check events logged - CPU 2 Bank 2 Error 0x0003",
            "kernel: mce: Machine check events logged - CPU 3 Bank 3 Error 0x0004",
            "kernel: mce: Machine check events logged - CPU 4 Bank 0 Error 0x0005",
            "kernel: mce: Machine check events logged - CPU 5 Bank 1 Error 0x0006",
            "kernel: mce: Machine check events logged - CPU 6 Bank 2 Error 0x0007",
            "kernel: mce: Machine check events logged - CPU 7 Bank 3 Error 0x0008"
        ]
        
        condition_type = "SysLogsMceErrors"
        
        # We need at least 20 occurrences to trigger the alert (count: 20)
        # Repeat each message 3 times: 8 messages × 3 = 24 total occurrences
        self.run_logger_error_injection_test(
            error_messages=error_messages,
            condition_type=condition_type,
            expected_reason=condition_type + "IsNotHealthy",
            repeat_count=3  # 8 messages × 3 = 24 total occurrences
        ) 