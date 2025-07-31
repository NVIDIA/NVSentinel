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
import time
import sys
from datetime import datetime, timezone
import gitlab

def manage_tag(project, tag_name, ref):
    """Create or force-update a tag in the given project."""
    try:
        # Delete if it already exists (GitLab returns 404 if not present)
        project.tags.delete(tag_name)
    except gitlab.exceptions.GitlabDeleteError:
        pass
    except gitlab.exceptions.GitlabGetError:
        pass
    project.tags.create({'tag_name': tag_name, 'ref': ref})

def main():
    # Parse input parameters from environment
    version = os.getenv('MR_VERSION')
    raw_mr_data = os.getenv('MR_DATA')
    
    print(f"[monitor-mr] Version: {version}")

    # Parse MR data
    try:
        mr_data = json.loads(raw_mr_data)
        print("[monitor-mr] MR data JSON parsed successfully")
    except Exception as e:
        print(f"[monitor-mr] Error parsing MR data: {e}")
        result = {
            'version': version,
            'mr_iid': None,
            'mr_url': '',
            'initial_status': 'json-parse-error',
            'final_status': 'failed',
            'message': f'JSON parsing error: {str(e)}',
            'started_at': datetime.now(timezone.utc).isoformat() + 'Z',
            'completed_at': datetime.now(timezone.utc).isoformat() + 'Z'
        }
        with open('/tmp/monitoring-result.json', 'w') as f:
            json.dump(result, f, indent=2)
        sys.exit(1)

    # Extract MR information (now pattern-based, not cluster-based)
    mr_iid = mr_data.get('mr_iid')
    mr_url = mr_data.get('mr_url', '')
    mr_status = mr_data.get('status', 'unknown')

    # Initialize monitoring result
    monitoring_result = {
        'version': version,
        'mr_iid': mr_iid,
        'mr_url': mr_url,
        'initial_status': mr_status,
        'final_status': 'failed',
        'message': 'unknown',
        'started_at': datetime.now(timezone.utc).isoformat() + 'Z'
    }

    # Get GitLab credentials
    gitlab_token = os.getenv('NVSENTINEL_COMPONENT_GITLAB_TOKEN')
    gitlab_url = os.getenv('GITLAB_URL')
    
    if not all([gitlab_token, gitlab_url]):
        print("[monitor-mr] ERROR: Missing GitLab credentials")
        monitoring_result['final_status'] = 'failed'
        monitoring_result['message'] = 'Missing GitLab credentials'
        monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
        with open('/tmp/monitoring-result.json', 'w') as f:
            json.dump(monitoring_result, f, indent=2)
        sys.exit(1)
    
    # Setup GitLab API
    project_path = "dgxcloud/mk8s/components/nvsentinel"
    gl = gitlab.Gitlab(gitlab_url, private_token=gitlab_token)
    project = gl.projects.get(project_path)
    branch_name = mr_data.get('branch') or f"nvsentinel/{version}"

    max_checks = 10080  # 7 days maximum (10080 * 1 minute)
    
    try:
        # Handle case with no MR immediately
        if not mr_iid:
            manage_tag(project, version, ref=branch_name)
            print(f"[monitor-mr] Tag {version} created (no MR)")
            monitoring_result['final_status'] = 'completed'
            monitoring_result['message'] = 'No MR created; tag pushed'
            monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
        else:
            print(f"[monitor-mr] Monitoring MR at: {mr_url}")
            for check_count in range(max_checks):
                print(f"[monitor-mr] Polling status ...", flush=True)
                try:
                    mr_obj = project.mergerequests.get(mr_iid)
                except gitlab.exceptions.GitlabGetError as e:
                    if e.response_code == 404:
                        print(f"MR !{mr_iid} not found - may have been deleted")
                        monitoring_result['final_status'] = 'failed'
                        monitoring_result['message'] = 'MR not found'
                        monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
                        break
                    else:
                        print(f"[monitor-mr] GitLab error {e.response_code}; retrying …")
                        time.sleep(60)
                        continue

                state = mr_obj.state
                merge_status = getattr(mr_obj, 'merge_status', 'unknown')
                print(f"[monitor-mr] Status: {state} (merge_status: {merge_status})")

                if state in ['merged', 'closed']:
                    print(f"[monitor-mr] Completed: {state}")
                    monitoring_result['final_status'] = 'completed'
                    monitoring_result['message'] = f"MR is {state}"
                    monitoring_result['mr_state'] = state
                    monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
                    manage_tag(project, version, ref=branch_name)
                    print(f"[monitor-mr] Tag {version} created")
                    break

                print(f"[monitor-mr] Still {state}; waiting 60s …")
                time.sleep(60)
            else:
                print(f"[monitor-mr] Timeout after {max_checks} minutes")
                monitoring_result['final_status'] = 'timeout'
                monitoring_result['message'] = 'Timeout reached'
                monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'

    except Exception as e:
        print(f"[monitor-mr] Exception: {str(e)}")
        sys.exit(1)

    # Save final result
    with open('/tmp/monitoring-result.json', 'w') as f:
        json.dump(monitoring_result, f, indent=2)

    print(f"[monitor-mr] Finished with status: {monitoring_result['final_status']}")

if __name__ == "__main__":
    main()