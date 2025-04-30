# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import pytest
from testcases.common.logger import Logger


class Base:
    """Base class providing common utilities and step management for CLI tests"""

    @pytest.fixture(autouse=True)
    def case_setup(self, request):
        if (
            request.node.get_closest_marker("v1")
            and not request.config.getoption("testcase_version") == "DGXC_Sprint_1.1"
        ):
            pytest.skip("v1 case is not supported in this version")

        if (
            request.node.get_closest_marker("v2")
            and not request.config.getoption("testcase_version") == "DGXC_Sprint_1.2"
        ):
            pytest.skip("v2 case is not supported in this version")

        self.logger = Logger(self.__class__.__name__).get_logger()
        self.step_manager = self.logger

        self.project = "NVSentinel"
        self.default_namespace = "runai-" + self.project
        self.csp = "TestCSP"
        self.cluster = "TestCluster"

    @pytest.fixture(autouse=True)
    def setup_log_directory(self, request):
        """Create a log directory for each test"""
        pass

    @pytest.fixture(autouse=True)
    def print_test_name(self, request):
        """Print test name and template ID at the start of each test"""
        self.logger.info(
            f" ==== Running test: {request.node.name} ==== "
        )
