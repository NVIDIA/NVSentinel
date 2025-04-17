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

#!/bin/bash

# --- Configuration ---
# Define variables matching the producer resources that were created.
# These need to match the values used in the corresponding creation script.
# --- MUST MATCH THE VALUES USED IN THE CREATION SCRIPT ---
PROJECT_ID="proj-dgxc-runai-np-msti07-mgmt" # Producer Project ID where resources reside
REGION="us-east5"                            # GCP Region where resources were created
RELEASE_NAME="nvsentinel"                  # Base name (e.g., Helm release) used for resources
SERVICE_ATTACHMENT_BASE_NAME="${RELEASE_NAME}-mongodb-psc-sa" # Base name used for Service Attachments
REPLICA_COUNT=3                              # Number of MongoDB replicas/resources to clean up
# --- End Configuration ---

# Exit on any error during critical checks
# We'll handle errors within the loop more gracefully using set +/-e

echo "Starting Producer-Side PSC Resource Cleanup..."
echo "Project: ${PROJECT_ID}"
echo "Region: ${REGION}"
echo "Release Name: ${RELEASE_NAME}"
echo "=========================================="

# --- Pre-check ---
# Verify the script is being run with gcloud configured for the correct producer project.
echo "Running pre-checks..."
CURRENT_PROJECT=$(gcloud config get-value project)
if [[ "${CURRENT_PROJECT}" != "${PROJECT_ID}" ]]; then
    echo "ERROR: gcloud is configured for project '${CURRENT_PROJECT}'. Please configure it for the PRODUCER project: '${PROJECT_ID}'"
    echo "Run: gcloud config set project ${PROJECT_ID}"
    exit 1
fi
echo "Pre-checks passed."
echo "------------------------------------------"


# Loop through replica indices (0 to REPLICA_COUNT - 1)
# Iterate through each replica to delete its associated Service Attachment and NAT subnet.
for (( i=0; i<${REPLICA_COUNT}; i++ )); do
  # Construct the names of the resources for the current replica index
  NAT_SUBNET_NAME="psc-nat-subnet-${i}" # Name of the dedicated NAT subnet
  SERVICE_ATTACHMENT_NAME="${SERVICE_ATTACHMENT_BASE_NAME}-${i}" # Name of the Service Attachment

  echo "Processing cleanup for replica ${i}..."

  # 1. Delete Service Attachment
  # Service Attachments must be deleted BEFORE the NAT subnets they reference.
  echo "  Attempting to delete Service Attachment '${SERVICE_ATTACHMENT_NAME}'..."
  # Check if it exists before trying to delete to make script idempotent and avoid unnecessary errors.
  # Temporarily disable exit on error for the describe command.
  set +e
  gcloud compute service-attachments describe "${SERVICE_ATTACHMENT_NAME}" --region="${REGION}" --project="${PROJECT_ID}" > /dev/null 2>&1
  SA_EXISTS=$?
  set -e # Re-enable exit on error

  if [[ $SA_EXISTS -eq 0 ]]; then
      # Service Attachment exists, proceed with deletion.
      echo "    Service Attachment found. Deleting..."
      gcloud compute service-attachments delete "${SERVICE_ATTACHMENT_NAME}" \
        --region="${REGION}" \
        --project="${PROJECT_ID}" \
        --quiet # Suppress confirmation prompt

      # Check the exit status of the delete command
      if [[ $? -ne 0 ]]; then
        echo "  WARNING: Failed to delete Service Attachment ${SERVICE_ATTACHMENT_NAME}. It might be in use (check connected endpoints) or require manual deletion."
      else
        echo "  Successfully deleted Service Attachment ${SERVICE_ATTACHMENT_NAME}."
      fi
  else
      # Service Attachment not found, likely already deleted.
      echo "  Service Attachment ${SERVICE_ATTACHMENT_NAME} not found, skipping deletion."
  fi

  # 2. Delete dedicated NAT Subnet
  # Now that the Service Attachment referencing it (should be) gone, delete the NAT subnet.
  echo "  Attempting to delete NAT subnet '${NAT_SUBNET_NAME}'..."
  # Check if it exists before trying to delete.
  set +e
  gcloud compute networks subnets describe "${NAT_SUBNET_NAME}" --region="${REGION}" --project="${PROJECT_ID}" > /dev/null 2>&1
  SUBNET_EXISTS=$?
  set -e

   if [[ $SUBNET_EXISTS -eq 0 ]]; then
      # Subnet exists, proceed with deletion.
      echo "    NAT Subnet found. Deleting..."
      gcloud compute networks subnets delete "${NAT_SUBNET_NAME}" \
        --region="${REGION}" \
        --project="${PROJECT_ID}" \
        --quiet # Suppress confirmation prompt

      # Check the exit status of the delete command
      if [[ $? -ne 0 ]]; then
        echo "  WARNING: Failed to delete NAT subnet ${NAT_SUBNET_NAME}. Check if it's still in use (e.g., by the Service Attachment if its deletion failed) or requires manual intervention."
      else
        echo "  Successfully deleted NAT subnet ${NAT_SUBNET_NAME}."
      fi
   else
        # Subnet not found, likely already deleted.
        echo "  NAT subnet ${NAT_SUBNET_NAME} not found, skipping deletion."
   fi

  echo "------------------------------------------"

done

echo "Producer-Side PSC Resource Cleanup Attempt Completed."