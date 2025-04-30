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
Module for base class of Functional Nvsentinel GPU Health Monitor testing
"""

import pytest
from testcases.nvsentinel.base import TestNVSentinelCaseBase

class GPUHealthMonitorBase(TestNVSentinelCaseBase):
    """
    Base class for test case nvsentinel gpu health monitor testing
    """

    @pytest.fixture(autouse=True)
    def setup_gpu_health_monitor(self, setup_runai_test):
        # Equivalent to setUp in unittest
        self.logger.info("[Setup] GPUHealthMonitorBase")
        self.daemonset_name = self.get_available_gpu_monitor_daemonset()
        yield
        # Equivalent to addCleanup in unittest
        self.client.rollout_daemonset(self.daemonset_name, namespace=self.nv_namespace)
