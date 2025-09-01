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
import json
from pathlib import Path

import pytest


SCRIPT_PATH = (
    Path(__file__).resolve().parent / "../run-autotest.py"
).as_posix()


def _load_module():
    import importlib.util as iu
    spec = iu.spec_from_file_location("run_autotest", SCRIPT_PATH)
    mod = iu.module_from_spec(spec)
    spec.loader.exec_module(mod)  # type: ignore[arg-type]
    return mod


@pytest.fixture(autouse=True)
def _clear_env(monkeypatch):
    for v in ["GITLAB_TOKEN", "GITLAB_URL", "CLUSTER_DATA"]:
        monkeypatch.delenv(v, raising=False)
    # Ensure gitlab stub exists with Gitlab attribute to satisfy type hints during import
    import types, sys as _s
    if "gitlab" not in _s.modules:
        _s.modules["gitlab"] = types.ModuleType("gitlab")
    _s.modules["gitlab"].Gitlab = lambda *a, **kw: None


# ---------------------- helpers -------------------------------------------


class _DummyGL:
    projects = None


def _setup_stubs(monkeypatch, *, pipeline_status="success"):
    """Patch helper functions inside script to fast-forward execution."""
    mod = _load_module()

    # get_gitlab_client returns dummy object
    monkeypatch.setattr(mod, "get_gitlab_client", lambda url, token: _DummyGL())
    monkeypatch.setattr(mod, "get_project_id", lambda gl, path: "123")

    # run_pipeline_for_cluster returns predefined result
    def _rpc(cluster_path, gl, pid):
        return {
            "cluster_path": cluster_path,
            "pipeline_id": 1,
            "pipeline_url": "http://pipe",
            "status": pipeline_status,
        }

    monkeypatch.setattr(mod, "run_pipeline_for_cluster", _rpc)

    # Skip long sleeps
    import time
    monkeypatch.setattr(time, "sleep", lambda x: None, raising=True)

    return mod


@pytest.fixture
def _redirect_results(monkeypatch, tmp_path):
    out = tmp_path / "auto.json"
    orig_open = open

    def _mo(path, mode="r", *a, **kw):
        if path == "/tmp/autotest-result.json":
            return orig_open(out, mode, *a, **kw)
        return orig_open(path, mode, *a, **kw)

    monkeypatch.setattr("builtins.open", _mo)
    return out


# ---------------------- tests ---------------------------------------------


def test_env_missing(monkeypatch):
    mod = _load_module()
    with pytest.raises(SystemExit):
        mod.main()


def test_spec_path_missing(monkeypatch):
    mod = _setup_stubs(monkeypatch)
    # Cluster entry without spec-path
    data = [{"foo": "bar"}]
    monkeypatch.setenv("GITLAB_TOKEN", "tok")
    monkeypatch.setenv("CLUSTER_DATA", json.dumps(data))
    with pytest.raises(SystemExit):  # exits due to no valid cluster paths
        mod.main()


@pytest.mark.parametrize("p_status, expect_exit", [("failed", 1), ("canceled", 0)])
def test_pipeline_failed_or_canceled(monkeypatch, _redirect_results, p_status, expect_exit):
    mod = _setup_stubs(monkeypatch, pipeline_status=p_status)
    data = [{"spec-path": "clusters/foo/cluster-spec.yaml"}]
    monkeypatch.setenv("GITLAB_TOKEN", "tok")
    monkeypatch.setenv("CLUSTER_DATA", json.dumps(data))

    with pytest.raises(SystemExit) as exc:
        mod.main()
    assert exc.value.code == expect_exit


def test_pipeline_success(monkeypatch, _redirect_results):
    mod = _setup_stubs(monkeypatch, pipeline_status="success")
    data = [{"spec-path": "clusters/foo/cluster-spec.yaml"}]
    monkeypatch.setenv("GITLAB_TOKEN", "tok")
    monkeypatch.setenv("CLUSTER_DATA", json.dumps(data))

    with pytest.raises(SystemExit) as exc:
        mod.main()
    assert exc.value.code == 0
    # Verify result file contains success entry
    res = json.loads(_redirect_results.read_text())
    assert res[0]["status"] == "success"
