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


class TestBMCHealthMockErrorInjection(SyslogMockTestBase):
    """
    Test class for BMC Health error injection using logger command
    
    Tests the syslog health monitor's ability to detect BMC health errors:
    - Pattern: 'BMC returned incorrect response'
    - Count: 4 (requires 4 occurrences to trigger)
    - Case insensitive matching
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_bmc_health_mock_error_injection(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Test BMC Health error injection using logger command.
        
        Test steps:
        1. Create debug pod
        2. Inject error messages using logger command
        3. Wait for monitoring cycle to detect errors
        4. Check if node conditions are updated
        5. Cleanup
        """
        
        # Define the error messages to inject
        error_messages = [
            "BMC returned incorrect response for sensor reading",
            "BMC returned incorrect response during temperature check",
            "BMC returned incorrect response for fan speed",
            "BMC returned incorrect response on power status",
            "BMC returned incorrect response for voltage monitoring"
        ]
        
        condition_type = "SysLogsBMCHealth"
        
        # We need at least 4 occurrences to trigger the alert (count: 4)
        # Inject 5 messages to ensure we exceed the threshold
        self.run_logger_error_injection_test(
            error_messages=error_messages,
            condition_type=condition_type,
            expected_reason="SysLogsBMCHealthIsNotHealthy",
            repeat_count=1  # Each message appears once = 5 total occurrences
        ) 