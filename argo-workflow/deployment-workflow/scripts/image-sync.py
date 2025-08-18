#!/usr/bin/env python3
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
import logging
import os
import sys
from datetime import datetime, timezone
from io import StringIO
from typing import Any, Dict

import gitlab  # type: ignore
from ruamel.yaml import YAML
from ruamel.yaml.comments import CommentedMap
from ruamel.yaml.scalarstring import DoubleQuotedScalarString

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
logger = logging.getLogger("image-sync")

def _save_result(path: str, data: dict) -> None:
    """Write *data* as JSON to *path* so the workflow can pick it up."""

    with open(path, "w", encoding="utf-8") as fh:
        json.dump(data, fh, indent=2)


def _get_env(var_name: str) -> str | None:
    """Return value of *var_name* or *None* if not set."""

    return os.environ.get(var_name)


def _get_project(gitlab_url: str, token: str, project_path: str):
    """Return GitLab project object for *project_path*."""

    gl = gitlab.Gitlab(gitlab_url, private_token=token)
    return gl.projects.get(project_path)


def _ensure_branch(project, branch_name: str, base_ref: str) -> None:
    try:
        project.branches.get(branch_name)
        logger.info("Branch %s already exists; resetting to %s", branch_name, base_ref)
        try:
            project.branches.delete(branch_name)
        except gitlab.exceptions.GitlabDeleteError as exc:
            logger.warning("Failed to delete branch %s (may already be gone): %s", branch_name, exc)
        project.branches.create({"branch": branch_name, "ref": base_ref})
    except gitlab.exceptions.GitlabGetError:
        project.branches.create({"branch": branch_name, "ref": base_ref})
        logger.info("Created branch %s from %s", branch_name, base_ref)


def _update_file_in_branch(
    project,
    branch_name: str,
    file_path: str,
    updated_content: str,
    commit_message: str,
) -> None:
    """Update *file_path* in *branch_name* with *updated_content* if needed."""

    try:
        f_branch = project.files.get(file_path=file_path, ref=branch_name)
    except gitlab.exceptions.GitlabGetError as exc:
        raise RuntimeError(f"{file_path} not found in branch {branch_name}") from exc

    original_content = base64.b64decode(f_branch.content).decode()

    if original_content.strip() == updated_content.strip():
        logger.info("No content change detected for %s; skipping commit", file_path)
        return

    f_branch.content = updated_content
    f_branch.save(branch=branch_name, commit_message=commit_message)
    logger.info("Updated %s in branch", file_path)


def _branch_has_changes(project, base_ref: str, branch_name: str) -> bool:
    """Return *True* if *branch_name* differs from *base_ref*."""

    compare = project.repository_compare(base_ref, branch_name)
    return bool(compare.get("diffs"))


def _create_or_reuse_mr(
    project,
    branch_name: str,
    base_ref: str,
    title: str,
    description: str,
):
    """Create a new MR or reuse an open one for *branch_name* → *base_ref*."""

    existing_mrs = project.mergerequests.list(source_branch=branch_name, state="opened")
    if existing_mrs:
        mr = existing_mrs[0]
        status = "already-exists"
        message = "Existing MR found"
        logger.info("Using existing MR !%s", mr.iid)
    else:
        mr = project.mergerequests.create(
            {
                "source_branch": branch_name,
                "target_branch": base_ref,
                "title": title,
                "description": description,
                "remove_source_branch": True,
            }
        )
        status = "created"
        message = "Successfully created MR"
        logger.info("Created new MR !%s", mr.iid)

    logger.info("MR URL: %s", mr.web_url)
    return mr, status, message

def _update_imagesync_file(original: str, version: str) -> str:
    yaml_rt = YAML(typ="rt")
    yaml_rt.preserve_quotes = True
    yaml_rt.indent(mapping=2, sequence=4, offset=2)
    yaml_rt.width = 4096

    try:
        doc = yaml_rt.load(original)
    except Exception:
        # Parsing failed – return original so caller can decide next step.
        return original

    if not isinstance(doc, dict):
        return original

    sync_entries = doc.get("sync")
    if not isinstance(sync_entries, list):
        return original

    modified = False
    prefix = "nv-ngc-devops.nvcr.io/nv-ngc-devops/nvsentinel"
    gpu_image_name = "nv-ngc-devops.nvcr.io/nv-ngc-devops/nvsentinel-gpu-health-monitor"

    for entry in sync_entries:
        if not isinstance(entry, dict):
            continue
        source = entry.get("source")
        if isinstance(source, str) and source.startswith(prefix):
            # Ensure tags / allow list exists
            tags_map = entry.setdefault("tags", CommentedMap())
            allow_list = tags_map.setdefault("allow", [])
            if not isinstance(allow_list, list):
                # Unexpected type – skip this entry
                continue

            # gpu health monitor tags
            if source.__eq__(gpu_image_name):
                version_dcgm_3x = version + "-dcgm-3.x"
                version_dcgm_4x = version + "-dcgm-4.x"
                if version_dcgm_3x not in allow_list:
                    allow_list.append(DoubleQuotedScalarString(version_dcgm_3x))
                    modified = True
                    logger.info("Added version %s to %s", version_dcgm_3x, source)
                if version_dcgm_4x not in allow_list:
                    allow_list.append(DoubleQuotedScalarString(version_dcgm_4x))
                    modified = True
                    logger.info("Added version %s to %s", version_dcgm_4x, source)

            elif version not in allow_list:
                allow_list.append(DoubleQuotedScalarString(version))
                modified = True
                logger.info("Added version %s to %s", version, source)

    if not modified:
        return original

    buf = StringIO()
    yaml_rt.dump(doc, buf)
    return buf.getvalue()

def _update_charts_file(original: str, version: str) -> str:

    yaml_rt = YAML(typ="rt")
    yaml_rt.preserve_quotes = True
    yaml_rt.indent(mapping=2, sequence=4, offset=2)
    yaml_rt.width = 4096

    try:
        doc = yaml_rt.load(original)
    except Exception:
        return original

    if not isinstance(doc, dict):
        return original

    charts = doc.get("charts")
    if not isinstance(charts, list):
        return original

    modified = False

    for item in charts:
        if not isinstance(item, dict):
            continue
        name = str(item.get("name", "")).lower()
        if name == "nvsentinel":
            versions = item.setdefault("versions", [])
            if not isinstance(versions, list):
                continue
            if version not in versions:
                versions.append(version)
                modified = True
                logger.info("Added version %s to nvSentinel chart", version)
            break  # nvSentinel section is unique – safe to stop looping

    if not modified:
        return original

    buf = StringIO()
    yaml_rt.dump(doc, buf)
    return buf.getvalue()

def _update_images_file(original: str, version: str) -> str:
    """Return updated images.yaml content with *version* added to nvSentinel image.

    If the nvSentinel image is not present or the version already exists, the
    *original* content is returned unchanged.
    """
    yaml_rt = YAML(typ="rt")
    yaml_rt.preserve_quotes = True
    yaml_rt.indent(mapping=2, sequence=4, offset=2)
    yaml_rt.width = 4096

    try:
        doc = yaml_rt.load(original)
    except Exception:
        return original

    if not isinstance(doc, dict):
        return original

    images = doc.get("images")
    if not isinstance(images, list):
        return original

    prefix = "nvcr.io/nv-ngc-devops/nvsentinel-"
    gpu_image_name = "nvcr.io/nv-ngc-devops/nvsentinel-gpu-health-monitor"
    modified = False

    for entry in images:
        if not isinstance(entry, dict):
            continue
        image_name = entry.get("image")
        if isinstance(image_name, str) and image_name.startswith(prefix):
            tags = entry.setdefault("tags", [])
            if not isinstance(tags, list):
                continue
            if image_name == gpu_image_name:
                for suffix in ("-dcgm-3.x", "-dcgm-4.x"):
                    tag = f"{version}{suffix}"
                    if tag not in tags:
                        tags.append(DoubleQuotedScalarString(tag))
                        modified = True
                        logger.info("Added version %s to GPU image %s", tag, image_name)
            else:
                if version not in tags:
                    tags.append(DoubleQuotedScalarString(version))
                    modified = True
                    logger.info("Added version %s to image %s", version, image_name)

    if not modified:
        return original

    buf = StringIO()
    yaml_rt.dump(doc, buf)
    return buf.getvalue()

def _update_bcp_images_file(original: str, version: str) -> str:

    yaml_rt = YAML(typ="rt")
    yaml_rt.preserve_quotes = True
    yaml_rt.indent(mapping=2, sequence=4, offset=2)
    yaml_rt.width = 4096

    try:
        doc = yaml_rt.load(original)
    except Exception:
        return original

    if not isinstance(doc, dict):
        return original

    images = doc.get("images")
    if not isinstance(images, list):
        return original

    prefix = "nvcr.io/nv-ngc-devops/nvsentinel"
    gpu_image_name = "nvcr.io/nv-ngc-devops/nvsentinel-gpu-health-monitor"
    modified = False

    for entry in images:
        if not isinstance(entry, dict):
            continue
        image_name = entry.get("image")
        if isinstance(image_name, str) and image_name.startswith(prefix):
            tags = entry.setdefault("tags", [])
            if not isinstance(tags, list):
                continue

            if image_name == gpu_image_name:
                for suffix in ("-dcgm-3.x", "-dcgm-4.x"):
                    tag = f"{version}{suffix}"
                    if tag not in tags:
                        tags.append(DoubleQuotedScalarString(tag))
                        modified = True
                        logger.info("Added version %s to GPU image %s", tag, image_name)
            else:
                if version not in tags:
                    tags.append(DoubleQuotedScalarString(version))
                    modified = True
                    logger.info("Added version %s to image %s", version, image_name)

    if not modified:
        return original

    buf = StringIO()
    yaml_rt.dump(doc, buf)
    return buf.getvalue()

def raise_ace_mr(version: str, gitlab_token: str, gitlab_url: str) -> Dict[str, Any]:
    """Perform update flow and return MR metadata for the workflow."""

    project_path = "dgxcloud/platform/release/ace"
    branch_name = f"nvsentinel-imagesync/{version}"
    imagesync_file_path = "imagesync.yaml"
    chartsync_file_path = "chartsync.yaml"
    base_ref = "main"

    try:
        project = _get_project(gitlab_url, gitlab_token, project_path)
        _ensure_branch(project, branch_name, base_ref)

        # Fetch current content
        f_branch = project.files.get(file_path=imagesync_file_path, ref=branch_name)
        original_content = base64.b64decode(f_branch.content).decode()
        updated_content = _update_imagesync_file(original_content, version)
        

        commit_message = f"chore: Add {version} to nvSentinel images in imagesync.yaml"
        _update_file_in_branch(project, branch_name, imagesync_file_path, updated_content, commit_message)

        # Update chartsync.yaml
        try:
            cs_branch = project.files.get(file_path=chartsync_file_path, ref=branch_name)
            chartsync_original = base64.b64decode(cs_branch.content).decode()
            chartsync_updated = _update_charts_file(chartsync_original, version)

            commit_message_cs = f"chore: Add {version} to nvSentinel chart in chartsync.yaml"
            _update_file_in_branch(
                project,
                branch_name,
                chartsync_file_path,
                chartsync_updated,
                commit_message_cs,
            )
        except gitlab.exceptions.GitlabGetError:
            logger.warning("chartsync.yaml not found in branch; skipping chart updates")

        # Exit early if nothing changed
        if not _branch_has_changes(project, base_ref, branch_name):
            logger.info("No differences between %s and %s – skipping MR", base_ref, branch_name)
            return {
                "status": 'no-changes',
                "message": 'No changes to commit',
                "version": version,
                "branch": branch_name,
            }

        title = f"chore: Add {version} to nvSentinel charts & images"
        description = (
            "This MR updates chartsync.yaml and imagesync.yaml to include the new nvSentinel "
            f"version {version}. "
            "JIRA: NO-REF"
        )
        mr, status, message = _create_or_reuse_mr(project, branch_name, base_ref, title, description)

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
        logger.error("Exception: %s", exc)
        _save_result(
            "/tmp/image-sync-mr-result",
            {"status": "failed", "error": str(exc)},
        )
        sys.exit(1)

def raise_nvcr_registry_mr(version: str, gitlab_token: str, gitlab_url: str) -> Dict[str, Any]:
    """Update charts.yaml and images.yaml in the nvcr-registry repo and open a MR."""

    project_path = "dgxcloud/mk8s/runai/nvcr-registry"
    branch_name = f"nvsentinel-charts/{version}"
    charts_file_path = "charts.yaml"
    images_file_path = "images.yaml"
    base_ref = "main"

    try:
        project = _get_project(gitlab_url, gitlab_token, project_path)
        _ensure_branch(project, branch_name, base_ref)

        # Update charts.yaml
        charts_file = project.files.get(file_path=charts_file_path, ref=branch_name)
        charts_original = base64.b64decode(charts_file.content).decode()
        charts_updated = _update_charts_file(charts_original, version)

        commit_message = f"chore: Add {version} to nvSentinel chart in charts.yaml"
        _update_file_in_branch(
            project,
            branch_name,
            charts_file_path,
            charts_updated,
            commit_message,
        )

        # Update images.yaml
        try:
            images_file = project.files.get(file_path=images_file_path, ref=branch_name)
            images_original = base64.b64decode(images_file.content).decode()
            images_updated = _update_images_file(images_original, version)

            commit_message_img = f"chore: Add {version} to nvSentinel images in images.yaml"
            _update_file_in_branch(
                project,
                branch_name,
                images_file_path,
                images_updated,
                commit_message_img,
            )
        except gitlab.exceptions.GitlabGetError:
            logger.warning("images.yaml not found in branch; skipping image updates")

        if not _branch_has_changes(project, base_ref, branch_name):
            logger.info("No differences between %s and %s – skipping MR", base_ref, branch_name)
            return {
                "status": 'no-changes',
                "message": 'No changes to commit',
                "version": version,
                "branch": branch_name,
            }

        title = f"chore: Add {version} to nvSentinel charts & images"
        description = (
            "This MR updates charts.yaml and images.yaml to include the new nvSentinel version "
            f"{version}."
            "JIRA: NO-REF"
        )
        mr, status, message = _create_or_reuse_mr(project, branch_name, base_ref, title, description)

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
        logger.error("Exception: %s", exc)
        _save_result(
            "/tmp/runai-charts-mr-result",
            {"status": "failed", "error": str(exc)},
        )
        sys.exit(1)

def raise_bcp_next_registry_mr(version: str, gitlab_token: str, gitlab_url: str) -> Dict[str, Any]:
    project_path = "ngcc/bcp-dot-next-registry"
    branch_name = f"nvsentinel-images/{version}"
    images_file_path = "images.yaml"
    base_ref = "main"

    try:
        project = _get_project(gitlab_url, gitlab_token, project_path)
        _ensure_branch(project, branch_name, base_ref)

        images_file = project.files.get(file_path=images_file_path, ref=branch_name)
        images_original = base64.b64decode(images_file.content).decode()
        images_updated = _update_bcp_images_file(images_original, version)

        commit_message = f"chore: Add {version} to nvSentinel images in images.yaml"
        _update_file_in_branch(
            project,
            branch_name,
            images_file_path,
            images_updated,
            commit_message,
        )

        if not _branch_has_changes(project, base_ref, branch_name):
            logger.info("No changes between %s and %s; skipping MR", base_ref, branch_name)
            return {
                "status": 'no-changes',
                "message": 'No changes to commit',
                "version": version,
                "branch": branch_name,
            }

        title = f"chore: Add {version} to nvSentinel images"
        description = (
            "This MR updates images.yaml to include the new nvSentinel image version "
            f"{version}."
            "JIRA: NO-REF"
        )
        mr, status, message = _create_or_reuse_mr(project, branch_name, base_ref, title, description)

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
        logger.error("Exception: %s", exc)
        _save_result(
            "/tmp/bcp-next-registry-mr-result",
            {"status": "failed", "error": str(exc)},
        )
        sys.exit(1)


def main() -> None:  # noqa: C901 (complexity)
    """Entry-point when executed as a script."""

    version = _get_env("VERSION")
    gitlab_url = _get_env("GITLAB_URL")
    ace_token = _get_env("ACE_GITLAB_TOKEN")
    nvcr_registry_token = _get_env("NVCR_REGISTRY_GITLAB_TOKEN")
    bcp_registry_token = _get_env("BCP_REGISTRY_GITLAB_TOKEN")

    if not version or not gitlab_url or not ace_token or not nvcr_registry_token or not bcp_registry_token:
        logger.error(
            "[image-sync] ERROR: Missing required environment variables (VERSION, GITLAB_URL, ACE_GITLAB_TOKEN, NVCR_REGISTRY_GITLAB_TOKEN, BCP_REGISTRY_GITLAB_TOKEN)"
        )
        _save_result(
            "/tmp/image-sync-mr-result",
            {
                "status": "failed",
                "error": "Missing required environment variables (VERSION, GITLAB_URL, ACE_GITLAB_TOKEN, NVCR_REGISTRY_GITLAB_TOKEN, BCP_REGISTRY_GITLAB_TOKEN)",
            },
        )
        _save_result(
            "/tmp/runai-charts-mr-result",
            {
                "status": "failed",
                "error": "Missing required environment variables (VERSION, GITLAB_URL, ACE_GITLAB_TOKEN, NVCR_REGISTRY_GITLAB_TOKEN, BCP_REGISTRY_GITLAB_TOKEN)",
            },
        )
        _save_result(
            "/tmp/bcp-next-registry-mr-result",
            {
                "status": "failed",
                "error": "Missing required environment variables (VERSION, GITLAB_URL, ACE_GITLAB_TOKEN, NVCR_REGISTRY_GITLAB_TOKEN, BCP_REGISTRY_GITLAB_TOKEN)",
            },
        )
        sys.exit(1)

    image_sync_result = raise_ace_mr(version, ace_token, gitlab_url)
    charts_result = raise_nvcr_registry_mr(version, nvcr_registry_token, gitlab_url)
    bcp_result = raise_bcp_next_registry_mr(version, bcp_registry_token, gitlab_url)

    _save_result("/tmp/ace-image-sync-mr-result", image_sync_result)
    _save_result("/tmp/nvcr-registry-mr-result", charts_result)
    _save_result("/tmp/bcp-registry-mr-result", bcp_result)


if __name__ == "__main__":
    main()