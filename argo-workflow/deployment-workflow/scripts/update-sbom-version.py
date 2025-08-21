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
from __future__ import annotations
import base64
import json
import os
import sys
from datetime import datetime, timezone
from io import StringIO
from typing import Any, Dict
import gitlab
import logging
from ruamel.yaml import YAML
from ruamel.yaml.comments import CommentedMap

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S'
)
logger = logging.getLogger('update-sbom-version')


def _save_result(path:str, data: dict) -> None:
    """Write *data* to the well-known path so that the workflow can pick it up."""

    with open(path, "w", encoding="utf-8") as fh:
        json.dump(data, fh, indent=2)


def _get_env(var_name: str) -> str | None:
    """Utility to fetch an environment variable returning *None* if missing."""

    return os.environ.get(var_name)

def _update_yaml_content(original: str, version: str) -> str:
    """Return *updated* yaml string or the *original* if nothing changed."""

    yaml_rt = YAML(typ="rt")
    yaml_rt.preserve_quotes = True
    yaml_rt.indent(mapping=2, sequence=4, offset=2)
    yaml_rt.width = 4096
    yaml_rt.map_indent = 2
    yaml_rt.sequence_indent = 4

    try:
        doc = yaml_rt.load(original)
    except Exception:
        # If we fail to parse we bail out, returning original so that the caller
        # can decide how to handle (likely skip the file).
        return original

    mk8s = doc.get("mk8s") if isinstance(doc, dict) else None
    if not isinstance(mk8s, dict):
        return original

    components = mk8s.get("components")
    if not isinstance(components, dict):
        return original

    infra = components.get("infra")
    if not isinstance(infra, dict):
        return original

    nvsentinel_new = CommentedMap()
    nvsentinel_new["version"] = version
    source_map = CommentedMap()
    source_map["revision"] = version
    source_map["url"] = "https://gitlab-master.nvidia.com/dgxcloud/mk8s/components/nvsentinel.git"
    nvsentinel_new["source"] = source_map
    infra["nvsentinel"] = nvsentinel_new

    # Dump back to string preserving formatting
    buf = StringIO()
    yaml_rt.dump(doc, buf)
    return buf.getvalue()


def _get_project(gitlab_url: str, token: str, project_path: str):
    """Return a GitLab *project* object for *project_path*."""

    gl = gitlab.Gitlab(gitlab_url, private_token=token)
    return gl.projects.get(project_path)


def _ensure_branch(project, branch_name: str, base_ref: str,) -> None:
    """Ensure *branch_name* exists – create it from *base_ref* when missing."""

    try:
        project.branches.get(branch_name)
        logger.info(f"Branch {branch_name} already exists")
    except gitlab.exceptions.GitlabGetError:
        project.branches.create({"branch": branch_name, "ref": base_ref})
        logger.info(f"Created branch {branch_name} from {base_ref}")


def _update_file_in_branch(
    project,
    branch_name: str,
    file_path: str,
    updated_content: str,
    commit_message: str,
) -> None:
    """Update *file_path* in *branch_name* with *updated_content* if it differs."""

    try:
        f_branch = project.files.get(file_path=file_path, ref=branch_name)
    except gitlab.exceptions.GitlabGetError:
        raise RuntimeError(f"{file_path} not found in branch {branch_name}")

    original_content = base64.b64decode(f_branch.content).decode()
    if original_content.strip() == updated_content.strip():
        logger.info(f"No content change; skipping commit for {file_path}")
        return

    f_branch.content = updated_content
    f_branch.save(branch=branch_name, commit_message=commit_message)
    logger.info(f"Updated {file_path} in branch")


def _branch_has_changes(project, base_ref: str, branch_name: str) -> bool:
    """Return *True* if *branch_name* differs from *base_ref* in *project*."""

    compare = project.repository_compare(base_ref, branch_name)
    return bool(compare.get("diffs"))


def _create_or_reuse_mr(
    project,
    branch_name: str,
    base_ref: str,
    title: str,
    description: str,
    user_id: str,
):
    """Create a new MR or reuse an open one for *branch_name* → *base_ref*."""

    existing_mrs = project.mergerequests.list(source_branch=branch_name, state="opened")
    if existing_mrs:
        mr = existing_mrs[0]
        status = "already-exists"
        message = "Existing MR found"
        logger.info(f"Using existing MR")
    else:
        mr = project.mergerequests.create(
            {
                "source_branch": branch_name,
                "target_branch": base_ref,
                "title": title,
                "description": description,
                "remove_source_branch": True,
                "assignee_id": user_id,
            }
        )
        status = "created"
        message = "Successfully created MR"
        logger.info(f"Created new MR")

    logger.info(f"MR URL: {mr.web_url}")
    return mr, status, message

def main() -> None:  # noqa: C901 (complexity) – acceptable for script entry
    version = _get_env("VERSION")
    gitlab_url = _get_env("GITLAB_URL")
    manifest_template_token = _get_env("MANIFEST_TEMPLATE_GITLAB_TOKEN")
    ace_token = _get_env("ACE_GITLAB_TOKEN")
    user_id = _get_env("USER_ID")
    
    if not version or not gitlab_url or not manifest_template_token or not ace_token or not user_id:
        logger.error(f"[update-sbom-version] ERROR: Missing required environment variables (VERSION, GITLAB_URL, *_GITLAB_TOKEN, USER_ID)")
        _save_result("/tmp/ace-mr-result",
            {
                "status": "failed",
                "error": "Missing required environment variables (VERSION, GITLAB_URL, *_GITLAB_TOKEN, USER_ID)",
            }
        )
        _save_result("/tmp/manifest-template-mr-result",
            {
                "status": "failed",
                "error": "Missing required environment variables (VERSION, GITLAB_URL, *_GITLAB_TOKEN, USER_ID)",
            }
        )
        sys.exit(1)

    # Run the two update flows sequentially – failures will exit immediately
    ace_mr = raise_ace_mr(version, ace_token, gitlab_url, user_id)
    manifest_mr = raise_manifest_template_mr(version, manifest_template_token, gitlab_url, user_id)
    _save_result("/tmp/ace-mr-result", ace_mr)
    _save_result("/tmp/manifest-template-mr-result", manifest_mr)


def _update_ace_manifest_content(original: str, version: str) -> str:
    """Update nvSentinel version in ACE manifest.yaml."""
    yaml_rt = YAML(typ="rt")
    yaml_rt.preserve_quotes = True
    yaml_rt.indent(mapping=2, sequence=4, offset=2)
                                                                        
    try:
        doc = yaml_rt.load(original)
    except Exception:
        return original

    if not isinstance(doc, dict):
        return original

    components = doc.get("components")
    if not isinstance(components, list):
        return original

    found = False
    for item in components:
        if isinstance(item, dict) and str(item.get("name", "")).lower() == "nvsentinel":
            item["version"] = version
            found = True
            break

    if not found:
        raise RuntimeError("nvSentinel component entry not found in manifest.yaml")

    buf = StringIO()
    yaml_rt.dump(doc, buf)
    return buf.getvalue()


def raise_ace_mr(version: str, ace_token: str, gitlab_url: str, user_id: str)-> Dict[str, Any]:
    project_path = "dgxcloud/platform/release/ace"
    branch_name = f"nvsentinel/{version}"

    try:
        project = _get_project(gitlab_url, ace_token, project_path)
        base_ref = "main"
        _ensure_branch(project, branch_name, base_ref)

        file_path = "manifest.yaml"
        commit_message = f"chore: Update nvSentinel version to {version}"

        # Fetch current content & generate updated version
        f_branch = project.files.get(file_path=file_path, ref=branch_name)
        original_content = base64.b64decode(f_branch.content).decode()
        updated_content = _update_ace_manifest_content(original_content, version)
        _update_file_in_branch(
            project,
            branch_name,
            file_path,
            updated_content,
            commit_message,
        )

        if not _branch_has_changes(project, base_ref, branch_name):
            logger.info(f"No differences between {base_ref} and {branch_name} – skipping MR")
            return {
                    "status": 'no-changes',
                    "message": 'No changes to commit',
                    "version": version,
                    "branch": branch_name,
                }

        title = f"chore: Update nvSentinel version to {version}"
        description = (
            f"This MR updates the nvSentinel component version to {version} in manifest.yaml."
        )
        mr, status, message = _create_or_reuse_mr(
            project,
            branch_name,
            base_ref,
            title,
            description,
            user_id,
        )

        return {
                "version": version,
                "status": status,
                "message": message,
                "mr_url": mr.web_url,
                "mr_iid": mr.iid,
                "branch": branch_name,
                "created_at": datetime.now(timezone.utc).isoformat() + "Z",
            }

    except Exception as exc:
        logger.error(f"Exception: {exc}")
        _save_result("/tmp/ace-mr-result",
            {
                "status": "failed",
                "error": str(exc),
            }
        )
        sys.exit(1)


def raise_manifest_template_mr(version: str, manifest_template_token: str, gitlab_url: str, user_id: str)-> Dict[str, Any]:
    project_path = "dgxcloud/mk8s/dgxc/manifests-templates"
    branch_name = f"nvsentinel-{version}"

    try:
        project = _get_project(gitlab_url, manifest_template_token, project_path)
        base_ref = "main"
        _ensure_branch(project, branch_name, base_ref)

        release_files = ["release-dgxc.yaml"]
        commit_message = f"chore: Update NVSentinel version to {version}"

        for file_path in release_files:
            logger.info(f"Processing {file_path}")
            f_branch = project.files.get(file_path=file_path, ref=branch_name)
            original_content = base64.b64decode(f_branch.content).decode()
            updated_content = _update_yaml_content(original_content, version)
            _update_file_in_branch(
                project,
                branch_name,
                file_path,
                updated_content,
                commit_message,
            )

        if not _branch_has_changes(project, base_ref, branch_name):
            logger.info(
                f"No differences between {base_ref} and {branch_name} – exiting early"
            )
            return{
                    "status": 'no-changes',
                    "message": 'No changes to commit',
                    "version": version,
                    "branch": branch_name,
                }

        title = f"chore: Update NVSentinel version to {version}"
        description = (
            f"This MR updates the NVSentinel version to {version} in release-dgxc-*.yaml files."
        )
        mr, status, message = _create_or_reuse_mr(
            project,
            branch_name,
            base_ref,
            title,
            description,
            user_id,
        )

        return  {
                "version": version,
                "status": status,
                "message": message,
                "mr_url": mr.web_url,
                "mr_iid": mr.iid,
                "branch": branch_name,
                "created_at": datetime.now(timezone.utc).isoformat() + "Z",
            }
        

    except Exception as exc:
        logger.error(f"Exception: {exc}")
        _save_result("/tmp/manifest-template-mr-result",
            {
                "status": "failed",
                "error": str(exc),
            }
        )
        sys.exit(1)

if __name__ == "__main__":
    main()