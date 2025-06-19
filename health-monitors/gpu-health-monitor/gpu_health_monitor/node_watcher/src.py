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


def pod_event_callback(event):
    event_type = event["type"]
    pod = event["object"]
    pod_name = pod.metadata.name
    logger.info(f"Event: {event_type} - Pod: {pod_name}")
    node_name = pod.spec.node_name

    if event_type == "ADDED":
        time.sleep(30)  # wait for the pod to be ready
        dcgm_version = [x.image for x in pod.spec.containers if re.match(r".+?dcgm:.*", x.image)]
        if dcgm_version:
            if re.match(r".+?dcgm:4.*", dcgm_version[0]):
                label_value = "4.x"
            elif re.match(r".+?dcgm:3.*", dcgm_version[0]):
                label_value = "3.x"
            else:
                logger.error(f"Unsupported DCGM version: {dcgm_version[0]}")
                return

            if node_name not in node_pod_map or node_pod_map[node_name] != label_value:
                node_pod_map[node_name] = label_value
                update_node_label(node_name, label_value)
            else:
                logger.info(f"Node {node_name} already has the label {label_value}")
    elif event_type == "DELETED":
        if node_name in node_pod_map:
            del node_pod_map[node_name]
            update_node_label(node_name, None)
    else:
        logger.debug(f"Event: {event_type} - Pod: {pod_name}")


def update_node_label(node_name, label_value):
    """Update the label of the node."""
    inital_delay = RETRY_DELAY
    v1 = client.CoreV1Api()
    for _ in range(MAX_RETRIES):
        try:
            v1.patch_node(name=node_name, body={"metadata": {"labels": {"dcgm.version": label_value}}})
            logger.info(f"Node {node_name} has been updated to {label_value}")
            return
        except client.rest.ApiException as e:
            if e.status == 409:
                logger.error(f"Failed to update node {node_name}: {e}")
                time.sleep(inital_delay)
                inital_delay *= 2
            elif e.status == 404:
                logger.error(f"Node {node_name} not found")
                return
            else:
                raise e


def main():
    config.load_incluster_config()
    v1 = client.CoreV1Api()
    w = watch.Watch()

    while True:
        try:
            for event in w.stream(
                v1.list_pod_for_all_namespaces,
                namespace="gpu-operator",
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
