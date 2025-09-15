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
import os
import sys
import json
import time
import logging
import argparse
from typing import Dict
import gitlab
from concurrent.futures import ThreadPoolExecutor, as_completed

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S'
)
logger = logging.getLogger('run-autotest')

TIMEOUT_IN_MINUTES = 240
POLL_INTERVAL = 60

def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description='Trigger autotest pipeline')
    parser.add_argument('--mr-data', type=str, required=True,
                       help='JSON data containing MR information')
    parser.add_argument('--version', type=str, required=True,
                       help='Version being deployed')
    return parser.parse_args()

def get_gitlab_client(gitlab_url: str, token: str) -> gitlab.Gitlab:
    """
    Create and return a GitLab client instance
    """
    try:
        gl = gitlab.Gitlab(url=gitlab_url, private_token=token)
        gl.auth()
        return gl
    except gitlab.exceptions.GitlabAuthenticationError:
        logger.error("Failed to authenticate with GitLab")
        sys.exit(1)
    except Exception as e:
        logger.error(f"Failed to initialize GitLab client: {str(e)}")
        sys.exit(1)

def get_project_id(gl: gitlab.Gitlab, repo_path: str) -> str:
    """
    Get project ID from repository path
    repo_path format: dgxcloud/mk8s/autotest
    """
    try:
        repo_path = repo_path.replace('.git', '')
        project = gl.projects.get(repo_path)
        return str(project.id)
    except gitlab.exceptions.GitlabGetError:
        logger.error(f"Failed to find project: {repo_path}")
        sys.exit(1)
    except Exception as e:
        logger.error(f"Error getting project ID: {str(e)}")
        sys.exit(1)

def wait_for_pipeline(pipeline) -> Dict:
    """
    Wait for pipeline to complete and return final status
    """

    for minutes in range(TIMEOUT_IN_MINUTES):
        pipeline.refresh()
        current_status = pipeline.status

        logger.info(f"Pipeline status: {current_status}")

        if current_status in ["success", "failed", "canceled", "skipped"]:
            return {
                "id": pipeline.id,
                "status": pipeline.status,
                "web_url": pipeline.web_url,
                "duration": pipeline.duration,
                "started_at": pipeline.started_at,
                "finished_at": pipeline.finished_at
            }

        logger.info(f"Will check again in {POLL_INTERVAL} seconds")
        time.sleep(POLL_INTERVAL)

    return {
        "id": pipeline.id,
        "status": "timeout",
        "web_url": pipeline.web_url,
        "duration": None,
        "started_at": pipeline.started_at,
        "finished_at": None
    }


def trigger_pipeline(gl: gitlab.Gitlab, project_id: str, cluster_path: str) -> Dict:
    """
    Trigger the autotest pipeline with specified variables and wait for completion
    
    Args:
        gl: GitLab client instance
        project_id: ID of the autotest project
        cluster_path: Path of the cluster to test
    """
    
    try:
        # Get project
        project = gl.projects.get(project_id)
        
        # Set up pipeline variables exactly as defined in GitLab UI
        variables = {
            "ENVIRONMENT": "non-prod",
            "API_ENVIRONMENT": "dev",
            "CLUSTER_TYPE": "MK8S",
            "CLUSTER_NAME": cluster_path,
            "RUN_AUTOTEST": "true",
            "TEST_SUITE": "k8s-platform/functional",
            "AUTOTEST_LOG_LEVEL": "default"
        }
        
        # Create pipeline
        pipeline_data = {
            'ref': 'skip_job', # to skip health check job
            'variables': [{'key': k, 'value': str(v)} for k, v in variables.items()]
        }
        
        logger.info(f"Triggering pipeline for cluster: {cluster_path} with configurations:\n {pipeline_data}")
        pipeline = project.pipelines.create(pipeline_data)
        
        logger.info(f"Pipeline created with ID: {pipeline.id}")
        return pipeline
        
    except gitlab.exceptions.GitlabCreateError as e:
        logger.error(f"Failed to create pipeline: {str(e)}")
        sys.exit(1)
    except Exception as e:
        logger.error(f"Unexpected error: {str(e)}")
        sys.exit(1)
def run_pipeline_for_cluster(cluster_path: str, gl: gitlab.Gitlab, project_id: str) -> Dict:
    """Trigger and monitor a pipeline for a single cluster and return its final result dict"""
    try:
        pipeline = trigger_pipeline(gl, project_id, cluster_path)
        logger.info(f"Pipeline triggered for {cluster_path}: {pipeline.web_url}")
        final_result = wait_for_pipeline(pipeline)
        return {
            "cluster_path": cluster_path,
            "pipeline_id": final_result["id"],
            "pipeline_url": final_result["web_url"],
            "status": final_result["status"],
            "duration": final_result.get("duration", 0),
            "started_at": final_result.get("started_at"),
            "finished_at": final_result.get("finished_at")
        }
    except Exception as e:
        logger.error(f"Error processing cluster {cluster_path}: {str(e)}")
        return {
            "cluster_path": cluster_path,
            "status": "error",
            "error": str(e)
        }

def main():
    gitlab_token = os.environ.get("GITLAB_TOKEN")
    gitlab_url = os.environ.get("GITLAB_URL", "https://gitlab-master.nvidia.com")
    cluster_data = os.environ.get("CLUSTER_DATA")
    
    if not all([gitlab_token, cluster_data]):
        logger.error("Required environment variables are missing")
        logger.error("Required: GITLAB_TOKEN, CLUSTER_DATA")
        sys.exit(1)
        
    gl = get_gitlab_client(gitlab_url, gitlab_token)
    
    repo_path = "dgxcloud/mk8s/autotest"
    project_id = get_project_id(gl, repo_path)
    
    try:
        mr_data_parsed = json.loads(cluster_data)
        
        # Build a list of MR dictionaries regardless of the original format
        if isinstance(mr_data_parsed, list):
            if not mr_data_parsed:
                logger.error("Error: CLUSTER_DATA list is empty")
                sys.exit(1)
            mr_data_list = mr_data_parsed
        elif isinstance(mr_data_parsed, dict):
            mr_data_list = [mr_data_parsed]
        else:
            logger.error(f"Error: Unexpected CLUSTER_DATA type {type(mr_data_parsed)}")
            sys.exit(1)
        
        # Extract and normalise cluster paths
        cluster_paths = []
        for mr_data in mr_data_list:
            cluster_path = mr_data.get("spec-path")
            if not cluster_path:
                logger.warning("Warning: spec-path missing in one of the MR entries, skipping")
                continue
            if cluster_path.startswith("clusters/"):
                cluster_path = cluster_path[len("clusters/"):]
            if cluster_path.endswith("/cluster-spec.yaml"):
                cluster_path = cluster_path[:-len("/cluster-spec.yaml")]
            cluster_paths.append(cluster_path)
        
        if not cluster_paths:
            logger.error("Error: No valid cluster paths found in CLUSTER_DATA")
            sys.exit(1)

    except json.JSONDecodeError:
        logger.error("Error: Invalid JSON in CLUSTER_DATA")
        sys.exit(1)

    # wait for the deployment to be completed
    logger.info("Deployment completed, waiting 10 minutes before running autotest pipelines")
    time.sleep(600)
    try:
        results = []
        all_success = True
        max_workers = len(cluster_paths)
        logger.info(f"Running pipelines in parallel using up to {max_workers} workers")

        with ThreadPoolExecutor(max_workers=max_workers) as executor:
            future_to_cluster = {executor.submit(run_pipeline_for_cluster, cp, gl, project_id): cp for cp in cluster_paths}
            for future in as_completed(future_to_cluster):
                res = future.result()
                results.append(res)
                status = res.get("status")
                cluster_path = res.get("cluster_path")
                if status != "success" and status != "canceled":
                    all_success = False
                    logger.error(f"Pipeline for {cluster_path} finished with status: {status}")
                else:
                    logger.info(f"Pipeline for {cluster_path} succeeded in {res.get('duration', 0)} seconds")

        # Write aggregated results
        with open("/tmp/autotest-result.json", "w") as f:
            json.dump(results, f)

        if all_success:
            sys.exit(0)
        else:
            sys.exit(1)
            
    except Exception as e:
        logger.error(f"Pipeline execution failed: {str(e)}")
        sys.exit(1)

if __name__ == "__main__":
    main()