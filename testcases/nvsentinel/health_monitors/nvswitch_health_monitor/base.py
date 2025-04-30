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
Module for base class of Functional Nvsentinel NVSwitch Health Monitor testing
"""

import pytest
from testcases.nvsentinel.base import TestNVSentinelCaseBase


class NVSwitchHealthMonitorBase(TestNVSentinelCaseBase):
    """
    Base class for test case nvsentinel nvswitch health monitor testing
    """

    daemonset_name = "nvsentinel-nvswitch-health-monitor"

    @pytest.fixture(autouse=True)
    def setup_nvswitch_health_monitor(self, setup_runai_test):
        # Equivalent to setUp in unittest
        self.logger.info("[Setup] NVSwitchHealthMonitorBase")
        yield
        # Equivalent to addCleanup in unittest
        self.client.rollout_daemonset(self.daemonset_name, namespace=self.nv_namespace)
        self.logger.debug(f"POD LOGS: {self.pod_logs}")
