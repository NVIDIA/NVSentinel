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

import pytest
import logging
import os
import yaml
import time
from testcases.utils.kubernetes_utils import KubernetesClient


logger = logging.getLogger(__name__)
RUNNING_CONFIG = "http://cqa-fs01.nvidia.com/Automation/RUNAI/runai_api.config"


@pytest.fixture(scope="package")
def nvsentinel_autosync_disabled_enabled():
    """
    Disable auto-sync when nvsentinel package tests start and enable when they complete.
    This fixture will:
    1. Disable auto-sync for mk8s, nvsentinel, and gpu-operator when nvsentinel tests begin
    2. Enable auto-sync for these applications after all nvsentinel tests in the package complete
    """
    client = KubernetesClient()
    logger.info("Setting up nvsentinel auto-sync control")

    try:
        # Setup - disable auto-sync
        logger.info("Disabling auto-sync for mk8s, nvsentinel, and gpu-operator")
        apps = ["mk8s", "nvsentinel", "gpu-operator"]
        for app in apps:
            result = client.disable_argocd_auto_sync(app)
            if not result.values[0]:
                logger.warning(f"Failed to disable auto-sync for {app}: {result.values[1]}")
            else:
                logger.info(f"Successfully disabled auto-sync for {app}")
        time.sleep(60)
        yield  # Run nvsentinel package tests

    finally:
        # Cleanup - enable auto-sync and trigger sync
        client = KubernetesClient()
        logger.info("Enabling auto-sync for mk8s, nvsentinel, and gpu-operator")
        for app in apps:
            try:
                # Enable auto-sync
                result = client.enable_argocd_auto_sync(app)
                if not result.values[0]:
                    logger.warning(
                        f"Failed to enable auto-sync for {app}: {result.values[1]}"
                    )
                else:
                    logger.info(f"Successfully enabled auto-sync for {app}")

                # Trigger manual sync
                logger.info(f"Triggering sync for {app}")
                sync_result = client.sync_argocd_application(app)
                if not sync_result.values[0]:
                    logger.warning(f"Failed to sync {app}: {sync_result.values[1]}")
                else:
                    logger.info(f"Successfully triggered sync for {app}")
            except Exception as e:
                logger.error(f"Error in cleanup operations for {app}: {e}")
        client.rollout_daemonset(
            "nvsentinel-gpu-health-monitor-dcgm-3.x", namespace="nvsentinel"
        )
        client.rollout_daemonset("nvsentinel-nic-health-monitor", namespace="nvsentinel")
        client.rollout_daemonset(
            "nvsentinel-nvswitch-health-monitor", namespace="nvsentinel"
        )
        client.rollout_daemonset("nvsentinel-platform-connector", namespace="nvsentinel")
