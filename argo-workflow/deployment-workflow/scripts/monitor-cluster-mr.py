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
import logging
import os
import requests
import time
import sys
from datetime import datetime, timezone

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S'
)
logger = logging.getLogger('monitor-cluster-mr')

def main():
    """
        Monitor the status of a GitLab merge request created for a deployment pattern.
    """

    # Parse input parameters from environment
    version = os.getenv('MR_VERSION')
    raw_mr_data = os.getenv('MR_DATA')
    max_checks = int(os.getenv('MAX_TIMEOUT_IN_MINUTES', '60'))
    logger.info(f"Version: {version}")

    # Parse MR data
    try:
        mr_data = json.loads(raw_mr_data)
        logger.debug("MR data JSON parsed successfully")
    except Exception as e:
        logger.error(f"Error parsing MR data: {e}")
        result = {
            'pattern_name': 'unknown',
            'clusters': {},
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
    pattern_name = mr_data.get('pattern_name', 'unknown')
    clusters = mr_data.get('clusters', {})
    mr_iid = mr_data.get('mr_iid')
    mr_url = mr_data.get('mr_url', '')
    mr_status = mr_data.get('status', 'unknown')

    cluster_count = len(clusters)
    cluster_names_str = ', '.join(clusters.keys())

    logger.info(f"Pattern: {pattern_name} | Clusters: {cluster_count} ({cluster_names_str}) | MR: !{mr_iid}")

    # Initialize monitoring result
    monitoring_result = {
        'pattern_name': pattern_name,
        'clusters': clusters,
        'version': version,
        'mr_iid': mr_iid,
        'mr_url': mr_url,
        'initial_status': mr_status,
        'final_status': 'failed',
        'message': 'unknown',
        'started_at': datetime.now(timezone.utc).isoformat() + 'Z'
    }

    # Skip monitoring if MR creation failed
    if mr_status == 'no-changes':
        logger.info("Skipping monitoring - MR not raised")
        monitoring_result['final_status'] = 'success'
        monitoring_result['message'] = 'MR not raised'
        monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
        with open('/tmp/monitoring-result.json', 'w') as f:
            json.dump(monitoring_result, f, indent=2)
        
        cluster_data = [{"name": name, "spec-path": path} for name, path in clusters.items()]
        with open('/tmp/cluster-data.json', 'w') as f:
            json.dump(cluster_data, f, indent=2)
        sys.exit(0)
    
    elif mr_status == 'failed':
        logger.error("Skipping monitoring - MR creation failed")
        monitoring_result['final_status'] = 'failed'
        monitoring_result['message'] = 'MR creation failed'
        monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
        with open('/tmp/monitoring-result.json', 'w') as f:
            json.dump(monitoring_result, f, indent=2)
        with open('/tmp/cluster-data.json', 'w') as f:
            json.dump([], f, indent=2)
        sys.exit(1)

    # Get GitLab credentials
    gitlab_token = os.getenv('MANIFEST_GITLAB_TOKEN')
    gitlab_url = os.getenv('GITLAB_URL')

    if not all([gitlab_token, gitlab_url]):
        logger.error("Missing GitLab credentials")
        monitoring_result['final_status'] = 'failed'
        monitoring_result['message'] = 'Missing GitLab credentials'
        monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
        with open('/tmp/monitoring-result.json', 'w') as f:
            json.dump(monitoring_result, f, indent=2)
        sys.exit(1)

    # Setup GitLab API
    project_path = "dgxcloud%2Fmk8s%2Fmanifests"
    headers = {
        'PRIVATE-TOKEN': gitlab_token,
        'Content-Type': 'application/json'
    }
    api_url_base = f"{gitlab_url}/api/v4/projects/{project_path}/merge_requests/{mr_iid}"
    api_url = f"{api_url_base}?include_diverged_commits_count=true"

    logger.info(f"Monitoring MR at: {mr_url}")
    logger.debug(f"API URL: {api_url}")
    
    for check_count in range(max_checks):
        try:
            logger.info(f"Check {check_count + 1}/{max_checks}: polling status ...")

            response = requests.get(api_url, headers=headers, timeout=30)

            if response.status_code == 200:
                mr_info = response.json()
                state = mr_info.get('state', 'unknown')
                merge_status = mr_info.get('merge_status', 'unknown')

                logger.info(f"Status: {state} (merge_status: {merge_status})")

                # Check if MR is completed
                if state in ['merged', 'closed']:
                    logger.info(f"Completed: {state}")

                    monitoring_result['final_status'] = 'success'
                    monitoring_result['message'] = f"MR is {state}"
                    monitoring_result['mr_state'] = state
                    monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
                    break

                diverged_commits = mr_info.get('diverged_commits_count', 0)
                rebase_in_progress = mr_info.get('rebase_in_progress', False)
                project_id = mr_info.get('project_id')
                logger.info(f"diverged_commits_count: {diverged_commits}, rebase_in_progress: {rebase_in_progress}, project_id: {project_id}")
                if diverged_commits is None:
                    diverged_commits = 0
                if diverged_commits > 0 and not rebase_in_progress:
                    rebase_url = f"{api_url_base}/rebase"
                    logger.info(f"Attempting rebase via PUT {rebase_url}")
                    try:
                        resp = requests.put(rebase_url, headers=headers, timeout=30)
                        if resp.status_code in [200, 201, 202]:
                            logger.info(f"Triggered rebase (diverged commits: {diverged_commits})")
                        elif resp.status_code == 409:
                            logger.info("Rebase already in progress or not possible (409)")
                        else:
                            logger.warning(f"Failed to trigger rebase: HTTP {resp.status_code} {resp.text}")
                    except Exception as rebase_exc:
                        logger.error(f"Exception when attempting rebase: {rebase_exc}")

                logger.info(f"Still {state}; waiting 60s …")
                time.sleep(60)

            elif response.status_code == 404:
                logger.error(f"MR !{mr_iid} not found - may have been deleted")
                monitoring_result['final_status'] = 'failed'
                monitoring_result['message'] = 'MR not found'
                monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
                break

            else:
                logger.warning(f"HTTP {response.status_code} from GitLab; retrying …")
                time.sleep(60)

        except Exception as e:
            logger.error(f"Exception: {str(e)}; retrying in 60s …")
            time.sleep(60)

    else :
        logger.warning(f"Timeout after {max_checks} minutes")
        monitoring_result['final_status'] = 'failed'
        monitoring_result['message'] = f'MR is not merged or closed after {max_checks} minutes'
        monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'

    # Save final result
    with open('/tmp/monitoring-result.json', 'w') as f:
        json.dump(monitoring_result, f, indent=2)
    
    # Transform clusters dict into list of objects with name and spec-path
    cluster_data = [{"name": name, "spec-path": path} for name, path in clusters.items()]
    with open('/tmp/cluster-data.json', 'w') as f:
        json.dump(cluster_data, f, indent=2)

    logger.info(f"Finished with status: {monitoring_result['final_status']}")

    if monitoring_result['final_status'] == 'failed':
        sys.exit(1)

if __name__ == "__main__":
    main()