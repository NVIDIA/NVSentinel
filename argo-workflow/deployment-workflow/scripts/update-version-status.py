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
import subprocess
from datetime import datetime, timezone
import shutil
import sys
from dataclasses import dataclass
from enum import Enum
from kubernetes import client, config
from kubernetes.client.rest import ApiException
from typing import List

class MRStatus(Enum):
    CLOSED = 'closed'
    MERGED = 'merged'
    FAILED = 'failed'
    TIMEOUT = 'timeout'

@dataclass
class StatusSummary:
    closed: int = 0
    merged: int = 0
    failed: int = 0
    timeout: int = 0
    total: int = 0

    def __str__(self) -> str:
        return (
            f"MR Results Summary:\n"
            f"  Closed: {self.closed}\n"
            f"  Merged: {self.merged}\n"
            f"  Failed: {self.failed}\n"
            f"  Timeout: {self.timeout}\n"
            f"  Total: {self.total}"
        )

def run_command(cmd: List[str], check: bool = True) -> str:
    """Run a command and return its output"""
    try:
        process = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            check=check
        )
        return process.stdout.strip()
    except subprocess.CalledProcessError as e:
        print(f"Error running command: {' '.join(cmd)}")
        print(f"Error output: {e.stderr}")
        if check:
            raise
        return ""

def get_current_timestamp() -> str:
    """Return ISO-8601 timestamp in UTC with explicit Z suffix (timezone-aware)."""
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace('+00:00', 'Z')

def ensure_kubectl() -> bool:
    """Install kubectl if it's not already available in the container.
    Uses Python's urllib so we don't rely on curl/wget binaries."""
    if shutil.which('kubectl'):
        print("kubectl already present – skipping installation")
        return True

    print("kubectl not found – downloading stable release …")
    import urllib.request, os, stat

    # Determine version (fallback to hard-coded stable if network to txt fails)
    try:
        with urllib.request.urlopen('https://dl.k8s.io/release/stable.txt', timeout=10) as r:
            version = r.read().decode().strip()
    except Exception as e:
        print(f"Warning: could not fetch stable.txt: {e}; using v1.29.0")
        version = 'v1.29.0'

    url = f"https://dl.k8s.io/release/{version}/bin/linux/amd64/kubectl"
    dest = '/usr/local/bin/kubectl'
    tmp_path = '/tmp/kubectl'

    try:
        print(f"Downloading {url} …")
        with urllib.request.urlopen(url, timeout=30) as resp, open(tmp_path, 'wb') as out:
            shutil.copyfileobj(resp, out)

        # Make executable and move into PATH
        os.chmod(tmp_path, stat.S_IRUSR | stat.S_IWUSR | stat.S_IXUSR |
                            stat.S_IRGRP | stat.S_IXGRP |
                            stat.S_IROTH | stat.S_IXOTH)
        shutil.move(tmp_path, dest)
        print(f"kubectl {version} installed at {dest}")
        return True
    except Exception as e:
        print(f"Failed to install kubectl: {e}")
        return False

def load_k8s_config() -> None:
    """Load Kubernetes configuration (prefers in-cluster, falls back to local kubeconfig)."""
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()

def summarize_mr_results(mr_results_json: str) -> StatusSummary:
    """Summarize MR results from JSON string"""
    summary = StatusSummary()
    
    try:
        results = json.loads(mr_results_json)
        if not isinstance(results, list):
            print("Warning: MR results is not a list")
            return summary

        summary.total = len(results)
        for result in results:
            status = result.get('final_status', '').lower()
            if status == MRStatus.CLOSED.value:
                summary.closed += 1
            elif status == MRStatus.MERGED.value:
                summary.merged += 1
            elif status == MRStatus.FAILED.value:
                summary.failed += 1
            elif status == MRStatus.TIMEOUT.value:
                summary.timeout += 1

    except json.JSONDecodeError as e:
        print(f"Error parsing MR results JSON: {e}")
    except Exception as e:
        print(f"Error processing MR results: {e}")

    return summary

def update_processed_tags(version: str, timestamp: str) -> bool:
    """Update the processed tags ConfigMap using the Kubernetes API"""
    try:
        v1 = client.CoreV1Api()
        cm = v1.read_namespaced_config_map(
            name="nvsentinel-processed-tags",
            namespace="nvsentinel-system",
        )

        current_content = cm.data.get("processed-tags.txt", "") if cm.data else ""
        updated_lines = [
            line for line in current_content.splitlines()
            if not line.startswith(f"{version}:")
        ]
        updated_lines.append(f"{version}:{timestamp}:completed")
        updated_content = "\n".join(updated_lines)

        body = {"data": {"processed-tags.txt": updated_content}}
        v1.patch_namespaced_config_map(
            name="nvsentinel-processed-tags",
            namespace="nvsentinel-system",
            body=body,
        )
        return True
    except ApiException as e:
        print(f"Failed to update processed tags ConfigMap: {e}")
        return False
    except Exception as e:
        print(f"Unexpected error updating ConfigMap: {e}")
        return False

def label_workflow(workflow_name: str, version: str, timestamp: str) -> bool:
    """Label the workflow with completion status using the Kubernetes API"""
    try:
        co = client.CustomObjectsApi()
        body = {
            "metadata": {
                "labels": {
                    "deployment-version": version,
                    "completed-at": timestamp.replace(":", "-"),
                }
            }
        }
        co.patch_namespaced_custom_object(
            group="argoproj.io",
            version="v1alpha1",
            namespace="nvsentinel-system",
            plural="workflows",
            name=workflow_name,
            body=body,
        )
        return True
    except ApiException as e:
        print(f"Failed to label workflow: {e}")
        return False
    except Exception as e:
        print(f"Unexpected error labeling workflow: {e}")
        return False

def main():
    # Initialize Kubernetes client (in-cluster or local)
    try:
        load_k8s_config()
    except Exception as e:
        print(f"Failed to load Kubernetes configuration: {e}")
        sys.exit(1)

    # Get environment variables
    version = os.environ.get('VERSION')
    mr_results = os.environ.get('MR_RESULTS')
    workflow_name = os.environ.get('WORKFLOW_NAME')

    if not all([version, mr_results, workflow_name]):
        print("Missing required environment variables")
        sys.exit(1)

    # Process MR results
    summary = summarize_mr_results(mr_results)
    print(summary)

    # Get current timestamp
    timestamp = get_current_timestamp()

    # Update processed tags
    if not update_processed_tags(version, timestamp):
        print("Warning: Failed to update processed tags")

    # Label workflow
    if not label_workflow(workflow_name, version, timestamp):
        print("Warning: Failed to label workflow")

    print(f"Version {version} marked as completed")
    print("All MRs have been processed successfully!")

if __name__ == '__main__':
    main()