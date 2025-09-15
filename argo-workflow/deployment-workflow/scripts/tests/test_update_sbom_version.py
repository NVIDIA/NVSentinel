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
import sys
import types
from pathlib import Path
from typing import Any, Dict, List

import pytest

if "gitlab" not in sys.modules:
    import types as _types

    _gl_stub = _types.ModuleType("gitlab")
    # Provide a dummy `Gitlab` attribute that can later be monkey-patched.
    _gl_stub.Gitlab = lambda *args, **kwargs: None  # type: ignore[assignment]

    # Minimal exceptions sub-module with placeholder classes
    _exc_mod = _types.ModuleType("gitlab.exceptions")
    _exc_mod.GitlabGetError = type("GitlabGetError", (Exception,), {})
    _exc_mod.GitlabDeleteError = type("GitlabDeleteError", (Exception,), {})
    _gl_stub.exceptions = _exc_mod  # type: ignore[attr-defined]

    sys.modules["gitlab"] = _gl_stub

# ---------------------------------------------------------------------------
# Utility – dynamically load the target script as a module so that we can call
# its helpers directly without executing the top-level `main()`.
# ---------------------------------------------------------------------------

SCRIPT_PATH = (
    Path(__file__).resolve().parent / "../update-sbom-version.py"
).as_posix()


def _load_module():
    spec = importlib.util.spec_from_file_location("update_sbom", SCRIPT_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)  # type: ignore[arg-type]
    return mod


# ---------------------------------------------------------------------------
# Very small GitLab stub implementation – only what the script actually uses
# ---------------------------------------------------------------------------


class _File:
    def __init__(self, content: str):
        self._update(content)

    def _update(self, text: str):
        self.plain = text
        self.content = base64.b64encode(text.encode()).decode()

    def save(self, *, branch=None, commit_message=None):  # noqa: D401 – stub
        # When the script updates `.content` with raw text, convert it to encoded form.
        raw = self.content  # may be plain text
        self._update(raw)  # _update handles base64 encoding and syncing `plain`/`content`


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
        b = data["branch"]
        self._branches[b] = _Branch(b)

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

    # Script sometimes sets assignee_id / etc. Provide attrs.
    assignee_id = None


class _MRManager:
    def __init__(self, mrs: List[_MR]):
        self._mrs = mrs

    def list(self, source_branch=None, state=None):
        return [mr for mr in self._mrs if mr.source_branch == source_branch]

    def create(self, data):
        new = _MR(999, "http://gitlab/mr/999", data["source_branch"])
        self._mrs.append(new)
        return new


class _Project:
    def __init__(self, *, files: Dict[str, str], branches: List[str], mrs: List[_MR], diffs: bool):
        self.files = _FileManager(files)
        self.branches = _BranchManager(branches)
        self.mergerequests = _MRManager(mrs)
        # Simple flag for repository_compare
        self._has_diffs = diffs

        # Stub commits API used by the script (only .create is called)
        class _Commits:
            @staticmethod
            def create(data):  # noqa: D401 – stub
                return None

        self.commits = _Commits()

    def repository_compare(self, base, head):
        return {"diffs": [1] if self._has_diffs else []}


class _GitlabGetError(Exception):
    pass


class _GitlabDeleteError(Exception):
    pass


def _install_gitlab_stub(monkeypatch, project_stub):
    """Patch `gitlab.Gitlab` used inside the script."""

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


# ---------------------------------------------------------------------------
# Helpers to interact with result files written by the script
# ---------------------------------------------------------------------------


def _read_json(path: Path):
    return json.loads(path.read_text())


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


@pytest.fixture(autouse=True)
def _clear_env(monkeypatch):
    for var in [
        "VERSION",
        "GITLAB_URL",
        "MANIFEST_TEMPLATE_GITLAB_TOKEN",
        "NVCR_REGISTRY_GITLAB_TOKEN",
        "BCP_REGISTRY_GITLAB_TOKEN",
        "ACE_GITLAB_TOKEN",
        "USER_ID",
    ]:
        monkeypatch.delenv(var, raising=False)


@pytest.fixture
def _mock_result_files(monkeypatch, tmp_path):
    """Redirect result paths into tmp dir for inspection."""

    ace_file = tmp_path / "ace.json"
    mani_file = tmp_path / "mani.json"

    builtin_open = open

    def _mock_open(path, mode="r", *args, **kwargs):
        if path == "/tmp/ace-mr-result":
            return builtin_open(ace_file, mode, *args, **kwargs)
        if path == "/tmp/manifest-template-mr-result":
            return builtin_open(mani_file, mode, *args, **kwargs)
        return builtin_open(path, mode, *args, **kwargs)

    monkeypatch.setattr("builtins.open", _mock_open)

    return ace_file, mani_file


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------


class TestUpdateSBOMVersionScript:
    def _set_basic_env(self, monkeypatch):
        monkeypatch.setenv("VERSION", "v1.2.3")
        monkeypatch.setenv("GITLAB_URL", "https://gitlab.example.com")
        monkeypatch.setenv("MANIFEST_TEMPLATE_GITLAB_TOKEN", "tok1")
        monkeypatch.setenv("ACE_GITLAB_TOKEN", "tok2")
        monkeypatch.setenv("NVCR_REGISTRY_GITLAB_TOKEN", "tok3")
        monkeypatch.setenv("BCP_REGISTRY_GITLAB_TOKEN", "tok4")
        monkeypatch.setenv("USER_ID", "42")

    # 1. Missing env variable – expect sys.exit(1) and error result
    def test_missing_env(self, monkeypatch, _mock_result_files):
        module = _load_module()

        with pytest.raises(SystemExit):
            module.main()

        ace, mani = _mock_result_files
        assert _read_json(ace)["status"] == "failed"

    # 2. Positive flow: files updated, MR created
    def test_positive_flow(self, monkeypatch, tmp_path, _mock_result_files):
        self._set_basic_env(monkeypatch)

        # minimal yaml needing update – version placeholder
        ori_yaml = """
mk8s:
  components:
    infra:
      nvsentinel:
        version: old
        source:
          revision: old
          url: foo
"""

        ace_manifest = "components:\n  - name: nvSentinel\n    version: old\n"

        proj_stub_ace = _Project(
            files={
                "manifest.yaml": ace_manifest,
                "imagesync.yaml": "charts: []\n",
                "chartsync.yaml": "images: []\n",
            },
            branches=[],
            mrs=[],
            diffs=True,
        )
        proj_stub_mani = _Project(
            files={"release-dgxc.yaml": ori_yaml}, branches=[], mrs=[], diffs=True
        )

        # install separate stubs depending on token used – easiest: return mani for first, ace for second call order
        call_counter = {"i": 0}

        def _factory(url, private_token):
            call_counter["i"] += 1
            if call_counter["i"] == 1:
                return types.SimpleNamespace(projects=types.SimpleNamespace(get=lambda p: proj_stub_ace))
            return types.SimpleNamespace(projects=types.SimpleNamespace(get=lambda p: proj_stub_mani))

        import gitlab as gl_mod  # ensure module exists
        if "gitlab" not in sys.modules:
            sys.modules["gitlab"] = types.ModuleType("gitlab")
        gl_mod = sys.modules["gitlab"]
        exc_mod = types.ModuleType("gitlab.exceptions")
        exc_mod.GitlabGetError = _GitlabGetError
        exc_mod.GitlabDeleteError = _GitlabDeleteError
        gl_mod.exceptions = exc_mod
        monkeypatch.setattr(gl_mod, "Gitlab", _factory, raising=False)

        module = _load_module()
        # Should not raise
        module.main()

        ace_res, mani_res = _mock_result_files
        assert _read_json(ace_res)["status"] == "created"
        
    # 3. create_or_reuse_mr helper
    @pytest.mark.parametrize("exists", [True, False])
    def test_create_or_reuse_mr(self, monkeypatch, exists):
        module = _load_module()

        existing = _MR(7, "url", "branch") if exists else None
        project = _Project(files={}, branches=["branch"], mrs=[existing] if existing else [], diffs=True)
        _install_gitlab_stub(monkeypatch, project)

        mr, status, _ = module._create_or_reuse_mr(project, "branch", "main", "t", "d", "42")
        if exists:
            assert status == "already-exists"
            assert mr.iid == 7
        else:
            assert status == "created"
            assert mr.iid == 999

    # 4. _branch_has_changes
    @pytest.mark.parametrize("has_diff", [True, False])
    def test_branch_has_changes(self, has_diff):
        module = _load_module()
        project = _Project(files={}, branches=[], mrs=[], diffs=has_diff)
        assert module._branch_has_changes(project, "main", "feat") is has_diff

    # 5. _update_file_in_branch – file missing should raise RuntimeError
    def test_update_file_missing(self, monkeypatch):
        module = _load_module()
        project = _Project(files={}, branches=["b"], mrs=[], diffs=True)
        with pytest.raises(RuntimeError):
            module._update_file_in_branch(project, "b", "absent.yaml", "x", "msg")

    # 6. _ensure_branch scenarios
    @pytest.mark.parametrize("exists", [True, False])
    def test_ensure_branch(self, exists):
        module = _load_module()
        branches = ["old"]
        if exists:
            branches.append("new")
        project = _Project(files={}, branches=branches, mrs=[], diffs=False)
        module._ensure_branch(project, "new", "old")
        # Branch should now exist regardless
        assert "new" in project.branches._branches

    # 7. edge case: repository_compare returns malformed data
    def test_branch_compare_edge(self):
        module = _load_module()

        class _P(_Project):
            def repository_compare(self, a, b):
                return {}  # missing diffs key ⇒ treat as no changes

        project = _P(files={}, branches=[], mrs=[], diffs=True)
        assert module._branch_has_changes(project, "a", "b") is False
