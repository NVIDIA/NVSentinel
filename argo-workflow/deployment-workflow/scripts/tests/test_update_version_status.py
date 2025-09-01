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
import importlib.util
import json
import sys
import types
from pathlib import Path
from typing import Any, Dict, List
from unittest.mock import MagicMock

import pytest

SCRIPT_PATH = (
    Path(__file__).resolve().parent / "../update-version-status.py"
).as_posix()

class ApiException(Exception):
    def __init__(self, status=None, reason=None):
        self.status = status
        self.reason = reason
        super().__init__(f"{status}: {reason}")

class _V1ConfigMap:
    def __init__(self, data: Dict[str, str] = None):
        self.data = data or {}

class _CoreV1Api:
    def __init__(self, config_map: _V1ConfigMap = None, should_raise: bool = False):
        self._config_map = config_map or _V1ConfigMap()
        self._should_raise = should_raise

    def read_namespaced_config_map(self, name, namespace):
        if self._should_raise:
            raise ApiException(status=404, reason="Not found")
        return self._config_map

    def patch_namespaced_config_map(self, name, namespace, body):
        if self._should_raise:
            raise ApiException(status=500, reason="Internal error")
        self._config_map.data = body.get("data", {})

class _CustomObjectsApi:
    def __init__(self, should_raise: bool = False):
        self._should_raise = should_raise
        self.patch_namespaced_custom_object = MagicMock()

    def patch_namespaced_custom_object(self, *args, **kwargs):
        if self._should_raise:
            raise ApiException(status=500, reason="Internal error")

def install_k8s_stubs(monkeypatch, core_v1_api=None, custom_objects_api=None):
    """Install Kubernetes client stubs."""
    # Create mock modules
    k8s = types.ModuleType("kubernetes")
    k8s_client = types.ModuleType("kubernetes.client")
    k8s_config = types.ModuleType("kubernetes.config")
    k8s_rest = types.ModuleType("kubernetes.client.rest")

    # Set up the module hierarchy
    sys.modules["kubernetes"] = k8s
    sys.modules["kubernetes.client"] = k8s_client
    sys.modules["kubernetes.config"] = k8s_config
    sys.modules["kubernetes.client.rest"] = k8s_rest

    # Add our ApiException to kubernetes.client.rest
    k8s_rest.ApiException = ApiException

    # Set up the client classes
    k8s_client.CoreV1Api = lambda: core_v1_api or _CoreV1Api()
    k8s_client.CustomObjectsApi = lambda: custom_objects_api or _CustomObjectsApi()

    # Set up config functions
    k8s_config.load_incluster_config = MagicMock()
    k8s_config.load_kube_config = MagicMock()

SAMPLE_MR_RESULTS = json.dumps([
    {"final_status": "merged"},
    {"final_status": "closed"},
    {"final_status": "failed"},
    {"final_status": "timeout"}
])

# -----------------------------------------------------------------------------
# Fixtures
# -----------------------------------------------------------------------------

@pytest.fixture(autouse=True)
def clear_env(monkeypatch):
    """Clear environment variables before each test."""
    for var in ["VERSION", "MR_RESULTS", "WORKFLOW_NAME"]:
        monkeypatch.delenv(var, raising=False)

@pytest.fixture
def set_env(monkeypatch):
    """Set required environment variables."""
    monkeypatch.setenv("VERSION", "v1.0.0")
    monkeypatch.setenv("MR_RESULTS", SAMPLE_MR_RESULTS)
    monkeypatch.setenv("WORKFLOW_NAME", "test-workflow")

def _load_script():
    """Load the script module."""
    spec = importlib.util.spec_from_file_location("update_version_status", SCRIPT_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

class TestUpdateVersionStatus:
    def test_successful_update(self, monkeypatch, set_env):
        """Test successful update of ConfigMap and workflow labels."""
        # Setup initial ConfigMap data
        initial_cm = _V1ConfigMap({"processed-tags.txt": "v0.9.0:2024-01-01T00:00:00Z:completed"})
        core_v1_api = _CoreV1Api(config_map=initial_cm)
        custom_objects_api = _CustomObjectsApi()
        install_k8s_stubs(monkeypatch, core_v1_api, custom_objects_api)

        # Run script
        module = _load_script()
        module.main()

        # Verify ConfigMap was updated
        assert "v1.0.0:" in initial_cm.data["processed-tags.txt"]
        assert ":completed" in initial_cm.data["processed-tags.txt"]

        # Verify workflow was labeled
        custom_objects_api.patch_namespaced_custom_object.assert_called_once()
        call_args = custom_objects_api.patch_namespaced_custom_object.call_args[1]
        assert call_args["name"] == "test-workflow"
        assert call_args["body"]["metadata"]["labels"]["deployment-version"] == "v1.0.0"

    def test_missing_env_variables(self, monkeypatch):
        """Test handling of missing environment variables."""
        install_k8s_stubs(monkeypatch)

        # Run script
        module = _load_script()

        with pytest.raises(SystemExit) as exc:
            module.main()
        assert exc.value.code == 1

    def test_configmap_error(self, monkeypatch, set_env):
        """Test handling of ConfigMap read/update errors."""
        # Setup APIs that raise errors
        core_v1_api = _CoreV1Api(should_raise=True)
        custom_objects_api = _CustomObjectsApi()
        install_k8s_stubs(monkeypatch, core_v1_api, custom_objects_api)

        # Run script
        module = _load_script()
        module.main()  # Should continue despite ConfigMap error

        # Verify workflow labeling was still attempted
        custom_objects_api.patch_namespaced_custom_object.assert_called_once()

    def test_workflow_label_error(self, monkeypatch, set_env):
        """Test handling of workflow labeling errors."""
        # Setup APIs
        core_v1_api = _CoreV1Api()
        custom_objects_api = _CustomObjectsApi(should_raise=True)
        install_k8s_stubs(monkeypatch, core_v1_api, custom_objects_api)

        # Run script
        module = _load_script()
        module.main()  # Should continue despite labeling error

    def test_mr_results_summary(self, monkeypatch, set_env):
        """Test MR results summary calculation."""
        install_k8s_stubs(monkeypatch)

        # Run script
        module = _load_script()

        # Test summarize_mr_results directly
        summary = module.summarize_mr_results(SAMPLE_MR_RESULTS)
        assert summary.merged == 1
        assert summary.closed == 1
        assert summary.failed == 1
        assert summary.timeout == 1
        assert summary.total == 4

    def test_invalid_mr_results_json(self, monkeypatch):
        """Test handling of invalid MR results JSON."""
        monkeypatch.setenv("VERSION", "v1.0.0")
        monkeypatch.setenv("MR_RESULTS", "invalid json")
        monkeypatch.setenv("WORKFLOW_NAME", "test-workflow")
        install_k8s_stubs(monkeypatch)

        # Run script
        module = _load_script()

        # Test summarize_mr_results directly
        summary = module.summarize_mr_results("invalid json")
        assert summary.total == 0  # Should handle invalid JSON gracefully 