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
import subprocess
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

    tenant_context = os.getenv("TENANT_KUBE_CONTEXT")
    tenant_profile = os.getenv("TENANT_CSP_PROFILE")
    nmc_context = os.getenv("NMC_KUBE_CONTEXT")
    nmc_profile = os.getenv("NMC_CSP_PROFILE")
    cloud_provider = os.getenv("CLOUD_PROVIDER")
    apps = ["mk8s"]
    if nmc_context:
        switch_context(nmc_context, nmc_profile, cloud_provider)
        apps += ["nvsentinel-mgmt", "nvsentinel-tenant", "gpu-operator-tenant"]
        nvsentinel_namespace = "dgxc-system"
    else:
        logger.info("No NMC context found, skipping context switch")
        apps += ["gpu-operator", "nvsentinel"]
        nvsentinel_namespace = "nvsentinel"

    client = KubernetesClient()
    logger.info("Setting up nvsentinel auto-sync control")

    try:
        # Setup - disable auto-sync
        logger.info(f"Disabling auto-sync for {apps}")
        for app in apps:
            result = client.disable_argocd_auto_sync(app)
            if not result.values[0]:
                logger.warning(f"Failed to disable auto-sync for {app}: {result.values[1]}")
            else:
                logger.info(f"Successfully disabled auto-sync for {app}")
        if tenant_context:
            switch_context(tenant_context, tenant_profile, cloud_provider)
        time.sleep(60)
        yield  # Run nvsentinel package tests

    finally:
        # Cleanup - enable auto-sync and trigger sync
        client = KubernetesClient()
        logger.info(f"Enabling auto-sync for {apps}")
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
        if tenant_context:
            switch_context(tenant_context, tenant_profile, cloud_provider)
            client = KubernetesClient()

        # Rollout restart DaemonSets that exist
        daemonsets_to_restart = [
            "nvsentinel-gpu-health-monitor-dcgm-3.x",
            "nvsentinel-gpu-health-monitor-dcgm-4.x",
            "nvsentinel-nic-health-monitor",
            "nvsentinel-nvswitch-health-monitor",
            "nvsentinel-platform-connector"
        ]
        
        for ds_name in daemonsets_to_restart:
            try:
                client.rollout_daemonset(ds_name, namespace=nvsentinel_namespace)
            except Exception as e:
                if "not found" in str(e).lower() or "404" in str(e):
                    logger.warning(f"DaemonSet {ds_name} not found in namespace {nvsentinel_namespace}, skipping rollout")
                else:
                    logger.error(f"Failed to rollout DaemonSet {ds_name}: {e}")

def switch_context(context_name, profile_name, cloud_provider):
    """Helper to switch both kubectl context and CSP profile"""
    # Switch kubectl context
    subprocess.run(['kubectl', 'config', 'use-context', context_name], check=True)
    logger.info(f"Switched to context (from the code): {context_name}")
    # Switch CSP profile
    if cloud_provider == 'gcp':
        subprocess.run(['gcloud', 'config', 'configurations', 'activate', profile_name], check=True)
    elif cloud_provider == 'aws':
        os.environ['AWS_PROFILE'] = profile_name
