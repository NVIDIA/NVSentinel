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
import os
import sys
import types
from pathlib import Path

import pytest


SCRIPT_PATH = (
    Path(__file__).resolve().parent / "../monitor-mr.py"
).as_posix()


def _load_module():
    import importlib.util as iu
    spec = iu.spec_from_file_location("monitor_mr", SCRIPT_PATH)
    mod = iu.module_from_spec(spec)
    spec.loader.exec_module(mod)  # type: ignore[arg-type]
    return mod


# ---------------------------- helpers --------------------------------------


class _StubResp:
    def __init__(self, status_code: int, payload=None):
        self.status_code = status_code
        self._payload = payload or {}

    def json(self):
        return self._payload


def _install_get_stub(monkeypatch, responses):
    """Patch requests.get to sequentially return *responses* list"""
    if "requests" not in sys.modules:
        monkeypatch.setitem(sys.modules, "requests", types.ModuleType("requests"))
    req = sys.modules["requests"]
    counter = {"i": 0}

    def _get(url, headers=None, timeout=None):
        i = counter["i"]
        counter["i"] += 1
        return responses[i] if i < len(responses) else responses[-1]

    monkeypatch.setattr(req, "get", _get, raising=False)


@pytest.fixture(autouse=True)
def _clear_env(monkeypatch):
    for v in [
        "MR_VERSION",
        "MR_DATA",
        "PROJECT_PATH",
        "GITLAB_TOKEN",
        "GITLAB_URL",
        "MAX_TIMEOUT_IN_MINUTES",
    ]:
        monkeypatch.delenv(v, raising=False)

@pytest.fixture
def _redirect_result(monkeypatch, tmp_path):
    res = tmp_path / "result.json"
    orig_open = open

    def _mo(path, mode="r", *a, **kw):
        if path == "/tmp/monitoring-result.json":
            return orig_open(res, mode, *a, **kw)
        return orig_open(path, mode, *a, **kw)

    monkeypatch.setattr("builtins.open", _mo)
    return res


def _basic_env(monkeypatch, status="created", mr_iid=1, mr_url="http://mr/1"):
    monkeypatch.setenv("MR_VERSION", "v1.0.0")
    data = {"status": status, "mr_iid": mr_iid, "mr_url": mr_url}
    monkeypatch.setenv("MR_DATA", json.dumps(data))
    monkeypatch.setenv("PROJECT_PATH", "proj")
    monkeypatch.setenv("GITLAB_TOKEN", "tok")
    monkeypatch.setenv("GITLAB_URL", "https://gitlab.example.com")
    # Replace sys.exit with no-op for positive/controlled flows
    monkeypatch.setattr(sys, "exit", lambda code=0: None)


# ---------------------------- tests ----------------------------------------


def test_missing_env(monkeypatch):
    mod = _load_module()
    with pytest.raises(SystemExit):
        mod.main()


def test_no_mr_id(monkeypatch, _redirect_result):
    _basic_env(monkeypatch, status="failed", mr_iid=None, mr_url="")
    mod = _load_module()
    mod.main()
    res = json.loads(_redirect_result.read_text())
    assert res["final_status"] == "failed"
    assert "no MR ID" in res["message"]


def test_status_failed(monkeypatch, _redirect_result):
    _basic_env(monkeypatch, status="failed")
    mod = _load_module()
    mod.main()
    res = json.loads(_redirect_result.read_text())
    assert res["final_status"] == "failed"


@pytest.mark.parametrize("state", ["merged", "closed"])
def test_mr_completed(monkeypatch, _redirect_result, state):
    _basic_env(monkeypatch)
    _install_get_stub(monkeypatch, [_StubResp(200, {"state": state, "merge_status": "can_be_merged"})])
    monkeypatch.setenv("MAX_TIMEOUT_IN_MINUTES", "1")
    # speed sleep
    import time
    monkeypatch.setattr(time, "sleep", lambda x: None, raising=True)
    mod = _load_module()
    mod.main()
    res = json.loads(_redirect_result.read_text())
    assert res["final_status"] == state


def test_mr_not_found(monkeypatch, _redirect_result):
    _basic_env(monkeypatch)
    _install_get_stub(monkeypatch, [_StubResp(404)])
    monkeypatch.setenv("MAX_TIMEOUT_IN_MINUTES", "1")
    import time
    monkeypatch.setattr(time, "sleep", lambda x: None, raising=True)
    mod = _load_module()
    mod.main()
    res = json.loads(_redirect_result.read_text())
    assert res["final_status"] == "failed"
    assert "not found" in res["message"].lower()


def test_timeout(monkeypatch, _redirect_result):
    _basic_env(monkeypatch)
    _install_get_stub(monkeypatch, [_StubResp(200, {"state": "opened"})])
    monkeypatch.setenv("MAX_TIMEOUT_IN_MINUTES", "1")
    import time
    monkeypatch.setattr(time, "sleep", lambda x: None, raising=True)
    mod = _load_module()
    mod.main()
    res = json.loads(_redirect_result.read_text())
    assert res["final_status"] == "failed"
    assert res["message"] == "MR is not merged or closed after 1 minutes"
