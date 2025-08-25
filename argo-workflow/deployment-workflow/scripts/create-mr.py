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
import logging
import os
import subprocess
import json
import tempfile
from datetime import datetime
from typing import List, Dict, Any, Optional
from pathlib import Path
import requests
from ruamel.yaml import YAML
from ruamel.yaml.comments import CommentedMap
from ruamel.yaml.scalarstring import PreservedScalarString
import re
import unicodedata


# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S'
)
logger = logging.getLogger('create-mr')

def run_command(cmd: List[str], cwd: Optional[str] = None, check: bool = True) -> str:
    """Run a command and return its output"""
    try:
        process = subprocess.run(
            cmd,
            cwd=cwd,
            capture_output=True,
            text=True,
            check=check
        )
        return process.stdout.strip()
    except subprocess.CalledProcessError as e:
        logger.error(f"Error running command: {' '.join(cmd)}")
        logger.error(f"Error output: {e.stderr}")
        if check:
            raise
        return ""

def _preserve_scalar(val):
    if isinstance(val, str) and "\n" in val:
        return PreservedScalarString(val)
    return val

def _merge(dst, src):
    for key, val in src.items():
        if isinstance(val, dict):
            sub_dst = dst.get(key)
            if not isinstance(sub_dst, dict):
                sub_dst = CommentedMap()
            dst[key] = sub_dst
            _merge(sub_dst, val)
        else:
            dst[key] = _preserve_scalar(val)

def safe_json_string(s: Any) -> str:
    """Escape a string to be safely included in JSON"""
    if not isinstance(s, str):
        s = str(s)
    return s.replace('\n', '\\n').replace('\r', '\\r').replace('\t', '\\t').replace('"', '\\"').replace('\\', '\\\\')

def pattern_to_branch(pattern: str, version: str) -> str:
    """Convert a pattern name to a valid branch name"""
    slug = pattern.lower()
    slug = unicodedata.normalize('NFKD', slug)
    slug = re.sub(r"[^a-z0-9-_]+", "-", slug)
    slug = re.sub(r"-+", "-", slug).strip("-")
    return f"nvsentinel-workflow/{slug}/{version}"

def setup_git(repo_dir: str):
    """Configure git settings"""
    run_command(['git', 'config', 'user.email', 'nvsentinel-bot@nvidia.com'], cwd=repo_dir)
    run_command(['git', 'config', 'user.name', 'NVSentinel Bot'], cwd=repo_dir)

def check_branch_exists(repo_dir: str, branch_name: str) -> bool:
    """Check if branch exists remotely"""
    output = run_command(['git', 'ls-remote', '--heads', 'origin', branch_name], 
                        cwd=repo_dir, check=False)
    return bool(output.strip())

def setup_branch(repo_dir: str, branch_name: str):
    """Checkout <branch_name> based on current main. If branch exists remotely it is reset to origin/main to pick up new files."""
    try:
        logger.info("Checking out and updating main branch …")
        run_command(['git', 'checkout', 'main'], cwd=repo_dir)
        run_command(['git', 'pull', '--ff-only', 'origin', 'main'], cwd=repo_dir)

        # Delete any local copy of the working branch so we always start fresh
        run_command(['git', 'branch', '-D', branch_name], cwd=repo_dir, check=False)

        branch_exists = check_branch_exists(repo_dir, branch_name)
        logger.info(f"Branch {branch_name} exists remotely: {branch_exists}")

        if branch_exists:
            # Create local branch that tracks the remote and hard-reset it to origin/main
            run_command(['git', 'checkout', '-B', branch_name, f'origin/{branch_name}'], cwd=repo_dir)
            logger.info(f"Resetting {branch_name} to origin/main …")
            run_command(['git', 'reset', '--hard', 'origin/main'], cwd=repo_dir)
        else:
            run_command(['git', 'checkout', '-b', branch_name], cwd=repo_dir)

        current_branch = run_command(['git', 'rev-parse', '--abbrev-ref', 'HEAD'], cwd=repo_dir)
        logger.info(f"Currently on branch: {current_branch}")
    except Exception as e:
        logger.error(f"Error in setup_branch: {e}")
        raise

def update_cluster_spec(file_path, version, pattern_spec):
    """Update a cluster spec file with NVSentinel configurations
    """
    try:
        logger.info(f"Processing spec file: {file_path}")
        yaml_rt = YAML()
        yaml_rt.preserve_quotes = True
        yaml_rt.indent(mapping=2, sequence=4, offset=2)
        yaml_rt.width = 4096
        yaml_rt.split_lines = False
        yaml_rt.map_indent = 2
        yaml_rt.sequence_indent = 4

        nvsentinel_entry = {
            'release': 'nvsentinel',
            'revision': f"{version}",
            'url': 'https://gitlab-master.nvidia.com/dgxcloud/mk8s/components/nvsentinel.git'
        }

        if not Path(file_path).exists():
            logger.error(f"Spec missing: {file_path}")
            return False

        with open(file_path, 'r') as f:
            doc = yaml_rt.load(f)

        if not isinstance(doc, dict):
            logger.warning(f"Skipping non-yaml file: {file_path}")
            return False

        # Update spec sections
        spec_section = doc.setdefault('spec', {})
        
        if pattern_spec:
            _merge(spec_section, pattern_spec)

        templates_section = spec_section.setdefault('templates', {})
        include_list = templates_section.setdefault('include', [])

        if not isinstance(include_list, list):
            include_list = []
            templates_section['include'] = include_list

        # Update nvsentinel entry
        include_list[:] = [item for item in include_list 
                            if not (isinstance(item, dict) and item.get('release') == 'nvsentinel')]
        include_list.append(nvsentinel_entry)
        
        logger.info(f"Saving changes to {file_path}")
        with open(file_path, 'w') as f:
            yaml_rt.dump(doc, f)
        return True
    except Exception as e:
        logger.error(f"Error updating cluster spec {file_path}: {str(e)}")
        raise

def create_or_find_mr(gitlab_url: str, project_path: str, headers: Dict[str, str],
                    branch_name: str, user_id: str, pattern_name: str, version: str,
                    cluster_names: List[str], pattern_spec: Dict[str, Any]) -> Dict[str, Any]:
    """Create or find an existing merge request"""
    description_lines = [
        f"NVSentinel Update → {version}",
        f"Pattern: {pattern_name}",
        "",
        "Clusters:"
    ]
    description_lines.extend([f"- {n}" for n in cluster_names])

    if pattern_spec:
        description_lines.extend([
            "",
            "The following configurations will be set/updated as per pattern configuration:"
        ])
        for flag_name, flag_value in pattern_spec.items():
            description_lines.append(f"- {flag_name}: {flag_value}")
        description_lines.extend([
            "",
            "Note: Existing configurations not mentioned above will be preserved."
        ])

    description_lines.extend([
        "",
        "JIRA: NO-REF"
    ])

    mr_data = {
        'source_branch': branch_name,
        'target_branch': 'main',
        'title': f"chore: Update NVSentinel to {version} (pattern: {pattern_name})",
        'description': "\n".join(description_lines),
        'remove_source_branch': True,
        'assignee_id': user_id
    }

    response = requests.post(
        f"{gitlab_url}/api/v4/projects/{project_path}/merge_requests",
        headers=headers,
        json=mr_data
    )

    if response.status_code == 201:
        mr_info = response.json()
        return {
            'pattern_name': pattern_name,
            'clusters': cluster_names,
            'version': version,
            'status': 'created',
            'mr_url': mr_info.get('web_url'),
            'mr_iid': mr_info.get('iid'),
            'branch': branch_name,
            'created_at': datetime.utcnow().isoformat() + 'Z'
        }
    elif response.status_code == 409:
        # Find existing MR
        list_response = requests.get(
            f"{gitlab_url}/api/v4/projects/{project_path}/merge_requests?source_branch={branch_name}",
            headers=headers
        )
        
        if list_response.status_code == 200:
            existing_mrs = list_response.json()
            if existing_mrs:
                mr_info = existing_mrs[0]
                return {
                    'pattern_name': pattern_name,
                    'clusters': cluster_names,
                    'version': version,
                    'status': 'existing',
                    'mr_url': mr_info.get('web_url'),
                    'mr_iid': mr_info.get('iid'),
                    'branch': branch_name,
                    'mr_state': mr_info.get('state', 'unknown')
                }

    return {
        'pattern_name': pattern_name,
        'clusters': cluster_names,
        'version': version,
        'status': 'failed',
        'error': f"HTTP {response.status_code} - Failed to create MR",
        'branch': branch_name
    }

    
def main():
    version = os.environ.get('VERSION')
    gitlab_token = os.environ.get('MANIFEST_GITLAB_TOKEN')
    gitlab_url = os.environ.get('GITLAB_URL')
    clusters_json = os.environ.get('CLUSTERS')
    user_id = os.environ.get('USER_ID')

    if not all([version, gitlab_token, gitlab_url, clusters_json, user_id]):
        logger.error("Missing required environment variables")
        return

    try:
        pattern_data_list = json.loads(clusters_json)
    except json.JSONDecodeError as e:
        logger.error(f"Failed to parse clusters JSON: {e}")
        return

    project_path = "dgxcloud%2Fmk8s%2Fmanifests"
    headers = {
        'PRIVATE-TOKEN': gitlab_token,
        'Content-Type': 'application/json'
    }

    mr_results = []
    repo_url = f"{gitlab_url}/dgxcloud/mk8s/manifests.git"
    auth_url = repo_url.replace('https://', f'https://oauth2:{gitlab_token}@')

    with tempfile.TemporaryDirectory() as temp_dir:
        logger.info("Cloning manifests repository …")
        run_command(['git', 'clone', auth_url, temp_dir])
        setup_git(temp_dir)

        for pattern_data in pattern_data_list:
            pattern_name = pattern_data.get('pattern_name', 'unknown')
            pattern_info = pattern_data.get('pattern_info', {})
            clusters = pattern_data.get('clusters', [])
            
            branch_name = pattern_to_branch(pattern_name, version)
            cluster_names = [c['name'] for c in clusters]
            spec_paths = [c.get('spec_file_path') for c in clusters if c.get('spec_file_path')]

            logger.info(f"Processing pattern: {pattern_name} ({len(cluster_names)} clusters)")

            if not spec_paths:
                mr_results.append({
                    'pattern_name': pattern_name,
                    'clusters': cluster_names,
                    'version': version,
                    'status': 'failed',
                    'error': 'No spec file paths provided'
                })
                continue

            try:
                setup_branch(temp_dir, branch_name)
                
                # Update all cluster specs
                pattern_spec = pattern_info.get('spec', {})
                if not isinstance(pattern_spec, dict):
                    raise TypeError(f"pattern_info.spec must be a dict, got {type(pattern_spec).__name__}")
                
                logger.info(f"Processing {len(spec_paths)} spec files")
                for spec_path in spec_paths:
                    full_path = os.path.join(temp_dir, spec_path)
                    if update_cluster_spec(full_path, version, pattern_spec):
                        logger.info(f"Adding modified file to git: {spec_path}")
                        run_command(['git', 'add', spec_path], cwd=temp_dir)

                # Check for changes
                git_status = run_command(['git', 'status', '--porcelain'], cwd=temp_dir, check=False)
                
                if not git_status:
                    logger.info("No changes detected in git status")
                    mr_results.append({
                        'pattern_name': pattern_name,
                        'clusters': cluster_names,
                        'version': version,
                        'status': 'no-changes',
                        'message': 'No changes to commit'
                    })
                    continue

                # Commit changes
                commit_msg = f"chore: Update NVSentinel to {version} (pattern: {pattern_name})"
                logger.info(f"Committing changes with message: {commit_msg}")
                run_command(['git', 'commit', '-m', commit_msg], cwd=temp_dir)

                # Push changes
                try:
                    logger.info(f"Pushing branch {branch_name}")
                    run_command(['git', 'push', '--force', 'origin', branch_name], cwd=temp_dir)
                except subprocess.CalledProcessError as e:
                    if "non-fast-forward" in str(e.stderr):
                        logger.info("Non-fast-forward push detected – remote branch is ahead/diverged")
                        logger.info("Deleting remote branch and retrying push …")
                        # Delete the remote branch first to avoid divergence, then push fresh copy
                        run_command(['git', 'push', 'origin', '--delete', branch_name], cwd=temp_dir)
                        logger.info("Remote branch deleted; pushing clean branch …")
                        run_command(['git', 'push', 'origin', branch_name], cwd=temp_dir)
                    else:
                        raise

                # Create or find MR
                logger.info("Creating or finding merge request")
                mr_result = create_or_find_mr(
                    gitlab_url, project_path, headers,
                    branch_name, user_id, pattern_name, version,
                    cluster_names, pattern_spec
                )
                mr_results.append(mr_result)
                logger.info(f"MR result: {mr_result}")

            except Exception as e:
                logger.error(f"Error processing pattern {pattern_name}: {str(e)}")
                mr_results.append({
                    'pattern_name': pattern_name,
                    'clusters': cluster_names,
                    'version': version,
                    'status': 'failed',
                    'error': safe_json_string(str(e)),
                    'branch': branch_name
                })

    # Save results
    with open('/tmp/mr-results.json', 'w') as f:
        json.dump(mr_results, f, indent=2)

    created = len([r for r in mr_results if r['status'] == 'created'])
    existing = len([r for r in mr_results if r['status'] == 'existing'])
    failed = len([r for r in mr_results if r['status'] == 'failed'])
    logger.info(f"Completed: {created} created, {existing} existing, {failed} failed")

if __name__ == '__main__':
    main() 