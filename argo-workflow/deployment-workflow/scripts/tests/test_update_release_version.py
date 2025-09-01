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
import base64
import importlib.util
import json
import os
import sys
import types
from pathlib import Path
from typing import Any, Dict, List

import pytest

SCRIPT_PATH = (
    Path(__file__).resolve().parent / "../update-release-version.py"
).as_posix()

class _File:
    def __init__(self, content: str):
        self._content = content
        self._encoded = base64.b64encode(content.encode()).decode()

    # Allow reading encoded content (getter)
    @property
    def content(self):
        """Return base64 encoded content as expected by the script."""
        return self._encoded

    @content.setter
    def content(self, new_plain: str):
        """Accept plain text, store encoded form so that script's .save() works."""
        self._content = new_plain
        self._encoded = base64.b64encode(new_plain.encode()).decode()

    def save(self, branch=None, commit_message=None):
        """Update the file content."""
        # nothing else needed for stub

class _FileManager:
    def __init__(self, files: Dict[str, str]):
        self._files = {path: _File(content) for path, content in files.items()}

    def get(self, file_path, ref):
        if file_path not in self._files:
            raise _GitlabGetError()
        return self._files[file_path]

    def create(self, data):
        self._files[data["file_path"]] = _File(data["content"])

class _Branch:
    def __init__(self, name: str):
        self.name = name

class _BranchManager:
    def __init__(self, branches: List[str]):
        self._branches = {name: _Branch(name) for name in branches}

    # New helper for script: delete existing branch
    def delete(self, name):
        if name in self._branches:
            del self._branches[name]

    def get(self, name):
        if name not in self._branches:
            raise _GitlabGetError()
        return self._branches[name]

    def create(self, data):
        name = data["branch"]
        self._branches[name] = _Branch(name)

class _MR:
    def __init__(self, iid: int, web_url: str):
        self.iid = iid
        self.web_url = web_url
        self.source_branch = ""
        self.assignee_id = None

    def save(self, **kwargs):
        # stub: nothing to persist
        pass

class _MergeRequestManager:
    def __init__(self, mrs: List[_MR]):
        self._mrs = mrs

    def list(self, source_branch=None, state=None):
        return [mr for mr in self._mrs if source_branch is None or mr.source_branch == source_branch]

    def create(self, data):
        mr = _MR(123, "http://gitlab/mr/123")
        mr.source_branch = data.get("source_branch", "")
        self._mrs.append(mr)
        return mr

class _Project:
    def __init__(self, files: Dict[str, str], branches: List[str], mrs: List[_MR]):
        self.files = _FileManager(files)
        self.branches = _BranchManager(branches)
        self.mergerequests = _MergeRequestManager(mrs)

class _GitlabGetError(Exception):
    pass

def install_gitlab_stub(monkeypatch, project_stub):
    if "gitlab" not in sys.modules:
        sys.modules["gitlab"] = types.ModuleType("gitlab")
    gl_mod = sys.modules["gitlab"]

    # Add exceptions
    exc_mod = types.ModuleType("gitlab.exceptions")
    exc_mod.GitlabGetError = _GitlabGetError
    gl_mod.exceptions = exc_mod

    # Gitlab factory
    def _factory(url, private_token):
        gl = types.SimpleNamespace()
        gl.projects = types.SimpleNamespace()
        gl.projects.get = lambda path: project_stub
        return gl

    monkeypatch.setattr(gl_mod, "Gitlab", _factory, raising=False)

@pytest.fixture
def mock_result_file(monkeypatch, tmp_path):
    """Redirect /tmp/release-mr-result to a temp file."""
    result_file = tmp_path / "release-mr-result"
    builtin_open = open

    def mock_open(path, mode="r", *args, **kwargs):
        if path == "/tmp/release-mr-result":
            return builtin_open(result_file, mode, *args, **kwargs)
        return builtin_open(path, mode, *args, **kwargs)

    monkeypatch.setattr("builtins.open", mock_open)
    return result_file

@pytest.fixture(autouse=True)
def clear_env(monkeypatch):
    for var in ["VERSION", "NVSENTINEL_COMPONENT_GITLAB_TOKEN", "GITLAB_URL"]:
        monkeypatch.delenv(var, raising=False)

def read_result(result_file):
    """Read the result from the temp file."""
    return json.loads(result_file.read_text())

class TestUpdateReleaseVersion:
    def _set_env(self, monkeypatch):
        monkeypatch.setenv("USER_ID", "57733")
        monkeypatch.setenv("VERSION", "v1.0.0")
        monkeypatch.setenv("NVSENTINEL_COMPONENT_GITLAB_TOKEN", "tok")
        monkeypatch.setenv("GITLAB_URL", "https://gitlab.example.com")

    def _load_script(self):
        """Load the script module."""
        spec = importlib.util.spec_from_file_location("update_release", SCRIPT_PATH)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_branch_exists(self, monkeypatch, mock_result_file):
        """When branch exists, should use it and update content."""
        self._set_env(monkeypatch)

        # Setup project with existing branch but no file
        project = _Project(
            files={},
            branches=["nvsentinel/v1.0.0"],  # branch already exists
            mrs=[]
        )
        install_gitlab_stub(monkeypatch, project)

        # Run script
        module = self._load_script()
        module.main()

        # Check results
        result = read_result(mock_result_file)
        assert result["status"] == "created"
        assert "Successfully created MR" in result["message"]

    def test_no_changes_needed(self, monkeypatch, mock_result_file):
        """When file content matches, no MR should be created."""
        self._set_env(monkeypatch)

        # Load script first to get release file content
        module = self._load_script()
        version = "v1.0.0"
        existing_content = module.get_release_file(version)

        # Create project with matching content
        project = _Project(
            files={"release-nvsentinel.yaml": existing_content},
            branches=["nvsentinel/v1.0.0"],
            mrs=[]
        )
        install_gitlab_stub(monkeypatch, project)

        # Run script
        module.main()

        # Check results
        result = read_result(mock_result_file)
        assert result["status"] == "no-changes"
        assert "No changes to commit" in result["message"]

    def test_mr_exists(self, monkeypatch, mock_result_file):
        """When MR exists for branch, should use existing MR."""
        self._set_env(monkeypatch)

        # Setup project with existing MR
        existing_mr = _MR(456, "http://gitlab/mr/456")
        existing_mr.source_branch = "nvsentinel/v1.0.0"
        project = _Project(
            files={},
            branches=["nvsentinel/v1.0.0"],
            mrs=[existing_mr]
        )
        install_gitlab_stub(monkeypatch, project)

        # Run script
        module = self._load_script()
        module.main()

        # Check results
        result = read_result(mock_result_file)
        assert result["status"] == "already-exists"
        assert "Existing MR found" in result["message"]
        assert result["mr_iid"] == 456

    def test_gitlab_error(self, monkeypatch, mock_result_file):
        """When GitLab API fails, should exit with error."""
        self._set_env(monkeypatch)

        def raise_error(*args, **kwargs):
            raise _GitlabGetError()

        # Create project that raises error on any operation
        project = types.SimpleNamespace()
        project.branches = types.SimpleNamespace(get=raise_error, create=raise_error)
        install_gitlab_stub(monkeypatch, project)

        # Run script
        module = self._load_script()

        with pytest.raises(SystemExit) as exc:
            module.main()
        assert exc.value.code == 1

        # Check results
        result = read_result(mock_result_file)
        assert result["status"] == "failed" 