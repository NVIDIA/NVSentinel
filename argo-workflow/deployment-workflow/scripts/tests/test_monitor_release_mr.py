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
import os
import sys
import types
from pathlib import Path
from typing import Any, List

import pytest

SCRIPT_PATH = (
    Path(__file__).resolve().parent / "../monitor-release-mr.py"
).as_posix()

class _TagManager:
    """Simple container to track tag operations"""
    def __init__(self):
        self.created = []  # list of (tag_name, ref)
        self.deleted = []

    def create(self, data):
        tag_name = data["tag_name"]
        # Simulate GitLab behavior - raise error if tag exists
        if any(tag == tag_name for tag, _ in self.created):
            raise _GitlabCreateError()
        self.created.append((data["tag_name"], data["ref"]))

    def delete(self, tag_name):
        self.deleted.append(tag_name)
        # Emulate raising 404 if not exist? not needed.

class _MR:
    def __init__(self, state: str):
        self.state = state
        self.merge_status = "can_be_merged"

class _MergeRequestManager:
    def __init__(self, sequence: List[Any]):
        # sequence can be MR objects or Exceptions
        self._seq = sequence
        self.calls = 0

    def get(self, iid):
        item = self._seq[min(self.calls, len(self._seq) - 1)]
        self.calls += 1
        if isinstance(item, Exception):
            raise item
        return item

class _Project:
    def __init__(self, tags_mgr, mr_mgr):
        self.tags = tags_mgr
        self.mergerequests = mr_mgr

class _Gitlab:
    def __init__(self, url, private_token):
        self._url = url
        self._token = private_token
        self.projects = self
        self._project_stub = None

    def set_project(self, project):
        self._project_stub = project

    def get(self, path):
        return self._project_stub

class _GitlabGetError(Exception):
    def __init__(self, code):
        self.response_code = code

class _GitlabDeleteError(Exception):
    pass

class _GitlabCreateError(Exception):
    pass

def install_gitlab_stub(monkeypatch, project_stub):
    if "gitlab" not in sys.modules:
        sys.modules["gitlab"] = types.ModuleType("gitlab")
    gl_mod = sys.modules["gitlab"]

    # Add exceptions submodule
    exc_mod = types.ModuleType("gitlab.exceptions")
    exc_mod.GitlabGetError = _GitlabGetError
    exc_mod.GitlabDeleteError = _GitlabDeleteError
    exc_mod.GitlabCreateError = _GitlabCreateError
    gl_mod.exceptions = exc_mod

    # Gitlab class factory
    def _factory(url, private_token):
        gl = _Gitlab(url, private_token)
        gl.set_project(project_stub)
        return gl

    monkeypatch.setattr(gl_mod, "Gitlab", _factory, raising=False)

def load_module():
    spec = importlib.util.spec_from_file_location("monitor_release", SCRIPT_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)  # type: ignore[arg-type]
    return mod

@pytest.fixture
def mock_result_file(monkeypatch, tmp_path):
    """Redirect /tmp/monitoring-result.json to a temp file."""
    result_file = tmp_path / "monitoring-result.json"
    builtin_open = open  # Store the real builtin open

    def mock_open(path, mode="r"):
        if path == "/tmp/monitoring-result.json":
            return builtin_open(result_file, mode)
        return builtin_open(path, mode)

    monkeypatch.setattr("builtins.open", mock_open)
    return result_file

@pytest.fixture(autouse=True)
def clear_env(monkeypatch):
    # ensure clean env variables between tests
    for var in [
        "MR_VERSION",
        "MR_DATA",
        "MAX_TIMEOUT_IN_MINUTES",
        "NVSENTINEL_COMPONENT_GITLAB_TOKEN",
        "GITLAB_URL",
    ]:
        monkeypatch.delenv(var, raising=False)

def fast_sleep(monkeypatch):
    import time
    monkeypatch.setattr(time, "sleep", lambda x: None, raising=True)

def read_result(result_file):
    """Read the monitoring result from the temp file."""
    return json.loads(result_file.read_text())

class TestMonitorReleaseMR:
    def _set_basic_env(self, monkeypatch, *, status="created", mr_iid=1):
        monkeypatch.setenv("MR_VERSION", "v1.0.0")
        data = {
            "status": status,
            "mr_iid": mr_iid,
            "mr_url": "http://gitlab/mr/1",
            "branch": "nvsentinel/1.0.0",
        }
        monkeypatch.setenv("MR_DATA", json.dumps(data))
        monkeypatch.setenv("NVSENTINEL_COMPONENT_GITLAB_TOKEN", "tok")
        monkeypatch.setenv("GITLAB_URL", "https://gitlab.example.com")
        monkeypatch.setenv("MAX_CHECKS", "2")
        return data

    # 1. MR state handling: merged should create tag; closed should exit without tag
    @pytest.mark.parametrize("state", ["merged", "closed"])
    def test_mr_state_handling(self, monkeypatch, mock_result_file, state):
        tags_mgr = _TagManager()
        mr_mgr = _MergeRequestManager([_MR(state)])
        proj = _Project(tags_mgr, mr_mgr)
        install_gitlab_stub(monkeypatch, proj)
        fast_sleep(monkeypatch)

        self._set_basic_env(monkeypatch, status="created", mr_iid=1)
        monkeypatch.setenv("MAX_TIMEOUT_IN_MINUTES", "1")

        mod = load_module()
        if state == "merged":
            # Expect normal completion
            mod.main()
            result = read_result(mock_result_file)
            assert result["final_status"] == "completed"
            expected_ref = "main"
            assert ("v1.0.0", expected_ref) in tags_mgr.created
        elif state == "closed":  # state == "closed"
            with pytest.raises(SystemExit):
                mod.main()
            result = read_result(mock_result_file)
            assert result["final_status"] == "failed"
            # Tag should NOT have been created
            assert tags_mgr.created == []
        else:
            raise ValueError(f"Invalid state: {state}")

    # 2. timeout case: MR remains opened
    def test_timeout(self, monkeypatch, mock_result_file):
        tags_mgr = _TagManager()
        mr_mgr = _MergeRequestManager([_MR("opened")])
        proj = _Project(tags_mgr, mr_mgr)
        install_gitlab_stub(monkeypatch, proj)
        fast_sleep(monkeypatch)

        self._set_basic_env(monkeypatch, status="created", mr_iid=2)
        monkeypatch.setenv("MAX_TIMEOUT_IN_MINUTES", "2")

        mod = load_module()
        with pytest.raises(SystemExit):
                mod.main()
        result = read_result(mock_result_file)
        assert result["final_status"] == "failed"
        assert result["message"] == "MR is not merged or closed after 2 minutes"
        assert tags_mgr.created == []

    # 3. Missing credentials env
    def test_missing_credentials(self, monkeypatch, mock_result_file):
        tags_mgr = _TagManager()
        proj = _Project(tags_mgr, _MergeRequestManager([]))
        install_gitlab_stub(monkeypatch, proj)
        self._set_basic_env(monkeypatch, status="created", mr_iid=3)
        # Deliberately not setting token/URL
        monkeypatch.delenv("NVSENTINEL_COMPONENT_GITLAB_TOKEN", raising=False)

        mod = load_module()
        with pytest.raises(SystemExit):
            mod.main()
        res = read_result(mock_result_file)
        assert res["final_status"] == "failed"
        assert "Missing GitLab credentials" in res["message"]

    # 4. GitLab 404 and 500 errors
    @pytest.mark.parametrize("code, expected_status, expected_msg", [(404, "failed", "MR not found"), (500, "failed", "MR is not merged or closed after 2 minutes")])
    def test_gitlab_errors(self, monkeypatch, mock_result_file, code, expected_status, expected_msg):
        # Setup MR manager to raise error then remain opened
        err = _GitlabGetError(code)
        seq = [err, _MR("opened")]
        tags_mgr = _TagManager()
        mr_mgr = _MergeRequestManager(seq)
        proj = _Project(tags_mgr, mr_mgr)
        install_gitlab_stub(monkeypatch, proj)
        fast_sleep(monkeypatch)

        self._set_basic_env(monkeypatch, status="created", mr_iid=4)
        monkeypatch.setenv("MAX_TIMEOUT_IN_MINUTES", "2")
        mod = load_module()
        with pytest.raises(SystemExit):
                mod.main()
        res = read_result(mock_result_file)
        assert res["final_status"] == expected_status
        assert expected_msg in res["message"]

    # 5. No MR raised -> tag created if doesn't exist
    def test_no_mr_tag(self, monkeypatch, mock_result_file):
        tags_mgr = _TagManager()
        proj = _Project(tags_mgr, _MergeRequestManager([]))
        install_gitlab_stub(monkeypatch, proj)
        fast_sleep(monkeypatch)

        # env with mr_iid None
        data = {
            "status": "created",
            "mr_iid": None,
            "mr_url": "",
            "branch": "nvsentinel/1.0.0",
        }
        monkeypatch.setenv("MR_VERSION", "v1.0.0")
        monkeypatch.setenv("MR_DATA", json.dumps(data))
        monkeypatch.setenv("NVSENTINEL_COMPONENT_GITLAB_TOKEN", "tok")
        monkeypatch.setenv("GITLAB_URL", "https://gitlab.example.com")

        mod = load_module()
        mod.main()
        result = read_result(mock_result_file)
        assert result["final_status"] == "completed"
        assert ("v1.0.0", "nvsentinel/1.0.0") in tags_mgr.created

    # 6. Tag already exists -> should not recreate new tag
    def test_existing_tag(self, monkeypatch, mock_result_file):
        """When tag already exists, should not recreate and indicate in message."""
        tags_mgr = _TagManager()
        # Pre-create the tag
        tags_mgr.created.append(("v1.0.0", "some/old/ref"))

        proj = _Project(tags_mgr, _MergeRequestManager([]))
        install_gitlab_stub(monkeypatch, proj)
        fast_sleep(monkeypatch)

        # env with mr_iid None to trigger immediate tag handling
        data = {
            "status": "created",
            "mr_iid": None,
            "mr_url": "",
            "branch": "nvsentinel/1.0.0",
        }
        monkeypatch.setenv("MR_VERSION", "v1.0.0")
        monkeypatch.setenv("MR_DATA", json.dumps(data))
        monkeypatch.setenv("NVSENTINEL_COMPONENT_GITLAB_TOKEN", "tok")
        monkeypatch.setenv("GITLAB_URL", "https://gitlab.example.com")

        mod = load_module()
        with pytest.raises(SystemExit):
            mod.main()
        # Tag deletion should not have been attempted
        assert "v1.0.0" not in tags_mgr.deleted
        # Tag list unchanged (creation failed)
        assert len(tags_mgr.created) == 1
        assert tags_mgr.created[0] == ("v1.0.0", "some/old/ref")