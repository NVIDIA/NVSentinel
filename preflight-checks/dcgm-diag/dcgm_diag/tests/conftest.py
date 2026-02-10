# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

"""Test configuration and fixtures for dcgm-diag tests."""

import os
import sys
from unittest.mock import MagicMock

import pytest

# ============================================================================
# Mock DCGM Modules (must be installed before any dcgm imports)
# ============================================================================


class MockDCGMStructs:
    """Mock dcgm_structs module for testing."""

    # Diagnostic level constants
    DCGM_DIAG_LVL_SHORT = 1
    DCGM_DIAG_LVL_MED = 2
    DCGM_DIAG_LVL_LONG = 3
    DCGM_DIAG_LVL_XLONG = 4

    # Diagnostic result constants
    DCGM_DIAG_RESULT_PASS = 0
    DCGM_DIAG_RESULT_SKIP = 1
    DCGM_DIAG_RESULT_WARN = 2
    DCGM_DIAG_RESULT_FAIL = 3
    DCGM_DIAG_RESULT_NOT_RUN = 4

    # Group constants
    DCGM_GROUP_EMPTY = 0
    DCGM_OPERATION_MODE_AUTO = 0

    class c_dcgmDiagResponse_v12:
        """Mock diagnostic response structure.

        Note: Initialized empty - tests must populate tests/results/errors
        arrays along with their counts for meaningful output.
        """

        def __init__(self) -> None:
            self.numTests = 0
            self.numResults = 0
            self.numErrors = 0
            self.tests: list = []
            self.results: list = []
            self.errors: list = []


class MockDCGMTestRun:
    """Mock c_dcgmDiagTestRun_v2 structure."""

    def __init__(self, name: str, num_results: int, result_indices: list[int]) -> None:
        self.name = name.encode() if isinstance(name, str) else name
        self.numResults = num_results
        self.resultIndices = result_indices


class MockDCGMEntityResult:
    """Mock c_dcgmDiagEntityResult_v1 structure."""

    def __init__(self, entity_id: int, result: int, test_id: int) -> None:
        self.entity = MagicMock()
        self.entity.entityId = entity_id
        self.result = result
        self.testId = test_id


class MockPyNVML:
    """Mock pynvml module for testing."""

    class NVMLError(Exception):
        pass

    @staticmethod
    def nvmlInit() -> None:
        pass

    @staticmethod
    def nvmlShutdown() -> None:
        pass

    @staticmethod
    def nvmlDeviceGetCount() -> int:
        return 2

    @staticmethod
    def nvmlDeviceGetHandleByIndex(index: int) -> MagicMock:
        return MagicMock()

    @staticmethod
    def nvmlDeviceGetUUID(handle: MagicMock) -> str:
        return f"GPU-test-uuid-{id(handle) % 100}"


class MockPyDCGM:
    """Mock pydcgm module for testing."""

    class DcgmHandle:
        def __init__(self, ipAddress: str | None = None, opMode: int | None = None) -> None:
            self.ipAddress = ipAddress
            self.opMode = opMode

        def Shutdown(self) -> None:
            pass

    class DcgmGroup:
        def __init__(
            self, handle: "MockPyDCGM.DcgmHandle | None" = None, groupName: str = "", groupType: int = 0
        ) -> None:
            self.handle = handle
            self.groupName = groupName
            self.groupType = groupType
            self.gpus: list[int] = []
            self.action = MagicMock()

        def AddGpu(self, gpu_idx: int) -> None:
            self.gpus.append(gpu_idx)

        def Delete(self) -> None:
            pass


# Install mocks in sys.modules before any imports
dcgm_structs_mock = MockDCGMStructs()
pydcgm_mock = MockPyDCGM()
pynvml_mock = MockPyNVML()

sys.modules["dcgm_structs"] = dcgm_structs_mock
sys.modules["pydcgm"] = pydcgm_mock
sys.modules["pynvml"] = pynvml_mock


# ============================================================================
# Pytest Fixtures
# ============================================================================


@pytest.fixture
def clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """Remove all DCGM-related env vars."""
    for key in list(os.environ.keys()):
        if key.startswith(("DCGM_", "PLATFORM_", "NODE_", "PROCESSING_")):
            monkeypatch.delenv(key, raising=False)


@pytest.fixture
def valid_env(monkeypatch: pytest.MonkeyPatch, clean_env: None) -> None:
    """Set minimum valid environment."""
    monkeypatch.setenv("DCGM_HOSTENGINE_ADDR", "nvidia-dcgm.gpu-operator.svc:5555")
    monkeypatch.setenv("PLATFORM_CONNECTOR_SOCKET", "/var/run/nvsentinel.sock")
    monkeypatch.setenv("NODE_NAME", "test-node")
