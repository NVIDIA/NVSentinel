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
from datetime import datetime, timezone
import sys
import base64
import gitlab

def save_result(result):
    """Save result to the output file"""
    with open('/tmp/release-mr-result', 'w') as f:
        json.dump(result, f, indent=2)

def get_release_file(version) -> str:
    """Create the release file"""
    release_content = "\n".join([
        "mk8s:",
        "  components:",
        "    infra:",
        "      nvsentinel:",
        f"        version: {version}",
        '        deployment-order: "250"',
        "        source:",
        f"          revision: {version}",
        "          url: https://gitlab-master.nvidia.com/dgxcloud/mk8s/components/nvsentinel.git",
        "  version: 0.0.1"
    ])
    
    return release_content

def main():
    
    version = os.environ.get('VERSION')
    gitlab_token = os.environ.get('NVSENTINEL_COMPONENT_GITLAB_TOKEN')
    gitlab_url = os.environ.get('GITLAB_URL')

    if not all([version, gitlab_token, gitlab_url]):
        save_result({'status': 'failed', 'error': 'Missing required environment variables'})
        sys.exit(1)

    project_path = "dgxcloud/mk8s/components/nvsentinel"
    branch_name = f"nvsentinel/{version}"

    try:
        gl = gitlab.Gitlab(gitlab_url, private_token=gitlab_token)
        project = gl.projects.get(project_path)

        try:
            project.branches.get(branch_name)
            print(f"[update-release-version] Branch {branch_name} already exists")
        except gitlab.exceptions.GitlabGetError:
            project.branches.create({'branch': branch_name, 'ref': 'main'})
            print(f"[update-release-version] Created branch {branch_name} from main")

        # Build release file content
        release_content = get_release_file(version)

        file_path = 'release-nvsentinel.yaml'
        commit_message = f'chore: Update nvsentinel release to {version}'
        needs_commit = True

        try:
            f = project.files.get(file_path=file_path, ref=branch_name)
            existing_content = base64.b64decode(f.content).decode()
            if existing_content.strip() == release_content.strip():
                needs_commit = False
                print("[update-release-version] No changes detected in release file")
        except gitlab.exceptions.GitlabGetError:
            pass

        if needs_commit:
            file_data = {
                'file_path': file_path,
                'branch': branch_name,
                'content': release_content,
                'commit_message': commit_message,
            }
            if 'f' in locals():
                f.content = release_content
                f.save(branch=branch_name, commit_message=commit_message)
            else:
                project.files.create(file_data)
            print("[update-release-version] Release file committed")


        # Create or retrieve merge request
        mrs = project.mergerequests.list(source_branch=branch_name, state='opened')
        if mrs:
            mr = mrs[0]
            status = 'already-exists'
            message = 'Existing MR found'
        else:
            mr = project.mergerequests.create({
                'source_branch': branch_name,
                'target_branch': 'main',
                'title': f'chore: Update nvsentinel version to {version}',
                'description': f'This MR updates the nvsentinel version to {version}.',
                'remove_source_branch': True
            })
            status = 'created'
            message = 'Successfully created MR'

        result = {
            'version': version,
            'status': status,
            'message': message,
            'mr_url': mr.web_url,
            'mr_iid': mr.iid,
            'branch': branch_name,
            'created_at': datetime.now(timezone.utc).isoformat() + 'Z'
        }
        save_result(result)
        print(f"[update-release-version] Result saved: {status}")
        if status == 'failed':
            sys.exit(1)

    except Exception as e:
        print(f"[update-release-version] Exception encountered: {e}")
        error_result = {
            'status': 'failed',
            'message': str(e),
            'version': version,
            'branch': branch_name
        }
        save_result(error_result)
        sys.exit(1)
    print(f"[update-release-version] Finished with output: {result}")


if __name__ == '__main__':
    main()