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


class TestXID119MockErrorInjection(SyslogMockTestBase):
    """
    Test class for XID119 error injection using logger command
    
    Tests the syslog health monitor's ability to detect XID119 errors:
    - Pattern: 'Xid .* waiting for RPC response from GPU'
    - Count: 0 (triggers immediately on first occurrence)
    - Case insensitive matching
    """

    @pytest.mark.author(email="ajmishra@nvidia.com")
    @pytest.mark.sysloghealthmonitor
    def test_xid119_mock_error_injection(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Test XID119 error injection using logger command.
        
        Test steps:
        1. Create debug pod
        2. Inject error messages using logger command
        3. Wait for monitoring cycle to detect errors
        4. Check if node conditions are updated
        5. Cleanup
        """
        
        # Define the error messages to inject
        # Note: This should match case-insensitively (ignoreCase: true)
        error_messages = [
            "[6085126.134786] NVRM: Xid (PCI:0002:00:00): 119, pid=1582259, name=nvc:[driver], Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8)."
        ]
        
        condition_type = "SysLogsXidError"
        
        # Count is 0, so any occurrence triggers the alert
        self.run_logger_error_injection_test(
            error_messages=error_messages,
            condition_type=condition_type,
            expected_reason=condition_type + "IsNotHealthy",
            repeat_count=1
        ) 