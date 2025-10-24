# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

"""Test configuration and fixtures for gpu-health-monitor tests."""

import sys
import os
from unittest.mock import MagicMock
import pytest

# Set environment variable to indicate we're in a test
os.environ["PYTEST_CURRENT_TEST"] = "true"


class MockDCGMModule:
    """Mock DCGM module for testing."""

    # Health watch constants
    DCGM_HEALTH_WATCH_PCIE = 1
    DCGM_HEALTH_WATCH_NVLINK = 2
    DCGM_HEALTH_WATCH_PMU = 3
    DCGM_HEALTH_WATCH_MCU = 4
    DCGM_HEALTH_WATCH_MEM = 5
    DCGM_HEALTH_WATCH_SM = 6
    DCGM_HEALTH_WATCH_INFOROM = 7
    DCGM_HEALTH_WATCH_THERMAL = 8
    DCGM_HEALTH_WATCH_POWER = 9
    DCGM_HEALTH_WATCH_DRIVER = 10
    DCGM_HEALTH_WATCH_NVSWITCH = 11
    DCGM_HEALTH_WATCH_CPUSET = 12
    DCGM_HEALTH_WATCH_ALL = 65535
    DCGM_HEALTH_WATCH_MAX_INCIDENTS = 128

    # Health result constants
    DCGM_HEALTH_RESULT_PASS = 0
    DCGM_HEALTH_RESULT_WARN = 1
    DCGM_HEALTH_RESULT_FAIL = 2

    # Diag result constants
    DCGM_DIAG_RESULT_PASS = 0
    DCGM_DIAG_RESULT_WARN = 1
    DCGM_DIAG_RESULT_FAIL = 2

    # Group constants
    DCGM_GROUP_EMPTY = 0
    DCGM_OPERATION_MODE_AUTO = 0

    # Entity constants
    DCGM_FE_GPU = 0
    DCGM_FE_SWITCH = 1

    # Version constants
    dcgmHealthResponse_version4 = 4

    # Exception classes
    class DCGMError_Timeout(Exception):
        pass

    class IncidentArrayType(type):
        """Metaclass to support array creation with * operator."""

        def __mul__(cls, other):
            if isinstance(other, int):

                class IncidentArray:
                    def __init__(self, size=None):
                        if size is None:
                            size = other
                        self.size = size
                        self._items = [cls() for _ in range(size)]

                    def __call__(self):
                        # When called with (), return the array itself for assignment
                        return self

                    def __getitem__(self, index):
                        # First check if we have explicit storage from assignments
                        if hasattr(self, "_storage") and index in self._storage:
                            return self._storage[index]
                        # Fall back to the default items if available
                        elif hasattr(self, "_items") and index < len(self._items):
                            return self._items[index]
                        else:
                            return None

                    def __setitem__(self, index, value):
                        if not hasattr(self, "_storage"):
                            self._storage = {}
                        self._storage[index] = value

                    def __len__(self):
                        return self.size

                return IncidentArray
            return NotImplemented

    class c_dcgmIncidentInfo_t(metaclass=IncidentArrayType):
        def __init__(self):
            self.system = 0
            self.health = 0
            self.error = MockDCGMModule.c_dcgmDiagErrorDetail_t()
            self.entityInfo = MockDCGMModule.c_dcgmGroupEntityPair_t()

    class c_dcgmDiagErrorDetail_t:
        def __init__(self):
            self.msg = ""
            self.code = 0

        def __setattr__(self, name, value):
            # Make sure assignments work properly
            super().__setattr__(name, value)

    class c_dcgmGroupEntityPair_t:
        def __init__(self):
            self.entityGroupId = 0
            self.entityId = 0

    class HealthResponseMeta(type):
        """Metaclass to make c_dcgmHealthResponse_v4 callable and preserve class attributes."""

        def __call__(cls, *args, **kwargs):
            # Create instance with default values
            instance = super().__call__(*args, **kwargs)
            # Copy any attributes that were set on the class
            if hasattr(cls, "version"):
                instance.version = cls.version
            if hasattr(cls, "overallHealth"):
                instance.overallHealth = cls.overallHealth
            if hasattr(cls, "incidentCount"):
                instance.incidentCount = cls.incidentCount
            if hasattr(cls, "incidents"):
                instance.incidents = cls.incidents
            return instance

    class c_dcgmHealthResponse_v4(metaclass=HealthResponseMeta):
        def __init__(self):
            self.version = MockDCGMModule.dcgmHealthResponse_version4
            self.overallHealth = MockDCGMModule.DCGM_HEALTH_RESULT_PASS
            self.incidentCount = 0
            self.incidents = None


class MockDCGMErrors:
    """Mock DCGM errors module for testing."""

    # Sample error codes used in tests
    DCGM_FR_PCI_REPLAY_RATE = 1
    DCGM_FR_NVLINK_DOWN = 2
    DCGM_FR_THERMAL_VIOLATIONS = 3

    # Add more error codes as needed by tests
    for i in range(4, 113):  # Mock 112 error codes total as expected by tests (3 manual + 109 generated = 112)
        locals()[f"DCGM_FR_ERROR_{i}"] = i


class MockDCGMFields:
    """Mock DCGM fields module for testing."""

    # Entity group constants
    DCGM_FE_GPU = 0
    DCGM_FE_SWITCH = 1

    # Add device field constants as needed
    for i in range(320):  # Mock 320 device fields as expected by tests
        locals()[f"DCGM_FI_DEV_FIELD_{i}"] = i


class MockDCGMValue:
    """Mock DCGM value module for testing."""

    pass


class MockPyDCGM:
    """Mock PyDCGM module for testing."""

    class DcgmHandle:
        def __new__(cls, ipAddress=None, opMode=None):
            instance = super().__new__(cls)
            return instance

        def __init__(self, ipAddress=None, opMode=None):
            self.ipAddress = ipAddress
            self.opMode = opMode

        def GetSystem(self):
            return MagicMock()

    class DcgmGroup:
        def __new__(cls, handle=None, groupName=None, groupType=None):
            instance = super().__new__(cls)
            return instance

        def __init__(self, handle=None, groupName=None, groupType=None):
            self.handle = handle
            self.groupName = groupName
            self.groupType = groupType
            self.health = MagicMock()

        def AddEntity(self, entityType, entityId):
            pass

        def GetGpuIds(self):
            return [0, 1, 2, 3]

        def Delete(self):
            pass


# Install mocks immediately when this module is imported
dcgm_structs_mock = MockDCGMModule()
dcgm_errors_mock = MockDCGMErrors()
dcgm_fields_mock = MockDCGMFields()
dcgm_value_mock = MockDCGMValue()
pydcgm_mock = MockPyDCGM()
dcgm_field_helpers_mock = MagicMock()

sys.modules["dcgm_structs"] = dcgm_structs_mock
sys.modules["dcgm_errors"] = dcgm_errors_mock
sys.modules["dcgm_fields"] = dcgm_fields_mock
sys.modules["dcgmvalue"] = dcgm_value_mock
sys.modules["pydcgm"] = pydcgm_mock
sys.modules["dcgm_field_helpers"] = dcgm_field_helpers_mock


# Mock the DCGM modules before any test imports
@pytest.fixture(scope="session", autouse=True)
def mock_dcgm_modules():
    """Automatically mock DCGM modules for all tests."""
    # Create mock modules
    dcgm_structs_mock = MockDCGMModule()
    dcgm_errors_mock = MockDCGMErrors()
    dcgm_fields_mock = MockDCGMFields()
    dcgm_value_mock = MockDCGMValue()
    pydcgm_mock = MockPyDCGM()
    dcgm_field_helpers_mock = MagicMock()

    # Install mocks in sys.modules
    sys.modules["dcgm_structs"] = dcgm_structs_mock
    sys.modules["dcgm_errors"] = dcgm_errors_mock
    sys.modules["dcgm_fields"] = dcgm_fields_mock
    sys.modules["dcgmvalue"] = dcgm_value_mock
    sys.modules["pydcgm"] = pydcgm_mock
    sys.modules["dcgm_field_helpers"] = dcgm_field_helpers_mock

    yield

    # Clean up (optional, pytest will handle this anyway)
    modules_to_remove = ["dcgm_structs", "dcgm_errors", "dcgm_fields", "dcgmvalue", "pydcgm", "dcgm_field_helpers"]
    for module in modules_to_remove:
        if module in sys.modules:
            del sys.modules[module]
