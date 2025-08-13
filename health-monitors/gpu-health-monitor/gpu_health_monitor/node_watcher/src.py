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

from kubernetes import client, config, watch
import re

import logging
import time
import urllib3
import sys

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

node_pod_map = {}


MAX_RETRIES = 5
RETRY_DELAY = 1


def get_dcgm_version_from_pod(pod):
    """Extract DCGM version from pod's container image."""
    dcgm_images = [x.image for x in pod.spec.containers if re.match(r".+?dcgm:.*", x.image)]
    if dcgm_images:
        if re.match(r".+?dcgm:4.*", dcgm_images[0]):
            return "4.x"
        elif re.match(r".+?dcgm:3.*", dcgm_images[0]):
            return "3.x"
        else:
            logger.error(f"Unsupported DCGM version: {dcgm_images[0]}")
            return None
    return None


def pod_event_callback(event):
    event_type = event["type"]
    pod = event["object"]
    pod_name = pod.metadata.name
    logger.debug(f"Event: {event_type} - Pod: {pod_name}")
    node_name = pod.spec.node_name

    if not node_name:
        logger.debug(f"Pod {pod_name} has no node assigned yet")
        return

    if event_type == "ADDED" or event_type == "MODIFIED":
        label_value = get_dcgm_version_from_pod(pod)
        if label_value:
            # Check if the label needs to be updated
            current_label = node_pod_map.get(node_name)
            if current_label != label_value:
                logger.info(f"Updating node {node_name} from {current_label} to {label_value} (Event: {event_type})")
                node_pod_map[node_name] = label_value
                update_node_label(node_name, label_value)
            else:
                logger.debug(f"Node {node_name} already has the correct label {label_value}")

    elif event_type == "DELETED":
        if node_name in node_pod_map:
            logger.info(f"DCGM pod deleted from node {node_name}, removing label")
            del node_pod_map[node_name]
            # Only try to remove label if node still exists
            update_node_label(node_name, None)


def update_node_label(node_name, label_value):
    """Update the label of the node. If label_value is None, remove the label."""
    inital_delay = RETRY_DELAY
    v1 = client.CoreV1Api()
    for _ in range(MAX_RETRIES):
        try:
            if label_value is None:
                # Remove the label using strategic merge patch
                # Setting to None in strategic merge patch removes the label
                body = {"metadata": {"labels": {"dcgm.version": None}}}
                v1.patch_node(name=node_name, body=body)
                logger.info(f"Removed dcgm.version label from node {node_name}")
            else:
                # Add or update the label
                v1.patch_node(name=node_name, body={"metadata": {"labels": {"dcgm.version": label_value}}})
                logger.info(f"Node {node_name} label updated to dcgm.version={label_value}")
            return
        except client.rest.ApiException as e:
            if e.status == 409:
                logger.error(f"Failed to update node {node_name}: {e}")
                time.sleep(inital_delay)
                inital_delay *= 2
            elif e.status == 404:
                # This is expected when a node is deleted - not an error
                logger.info(f"Node {node_name} no longer exists (likely deleted)")
                return
            elif e.status == 422 and label_value is None:
                # Label doesn't exist, can't remove it - this is fine
                logger.info(f"Label dcgm.version doesn't exist on node {node_name}, nothing to remove")
                return
            else:
                logger.error(f"Failed to update node {node_name}: {e}")
                sys.exit(1)


def main():
    try:
        config.load_incluster_config()
        logger.info("Using in-cluster configuration")
    except config.ConfigException:
        logger.error("Failed to load in-cluster configuration")
        sys.exit(1)

    v1 = client.CoreV1Api()
    w = watch.Watch()

    while True:
        try:
            for event in w.stream(
                v1.list_pod_for_all_namespaces,
                timeout_seconds=0,
                label_selector="app=nvidia-dcgm",
            ):
                pod_event_callback(event)

        except urllib3.exceptions.ProtocolError:
            logger.error("Protocol error")
            time.sleep(RETRY_DELAY)
        except Exception as e:
            logger.error(f"Error: {e}")
            sys.exit(1)


if __name__ == "__main__":
    main()
