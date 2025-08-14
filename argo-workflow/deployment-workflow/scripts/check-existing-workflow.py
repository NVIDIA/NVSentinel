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
from datetime import datetime, timezone
import sys
from kubernetes import client, config
from kubernetes.client.rest import ApiException


# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s [%(levelname)s] %(message)s',
    datefmt='%Y-%m-%d %H:%M:%S'
)
logger = logging.getLogger('check-existing-workflow')

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
        logger.error(f"Failed to load Kubernetes configuration: {e}")
        sys.exit(1)

    v1 = client.CoreV1Api()

    VERSION = os.environ.get('VERSION')
    if not VERSION:
        logger.error("VERSION environment variable not set")
        sys.exit(1)

    logger.info(f"Checking processed tags ConfigMap for version {VERSION}")

    # Retrieve processed tags via Kubernetes API
    try:
        cm = v1.read_namespaced_config_map("nvsentinel-processed-tags", "nvsentinel-system")
        processed_tags = cm.data.get("processed-tags.txt", "") if cm.data else ""
    except ApiException as e:
        if e.status == 404:
            cm = None
            processed_tags = ""
        else:
            logger.error(f"Unable to read ConfigMap: {e}")
            sys.exit(1)

    for line in processed_tags.splitlines():
        if line.startswith(f"{VERSION}:") and line.endswith(":completed"):
            logger.warning(f"WARNING: Version {VERSION} already completed (found in processed tags)")
            with open('/tmp/should-proceed.txt', 'w') as f:
                f.write("false")
            with open('/tmp/message.txt', 'w') as f:
                f.write(f"Version {VERSION} already completed")
            logger.error("ABORT: Skipping deployment - version already completed")
            sys.exit(1)
        if line.startswith(f"{VERSION}:") and line.endswith(":in-progress"):
            logger.warning(f"WARNING: Version {VERSION} already in-progress (found in processed tags)")
            with open('/tmp/should-proceed.txt', 'w') as f:
                f.write("false")
            with open('/tmp/message.txt', 'w') as f:
                f.write(f"Version {VERSION} already being processed")
            logger.error("ABORT: Skipping deployment - version already in-progress")
            sys.exit(1)

    # Update ConfigMap to mark as in-progress
    logger.info(f"Marking version {VERSION} as in-progress...")

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
        logger.error(f"Failed to update processed tags ConfigMap: {e}")
        sys.exit(1)

    with open('/tmp/should-proceed.txt', 'w') as f:
        f.write("true")
    with open('/tmp/message.txt', 'w') as f:
        f.write(f"Proceeding with deployment for version {VERSION}")
    logger.info("SUCCESS: Proceeding with deployment") 

if __name__ == "__main__":
    main()