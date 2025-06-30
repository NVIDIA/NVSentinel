# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

"""
Module for class of NVsentinel Fault Remediation integration tests
"""

import time
import pytest
import logging
from testcases.nvsentinel.base import TestNVSentinelCaseBase
import os
import yaml
import subprocess
from kubernetes import client, config
from kubernetes.client import CustomObjectsApi, ApiextensionsV1Api, CoreV1Api
from kubernetes.client.rest import ApiException
import json
import tempfile

class TestFaultRemediation(TestNVSentinelCaseBase):
    """
    Class for test cases of NVsentinel Fault Remediation
    """
    
    # Constants
    MAINTENANCE_CRD_NAME = "rebootnodes.janitor.dgxc.nvidia.com"
    MAINTENANCE_CRD_GROUP = "janitor.dgxc.nvidia.com"
    MAINTENANCE_CRD_VERSION = "v1alpha1"
    MAINTENANCE_CRD_PLURAL = "rebootnodes"
    JANITOR_NAMESPACE = "dgxc-janitor"
    REMEDIATION_WAIT_TIME = 30  # seconds

    @pytest.fixture
    def setup_fault_remediation(self, setup_runai_test):
        """
        Setup method to create CRD and namespace before test
        """
        self.skip_if_fault_remediation_deployment_not_found()
        # Initialize logger
        self.logger = logging.getLogger(__name__)
        if not self.logger.handlers:
            handler = logging.StreamHandler()
            formatter = logging.Formatter('%(asctime)s - %(name)s - %(levelname)s - %(message)s')
            handler.setFormatter(formatter)
            self.logger.addHandler(handler)
            self.logger.setLevel(logging.INFO)

        # Initialize Kubernetes clients
        self.api_extensions = ApiextensionsV1Api(self.client.apiClient)
        self.core_v1 = CoreV1Api(self.client.apiClient)
        self.custom_api = CustomObjectsApi(self.client.apiClient)
        
        # Setup required resources
        self._ensure_rebootnode_crd_exists()
        self._ensure_janitor_namespace_exists()
        self._cleanup_rebootnode_crs()


    @pytest.mark.author(email="nitijain@nvidia.com")
    @pytest.mark.faultremediation
    def test_rebootnode_cr_creation(self, request, setup_fault_remediation):
        """
        Test case of NVsentinel Fault Remediation: Inject XID error triggering rebootnode CR creation
        """
        self.logger.info("Inject XID error triggers rebootnode CR test")
        self.step_manager.print_header("Check the fault remediation pod is running")
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-fault-remediation*"
        )
        assert pods, "No nvsentinel-fault-remediation pod found"

        fault_remediation_pod = pods[-1]
        assert fault_remediation_pod.status.phase == "Running", \
        f"Pod {fault_remediation_pod.metadata.name} is not in Running state. Current state: {fault_remediation_pod.status.phase}"

        self.step_manager.print_header("Inject fatal XID error on the node")
        pods, _ = self.client.list_pods(
            self.nv_namespace, name_pattern="nvsentinel-gpu-health-monitor-dcgm*"
        )
        self.gpu_healthy_pod = pods[0]
        self.node_name = self.gpu_healthy_pod.spec.node_name
        self.remove_managed_by_nvsentinel_label(self.node_name)
        self.logger.info(f"POD Name: {self.gpu_healthy_pod.metadata.name}")
        self.logger.info(f"Node Name: {self.node_name}")

        command = [
            "/bin/sh",
            "-c",
            f"dcgmi test --host nvidia-dcgm.{self.gpu_operator_namespace}.svc:5555 --inject --gpuid 0 -f 84 -v 0",
        ]

        output, _ = self.client.exec_command_in_pod(self.gpu_healthy_pod, command)
        assert "Successfully injected" in output, "Failed to inject GPU error"

        self.step_manager.print_header("Check the node is cordoned")
        success, err = self.client.check_node_cordoned(self.node_name)
        assert success, f"FAIL: Node {self.node_name} is not cordoned"

        self.step_manager.print_header("Wait for remediation")
        time.sleep(self.REMEDIATION_WAIT_TIME)

        self.step_manager.print_header("Verify rebootnode CR was created")
        self._verify_rebootnode_cr()


        self.step_manager.print_header("Clear the injected error")
        
        command = [
            "/bin/sh",
            "-c",
            f"dcgmi test --host nvidia-dcgm.{self.gpu_operator_namespace}.svc:5555 --inject --gpuid 0 -f 84 -v 1",
        ]
        output, _ = self.client.exec_command_in_pod(self.gpu_healthy_pod, command)
        assert "Successfully injected" in output, "Failed to inject GPU error"

        time.sleep(30)
        self.step_manager.print_header("Check the node is uncordoned")
        node_info, _ = self.client.get_node_by_name(self.node_name)
        assert node_info.spec.unschedulable is None, f"FAIL: Node {self.node_name} is not uncordoned"
        self.restore_managed_by_nvsentinel_label(self.node_name)


    def _ensure_rebootnode_crd_exists(self):
        """Ensure the rebootnode CRD exists, create it if it doesn't"""
        self.logger.info("Checking rebootnode CRD")
        try:
            self.api_extensions.read_custom_resource_definition(name=self.MAINTENANCE_CRD_NAME)
            self.logger.info("RebootNode CRD already exists")
        except ApiException as e:
            if e.status == 404:
                self.logger.info("RebootNode CRD not found, creating it")
                crd_file = os.path.join(
                    os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "janitor.dgxc.nvidia.com_rebootnode.yaml"
                )
                try:
                    subprocess.run(["kubectl", "apply", "-f", crd_file], check=True)
                    self.logger.info("Successfully created rebootnode CRD")
                except subprocess.CalledProcessError as e:
                    self.logger.error(f"Failed to create rebootnode CRD: {e}")
                    raise
            else:
                self.logger.error(f"Error checking rebootnode CRD: {e}")
                raise

    def _ensure_janitor_namespace_exists(self):
        """Ensure the janitor namespace exists, create it if it doesn't"""
        self.logger.info("Checking dgxc-janitor namespace")
        try:
            self.core_v1.read_namespace(name=self.JANITOR_NAMESPACE)
            self.logger.info("Namespace dgxc-janitor already exists")
        except ApiException as e:
            if e.status == 404:
                self.logger.info("Namespace dgxc-janitor not found, creating it")
                try:
                    subprocess.run(["kubectl", "create", "namespace", self.JANITOR_NAMESPACE], check=True)
                    self.logger.info("Successfully created dgxc-janitor namespace")
                except subprocess.CalledProcessError as e:
                    self.logger.error(f"Failed to create janitor namespace: {e}")
                    raise
            else:
                self.logger.error(f"Error checking janitor namespace: {e}")
                raise

    def _cleanup_rebootnode_crs(self):
        """Clean up existing rebootnode CRs"""
        self.step_manager.print_header("Clean up existing rebootnode CRs")
        try:
            rebootnode_crs = self.custom_api.list_cluster_custom_object(
                group=self.MAINTENANCE_CRD_GROUP,
                version=self.MAINTENANCE_CRD_VERSION,
                plural=self.MAINTENANCE_CRD_PLURAL
            )
            
            if "items" in rebootnode_crs and len(rebootnode_crs["items"]) > 0:
                self.logger.info(f"Found {len(rebootnode_crs['items'])} existing rebootnode CRs. Deleting...")
                for cr in rebootnode_crs["items"]:
                    cr_name = cr["metadata"]["name"]
                    self.logger.info(f"Deleting rebootnode CR: {cr_name}")
                    try:
                        self.custom_api.delete_cluster_custom_object(
                            group=self.MAINTENANCE_CRD_GROUP,
                            version=self.MAINTENANCE_CRD_VERSION,
                            plural=self.MAINTENANCE_CRD_PLURAL,
                            name=cr_name
                        )
                    except ApiException as e:
                        self.logger.warning(f"Failed to delete rebootnode CR {cr_name}: {e}")
                self.logger.info("Successfully deleted all existing rebootnode CRs")
            else:
                self.logger.info("No existing rebootnode CRs found")
        except ApiException as e:
            self.logger.warning(f"Error while listing rebootnode CRs: {e}")

    def _verify_rebootnode_cr(self):
        """Verify that a rebootnode CR was created with the correct specifications"""
        self.step_manager.print_header("Verify rebootnode CR was created")
        
        try:
            # Use kubectl to get rebootnode CR
            kubectl_command = [
                "kubectl",
                "get",
                "rebootnode",
                "-o",
                "yaml"
            ]
            result = subprocess.run(kubectl_command, capture_output=True, text=True, check=True)
            actual_cr = yaml.safe_load(result.stdout)
            
            if not actual_cr.get("items") or len(actual_cr["items"]) == 0:
                self.logger.error("No rebootnode CR was created")
                raise AssertionError("No rebootnode CR was created")
            
            # Get the first rebootnode CR
            rebootnode_cr = actual_cr["items"][0]
            
            # Remove metadata fields
            rebootnode_cr.pop('metadata', None)
            rebootnode_cr.pop('status', None)
            
            # Load expected YAML
            expected_yaml_path = os.path.join(
                os.getcwd(), "nvsentinel", "testcases", "data", "cli", "nvsentinel", "expected_rebootnode_cr.yaml"
            )
            with open(expected_yaml_path, 'r') as f:
                expected_cr = yaml.safe_load(f)
            
            # Remove metadata from expected CR
            expected_cr.pop('metadata', None)
            
            # Update expected CR with actual node name for comparison
            expected_cr['spec']['nodeName'] = self.node_name
            
            # Compare the CRs
            if rebootnode_cr != expected_cr:
                self.logger.error("RebootNode CR does not match expected structure")
                self.logger.error("Expected CR:")
                self.logger.error(yaml.dump(expected_cr))
                self.logger.error("Actual CR:")
                self.logger.error(yaml.dump(rebootnode_cr))
                raise AssertionError("RebootNode CR does not match expected structure")
            
            self.logger.info("Successfully verified rebootnode CR creation")
            return True
            
        except subprocess.CalledProcessError as e:
            self.logger.error(f"Failed to get rebootnode CRs: {e.stderr}")
            raise AssertionError(f"Failed to get rebootnode CRs: {e.stderr}")
        except yaml.YAMLError as e:
            self.logger.error(f"Failed to parse YAML: {e}")
            raise AssertionError(f"Failed to parse YAML: {e}")
        except Exception as e:
            self.logger.error(f"Unexpected error verifying rebootnode CR: {e}")
            raise AssertionError(f"Unexpected error verifying rebootnode CR: {e}")
        finally:
            self._cleanup_rebootnode_crs()

