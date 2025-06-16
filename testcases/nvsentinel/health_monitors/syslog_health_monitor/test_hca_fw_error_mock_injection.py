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


class TestHCAFwErrorMockErrorInjection(SyslogMockTestBase):
    """
    Test class for HCA Firmware Error injection using logger command
    
    Tests the syslog health monitor's ability to detect HCA firmware errors:
    - Pattern: 'Health issue observed, firmware internal error'
    - Count: 0 (triggers immediately on first occurrence)
    - Case insensitive matching
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_hca_fw_error_mock_error_injection(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Test HCA Firmware Error injection using logger command.
        
        Test steps:
        1. Create debug pod
        2. Inject error messages using logger command
        3. Wait for monitoring cycle to detect errors
        4. Check if node conditions are updated
        5. Cleanup
        """
        
        # Define the error messages to inject
        error_messages = [
            "mlx5_core 0000:17:00.0: Health issue observed, firmware internal error detected on device",
            "HCA firmware encountered critical error: Health issue observed, firmware internal error during operation"
        ]
        
        condition_type = "SysLogshcaFwError"
        
        # Count is 0, so any occurrence triggers the alert
        self.run_logger_error_injection_test(
            error_messages=error_messages,
            condition_type=condition_type,
            expected_reason=condition_type + "IsNotHealthy",
            repeat_count=1
        ) 