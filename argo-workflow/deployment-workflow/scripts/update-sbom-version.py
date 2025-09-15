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
from ruamel.yaml.scalarstring import DoubleQuotedScalarString

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


def _get_env(var_name: str, default: str | None = None) -> str | None:
    """Utility to fetch an environment variable returning *default* if missing or empty."""

    value = os.environ.get(var_name)
    return value if value else default

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
    ace_token = _get_env("ACE_GITLAB_TOKEN")
    release_branch = _get_env("SBOM_RELEASE_BRANCH", "main")
    manifest_template_token = _get_env("MANIFEST_TEMPLATE_GITLAB_TOKEN")
    nvcr_registry_token = _get_env("NVCR_REGISTRY_GITLAB_TOKEN")
    bcp_registry_token = _get_env("BCP_REGISTRY_GITLAB_TOKEN")
    
    user_id = _get_env("USER_ID")
    
    if not version or not gitlab_url or not manifest_template_token or not ace_token or not nvcr_registry_token or not bcp_registry_token or not user_id:
        logger.error(f"[update-sbom-version] ERROR: Missing required environment variables (VERSION, GITLAB_URL, *_GITLAB_TOKEN, USER_ID)")
        _save_result("/tmp/ace-mr-result",
            {
                "status": "failed",
                "error": "Missing required environment variables (VERSION, GITLAB_URL, *_GITLAB_TOKEN, USER_ID)",
            }
        )

        # TODO: Uncomment this when repo changes are finished
        # _save_result("/tmp/manifest-template-mr-result",
        #     {
        #         "status": "failed",
        #         "error": "Missing required environment variables (VERSION, GITLAB_URL, *_GITLAB_TOKEN, USER_ID)",
        #     }
        # )
        # _save_result(
        #     "/tmp/runai-charts-mr-result",
        #     {
        #         "status": "failed",
        #         "error": "Missing required environment variables (VERSION, GITLAB_URL, ACE_GITLAB_TOKEN, NVCR_REGISTRY_GITLAB_TOKEN, BCP_REGISTRY_GITLAB_TOKEN, USER_ID)",
        #     },
        # )
        # _save_result(
        #     "/tmp/bcp-next-registry-mr-result",
        #     {
        #         "status": "failed",
        #         "error": "Missing required environment variables (VERSION, GITLAB_URL, ACE_GITLAB_TOKEN, NVCR_REGISTRY_GITLAB_TOKEN, BCP_REGISTRY_GITLAB_TOKEN, USER_ID)",
        #     },
        # )
        sys.exit(1)

    ace_mr = raise_ace_mr(version, ace_token, gitlab_url, user_id)
    
    # TODO: Uncomment this when repo changes are finished
    # manifest_mr = raise_manifest_template_mr(version, manifest_template_token, gitlab_url, release_branch, user_id)
    # charts_result = raise_nvcr_registry_mr(version, nvcr_registry_token, gitlab_url, user_id)
    # bcp_result = raise_bcp_next_registry_mr(version, bcp_registry_token, gitlab_url, user_id)

    _save_result("/tmp/ace-mr-result", ace_mr)
    
    # TODO: Uncomment this when repo changes are finished
    #_save_result("/tmp/manifest-template-mr-result", manifest_mr)
    #_save_result("/tmp/nvcr-registry-mr-result", charts_result)
    #_save_result("/tmp/bcp-registry-mr-result", bcp_result)


def _update_ace_manifest_content(original: str, version: str) -> str:
    """Update nvSentinel version in ACE manifest.yaml."""
    logger.info(f"Updating ACE manifest.yaml with version {version}")
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

def _update_imagesync_file(original: str, version: str) -> str:
    logger.info(f"Updating imagesync.yaml with version {version}")
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
    prefix = "source-alias.io/nv-ngc-devops/nvsentinel"
    gpu_image_name = "source-alias.io/nv-ngc-devops/nvsentinel-gpu-health-monitor"

    for entry in sync_entries:
        if not isinstance(entry, dict):
            logger.info(f"Skipping update for {entry}; entry not found in imagesync.yaml")
            continue
        source = entry.get("source")
        if isinstance(source, str) and source.startswith(prefix):
            # Ensure tags / allow list exists
            tags_map = entry.setdefault("tags", CommentedMap())
            allow_list = tags_map.setdefault("allow", [])
            if not isinstance(allow_list, list):
                # Unexpected type – skip this entry
                logger.info(f"Skipping update for {entry}; allow list not found in imagesync.yaml")
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
    logger.info(f"Updating chartsync.yaml with version {version}")
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

def raise_ace_mr(version: str, ace_token: str, gitlab_url: str, user_id: str)-> Dict[str, Any]:
    project_path = "dgxcloud/platform/release/ace"
    branch_name = f"nvsentinel/{version}"
    imagesync_file_path = "imagesync.yaml"
    chartsync_file_path = "chartsync.yaml"
    manifest_file_path = "manifest.yaml"
    base_ref = "main"

    try:
        project = _get_project(gitlab_url, ace_token, project_path)
        _ensure_branch(project, branch_name, base_ref)

        commit_message = f"chore: Update nvSentinel version to {version}"

        files_to_process = [
            (manifest_file_path, _update_ace_manifest_content),
            (imagesync_file_path, _update_imagesync_file),
            (chartsync_file_path, _update_charts_file),
        ]

        actions = []
        for file_path, updater in files_to_process:
            f_branch = project.files.get(file_path=file_path, ref=branch_name)
            original_content = base64.b64decode(f_branch.content).decode()
            updated_content = updater(original_content, version)

            # Only add to commit if content has changed
            if original_content.strip() != updated_content.strip():
                actions.append(
                    {
                        "action": "update",
                        "file_path": file_path,
                        "content": updated_content,
                    }
                )

        if actions:
            project.commits.create(
                {
                    "branch": branch_name,
                    "commit_message": commit_message,
                    "actions": actions,
                }
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

def raise_manifest_template_mr(version: str, manifest_template_token: str, gitlab_url: str, release_branch: str, user_id: str)-> Dict[str, Any]:
    project_path = "dgxcloud/mk8s/dgxc/manifests-templates"
    branch_name = f"nvsentinel-{version}"

    try:
        project = _get_project(gitlab_url, manifest_template_token, project_path)
        base_ref = release_branch
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

def raise_nvcr_registry_mr(version: str, gitlab_token: str, gitlab_url: str, user_id: str) -> Dict[str, Any]:
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
        mr, status, message = _create_or_reuse_mr(project, branch_name, base_ref, title, description, user_id)

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

def raise_bcp_next_registry_mr(version: str, gitlab_token: str, gitlab_url: str, user_id: str) -> Dict[str, Any]:
    project_path = "ngcc/bcp-dot-next-registry"
    branch_name = f"nvsentinel-images/{version}"
    images_file_path = "images.yaml"
    base_ref = "main"

    try:
        project = _get_project(gitlab_url, gitlab_token, project_path)
        _ensure_branch(project, branch_name, base_ref)

        images_file = project.files.get(file_path=images_file_path, ref=branch_name)
        images_original = base64.b64decode(images_file.content).decode()
        images_updated = _update_images_file(images_original, version)

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
        mr, status, message = _create_or_reuse_mr(project, branch_name, base_ref, title, description, user_id)

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

if __name__ == "__main__":
    main()