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
from datetime import datetime, timezone
import sys
from kubernetes import client, config
from kubernetes.client.rest import ApiException

def load_k8s_config():
    """Load Kubernetes configuration (in-cluster preferred)."""
    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()

def main():
    # Initialize Kubernetes client
    try:
        load_k8s_config()
    except Exception as e:
        print(f"[check-existing-workflow] ERROR: Failed to load Kubernetes configuration: {e}")
        sys.exit(1)

    v1 = client.CoreV1Api()

    VERSION = os.environ.get('VERSION')
    if not VERSION:
        print("[check-existing-workflow] ERROR: VERSION environment variable not set")
        sys.exit(1)

    print(f"[check-existing-workflow] Checking processed tags ConfigMap for version {VERSION}")

    # Retrieve processed tags via Kubernetes API
    try:
        cm = v1.read_namespaced_config_map("nvsentinel-processed-tags", "nvsentinel-system")
        processed_tags = cm.data.get("processed-tags.txt", "") if cm.data else ""
    except ApiException as e:
        if e.status == 404:
            cm = None
            processed_tags = ""
        else:
            print(f"[check-existing-workflow] ERROR: Unable to read ConfigMap: {e}")
            sys.exit(1)

    for line in processed_tags.splitlines():
        if line.startswith(f"{VERSION}:") and line.endswith(":completed"):
            print(f"[check-existing-workflow] WARNING: Version {VERSION} already completed (found in processed tags)")
            with open('/tmp/should-proceed.txt', 'w') as f:
                f.write("false")
            with open('/tmp/message.txt', 'w') as f:
                f.write(f"Version {VERSION} already completed")
            print("[check-existing-workflow] ABORT: Skipping deployment - version already completed")
            sys.exit(1)
        if line.startswith(f"{VERSION}:") and line.endswith(":in-progress"):
            print(f"[check-existing-workflow] WARNING: Version {VERSION} already in-progress (found in processed tags)")
            with open('/tmp/should-proceed.txt', 'w') as f:
                f.write("false")
            with open('/tmp/message.txt', 'w') as f:
                f.write(f"Version {VERSION} already being processed")
            print("[check-existing-workflow] ABORT: Skipping deployment - version already in-progress")
            sys.exit(1)

    # Update ConfigMap to mark as in-progress
    print(f"[check-existing-workflow] Marking version {VERSION} as in-progress...")

    lines = [line for line in processed_tags.splitlines() if not line.startswith(f"{VERSION}:")]
    timestamp = datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
    new_entry = f"{VERSION}:{timestamp}:in-progress"
    lines.append(new_entry)
    updated_content = "\n".join(lines)

    body = {"data": {"processed-tags.txt": updated_content}}
    try:
        if cm:
            v1.patch_namespaced_config_map(
                name="nvsentinel-processed-tags",
                namespace="nvsentinel-system",
                body=body,
            )
        else:
            v1.create_namespaced_config_map(
                namespace="nvsentinel-system",
                body=client.V1ConfigMap(
                    metadata=client.V1ObjectMeta(name="nvsentinel-processed-tags"),
                    data={"processed-tags.txt": updated_content},
                ),
            )
    except ApiException as e:
        print(f"[check-existing-workflow] WARNING: Failed to update processed tags ConfigMap: {e}")
        sys.exit(1)

    with open('/tmp/should-proceed.txt', 'w') as f:
        f.write("true")
    with open('/tmp/message.txt', 'w') as f:
        f.write(f"Proceeding with deployment for version {VERSION}")
    print("[check-existing-workflow] SUCCESS: Proceeding with deployment") 

if __name__ == "__main__":
    main()