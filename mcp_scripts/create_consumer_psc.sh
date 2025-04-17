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
# Define variables for consistent resource naming and targeting.
# These values specify the consumer environment where PSC endpoints will live
# and the producer environment where the target service resides.
# --- PLEASE VERIFY AND UPDATE THESE VALUES ---
CONSUMER_PROJECT_ID="proj-dgxc-runai-np-ldit01" # Your CONSUMER project ID
PRODUCER_PROJECT_ID="proj-dgxc-runai-np-msti07-mgmt" # Your PRODUCER project ID (where MongoDB and SAs live)
REGION="us-east5" # The GCP region for deploying resources
CONSUMER_VPC_NETWORK="default" # VPC network in the CONSUMER project where clients reside
CONSUMER_SUBNET="default"      # Subnet within CONSUMER_VPC_NETWORK to reserve IPs from for the PSC endpoints

# --- Cloud DNS Configuration (Consumer Side) ---
# Configures DNS resolution within the consumer VPC for the PSC endpoints.
# This allows clients to use user-friendly names instead of raw IPs.
# A name for the Cloud DNS Managed Zone resource in the CONSUMER project
CONSUMER_DNS_ZONE_NAME="consumer-psc-private-zone" # Name for the private zone to be created/used
# The DNS domain name to manage (MUST match the names used in producer certs/connection strings)
DNS_DOMAIN_NAME="psc.gcp.internal." # Note the trailing dot!

# --- Resource Naming Conventions ---
# Define base names for resources related to the MongoDB service for consistency.
RELEASE_NAME="nvsentinel" # Base name used in producer resources (e.g., Helm release)
REPLICA_COUNT=3           # Number of MongoDB replicas/endpoints to create connections for

# Base names used for naming GCP resources like IPs and endpoints
PSC_IP_BASE_NAME="${RELEASE_NAME}-mongo-psc-ip"        # Base name for static IPs reserved for PSC endpoints
PSC_ENDPOINT_BASE_NAME="${RELEASE_NAME}-mongodb-psc-ep" # Base name for PSC endpoints (Forwarding Rules)
PRODUCER_SA_BASE_NAME="${RELEASE_NAME}-mongodb-psc-sa"  # Base name for producer Service Attachments (used to target them)
# --- End Configuration ---

# Exit immediately if any command fails, ensuring script stops on errors.
set -e

# --- Script Header ---
echo "Starting Consumer-Side PSC Setup..."
echo "Consumer Project: ${CONSUMER_PROJECT_ID}"
echo "Producer Project: ${PRODUCER_PROJECT_ID}"
echo "Region: ${REGION}"
echo "Consumer VPC: ${CONSUMER_VPC_NETWORK}"
echo "Consumer Subnet: ${CONSUMER_SUBNET}"
echo "DNS Zone: ${CONSUMER_DNS_ZONE_NAME}"
echo "DNS Domain: ${DNS_DOMAIN_NAME}"
echo "=========================================="

# --- Pre-checks ---
# Validate the execution environment before proceeding.
echo "Running pre-checks..."
# Ensure gcloud is configured for the correct CONSUMER project
CURRENT_PROJECT_CHECK=$(gcloud config get-value project)
if [[ "${CURRENT_PROJECT_CHECK}" != "${CONSUMER_PROJECT_ID}" ]]; then
    echo "ERROR: gcloud is configured for project '${CURRENT_PROJECT_CHECK}'. Please configure it for the CONSUMER project: '${CONSUMER_PROJECT_ID}'"
    echo "Run: gcloud config set project ${CONSUMER_PROJECT_ID}"
    exit 1
fi
# Temporarily disable exit on error for describe commands to provide specific error messages
set +e
# Verify the specified consumer VPC network exists
gcloud compute networks describe "${CONSUMER_VPC_NETWORK}" --project="${CONSUMER_PROJECT_ID}" > /dev/null 2>&1
if [[ $? -ne 0 ]]; then
  echo "ERROR: Consumer VPC Network '${CONSUMER_VPC_NETWORK}' not found in project '${CONSUMER_PROJECT_ID}'."; set -e; exit 1;
fi
# Verify the specified consumer subnet exists in the correct region
gcloud compute networks subnets describe "${CONSUMER_SUBNET}" --region="${REGION}" --project="${CONSUMER_PROJECT_ID}" > /dev/null 2>&1
if [[ $? -ne 0 ]]; then
  echo "ERROR: Consumer Subnet '${CONSUMER_SUBNET}' not found in region '${REGION}' of project '${CONSUMER_PROJECT_ID}'."; set -e; exit 1;
fi
set -e # Re-enable exit on error
echo "Pre-checks passed."
echo "------------------------------------------"

# --- Setup Cloud DNS Zone (Consumer Project) ---
# Ensures a private DNS zone exists within the consumer project to host the A records
# mapping service DNS names to the consumer-side PSC endpoint IPs.
echo "Checking/Creating Consumer DNS Zone '${CONSUMER_DNS_ZONE_NAME}' for domain '${DNS_DOMAIN_NAME}'..."
# Temporarily disable exit on error to check zone existence without stopping the script
set +e
gcloud dns managed-zones describe "${CONSUMER_DNS_ZONE_NAME}" --project="${CONSUMER_PROJECT_ID}" > /dev/null 2>&1
ZONE_EXISTS=$?
set -e # Re-enable exit on error

# If the describe command failed (exit code != 0), the zone doesn't exist
if [[ ${ZONE_EXISTS} -ne 0 ]]; then
  echo "  DNS Zone '${CONSUMER_DNS_ZONE_NAME}' not found, attempting to create..."
  # Create the private DNS zone, making it visible only to the specified consumer VPC network
  gcloud dns managed-zones create "${CONSUMER_DNS_ZONE_NAME}" \
    --description="Private DNS zone for PSC endpoints" \
    --dns-name="${DNS_DOMAIN_NAME}" \
    --visibility=private \
    --networks="${CONSUMER_VPC_NETWORK}" \
    --project="${CONSUMER_PROJECT_ID}"
  # Explicitly check the exit status of the create command
  if [[ $? -ne 0 ]]; then
      echo "ERROR: Failed to create DNS Zone ${CONSUMER_DNS_ZONE_NAME}."
      echo "Possible reasons: Permissions (needs dns.admin), API not enabled, Name conflict."
      exit 1
  fi
  echo "  Successfully created DNS Zone '${CONSUMER_DNS_ZONE_NAME}'."
else
  # If describe succeeded (exit code 0), the zone already exists
  echo "  DNS Zone '${CONSUMER_DNS_ZONE_NAME}' already exists."
fi
echo "------------------------------------------"


# --- Loop through replicas ---
# Process each MongoDB replica to set up its corresponding PSC endpoint and DNS record.
for (( i=0; i<${REPLICA_COUNT}; i++ )); do
  # Construct resource names for the current replica index
  PSC_IP_NAME="${PSC_IP_BASE_NAME}-${i}"
  PSC_ENDPOINT_NAME="${PSC_ENDPOINT_BASE_NAME}-${i}"
  PRODUCER_SA_NAME="${PRODUCER_SA_BASE_NAME}-${i}"
  # Generate the specific DNS hostname for this replica (e.g., nvsentinel-mongodb-0.psc.gcp.internal)
  SERVICE_DNS_NAME="${RELEASE_NAME}-mongodb-${i}.${DNS_DOMAIN_NAME%?}" # Remove trailing dot for DNS record name
  # Construct the full URI of the target producer Service Attachment
  PRODUCER_SA_URI="projects/${PRODUCER_PROJECT_ID}/regions/${REGION}/serviceAttachments/${PRODUCER_SA_NAME}"

  echo "Processing Replica ${i}..."
  echo "  IP Name: ${PSC_IP_NAME}"
  echo "  Endpoint Name: ${PSC_ENDPOINT_NAME}"
  echo "  Target SA URI: ${PRODUCER_SA_URI}"
  echo "  DNS Name: ${SERVICE_DNS_NAME}."

  # 1. Reserve Static IP Address for the PSC Endpoint
  # A dedicated internal IP is required in the consumer subnet for each PSC endpoint.
  echo "  Checking/Reserving static IP '${PSC_IP_NAME}'..."
  set +e
  # Check if an address resource with this name already exists
  gcloud compute addresses describe "${PSC_IP_NAME}" --region="${REGION}" --project="${CONSUMER_PROJECT_ID}" > /dev/null 2>&1
  IP_EXISTS=$?
  set -e

  if [[ $IP_EXISTS -ne 0 ]]; then
    # If it doesn't exist, create it. Let GCP assign an available IP from the subnet.
    echo "    Attempting to create IP Address ${PSC_IP_NAME}..."
    gcloud compute addresses create "${PSC_IP_NAME}" \
      --region="${REGION}" \
      --subnet="${CONSUMER_SUBNET}" \
      --purpose=GCE_ENDPOINT `# Specifies the IP is for PSC/PrivateLink endpoint` \
      --project="${CONSUMER_PROJECT_ID}"
    # Explicitly check create status
    if [[ $? -ne 0 ]]; then
        echo "ERROR: Failed to create IP Address ${PSC_IP_NAME}. Check permissions/quota."
        exit 1
    fi
    echo "    Successfully reserved IP '${PSC_IP_NAME}'."
    # Add a small delay after creation for propagation before describing
    sleep 5
  else
    echo "    Static IP '${PSC_IP_NAME}' already exists."
  fi

  # 2. Get the actual IP address value assigned to the reserved name.
  # This is needed for creating the DNS record later. Add retry logic for robustness.
  echo "  Attempting to retrieve value for IP Address '${PSC_IP_NAME}'..."
  RESERVED_IP=""
  IP_RETRY_COUNT=5
  IP_RETRY_DELAY=5
  for ((ip_retry=1; ip_retry<=IP_RETRY_COUNT; ip_retry++)); do
      set +e # Temporarily disable exit on error for describe
      RESERVED_IP=$(gcloud compute addresses describe "${PSC_IP_NAME}" --region="${REGION}" --project="${CONSUMER_PROJECT_ID}" --format='value(address)' 2>/dev/null)
      DESCRIBE_IP_STATUS=$?
      set -e # Re-enable

      # Check if describe succeeded and returned a non-empty IP
      if [[ $DESCRIBE_IP_STATUS -eq 0 && -n "$RESERVED_IP" ]]; then
          echo "    Successfully retrieved IP Address: ${RESERVED_IP}"
          break # Exit loop on success
      else
          echo "    Warn: Failed to retrieve IP on attempt ${ip_retry}/${IP_RETRY_COUNT}. Retrying in ${IP_RETRY_DELAY}s..."
          sleep ${IP_RETRY_DELAY}
      fi
  done

  # Check if IP was retrieved successfully after retries
  if [[ -z "${RESERVED_IP}" ]]; then
      echo "ERROR: Failed to get reserved IP address value for '${PSC_IP_NAME}' after multiple attempts."
      echo "Please check if the address resource exists and is configured correctly in GCP."
      exit 1
  fi
  # --- IP value should now be set ---

  # 3. Create PSC Endpoint (Google Cloud Forwarding Rule)
  # This rule directs traffic sent to the RESERVED_IP towards the producer's Service Attachment.
  echo "  Creating PSC Endpoint (Forwarding Rule) '${PSC_ENDPOINT_NAME}' targeting '${PRODUCER_SA_URI}'..."
  # Check if forwarding rule already exists before attempting creation
  set +e
  gcloud compute forwarding-rules describe "${PSC_ENDPOINT_NAME}" --region="${REGION}" --project="${CONSUMER_PROJECT_ID}" > /dev/null 2>&1
  FW_EXISTS=$?
  set -e

  if [[ $FW_EXISTS -ne 0 ]]; then
      # Create the forwarding rule using the reserved IP name and targeting the producer SA URI
      gcloud compute forwarding-rules create "${PSC_ENDPOINT_NAME}" \
        --region="${REGION}" \
        --network="${CONSUMER_VPC_NETWORK}" \
        --address="${PSC_IP_NAME}" `# Use the reserved IP name` \
        --target-service-attachment="${PRODUCER_SA_URI}" `# Target the producer's service` \
        --project="${CONSUMER_PROJECT_ID}"

      if [[ $? -ne 0 ]]; then
          echo "ERROR: Failed to create PSC Endpoint (Forwarding Rule) ${PSC_ENDPOINT_NAME}."
          exit 1
      fi
      echo "  Successfully initiated creation of PSC Endpoint ${PSC_ENDPOINT_NAME}."
      # Add a small delay after creation before checking status
      sleep 10
  else
      echo "  PSC Endpoint (Forwarding Rule) '${PSC_ENDPOINT_NAME}' already exists."
  fi

  # 3.5 Wait for PSC Endpoint connection status to become ACCEPTED
  # The connection needs to be established and accepted by the producer (or auto-accepted)
  # before traffic can flow and before DNS records should ideally be finalized.
  echo "  Waiting for PSC Endpoint '${PSC_ENDPOINT_NAME}' connection status to become ACCEPTED..."
  ENDPOINT_STATUS=""
  WAIT_TIMEOUT=300 # 5 minutes total wait time
  WAIT_INTERVAL=10 # Check every 10 seconds
  SECONDS_WAITED=0
  while [[ "${ENDPOINT_STATUS}" != "ACCEPTED" ]]; do
      # Check timeout
      if [[ ${SECONDS_WAITED} -ge ${WAIT_TIMEOUT} ]]; then
          echo "ERROR: Timeout waiting for PSC Endpoint ${PSC_ENDPOINT_NAME} to become ACCEPTED. Last status: ${ENDPOINT_STATUS}"
          exit 1
      fi
      # Wait before checking status
      sleep ${WAIT_INTERVAL}
      SECONDS_WAITED=$((SECONDS_WAITED + WAIT_INTERVAL))
      # Use set +e to prevent script exit if describe fails temporarily
      set +e
      ENDPOINT_STATUS=$(gcloud compute forwarding-rules describe "${PSC_ENDPOINT_NAME}" --region="${REGION}" --project="${CONSUMER_PROJECT_ID}" --format="value(pscConnectionStatus)" 2>/dev/null)
      DESCRIBE_STATUS=$?
      set -e # Re-enable exit on error
      # Handle cases where describe might fail initially
      if [[ $DESCRIBE_STATUS -ne 0 ]]; then
          ENDPOINT_STATUS="NOT_FOUND_YET_OR_ERROR"
      fi
      echo "    Current status: ${ENDPOINT_STATUS} (waited ${SECONDS_WAITED}s)"
      # Optional: Add checks for explicit FAILED/REJECTED states if needed
  done
  echo "  PSC Endpoint ${PSC_ENDPOINT_NAME} is ACCEPTED."

  # 4. Create/Update DNS 'A' Record in the Consumer Zone
  # This maps the human-readable DNS name (e.g., nvsentinel-mongodb-0.psc.gcp.internal)
  # to the specific IP address of the PSC endpoint created in the consumer VPC.
  echo "  Attempting to ensure DNS A Record for '${SERVICE_DNS_NAME}' -> ${RESERVED_IP}..."

  # Use the "delete then create" pattern for idempotency, avoiding transaction issues.
  # Attempt to delete any existing record for this name FIRST.
  echo "    Attempting to delete existing record (if any)..."
  set +e
  gcloud dns record-sets delete "${SERVICE_DNS_NAME}." \
    --type=A \
    --zone="${CONSUMER_DNS_ZONE_NAME}" \
    --project="${CONSUMER_PROJECT_ID}"
  DELETE_STATUS=$?
  if [[ $DELETE_STATUS -ne 0 ]]; then
     # 404 is expected if the record doesn't exist, only warn on other errors
     if [[ $(gcloud --quiet compute addresses describe "${SERVICE_DNS_NAME}" 2>&1 | grep -c "NotFoundError") -eq 0 ]]; then
       echo "    Note: Delete command finished with status ${DELETE_STATUS} (404/Not Found is expected if record didn't exist)."
     fi
  fi
  set -e # Re-enable error checking

  # Now, explicitly create the record with the current reserved IP.
  echo "    Attempting to create record pointing to ${RESERVED_IP}..."
  gcloud dns record-sets create "${SERVICE_DNS_NAME}." \
    --rrdatas="${RESERVED_IP}" \
    --type=A \
    --ttl=300 \
    --zone="${CONSUMER_DNS_ZONE_NAME}" \
    --project="${CONSUMER_PROJECT_ID}"
  CREATE_STATUS=$?

  # Check if create succeeded
  if [[ $CREATE_STATUS -eq 0 ]]; then
      echo "    DNS Record created/updated successfully."
  else
      echo "    ERROR: Failed to create DNS record set for '${SERVICE_DNS_NAME}' with IP ${RESERVED_IP}. Check permissions or zone status."
      # Decide whether to exit or continue loop based on requirements
      exit 1
  fi

  echo "------------------------------------------"

done # End loop

echo "Consumer-Side PSC Setup Completed Successfully!"
echo "You should now be able to resolve '${RELEASE_NAME}-mongodb-X.${DNS_DOMAIN_NAME%?}' within the '${CONSUMER_VPC_NETWORK}' network to the PSC endpoint IPs."