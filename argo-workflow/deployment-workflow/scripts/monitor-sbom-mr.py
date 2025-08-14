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
import requests
import time
import sys
from datetime import datetime, timezone

import logging

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S'
)
logger = logging.getLogger('monitor-sbom-mr')

def main():
    """Monitor the status of a GitLab merge request created for a deployment pattern.
    """

    # Parse input parameters from environment
    version = os.getenv('MR_VERSION')
    raw_mr_data = os.getenv('MR_DATA')
    project_path = os.getenv('PROJECT_PATH')
    gitlab_token = os.getenv('GITLAB_TOKEN')
    gitlab_url = os.getenv('GITLAB_URL')

    logger.info(f"Version: {version}")

    # Parse MR data
    try:
        mr_data = json.loads(raw_mr_data)
        logger.info("MR data JSON parsed successfully")
    except Exception as e:
        logger.error(f"Error parsing MR data: {e}")
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
        sys.exit(0)

    # Extract MR information
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

    if not all([gitlab_token, gitlab_url]):
        logger.error("Missing GitLab credentials")
        monitoring_result['final_status'] = 'failed'
        monitoring_result['message'] = 'Missing GitLab credentials'
        monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
        with open('/tmp/monitoring-result.json', 'w') as f:
            json.dump(monitoring_result, f, indent=2)
        sys.exit(1)

    # Skip monitoring if MR creation failed
    if mr_status in ['failed', 'error'] or not mr_iid:
        logger.info(f"Skipping monitoring - MR creation failed or no MR ID")
        monitoring_result['final_status'] = 'failed'
        monitoring_result['message'] = 'MR creation failed or no MR ID'
        monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
        with open('/tmp/monitoring-result.json', 'w') as f:
            json.dump(monitoring_result, f, indent=2)
        sys.exit(0)

    # Setup GitLab API
    headers = {
        'PRIVATE-TOKEN': gitlab_token,
        'Content-Type': 'application/json'
    }
    api_url = f"{gitlab_url}/api/v4/projects/{project_path}/merge_requests/{mr_iid}"

    logger.info(f"Monitoring MR at: {mr_url}")
    logger.info(f"API URL: {api_url}")

    # Monitor MR status
    max_checks = 10080  # 7 days maximum (10080 * 1 minute)

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

                    monitoring_result['final_status'] = state
                    monitoring_result['message'] = f"MR is {state}"
                    monitoring_result['mr_state'] = state
                    monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'
                    break

                # Continue monitoring
                logger.info(f"Still {state}; waiting 60s …")
                time.sleep(60)

            elif response.status_code == 404:
                logger.warning(f"MR !{mr_iid} not found - may have been deleted")
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

    else:
        # Timeout reached
        logger.warning(f"Timeout after {max_checks} minutes")
        monitoring_result['final_status'] = 'timeout'
        monitoring_result['message'] = 'Timeout reached'
        monitoring_result['completed_at'] = datetime.now(timezone.utc).isoformat() + 'Z'

    # Save final result
    with open('/tmp/monitoring-result.json', 'w') as f:
        json.dump(monitoring_result, f, indent=2)

    logger.info(f"Finished with status: {monitoring_result['final_status']}")

    return monitoring_result

if __name__ == "__main__":
    main()

