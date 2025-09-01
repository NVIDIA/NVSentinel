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
from typing import Any, List

import pytest

SCRIPT_PATH = (
    Path(__file__).resolve().parent / "../monitor-cluster-mr.py"
).as_posix()

# Helper to import the script as module
def _load_module():
    spec = importlib.util.spec_from_file_location("monitor_mrs", SCRIPT_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)  # type: ignore[arg-type]
    return mod

# Response stub
class _StubResp:
    def __init__(self, status_code: int, payload: Any = None):
        self.status_code = status_code
        self._payload = payload or {}

    def json(self):
        return self._payload

# requests.get stub installer
def install_get_stub(monkeypatch, responses: List[_StubResp]):
    """Patch requests.get so that sequential calls yield items in `responses`."""
    counter = {"i": 0}

    def _get(url: str, headers=None, timeout=None):
        idx = counter["i"]
        if idx < len(responses):
            resp = responses[idx]
        else:
            resp = responses[-1]
        counter["i"] += 1
        return resp

    if "requests" not in sys.modules:
        monkeypatch.setitem(sys.modules, "requests", types.ModuleType("requests"))
    req_mod = sys.modules["requests"]
    monkeypatch.setattr(req_mod, "get", _get, raising=False)

# Patch time.sleep to avoid delays
def fast_sleep(monkeypatch):
    import time
    monkeypatch.setattr(time, "sleep", lambda x: None, raising=True)

# Fixtures
@pytest.fixture(autouse=True)
def clear_env(monkeypatch):
    # ensure clean env variables between tests
    for var in [
        "MR_VERSION",
        "MR_DATA",
        "MANIFEST_GITLAB_TOKEN",
        "GITLAB_URL",
        "MAX_CHECKS",
        "MAX_TIMEOUT_IN_MINUTES",
    ]:
        monkeypatch.delenv(var, raising=False)

class TestMonitorMR:
    def _basic_env(self, monkeypatch):
        monkeypatch.setenv("MR_VERSION", "v1.0.0")
        # minimal MR data template
        data = {
            "pattern_name": "gpu-basic",
            "clusters": {"cluster-a":"spec/cluster-a/cluster-spec.yaml"},
            "status": "created",
            "mr_iid": 123,
            "mr_url": "http://gitlab/mr/123",
        }
        monkeypatch.setenv("MR_DATA", json.dumps(data))
        monkeypatch.setenv("MANIFEST_GITLAB_TOKEN", "tok")
        monkeypatch.setenv("GITLAB_URL", "https://gitlab.example.com")
        # newer script expects MAX_TIMEOUT_IN_MINUTES
        monkeypatch.setenv("MAX_TIMEOUT_IN_MINUTES", "2")
        return data

    # 1. Positive merged case
    def test_positive_merged(self, monkeypatch):
        mod = _load_module()
        self._basic_env(monkeypatch)

        # first poll returns merged
        install_get_stub(
            monkeypatch,
            [_StubResp(200, {"state": "merged", "merge_status": "can_be_merged"})],
        )
        fast_sleep(monkeypatch)

        # should complete without SystemExit
        mod.main()
        res = json.loads(Path("/tmp/monitoring-result.json").read_text())
        assert res["final_status"] == "success"
        assert res["mr_state"] == "merged"
        assert res["message"] == "MR is merged"

    # 2. Timeout path
    def test_timeout(self, monkeypatch):
        mod = _load_module()
        self._basic_env(monkeypatch)
        # always open
        install_get_stub(monkeypatch, [_StubResp(200, {"state": "opened", "merge_status": "checking"})])
        fast_sleep(monkeypatch)

        with pytest.raises(SystemExit):
            mod.main()
        res = json.loads(Path("/tmp/monitoring-result.json").read_text())
        assert res["final_status"] == "timeout"

    # 3. MR not found
    def test_mr_not_found(self, monkeypatch):
        mod = _load_module()
        self._basic_env(monkeypatch)
        install_get_stub(monkeypatch, [_StubResp(404)])
        fast_sleep(monkeypatch)

        with pytest.raises(SystemExit):
            mod.main()
        res = json.loads(Path("/tmp/monitoring-result.json").read_text())
        assert res["final_status"] == "failed"
        assert res["message"] == "MR not found"

    # 4. Credentials missing
    def test_missing_credentials(self, monkeypatch):
        mod = _load_module()
        data = {
            "pattern_name": "gpu-basic",
            "clusters": {},
            "status": "created",
            "mr_iid": 124,
            "mr_url": "http://gitlab/mr/124",
        }
        monkeypatch.setenv("MR_VERSION", "v1.1.0")
        monkeypatch.setenv("MR_DATA", json.dumps(data))
        
        with pytest.raises(SystemExit):
            mod.main()
        res = json.loads(Path("/tmp/monitoring-result.json").read_text())
        assert res["final_status"] == "failed"
        assert res["message"] == "Missing GitLab credentials"

    # 5. MR creation already failed – should skip polling
    def test_skip_failed_mr(self, monkeypatch):
        mod = _load_module()
        monkeypatch.setenv("MR_VERSION", "v1.2.0")
        data = {
            "pattern_name": "gpu-basic",
            "clusters": {},
            "status": "failed",
            "mr_iid": None,
            "mr_url": "",
        }
        monkeypatch.setenv("MR_DATA", json.dumps(data))

        with pytest.raises(SystemExit):
            mod.main()
        res = json.loads(Path("/tmp/monitoring-result.json").read_text())
        assert res["final_status"] == "failed"
        assert res["message"] == "MR creation failed or no MR ID" 