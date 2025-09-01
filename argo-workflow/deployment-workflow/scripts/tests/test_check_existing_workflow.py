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
import os
import sys
import types

import pytest

SCRIPT_PATH = os.path.join(os.path.dirname(__file__), "../check-existing-workflow.py")

class _StubConfig:
    class ConfigException(Exception):
        pass

    @staticmethod
    def load_incluster_config():
        # Simulate in-cluster failure so that kubeconfig path is tried.
        raise _StubConfig.ConfigException("not in cluster")

    @staticmethod
    def load_kube_config():
        # No-op for tests
        return None


class _StubApiException(Exception):
    def __init__(self, status=500, *args, **kwargs):
        super().__init__(*args)
        self.status = status


class _BaseCoreV1Api:
    """Base stub used by concrete test cases via subclassing."""

    def __init__(self):
        # storage used by tests to introspect what was written
        self.patched_data = None
        self.created_config_map = None

    def read_namespaced_config_map(self, *args, **kwargs):  # pragma: no cover
        raise NotImplementedError

    def patch_namespaced_config_map(self, *args, **kwargs):  # pragma: no cover
        raise NotImplementedError

    def create_namespaced_config_map(self, *args, **kwargs):  # pragma: no cover
        raise NotImplementedError


def _install_k8s_stubs(monkeypatch, core_v1_api_cls):
    """Inject a fake `kubernetes` module graph into sys.modules."""

    # Root package
    k8s_mod = types.ModuleType("kubernetes")

    # kubernetes.config
    k8s_config_mod = types.ModuleType("kubernetes.config")
    k8s_config_mod.ConfigException = _StubConfig.ConfigException
    k8s_config_mod.load_incluster_config = staticmethod(_StubConfig.load_incluster_config)
    k8s_config_mod.load_kube_config = staticmethod(_StubConfig.load_kube_config)

    # kubernetes.client
    k8s_client_mod = types.ModuleType("kubernetes.client")
    k8s_client_mod.CoreV1Api = core_v1_api_cls
    # minimal stubs for data classes used in script
    class _V1ObjectMeta:
        def __init__(self, name: str):
            self.name = name

    class _V1ConfigMap:
        def __init__(self, metadata=None, data=None):
            self.metadata = metadata
            self.data = data

    k8s_client_mod.V1ObjectMeta = _V1ObjectMeta  # type: ignore
    k8s_client_mod.V1ConfigMap = _V1ConfigMap  # type: ignore

    # kubernetes.client.rest
    k8s_rest_mod = types.ModuleType("kubernetes.client.rest")
    k8s_rest_mod.ApiException = _StubApiException

    # Wire sub-modules
    k8s_mod.config = k8s_config_mod
    k8s_mod.client = k8s_client_mod

    sys.modules["kubernetes"] = k8s_mod
    sys.modules["kubernetes.config"] = k8s_config_mod
    sys.modules["kubernetes.client"] = k8s_client_mod
    sys.modules["kubernetes.client.rest"] = k8s_rest_mod


def _load_script_module():
    spec = importlib.util.spec_from_file_location("check_existing_workflow", SCRIPT_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)  # type: ignore[arg-type]
    return module


def test_version_not_processed(monkeypatch, tmp_path):
    """Happy path: version entry not present -> marks in-progress and continues."""

    version = "v1.0.0"
    monkeypatch.setenv("VERSION", version)

    class CoreV1Api(_BaseCoreV1Api):
        processed = "old:entry:completed\n"

        def read_namespaced_config_map(self, name, namespace):
            class _CM:  # minimal object matching attributes used in script
                def __init__(self, data):
                    self.data = data
            return _CM({"processed-tags.txt": self.processed})

        def patch_namespaced_config_map(self, *, body, **_):
            # Capture patched content for assertions
            self.patched_data = body
            return None

        # Should not be called in this scenario
        def create_namespaced_config_map(self, *_, **__):
            raise AssertionError("create_namespaced_config_map should not be called")

    _install_k8s_stubs(monkeypatch, CoreV1Api)

    module = _load_script_module()
    module.main()

    # Assert file created with 'true'
    with open("/tmp/should-proceed.txt", "r") as f:
        assert f.read() == "true"


def test_version_already_completed(monkeypatch):
    """If version entry is marked completed, script should abort with exit=1."""

    version = "v2.0.0"
    monkeypatch.setenv("VERSION", version)

    cm_content = f"{version}:2024-01-01T00:00:00Z:completed\n"

    class CoreV1Api(_BaseCoreV1Api):
        def read_namespaced_config_map(self, name, namespace):
            class _CM:
                def __init__(self, data):
                    self.data = data
            return _CM({"processed-tags.txt": cm_content})

    _install_k8s_stubs(monkeypatch, CoreV1Api)

    module = _load_script_module()

    with pytest.raises(SystemExit) as exc:
        module.main()
    assert exc.value.code == 1

    # /tmp/should-proceed.txt should contain "false"
    with open("/tmp/should-proceed.txt", "r") as f:
        assert f.read() == "false"


def test_update_configmap_failure(monkeypatch):
    """If patch/create of ConfigMap raises ApiException, script exits with error."""

    version = "v3.0.0"
    monkeypatch.setenv("VERSION", version)

    class CoreV1Api(_BaseCoreV1Api):
        def read_namespaced_config_map(self, name, namespace):
            # Simulate non-existent ConfigMap (404)
            raise _StubApiException(status=404)

        def create_namespaced_config_map(self, *_, **__):
            # Simulate failure when trying to create the CM
            raise _StubApiException(status=500, args=("create failed",))

    _install_k8s_stubs(monkeypatch, CoreV1Api)

    module = _load_script_module()

    with pytest.raises(SystemExit) as exc:
        module.main()
    assert exc.value.code == 1 