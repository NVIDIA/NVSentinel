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
import tempfile
import types
from pathlib import Path
from typing import Any, Dict, List

import pytest
from ruamel.yaml import YAML

SCRIPT_PATH = (
    Path(__file__).resolve().parent / "../create-mr.py"
).as_posix()

def _load_script_module():
    spec = importlib.util.spec_from_file_location("create_mr", SCRIPT_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)  # type: ignore[arg-type]
    return module


class _StubResponse:
    def __init__(self, status_code: int, payload: Any = None):
        self.status_code = status_code
        self._payload = payload or {}

    def json(self):
        return self._payload

def make_git_runner(*, branch_exists: bool = False, changes_present: bool = True):
    """Return a stub function that mimics `run_command` expected by the script."""
    state: Dict[str, Any] = {
        "current_branch": "main",
    }

    def _runner(cmd: List[str], cwd: str | None = None, check: bool = True):
        nonlocal state
        joined = " ".join(cmd)
        # Simulate various commands
        if cmd[:3] == ["git", "ls-remote", "--heads"]:
            # branch existence check
            branch = cmd[-1]
            return "hash\trefs/heads/%s" % branch if branch_exists else ""
        if cmd[:3] == ["git", "checkout", "main"]:
            state["current_branch"] = "main"
            return "Checked out main"
        if cmd[:2] == ["git", "checkout"] and cmd[2] == "-b":
            # new branch
            state["current_branch"] = cmd[3]
            return "Switched to a new branch %s" % cmd[3]
        if cmd[:2] == ["git", "checkout"] and cmd[2] == "-B":
            state["current_branch"] = cmd[3]
            return "Reset branch %s" % cmd[3]
        if cmd[:3] == ["git", "rev-parse", "--abbrev-ref"]:
            return state["current_branch"]
        if cmd[:3] == ["git", "status", "--porcelain"]:
            return "M modified-file" if changes_present else ""
        # All other git commands simply succeed and return empty output
        return ""

    return _runner

def install_requests_stubs(monkeypatch, *, create_status: int = 201, existing: bool = False):
    """Patch `requests.post` and `requests.get` to return controlled responses."""

    def _post(url: str, headers: Dict[str, str], json: Dict[str, Any]):
        if create_status == 201:
            return _StubResponse(201, {"web_url": "http://gitlab/mr/123", "iid": 123})
        if create_status == 409:
            # MR already exists
            return _StubResponse(409, {})
        return _StubResponse(create_status, {})

    def _get(url: str, headers: Dict[str, str]):
        if existing:
            return _StubResponse(200, [{"web_url": "http://gitlab/mr/123", "iid": 123, "state": "opened"}])
        return _StubResponse(200, [])

    # If requests already imported, patch its attributes; otherwise create stub module
    if "requests" not in sys.modules:
        monkeypatch.setitem(sys.modules, "requests", types.ModuleType("requests"))
    requests_mod = sys.modules["requests"]
    monkeypatch.setattr(requests_mod, "post", _post, raising=False)
    monkeypatch.setattr(requests_mod, "get", _get, raising=False)


@pytest.fixture()
def env_setup(monkeypatch):
    # Common environment variables for all tests
    monkeypatch.setenv("MANIFEST_GITLAB_TOKEN", "token123")
    monkeypatch.setenv("GITLAB_URL", "https://gitlab.example.com")
    monkeypatch.setenv("USER_ID", "57733")
    monkeypatch.setenv("DEPLOY_TYPE", "prod")  

def _prepare_repo(tmp_path: Path, spec_relative_path: str, initial_yaml: str):
    # Create required directories and spec file
    spec_path = tmp_path / spec_relative_path
    spec_path.parent.mkdir(parents=True, exist_ok=True)
    spec_path.write_text(initial_yaml)
    return spec_path


def _patch_tempdir(monkeypatch, temp_repo_dir: Path):
    class _FakeTempDir:
        def __enter__(self):
            return str(temp_repo_dir)

        def __exit__(self, exc_type, exc_val, exc_tb):
            return False

    monkeypatch.setattr(tempfile, "TemporaryDirectory", lambda: _FakeTempDir())


class TestCreateMR:
    def test_create_mr(self, monkeypatch, tmp_path, env_setup):
        """Case 1: branch does not exist -> create MR."""
        version = "v1.2.3"
        monkeypatch.setenv("VERSION", version)

        # Prepare fake repo & spec
        spec_rel = "spec.yaml"
        spec_initial = """
spec:
  templates:
    include: []
"""
        _prepare_repo(tmp_path, spec_rel, spec_initial)
        _patch_tempdir(monkeypatch, tmp_path)

        # Set clusters JSON env
        clusters_json = json.dumps([
            {
                "pattern_name": "gpu-basic",
                "pattern_info": {"spec": {"foo": "bar"}},
                "clusters": [
                    {"name": "cluster-a", "spec_file_path": spec_rel}
                ],
            }
        ])
        monkeypatch.setenv("CLUSTERS", clusters_json)

        # Install stubs
        install_requests_stubs(monkeypatch, create_status=201)
        git_runner = make_git_runner(branch_exists=False, changes_present=True)
        module = _load_script_module()
        monkeypatch.setattr(module, "run_command", git_runner)

        module.main()

        # Validate MR results
        results = json.loads(Path("/tmp/mr-results.json").read_text())
        assert results[0]["status"] == "created"
        # Validate spec file updated with revision
        doc = YAML().load((tmp_path / spec_rel).read_text())
        include_list = doc["spec"]["templates"]["include"]
        sentinel_entry = next(i for i in include_list if isinstance(i, dict) and i.get("release") == "nvsentinel")
        assert sentinel_entry["revision"] == version

    def test_branch_exists_path(self, monkeypatch, tmp_path, env_setup):
        """Case 2: remote branch already exists but MR creation succeeds."""
        version = "v2.0.0"
        monkeypatch.setenv("VERSION", version)

        spec_rel = "cluster/spec.yaml"
        spec_initial = "spec:\n  templates:\n    include: []\n"
        _prepare_repo(tmp_path, spec_rel, spec_initial)
        _patch_tempdir(monkeypatch, tmp_path)

        clusters_json = json.dumps([
            {
                "pattern_name": "gpu-basic",
                "pattern_info": {"spec": {"foo": "baz"}},
                "clusters": [{"name": "cluster-b", "spec_file_path": spec_rel}],
            }
        ])
        monkeypatch.setenv("CLUSTERS", clusters_json)

        # branch exists
        install_requests_stubs(monkeypatch, create_status=201)
        git_runner = make_git_runner(branch_exists=True, changes_present=True)
        module = _load_script_module()
        monkeypatch.setattr(module, "run_command", git_runner)

        module.main()
        results = json.loads(Path("/tmp/mr-results.json").read_text())
        assert results[0]["status"] == "created"

    def test_existing_mr(self, monkeypatch, tmp_path, env_setup):
        """Case 3: branch and MR already present -> existing MR returned."""
        version = "v3.0.0"
        monkeypatch.setenv("VERSION", version)

        spec_rel = "specs/cluster.yaml"
        spec_initial = "spec:\n  templates:\n    include: []\n"
        _prepare_repo(tmp_path, spec_rel, spec_initial)
        _patch_tempdir(monkeypatch, tmp_path)

        clusters_json = json.dumps([
            {
                "pattern_name": "gpu-basic",
                "pattern_info": {"spec": {"alpha": 1}},
                "clusters": [{"name": "cluster-c", "spec_file_path": spec_rel}],
            }
        ])
        monkeypatch.setenv("CLUSTERS", clusters_json)

        install_requests_stubs(monkeypatch, create_status=409, existing=True)
        git_runner = make_git_runner(branch_exists=True, changes_present=True)
        module = _load_script_module()
        monkeypatch.setattr(module, "run_command", git_runner)

        module.main()
        results = json.loads(Path("/tmp/mr-results.json").read_text())
        assert results[0]["status"] == "existing"
        assert results[0]["mr_url"] == "http://gitlab/mr/123"

    def test_only_revision_update(self, monkeypatch, tmp_path, env_setup):
        """Case 4: No pattern spec changes, only update revision/url."""
        version = "v4.0.0"
        monkeypatch.setenv("VERSION", version)

        spec_rel = "spec.yaml"
        spec_initial = (
            "spec:\n  templates:\n    include:\n      - release: nvsentinel\n        revision: oldver\n        url: https://old.url.git\n"
        )
        _prepare_repo(tmp_path, spec_rel, spec_initial)
        _patch_tempdir(monkeypatch, tmp_path)

        clusters_json = json.dumps([
            {
                "pattern_name": "gpu-basic",
                "pattern_info": {"spec": {}},
                "clusters": [{"name": "cluster-d", "spec_file_path": spec_rel}],
            }
        ])
        monkeypatch.setenv("CLUSTERS", clusters_json)

        install_requests_stubs(monkeypatch, create_status=201)
        git_runner = make_git_runner(branch_exists=False, changes_present=True)
        module = _load_script_module()
        monkeypatch.setattr(module, "run_command", git_runner)

        module.main()

        doc = YAML().load((tmp_path / spec_rel).read_text())
        include_list = doc["spec"]["templates"]["include"]
        nvsentinel_entry = next(i for i in include_list if isinstance(i, dict) and i.get("release") == "nvsentinel")
        assert nvsentinel_entry["revision"] == version
        assert "nvsentinel.git" in nvsentinel_entry["url"]

    def test_spec_file_missing(self, tmp_path):
        """update_cluster_spec should return False when YAML file is absent."""

        module = _load_script_module()

        missing_file = tmp_path / "missing.yaml"
        # Ensure it truly doesn't exist
        assert not missing_file.exists()

        result = module.update_cluster_spec(str(missing_file), "1.0.0", pattern_spec={})
        assert result is False

    def test_gitlab_http_error(self, monkeypatch):
        """create_or_find_mr should return status failed on HTTP 500."""

        install_requests_stubs(monkeypatch, create_status=500)
        module = _load_script_module()

        result = module.create_or_find_mr(
            gitlab_url="https://gitlab.example.com",
            project_path="dummy%2Fproj",
            headers={"PRIVATE-TOKEN": "t"},
            branch_name="branch",
            user_id="57733",
            pattern_name="pat",
            version="1.0.0",
            cluster_names=["c1"],
            cluster_specs={},
            pattern_spec={},
        )

        assert result["status"] == "failed"
        assert result["error"].startswith("HTTP 500")

    def test_pattern_to_branch(self):
        """pattern_to_branch slugifies correctly."""

        module = _load_script_module()
        version = "2.3.4"
        slug_branch = module.pattern_to_branch("GPU Pattern V1!", version)
        assert slug_branch == f"nvsentinel-workflow/gpu-pattern-v1/{version}" 