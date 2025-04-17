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
# Define variables for the producer environment where MongoDB runs
# and where the PSC Service Attachments will be published.
PROJECT_ID="proj-dgxc-runai-np-msti07-mgmt" # Producer GCP Project ID
REGION="us-east5"                            # GCP Region for resources
VPC_NETWORK="mgmt"                         # VPC network in the producer project
NAMESPACE="dgxc-system"                          # Kubernetes namespace where MongoDB service runs
RELEASE_NAME="nvsentinel"                  # Base name (e.g., Helm release) for MongoDB resources
SERVICE_ATTACHMENT_BASE_NAME="${RELEASE_NAME}-mongodb-psc-sa" # Base name for created Service Attachments
REPLICA_COUNT=3                              # Number of MongoDB replicas/services to publish

# --- NAT Subnet Configuration ---
# Configuration for creating dedicated subnets required for PSC Service Attachments.
# Each Service Attachment needs its own NAT subnet within the producer VPC.
# Define a base CIDR block that is FREE in your VPC and large enough
# to hold REPLICA_COUNT number of /29 subnets.
NAT_SUBNET_BASE_NETWORK="10.10.10.0" # First part of the base CIDR (e.g., 10.10.10.0)
NAT_SUBNET_BASE_PREFIX=27           # Prefix of the base CIDR (e.g., 27 -> allows for multiple /29s)
NAT_SUBNET_ALLOCATION_PREFIX=29     # Size of each individual NAT subnet (usually /29)

# --- Basic Validation for NAT Subnet Space ---
# Ensure the chosen base CIDR block is large enough for the required number of NAT subnets.
if [[ $((NAT_SUBNET_ALLOCATION_PREFIX - NAT_SUBNET_BASE_PREFIX)) -lt $((REPLICA_COUNT / 2)) ]]; then
  echo "ERROR: NAT_SUBNET_BASE_NETWORK/NAT_SUBNET_BASE_PREFIX (${NAT_SUBNET_BASE_NETWORK}/${NAT_SUBNET_BASE_PREFIX}) is not large enough to hold ${REPLICA_COUNT} subnets of size /${NAT_SUBNET_ALLOCATION_PREFIX}."
  exit 1
fi
# --- End Configuration ---

# --- Script Header ---
echo "Starting PSC Service Attachment creation..."
echo "Project: ${PROJECT_ID}"
echo "Region: ${REGION}"
echo "VPC Network: ${VPC_NETWORK}"
echo "Namespace: ${NAMESPACE}" # K8s namespace where source services reside
echo "Release Name: ${RELEASE_NAME}"
echo "=========================================="

# Exit immediately if any command fails.
set -e

# --- Pre-checks (Optional but Recommended) ---
# Verify execution environment
echo "Running pre-checks..."
CURRENT_PROJECT_CHECK=$(gcloud config get-value project)
if [[ "${CURRENT_PROJECT_CHECK}" != "${PROJECT_ID}" ]]; then
    echo "ERROR: gcloud is configured for project '${CURRENT_PROJECT_CHECK}'. Please configure it for the PRODUCER project: '${PROJECT_ID}'"
    echo "Run: gcloud config set project ${PROJECT_ID}"
    exit 1
fi
# Verify producer network exists
set +e
gcloud compute networks describe "${VPC_NETWORK}" --project="${PROJECT_ID}" > /dev/null 2>&1
if [[ $? -ne 0 ]]; then
  echo "ERROR: Producer VPC Network '${VPC_NETWORK}' not found in project '${PROJECT_ID}'."; set -e; exit 1;
fi
set -e
echo "Pre-checks passed."
echo "------------------------------------------"


# --- Loop through replica indices (0 to REPLICA_COUNT - 1) ---
# Iterate through each MongoDB replica to create its corresponding NAT subnet and Service Attachment.
for (( i=0; i<${REPLICA_COUNT}; i++ )); do
  # Define names for Kubernetes service, NAT subnet, and Service Attachment for this replica
  K8S_SERVICE_NAME="${RELEASE_NAME}-mongodb-${i}-external" # K8s Service created by Helm (type: LoadBalancer)
  NAT_SUBNET_NAME="psc-nat-subnet-${i}"                   # Unique name for the dedicated NAT subnet
  SERVICE_ATTACHMENT_NAME="${SERVICE_ATTACHMENT_BASE_NAME}-${i}" # Unique name for the Service Attachment

  echo "Processing replica ${i} (K8s Service: ${K8S_SERVICE_NAME})..."

  # --- Create dedicated NAT Subnet for this replica ---
  # Each Service Attachment requires its own subnet with purpose PRIVATE_SERVICE_CONNECT.
  echo "  Checking/Creating dedicated NAT subnet ${NAT_SUBNET_NAME}..."

  # Calculate the specific CIDR for this subnet based on the base network and index
  # (Simple implementation assuming adjacent /29 blocks)
  octet1=$(echo $NAT_SUBNET_BASE_NETWORK | cut -d. -f1)
  octet2=$(echo $NAT_SUBNET_BASE_NETWORK | cut -d. -f2)
  octet3=$(echo $NAT_SUBNET_BASE_NETWORK | cut -d. -f3)
  octet4_base=$(echo $NAT_SUBNET_BASE_NETWORK | cut -d. -f4)
  octet4=$(( octet4_base + (i * 8) )) # 8 IPs per /29 block

  # Validate calculated IP octet
  if [[ $octet4 -gt 255 ]]; then
     echo "ERROR: Calculated NAT subnet IP range exceeds valid octet value. Adjust NAT_SUBNET_BASE_NETWORK."
     exit 1
  fi
  NAT_SUBNET_CIDR="${octet1}.${octet2}.${octet3}.${octet4}/${NAT_SUBNET_ALLOCATION_PREFIX}"
  echo "  Calculated NAT Subnet CIDR: ${NAT_SUBNET_CIDR}"

  # Check if subnet already exists to make script idempotent
  set +e
  gcloud compute networks subnets describe "${NAT_SUBNET_NAME}" --region="${REGION}" --project="${PROJECT_ID}" > /dev/null 2>&1
  SUBNET_EXISTS=$?
  set -e

  if [[ ${SUBNET_EXISTS} -ne 0 ]]; then
      # Subnet does not exist, create it
      echo "    Attempting to create NAT subnet '${NAT_SUBNET_NAME}'..."
      gcloud compute networks subnets create "${NAT_SUBNET_NAME}" \
        --network="${VPC_NETWORK}" \
        --region="${REGION}" \
        --purpose=PRIVATE_SERVICE_CONNECT `# Critical: Sets the subnet purpose` \
        --role=ACTIVE \
        --range="${NAT_SUBNET_CIDR}" \
        --project="${PROJECT_ID}"

      if [[ $? -ne 0 ]]; then
        echo "  ERROR: Failed to create NAT subnet ${NAT_SUBNET_NAME}. Check gcloud output/permissions."
        exit 1
      else
        echo "  Successfully created NAT subnet ${NAT_SUBNET_NAME}."
      fi
  else
       echo "  NAT subnet ${NAT_SUBNET_NAME} already exists."
  fi
  # --- End NAT Subnet Creation ---


  # 1. Get the External IP assigned to the Kubernetes service
  # This IP belongs to the Internal Load Balancer created by GKE for the K8s Service.
  MAX_RETRIES=10
  RETRY_DELAY=15 # seconds
  IP_ADDRESS=""
  echo "  Attempting to get IP address for service ${K8S_SERVICE_NAME} from Kubernetes..."
  # Retry loop to wait for the LoadBalancer IP to be assigned by the GKE controller
  for ((retry=1; retry<=MAX_RETRIES; retry++)); do
    # Use kubectl (requires context set to producer cluster) to get the service status
    # Use set +e because kubectl might fail if service doesn't exist yet
    set +e
    IP_ADDRESS=$(kubectl get svc "${K8S_SERVICE_NAME}" -n "${NAMESPACE}" -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null)
    KUBECTL_STATUS=$?
    set -e

    # Check if kubectl succeeded and IP is assigned and not empty
    if [[ $KUBECTL_STATUS -eq 0 && -n "$IP_ADDRESS" && "$IP_ADDRESS" != "<pending>" ]]; then
      echo "  Found IP Address: ${IP_ADDRESS}"
      break # Exit loop successfully
    else
      IP_ADDRESS="" # Reset IP if not found or pending
      if [[ ${retry} -eq ${MAX_RETRIES} ]]; then
         echo "  ERROR: IP address for ${K8S_SERVICE_NAME} still pending after ${MAX_RETRIES} attempts."
         echo "  Check 'kubectl describe svc ${K8S_SERVICE_NAME} -n ${NAMESPACE}' for errors."
         exit 1
      fi
      echo "  IP address for ${K8S_SERVICE_NAME} not found or pending (Attempt ${retry}/${MAX_RETRIES}). Waiting ${RETRY_DELAY}s..."
      sleep ${RETRY_DELAY}
    fi
  done

  # 2. Get the GCP Forwarding Rule name associated with that IP address
  # The Service Attachment needs to target the Forwarding Rule, not just the IP.
  echo "  Querying GCP for Forwarding Rule name with IP ${IP_ADDRESS}..."
  FWD_RULE_NAME=""
  # Retry loop in case the Forwarding Rule isn't immediately visible via API after creation
  for ((fwd_retry=1; fwd_retry<=MAX_RETRIES; fwd_retry++)); do
      set +e # Disable exit on error for list command
      FWD_RULE_NAME=$(gcloud compute forwarding-rules list \
        --filter="loadBalancingScheme=INTERNAL AND region=${REGION} AND IPAddress='${IP_ADDRESS}'" \
        --project="${PROJECT_ID}" \
        --format="value(name)" 2>/dev/null)
      LIST_STATUS=$?
      set -e # Re-enable

      if [[ $LIST_STATUS -eq 0 && -n "$FWD_RULE_NAME" ]]; then
          echo "  Found Forwarding Rule Name: ${FWD_RULE_NAME} (on attempt ${fwd_retry})"
          break # Exit loop on success
      else
          FWD_RULE_NAME="" # Ensure it's empty if not found
          if [[ ${fwd_retry} -eq ${MAX_RETRIES} ]]; then
              echo "ERROR: Could not find GCP Forwarding Rule for IP ${IP_ADDRESS} after ${MAX_RETRIES} attempts."
              echo "Ensure the Helm chart was deployed successfully and the GKE service controller created the ILB."
              exit 1
          fi
          echo "  Forwarding rule for IP ${IP_ADDRESS} not found yet (Attempt ${fwd_retry}/${MAX_RETRIES}). Waiting ${RETRY_DELAY}s..."
          sleep ${RETRY_DELAY}
      fi
  done

  # 3. Create the Service Attachment
  # This resource publishes the service (via the Forwarding Rule) for PSC consumption.
  echo "  Checking/Creating Service Attachment ${SERVICE_ATTACHMENT_NAME} using NAT subnet ${NAT_SUBNET_NAME}..."
  set +e
  gcloud compute service-attachments describe "${SERVICE_ATTACHMENT_NAME}" --region="${REGION}" --project="${PROJECT_ID}" > /dev/null 2>&1
  SA_EXISTS=$?
  set -e

  if [[ ${SA_EXISTS} -ne 0 ]]; then
      echo "    Attempting to create Service Attachment '${SERVICE_ATTACHMENT_NAME}'..."
      gcloud compute service-attachments create "${SERVICE_ATTACHMENT_NAME}" \
        --region="${REGION}" \
        --producer-forwarding-rule="${FWD_RULE_NAME}" `# Target the ILB's Forwarding Rule` \
        --connection-preference=ACCEPT_AUTOMATIC `# Auto-accept connections from consumers` \
        --nat-subnets="${NAT_SUBNET_NAME}" `# Use the dedicated NAT subnet` \
        --project="${PROJECT_ID}"

      if [[ $? -ne 0 ]]; then
        echo "  ERROR: Failed to create Service Attachment ${SERVICE_ATTACHMENT_NAME}. Check gcloud output/permissions."
        exit 1
      else
        echo "  Successfully created Service Attachment ${SERVICE_ATTACHMENT_NAME}."
      fi
  else
       echo "  Service Attachment ${SERVICE_ATTACHMENT_NAME} already exists."
  fi

  echo "------------------------------------------"

done

echo "All Service Attachments created/verified successfully."