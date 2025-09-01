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

# Path to script
SCRIPT_PATH = (
    Path(__file__).resolve().parent / "../image-sync.py"
).as_posix()


def _load_module():
    spec = importlib.util.spec_from_file_location("image_sync", SCRIPT_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)  # type: ignore[arg-type]
    return mod


# ----------------------------- GitLab stub helpers -------------------------


class _File:
    def __init__(self, text: str):
        self._set(text)

    def _set(self, text: str):
        self.plain = text
        self.content = base64.b64encode(text.encode()).decode()

    def save(self, *, branch=None, commit_message=None):  # noqa: D401 – stub
        # The script may set `content` to raw text; ensure we encode + sync.
        self._set(self.content if isinstance(self.content, str) else self.plain)

class _FileManager:
    def __init__(self, files: Dict[str, str]):
        self._files = {p: _File(c) for p, c in files.items()}

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
        self._branches = {b: _Branch(b) for b in branches}

    def get(self, name):
        if name not in self._branches:
            raise _GitlabGetError()
        return self._branches[name]

    def create(self, data):
        self._branches[data["branch"]] = _Branch(data["branch"])

    def delete(self, name):
        if name in self._branches:
            del self._branches[name]
        else:
            raise _GitlabDeleteError()


class _MR:
    def __init__(self, iid: int, web_url: str, source_branch: str):
        self.iid = iid
        self.web_url = web_url
        self.source_branch = source_branch
        self.assignee_id = None

    def save(self):
        pass  # no-op


class _MRManager:
    def __init__(self, items: List[_MR]):
        self._items = items

    def list(self, source_branch=None, state=None):
        return [mr for mr in self._items if mr.source_branch == source_branch]

    def create(self, data):
        new = _MR(888, "http://gitlab/mr/888", data["source_branch"])
        self._items.append(new)
        return new


class _Project:
    def __init__(self, *, files: Dict[str, str], branches: List[str], mrs: List[_MR], has_diff: bool):
        self.files = _FileManager(files)
        self.branches = _BranchManager(branches)
        self.mergerequests = _MRManager(mrs)
        self._has_diff = has_diff

    def repository_compare(self, base, head):
        return {"diffs": [1] if self._has_diff else []}


# Exceptions expected by script


class _GitlabGetError(Exception):
    pass


class _GitlabDeleteError(Exception):
    pass


def _install_gitlab_stub(monkeypatch, project_stub):
    if "gitlab" not in sys.modules:
        sys.modules["gitlab"] = types.ModuleType("gitlab")
    gl_mod = sys.modules["gitlab"]

    exc_mod = types.ModuleType("gitlab.exceptions")
    exc_mod.GitlabGetError = _GitlabGetError
    exc_mod.GitlabDeleteError = _GitlabDeleteError
    gl_mod.exceptions = exc_mod

    def _factory(url, private_token):
        gl = types.SimpleNamespace()
        gl.projects = types.SimpleNamespace()
        gl.projects.get = lambda path: project_stub
        return gl

    monkeypatch.setattr(gl_mod, "Gitlab", _factory, raising=False)


# ----------------------------- Fixtures ------------------------------------


@pytest.fixture(autouse=True)
def _clear_env(monkeypatch):
    for v in [
        "VERSION",
        "GITLAB_URL",
        "IMAGE_SYNC_GITLAB_TOKEN",
        "ACE_GITLAB_TOKEN",
        "NVCR_REGISTRY_GITLAB_TOKEN",
        "BCP_REGISTRY_GITLAB_TOKEN",
        "USER_ID",
    ]:
        monkeypatch.delenv(v, raising=False)


# ----------------------------- Tests ---------------------------------------


class TestHelpers:
    def test_ensure_branch_exists(self, monkeypatch):
        module = _load_module()
        project = _Project(files={}, branches=["b"], mrs=[], has_diff=False)
        _install_gitlab_stub(monkeypatch, project)
        module._ensure_branch(project, "b", "main")
        assert "b" in project.branches._branches  # unchanged

    def test_ensure_branch_create(self, monkeypatch):
        module = _load_module()
        project = _Project(files={}, branches=["main"], mrs=[], has_diff=False)
        _install_gitlab_stub(monkeypatch, project)
        module._ensure_branch(project, "feat", "main")
        assert "feat" in project.branches._branches

    def test_update_file_missing(self, monkeypatch):
        module = _load_module()
        project = _Project(files={}, branches=["b"], mrs=[], has_diff=False)
        _install_gitlab_stub(monkeypatch, project)
        with pytest.raises(RuntimeError):
            module._update_file_in_branch(project, "b", "nofile.yaml", "data", "msg")

    @pytest.mark.parametrize("has_diff", [True, False])
    def test_branch_has_changes(self, has_diff):
        module = _load_module()
        project = _Project(files={}, branches=[], mrs=[], has_diff=has_diff)
        assert module._branch_has_changes(project, "main", "feat") is has_diff

    @pytest.mark.parametrize("mr_exists", [True, False])
    def test_create_or_reuse_mr(self, monkeypatch, mr_exists):
        module = _load_module()
        existing = _MR(7, "url", "feat") if mr_exists else None
        project = _Project(files={}, branches=["feat"], mrs=[existing] if existing else [], has_diff=True)
        _install_gitlab_stub(monkeypatch, project)
        mr, status, _ = module._create_or_reuse_mr(project, "feat", "main", "t", "d", "42")
        if mr_exists:
            assert status == "already-exists" and mr.iid == 7
        else:
            assert status == "created" and mr.iid == 888


class TestPositiveFlow:
    def test_full_flow_success(self, monkeypatch, tmp_path):
        module = _load_module()

        # --- prepare env vars ---
        monkeypatch.setenv("VERSION", "v9.9.9")
        monkeypatch.setenv("GITLAB_URL", "https://gitlab.example.com")
        monkeypatch.setenv("ACE_GITLAB_TOKEN", "tok")
        monkeypatch.setenv("NVCR_REGISTRY_GITLAB_TOKEN", "tok")
        monkeypatch.setenv("BCP_REGISTRY_GITLAB_TOKEN", "tok")
        monkeypatch.setenv("USER_ID", "99")

        # minimal yaml requiring update (image list)
        ori = """
sync:
  - source: nv-ngc-devops.nvcr.io/nv-ngc-devops/nvsentinel
    tags: {allow: []}
"""

        project = _Project(
            files={
                "imagesync.yaml": ori,
                "charts.yaml": "charts: []\n",
                "images.yaml": "images: []\n",
            },
            branches=["main"],
            mrs=[],
            has_diff=True,
        )

        _install_gitlab_stub(monkeypatch, project)

        # Redirect result file
        res_file = tmp_path / "result.json"

        builtin_open = open

        def _mock_open(path, mode="r", *args, **kwargs):
            if path in {
                "/tmp/image-sync-mr-result",
                "/tmp/ace-image-sync-mr-result",
            }:
                return builtin_open(res_file, mode, *args, **kwargs)
            # other result files can go to devnull
            if path in {
                "/tmp/nvcr-registry-mr-result",
                "/tmp/bcp-registry-mr-result",
            }:
                return builtin_open(os.devnull, mode, *args, **kwargs)
            return builtin_open(path, mode, *args, **kwargs)

        monkeypatch.setattr("builtins.open", _mock_open)

        # Run script (main defined at end)
        module.main()

        result = json.loads(res_file.read_text())
        assert result["status"] in {"created", "already-exists"}
        assert result["version"] == "v9.9.9"
