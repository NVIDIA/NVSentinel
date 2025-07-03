# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.

# Suppress warnings before any imports that might trigger them
import warnings
import urllib3

# Disable SSL warnings from urllib3
urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)

# Suppress the specific DeprecationWarning from kubernetes client
warnings.filterwarnings("ignore", category=DeprecationWarning, module="kubernetes.client.rest")
warnings.filterwarnings("ignore", message="HTTPResponse.getheaders()")
warnings.filterwarnings("ignore", message="Unverified HTTPS request")

import json
import os
import random
import subprocess
import time
import uuid
import urllib.request
import urllib.error

import pytest
from testcases.nvsentinel.base import TestNVSentinelCaseBase

# Apply warning filters at module level
pytestmark = [
    pytest.mark.filterwarnings("ignore::DeprecationWarning:kubernetes.client.rest"),
    pytest.mark.filterwarnings("ignore::urllib3.exceptions.InsecureRequestWarning"),
    pytest.mark.filterwarnings("ignore:HTTPResponse.getheaders"),
    pytest.mark.filterwarnings("ignore:Unverified HTTPS request"),
]

class TestGCPMaintenanceEvent(TestNVSentinelCaseBase):
    """
    Class for test case of NVSentinel CSP Health Monitor: GCP Maintenance Event
    """
    backup_cm = "backup_csp_health_monitor_cm.yaml"

    @pytest.fixture(autouse=True)
    def setup_and_teardown_csp_test(self):
        """
        Fixture to handle teardown for the CSP health monitor test,
        including restoring the original configmap.
        """
        yield
        
        self.logger.info("Starting teardown for GCP maintenance event test")
        
        deployment = self.client.get_deployments(self.nv_namespace, "nvsentinel-csp-health-monitor")
        if not deployment:
            self.logger.info("CSP health monitor deployment not found, skipping configmap restore.")
            return

        self.logger.info(f"Restoring original CSP health monitor config from {self.backup_cm}")
        try:
            if os.path.exists(self.backup_cm):
                self.client.apply_configmap(self.backup_cm)
                self.logger.info(f"Successfully restored configmap from {self.backup_cm}")
                os.remove(self.backup_cm)
            else:
                self.logger.warning(f"Backup configmap file not found at {self.backup_cm}, cannot restore.")
        except Exception as e:
            self.logger.error(f"Failed to restore and cleanup backup config map: {e}")

    def _get_target_node_details(self):
        """
        Get details of a worker node for testing, preferring GPU nodes.
        First tries to find nodes with nvidia.com/gpu.present=true label,
        then falls back to nodeGroup=customer-cpu nodes.
        Returns a dict with node details needed for the test.
        """
        node_names = []
        
        # First, try to find GPU nodes
        gpu_label_selector = "nvidia.com/gpu.present=true"
        self.logger.info(f"Searching for GPU nodes with label selector: '{gpu_label_selector}'")
        
        gpu_nodes, err = self.client.get_node_names_by_label(gpu_label_selector)
        if gpu_nodes and not err:
            node_names.extend(gpu_nodes)
            self.logger.info(f"Found {len(gpu_nodes)} GPU nodes")
        else:
            self.logger.info("No GPU nodes found, checking for customer-cpu nodes...")
            
            # If no GPU nodes, try customer-cpu nodes
            cpu_label_selector = "nodeGroup=customer-cpu"
            self.logger.info(f"Searching for CPU nodes with label selector: '{cpu_label_selector}'")
            
            cpu_nodes, err = self.client.get_node_names_by_label(cpu_label_selector)
            if cpu_nodes and not err:
                node_names.extend(cpu_nodes)
                self.logger.info(f"Found {len(cpu_nodes)} customer-cpu nodes")
        
        # Skip if no nodes found with either label
        if not node_names:
            pytest.skip("No nodes found with labels: nvidia.com/gpu.present=true OR nodeGroup=customer-cpu")
        
        # Select a random node name
        selected_node_name = random.choice(node_names)
        
        # Get the full node object
        target_node = self.client.coreV1Api.read_node(selected_node_name)
        node_name = target_node.metadata.name
        self.logger.info(f"Selected node for testing: {node_name}")

        # Check if it's a GPU node
        if hasattr(target_node.metadata, 'labels') and target_node.metadata.labels:
            if target_node.metadata.labels.get("nvidia.com/gpu.present") == "true":
                self.logger.info("Selected node is a GPU node")
            elif target_node.metadata.labels.get("nodeGroup") == "customer-cpu":
                self.logger.info("Selected node is a customer-cpu node")
        
        # Extract instance details from provider ID
        # Format: gce://PROJECT/ZONE/INSTANCE_NAME
        provider_id = target_node.spec.provider_id
        if not provider_id or not provider_id.startswith("gce://"):
            pytest.skip(f"Node {node_name} does not have a valid GCP provider ID")
        
        parts = provider_id.replace("gce://", "").split("/")
        if len(parts) != 3:
            pytest.skip(f"Invalid provider ID format: {provider_id}")
        
        project_id = os.getenv("GCP_PROJECT_ID")
        if not project_id:
            pytest.skip("GCP_PROJECT_ID environment variable not set")
        
        zone = parts[1]
        instance_name = parts[2]
        
        # Get the actual zone from the node's label
        if hasattr(target_node.metadata, 'labels') and target_node.metadata.labels:
            actual_zone = target_node.metadata.labels.get("topology.kubernetes.io/zone")
            if actual_zone:
                zone = actual_zone
        
        # Get instance ID from node annotation
        instance_id = None
        if hasattr(target_node.metadata, 'annotations') and target_node.metadata.annotations:
            instance_id = target_node.metadata.annotations.get("container.googleapis.com/instance_id")
        
        # Fail if instance ID is not found in annotation
        if not instance_id:
            pytest.fail(f"Instance ID not found in node annotation 'container.googleapis.com/instance_id' for node {node_name}")
        
        return {
            "node_name": node_name,
            "instance_name": instance_name,
            "instance_id": instance_id,
            "zone": zone,  # This is now the actual zone from the node label
            "project_id": project_id,
            "log_name": "csp-health-monitor-integration-test",  # Just the log name, gcloud will add the project prefix
            "operation_id": f"test-op-{int(time.time())}"  # Store operation ID for consistency across events
        }

    def _update_csp_monitor_config(self, node_details):
        """
        Update CSP health monitor configuration to watch for the specific test node
        and reduce recovery delay for faster testing.
        """
        # Check for pods with the CSP health monitor label
        csp_pods, _ = self.client.list_pods(
            namespace=self.nv_namespace,
            name_pattern=".*csp-health-monitor.*"
        )

        # Check pod health
        healthy_pod_found = False
        for pod in csp_pods:
            if pod.status.phase == "Running":
                if pod.status.container_statuses:
                    all_containers_ready = all(cs.ready for cs in pod.status.container_statuses)
                    if all_containers_ready:
                        healthy_pod_found = True
            elif pod.status.phase in ["CrashLoopBackOff", "Error", "Failed"]:
                self.logger.warning(f"Pod {pod.metadata.name} is in unhealthy state: {pod.status.phase}")
        
        if not healthy_pod_found:
            pytest.fail("No healthy CSP health monitor pods found. Pods may be in CrashLoopBackOff or other error state")
        
        config_map_name = f"{self.nv_namespace}-csp-health-monitor-config"

        # Read current config
        config_map_result = self.client.get_configmap(
            self.nv_namespace, config_map_name
        )
        if not config_map_result.values[0]:
            pytest.fail(f"Failed to read config map: {config_map_result.values[1]}")
        config_map = config_map_result.values[0]
        
        # Parse and update the configuration
        config_data = config_map.data.get("config.toml", "")
        if not config_data:
            pytest.fail("Config map does not contain config.toml")
        
        # Update configuration using string manipulation
        lines = config_data.split('\n')
        updated_lines = []
        in_gcp_section = False
        gcp_section_found = False
        
        for i, line in enumerate(lines):
            stripped = line.strip()
            
            # Check if we're entering the [gcp] section
            if stripped == "[gcp]":
                in_gcp_section = True
                gcp_section_found = True
                updated_lines.append(line)
                # Add the logFilter right after [gcp]
                log_filter_line = f'logFilter = \'logName="projects/{node_details["project_id"]}/logs/{node_details["log_name"]}" AND operation.producer="compute.instances.upcomingMaintenance"\''
                updated_lines.append(log_filter_line)
                continue
            
            # Check if we're leaving the [gcp] section
            if in_gcp_section and stripped.startswith("[") and stripped != "[gcp]":
                in_gcp_section = False
            
            # Update specific fields
            if stripped.startswith("postMaintenanceHealthyDelayMinutes"):
                updated_lines.append("postMaintenanceHealthyDelayMinutes = 1")
            elif stripped.startswith("clusterName"):
                cluster_name = os.getenv("CLOUD_CLUSTER_NAME", "test-cluster")
                updated_lines.append(f'clusterName = "{cluster_name}"')
            elif in_gcp_section:
                if stripped.startswith("enabled"):
                    updated_lines.append("enabled = true")
                elif stripped.startswith("targetProjectId"):
                    updated_lines.append(f'targetProjectId = "{node_details["project_id"]}"')
                elif stripped.startswith("apiPollingIntervalSeconds"):
                    updated_lines.append("apiPollingIntervalSeconds = 30")
                elif stripped.startswith("logFilter"):
                    # Skip the original logFilter line, we already added our own
                    continue
                else:
                    updated_lines.append(line)
            else:
                updated_lines.append(line)
        
        # If [gcp] section wasn't found, add it
        if not gcp_section_found:
            updated_lines.append("")
            updated_lines.append("[gcp]")
            updated_lines.append("enabled = true")
            updated_lines.append(f'targetProjectId = "{node_details["project_id"]}"')
            updated_lines.append("apiPollingIntervalSeconds = 30")
            log_filter_line = f'logFilter = \'logName="projects/{node_details["project_id"]}/logs/{node_details["log_name"]}" AND operation.producer="compute.instances.upcomingMaintenance"\''
            updated_lines.append(log_filter_line)
        
        config_map.data["config.toml"] = '\n'.join(updated_lines)
        
        # Use the Kubernetes API directly to update the config map
        try:
            self.client.coreV1Api.replace_namespaced_config_map(
                name=config_map_name,
                namespace=self.nv_namespace,
                body=config_map
            )
        except Exception as e:
            pytest.fail(f"Failed to update config map: {e}")
        
        self.logger.info("Updated CSP health monitor configuration")
        
        # Restart the CSP health monitor to pick up new config
        # Delete the existing pod(s) to force a restart
        if csp_pods:
            for pod in csp_pods:
                self.client.delete_pod_by_name(pod.metadata.name, self.nv_namespace)
                self.logger.info(f"Deleted pod {pod.metadata.name} to force restart")
        
        # Wait for new pods to be ready
        max_wait_time = 185
        start_time = time.time()
        pods_ready = False
        
        while time.time() - start_time < max_wait_time:
            # Check for new CSP health monitor pods
            new_pods, _ = self.client.list_pods(
                namespace=self.nv_namespace,
                name_pattern=".*csp-health-monitor.*"
            )
            
            if new_pods:
                all_ready = True
                for pod in new_pods:
                    if pod.status.phase != "Running":
                        all_ready = False
                    else:
                        # Check if all containers are ready
                        if pod.status.container_statuses:
                            for container in pod.status.container_statuses:
                                if not container.ready:
                                    all_ready = False
                
                if all_ready and len(new_pods) > 0:
                    pods_ready = True
                    self.logger.info("CSP health monitor pod(s) are ready")
                    break
            
            self.logger.debug("Waiting for CSP health monitor pods to be ready...")
            time.sleep(5)
        
        if not pods_ready:
            pytest.fail(f"CSP health monitor pods did not become ready within {max_wait_time} seconds")
        
        # Wait for the CSP monitor to fully initialize and start polling
        self.logger.info("Waiting for CSP monitor to initialize and start polling...")
        time.sleep(20)  # Give the monitor time to start up and begin watching logs
        
        # Wait for initial poll to complete
        self.logger.info("Waiting for CSP monitor's initial poll to complete...")
        initial_poll_wait = 40  # Initial poll + buffer time
        self.logger.info(f"Waiting {initial_poll_wait} seconds to ensure initial poll is complete...")
        time.sleep(initial_poll_wait)
        
        # Verify the configuration was properly applied
        self.logger.info("Verifying CSP monitor configuration...")
        verify_result = self.client.get_configmap(self.nv_namespace, config_map_name)
        if verify_result.values[0]:
            verify_config_data = verify_result.values[0].data.get("config.toml", "")
            if f'logName="projects/{node_details["project_id"]}/logs/{node_details["log_name"]}"' in verify_config_data:
                self.logger.info(f"✓ Configuration verified: CSP monitor is configured to watch for logs with name: {node_details['log_name']}")
            else:
                self.logger.warning("Configuration verification failed: Expected log filter not found in config")
                self.logger.debug(f"Current config.toml content:\n{verify_config_data}")
        else:
            self.logger.warning("Could not verify configuration: Failed to read configmap")

    def _insert_gcp_maintenance_log(self, node_details, event_type="PENDING", custom_timestamp=None):
        """
        Insert a maintenance event log entry into Google Cloud Logging.
        Following the exact structure from insert-cloud-log.sh
        """
        # Generate proper timestamp
        current_time = time.time()
        
        if custom_timestamp:
            future_time = custom_timestamp
        else:
            # Use a timestamp 10 seconds in the future to ensure it falls within the next polling window
            # The CSP monitor uses exclusive start time (>) and inclusive end time (<=) in its queries
            future_time = current_time + 10
        
        timestamp = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(future_time))
        timestamp += f".{int((future_time % 1) * 1000000):06d}Z"
        
        self.logger.info(f"Using timestamp for log entry: {timestamp}")
        
        # Generate time windows for maintenance
        # Set maintenance window to start in the future (within 30 minutes) to trigger quarantine
        # The CSP monitor requires scheduledStartTime to be > now and <= now + 30 minutes
        window_start = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(current_time + 600))  # +10 minutes from now
        window_end = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(current_time + 4200))  # +70 minutes from now
        latest_window_start = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(current_time + 7200))  # +120 minutes
        
        # Use the same operation ID for all events in this test run
        operation_id = node_details.get("operation_id", f"test-op-{int(current_time)}")
        insert_id = f"test-maintenance-{event_type.lower()}-{int(current_time)}"
        
        log_entry_payload = {
            "serviceName": "compute.googleapis.com",
            "methodName": "compute.instances.upcomingMaintenance",
            "resourceName": f"projects/{node_details['project_id']}/zones/{node_details['zone']}/instances/{node_details['instance_name']}",
            "authenticationInfo": {
                "principalEmail": "system@google.com"
            },
        }

        if event_type == "COMPLETE":
            log_entry_payload["status"] = { "message": "Maintenance window has completed for this instance. All maintenance notifications on the instance have been removed." }
        else:
            log_entry_payload["status"] = { "message": f"Test Script: Maintenance {event_type} for {node_details['instance_name']}" }
            log_entry_payload["metadata"] = {
                "@type": "type.googleapis.com/google.cloud.compute.v1.UpcomingMaintenance",
                "canReschedule": event_type == "PENDING",
                "maintenanceStatus": event_type,
                "type": "SCHEDULED",
                "windowStartTime": window_start,
                "windowEndTime": window_end,
                "latestWindowStartTime": latest_window_start
            }

        log_entry = {
            "logName": f"projects/{node_details['project_id']}/logs/{node_details['log_name']}",
            "resource": {
                "type": "gce_instance",
                "labels": {
                    "project_id": node_details["project_id"],
                    "zone": node_details["zone"],
                    "instance_id": node_details["instance_id"]
                }
            },
            "severity": "NOTICE",
            "protoPayload": {
                "@type": "type.googleapis.com/google.cloud.audit.AuditLog",
                **log_entry_payload
            },
            "operation": {
                "id": operation_id,
                "producer": "compute.instances.upcomingMaintenance",
                "first": event_type == "PENDING",
                "last": event_type == "COMPLETE"
            },
            "insertId": insert_id,
            "timestamp": timestamp
        }
        
        self.logger.info(f"Inserting GCP log entry with event_type={event_type}")
        self.logger.info("Log entry JSON:")
        self.logger.info(json.dumps(log_entry, indent=2))
        
        try:
            self.logger.info("Attempting to get gcloud access token...")
            token_cmd = ["gcloud", "auth", "print-access-token"]
            token_result = subprocess.run(token_cmd, capture_output=True, text=True, check=True)
            access_token = token_result.stdout.strip()
            self.logger.info("Successfully got gcloud access token.")
            
            api_url = "https://logging.googleapis.com/v2/entries:write"
            self.logger.info(f"Making POST request to {api_url}")
            headers = {
                "Authorization": f"Bearer {access_token}",
                "Content-Type": "application/json; charset=utf-8"
            }
            request_body = {"entries": [log_entry]}
            data = json.dumps(request_body).encode("utf-8")
            
            req = urllib.request.Request(api_url, data=data, headers=headers, method='POST')
            with urllib.request.urlopen(req) as response:
                self.logger.info(f"Received HTTP status {response.status} from Google Logging API")
                if response.status == 200:
                    self.logger.info(f"Successfully inserted {event_type} maintenance log via API")
                else:
                    error_response = response.read().decode('utf-8')
                    self.logger.error(f"Google Logging API error response: {error_response}")
                    pytest.fail(f"Failed to insert log entry. HTTP {response.status}: {error_response}")

            time.sleep(5)  # Allow time for log propagation

        except urllib.error.HTTPError as e:
            error_body = e.read().decode('utf-8')
            self.logger.error(f"HTTPError during log insertion: {e.code} {e.reason}. Response body: {error_body}")
            pytest.fail(f"Failed to insert log entry: HTTP {e.code} {e.reason}. Response: {error_body}")
        except subprocess.CalledProcessError as e:
            self.logger.error(f"Subprocess call failed while getting token. Command: {e.cmd}. Stderr: {e.stderr}")
            pytest.fail(f"A subprocess call failed: {e.cmd}, stderr: {e.stderr}")
        except Exception as e:
            self.logger.error(f"An unexpected error occurred during log insertion: {e}", exc_info=True)
            pytest.fail(f"An unexpected error occurred during log insertion: {e}")

    @pytest.mark.author(email="kdabhadkar@nvidia.com")
    @pytest.mark.csphealthmonitor
    def test_gcp_maintenance_event_cordon_and_recover(self, request, nvsentinel_autosync_disabled_enabled):
        """
        Tests if the CSPMaintenance condition is set correctly when a GCP maintenance event is detected,
        and if it's cleared upon maintenance completion.
        """
        if os.getenv("CLOUD_PROVIDER") != "gcp":
            pytest.skip("This test is specifically for GCP environments.")
        
        self.skip_if_csp_health_monitor_deployment_not_found()
        
        # Log environment variables for debugging
        self.logger.info("Environment variables:")
        self.logger.info(f"  CLOUD_PROVIDER: {os.getenv('CLOUD_PROVIDER')}")
        self.logger.info(f"  GCP_PROJECT_ID: {os.getenv('GCP_PROJECT_ID')}")
        self.logger.info(f"  GCP_PROJECT_NUMBER: {os.getenv('GCP_PROJECT_NUMBER')}")
        self.logger.info(f"  GCP_REGION: {os.getenv('GCP_REGION')}")
        self.logger.info(f"  CLOUD_CLUSTER_NAME: {os.getenv('CLOUD_CLUSTER_NAME')}")
        
        self.step_manager.print_header("Get target node for maintenance event")
        node_details = self._get_target_node_details()
        node_name = node_details["node_name"]
        
        # Verify node is initially ready using direct API call
        try:
            node = self.client.coreV1Api.read_node(node_name)
            node_ready = False
            for condition in node.status.conditions:
                if condition.type == "Ready" and condition.status == "True":
                    node_ready = True
                    break
            if not node_ready:
                pytest.fail(f"Node {node_name} is not ready before test")
        except Exception as e:
            pytest.fail(f"Failed to check node readiness: {e}")
        
        # Use a unique log name for this test run to avoid conflicts with other tests
        unique_test_id = str(uuid.uuid4())[:8]
        node_details["log_name"] = f"csp-health-monitor-test-{unique_test_id}"
        self.logger.info(f"Using unique log name for this test: {node_details['log_name']}")
        
        # Backup the configmap before updating
        config_map_name = f"{self.nv_namespace}-csp-health-monitor-config"
        self.client.backup_configmap(
            self.nv_namespace, config_map_name, self.backup_cm
        )

        self.step_manager.print_header("Update CSP monitor configuration")
        self._update_csp_monitor_config(node_details)
        
        # Add a small delay to ensure CSP monitor is fully ready
        self.logger.info("Waiting 5 seconds to ensure CSP monitor is fully ready after initial poll...")
        time.sleep(5)
        
        # Check the system time to debug timestamp issues
        try:
            date_result = subprocess.run(["date", "-u", "+%Y-%m-%dT%H:%M:%S"], capture_output=True, text=True)
            self.logger.info(f"System UTC time: {date_result.stdout.strip()}")
        except Exception as e:
            self.logger.warning(f"Could not check system time: {e}")
        
        self.step_manager.print_header("Insert PENDING maintenance event log")
        self._insert_gcp_maintenance_log(node_details, event_type="PENDING")
        
        # Wait a bit longer for log propagation
        self.logger.info("Waiting 10 seconds for log propagation in Cloud Logging...")
        time.sleep(10)

        # Check what the CSP monitor should be looking for
        self.logger.info("CSP monitor is configured with:")
        self.logger.info(f"  Project: {node_details['project_id']}")
        self.logger.info(f"  Log name filter: logName=\"projects/{node_details['project_id']}/logs/{node_details['log_name']}\"")
        self.logger.info(f"  Polling interval: 30 seconds")

        self.step_manager.print_header("Waiting for node to be cordoned and condition to be set")
        # Wait for the CSPMaintenance condition to be set
        condition_found = False
        cordoned = False
        max_wait_time = 185
        start_time = time.time()
        check_count = 0
        
        while time.time() - start_time < max_wait_time:
            check_count += 1
            elapsed = int(time.time() - start_time)
            
            # Check if node is cordoned using direct API
            try:
                node = self.client.coreV1Api.read_node(node_name)
                current_cordon_state = node.spec.unschedulable
                if current_cordon_state:
                    if not cordoned:
                        cordoned = True
                        self.logger.info(f"Node {node_name} has been cordoned")
                else:
                    self.logger.debug(f"Check {check_count} ({elapsed}s): Node not cordoned yet. Current state: {current_cordon_state}")
            except Exception as e:
                self.logger.error(f"Error reading node: {e}")
            
            # Check for CSPMaintenance condition
            condition, err = self.client.read_node_condition_by_type(node_name, "CSPMaintenance")
            if err:
                self.logger.error(f"Error reading node condition: {err}")
            elif condition and condition.status == "True":
                if not condition_found:
                    condition_found = True
                    self.logger.info(f"CSPMaintenance condition set to True: {condition.message}")
            else:
                current_condition_status = "Not Found" if not condition else condition.status
                self.logger.debug(f"Check {check_count} ({elapsed}s): CSPMaintenance condition not True. Current status: {current_condition_status}")
            
            # Exit loop only when BOTH conditions are met
            if condition_found and cordoned:
                self.logger.info("Both CSPMaintenance condition and node cordoning are complete!")
                break
            
            # Check CSP monitor logs periodically for debugging
            if elapsed > 60 and elapsed % 30 == 0:  # Check every 30s after first 60s
                self.logger.info("Checking CSP monitor pod logs for debugging...")
                try:
                    csp_pods, _ = self.client.list_pods(
                        namespace=self.nv_namespace,
                        name_pattern=".*csp-health-monitor.*"
                    )
                    if csp_pods:
                        pod_name = csp_pods[0].metadata.name
                        logs = self.client.get_pod_logs(pod_name, self.nv_namespace)
                        if logs.values[0]:
                            self.logger.info(f"Recent CSP monitor logs:\n{logs.values[0]}")
                        else:
                            self.logger.warning(f"Could not get CSP monitor logs: {logs.values[1]}")
                except Exception as e:
                    self.logger.warning(f"Error checking CSP monitor logs: {e}")
            
            # Check CSP monitor logs more frequently initially, then less frequently
            check_interval = 2 if elapsed < 20 else 5  # Check every 2 seconds for first 20 seconds, then every 5 seconds
            if check_count == 1 or elapsed % (15 if elapsed > 20 else 5) == 0:  # Log every 5s initially, then every 15s
                self.logger.info(f"Still waiting for node cordon... ({elapsed}s elapsed)")
            
            time.sleep(check_interval)
        
        assert cordoned, f"Node {node_name} was not cordoned within {max_wait_time} seconds"
        assert condition_found, f"CSPMaintenance condition was not set within {max_wait_time} seconds"
        
        self.step_manager.print_header("Insert ONGOING maintenance event log")
        self._insert_gcp_maintenance_log(node_details, event_type="ONGOING")
        
        # Wait a bit for the ONGOING event to be processed
        self.logger.info("Waiting 40 seconds for ONGOING event to be processed...")
        time.sleep(40)
        
        # Verify node is still cordoned and condition is still set after ONGOING event
        try:
            node = self.client.coreV1Api.read_node(node_name)
            assert node.spec.unschedulable, f"Node {node_name} should still be cordoned after ONGOING event"
            self.logger.info(f"✓ Node {node_name} is still cordoned after ONGOING event")
        except Exception as e:
            pytest.fail(f"Failed to verify node is still cordoned: {e}")
        
        condition, _ = self.client.read_node_condition_by_type(node_name, "CSPMaintenance")
        assert condition and condition.status == "True", "CSPMaintenance condition should still be set after ONGOING event"
        self.logger.info("✓ CSPMaintenance condition is still set after ONGOING event")
        
        self.step_manager.print_header("Insert COMPLETE maintenance event log")
        self._insert_gcp_maintenance_log(node_details, event_type="COMPLETE")
        
        self.step_manager.print_header("Waiting for node to be uncordoned and condition to be removed")
        # Wait for recovery
        uncordoned = False
        condition_removed = False
        start_time = time.time()
        
        while time.time() - start_time < max_wait_time:
            # Check if node is uncordoned using direct API
            try:
                node = self.client.coreV1Api.read_node(node_name)
                if not node.spec.unschedulable:
                    if not uncordoned: # Log only on the first successful check
                        self.logger.info(f"Node {node_name} has been uncordoned")
                    uncordoned = True
                else:
                    self.logger.debug(f"Node {node_name} is still cordoned.")
            except Exception as e:
                self.logger.error(f"Error reading node: {e}")
            
            # Check if CSPMaintenance condition is removed or set to False
            condition, _ = self.client.read_node_condition_by_type(node_name, "CSPMaintenance")
            if not condition or condition.status == "False":
                if not condition_removed: # Log only on the first successful check
                    self.logger.info("CSPMaintenance condition has been removed/cleared")
                condition_removed = True
            else:
                status_msg = condition.status if condition else "Not Found"
                self.logger.debug(f"CSPMaintenance condition still present with status: {status_msg}")

            if uncordoned and condition_removed:
                self.logger.info("Node recovered successfully.")
                break

            time.sleep(5)
        
        assert uncordoned, f"Node {node_name} was not uncordoned within {max_wait_time} seconds"
        assert condition_removed, f"CSPMaintenance condition was not removed within {max_wait_time} seconds"
        
        self.logger.info("Test completed successfully: Node was cordoned during maintenance and recovered after completion")
