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
# Define variables matching the consumer resources that were created.
# These MUST match the values used in the corresponding creation script.
# --- MUST MATCH THE VALUES USED IN THE CREATION SCRIPT ---
CONSUMER_PROJECT_ID="proj-dgxc-runai-np-ldit01" # Your CONSUMER project ID where resources reside
REGION="us-east5"                            # GCP Region where resources were created

# --- Cloud DNS Configuration (Consumer Side) ---
# Specify the DNS zone where records were created.
CONSUMER_DNS_ZONE_NAME="consumer-psc-private-zone" # The name of the managed zone to clean up records in
DNS_DOMAIN_NAME="psc.gcp.internal."                 # The domain name used (needed to construct record names)

# --- Resource Naming Conventions ---
# Define base names used for the consumer-side resources.
RELEASE_NAME="nvsentinel" # Base name (e.g., Helm release) used for resources
REPLICA_COUNT=3           # Number of MongoDB replicas/endpoints to clean up

# Base names used when creating consumer resources
PSC_IP_BASE_NAME="${RELEASE_NAME}-mongo-psc-ip"        # Base name for the static IPs
PSC_ENDPOINT_BASE_NAME="${RELEASE_NAME}-mongodb-psc-ep" # Base name for the PSC endpoints (Forwarding Rules)
# --- End Configuration ---

# Exit on any error during critical checks
# We'll handle errors within the loop more gracefully using set +/-e

echo "Starting Consumer-Side PSC Resource Cleanup..."
echo "Consumer Project: ${CONSUMER_PROJECT_ID}"
echo "Region: ${REGION}"
echo "DNS Zone Name: ${CONSUMER_DNS_ZONE_NAME}"
echo "DNS Domain: ${DNS_DOMAIN_NAME}"
echo "=========================================="

# --- Pre-check ---
echo "Running pre-checks..."
CURRENT_PROJECT=$(gcloud config get-value project)
if [[ "${CURRENT_PROJECT}" != "${CONSUMER_PROJECT_ID}" ]]; then
    echo "ERROR: gcloud is configured for project '${CURRENT_PROJECT}'. Please configure it for the CONSUMER project: '${CONSUMER_PROJECT_ID}'"
    echo "Run: gcloud config set project ${CONSUMER_PROJECT_ID}"
    exit 1
fi
echo "Pre-checks passed."
echo "------------------------------------------"


# --- Loop through replicas ---
for (( i=0; i<${REPLICA_COUNT}; i++ )); do
  PSC_IP_NAME="${PSC_IP_BASE_NAME}-${i}"
  PSC_ENDPOINT_NAME="${PSC_ENDPOINT_BASE_NAME}-${i}"
  # DNS Name (without trailing dot for record set name, add dot for commands)
  SERVICE_DNS_NAME="${RELEASE_NAME}-mongodb-${i}.${DNS_DOMAIN_NAME%?}"

  echo "Processing cleanup for Replica ${i}..."
  echo "  Targeting IP: ${PSC_IP_NAME}"
  echo "  Targeting Endpoint: ${PSC_ENDPOINT_NAME}"
  echo "  Targeting DNS Record: ${SERVICE_DNS_NAME}. "

  # 1. Delete DNS 'A' Record
  # Remove the DNS record mapping the service name to the PSC endpoint IP.
  echo "  Attempting to delete DNS A Record '${SERVICE_DNS_NAME}.'..."
  # Get the IP address the record currently points to, needed for deletion via transaction.
  # Temporarily disable exit on error as list fails if record doesn't exist.
  set +e
  EXISTING_IP=$(gcloud dns record-sets list --zone="${CONSUMER_DNS_ZONE_NAME}" --name="${SERVICE_DNS_NAME}." --type=A --project="${CONSUMER_PROJECT_ID}" --format="value(rrdatas[0])" 2>/dev/null)
  LIST_STATUS=$?
  set -e # Re-enable

  if [[ $LIST_STATUS -eq 0 && -n "$EXISTING_IP" ]]; then
      # Record exists, delete it using a transaction for atomicity.
      # Get TTL for the existing record to ensure correct removal.
      set +e
      EXISTING_TTL=$(gcloud dns record-sets list --zone="${CONSUMER_DNS_ZONE_NAME}" --name="${SERVICE_DNS_NAME}." --type=A --project="${CONSUMER_PROJECT_ID}" --format="value(ttl)" 2>/dev/null)
      if [[ $? -ne 0 || -z "$EXISTING_TTL" ]]; then
          echo "    WARNING: Could not determine TTL for A record ${SERVICE_DNS_NAME}. Using default 300."
          EXISTING_TTL=300
      fi
      set -e

      echo "    Record found. Starting transaction to delete..."
      set +e # Disable exit on error for transaction block
      gcloud dns record-sets transaction start --zone="${CONSUMER_DNS_ZONE_NAME}" --project="${CONSUMER_PROJECT_ID}"
      if [[ $? -eq 0 ]]; then
          gcloud dns record-sets transaction remove "${EXISTING_IP}" --name="${SERVICE_DNS_NAME}." --ttl=${EXISTING_TTL} --type=A --zone="${CONSUMER_DNS_ZONE_NAME}" --project="${CONSUMER_PROJECT_ID}"
          gcloud dns record-sets transaction execute --zone="${CONSUMER_DNS_ZONE_NAME}" --project="${CONSUMER_PROJECT_ID}"
          if [[ $? -eq 0 ]]; then
              echo "    Successfully deleted DNS A Record via transaction."
          else
              echo "    WARNING: Failed to execute DNS delete transaction for '${SERVICE_DNS_NAME}.'. Manual cleanup might be needed."
              gcloud dns record-sets transaction abort --zone="${CONSUMER_DNS_ZONE_NAME}" --project="${CONSUMER_PROJECT_ID}" > /dev/null 2>&1
          fi
      else
          echo "    WARNING: Failed to start DNS transaction for deleting '${SERVICE_DNS_NAME}.'."
      fi
      set -e # Re-enable exit on error
  else
      echo "    DNS A Record '${SERVICE_DNS_NAME}.' not found, skipping deletion."
  fi

  # 2. Delete PSC Endpoint (Forwarding Rule)
  # This removes the endpoint that tunnels traffic to the producer.
  # MUST be done before deleting the Static IP it uses.
  echo "  Attempting to delete PSC Endpoint '${PSC_ENDPOINT_NAME}'..."
  # Check if it exists before trying to delete.
  set +e
  gcloud compute forwarding-rules describe "${PSC_ENDPOINT_NAME}" --region="${REGION}" --project="${CONSUMER_PROJECT_ID}" > /dev/null 2>&1
  FW_EXISTS=$?
  set -e

  if [[ $FW_EXISTS -eq 0 ]]; then
      # Forwarding rule exists, proceed with deletion.
      echo "    Endpoint found. Deleting..."
      gcloud compute forwarding-rules delete "${PSC_ENDPOINT_NAME}" \
        --region="${REGION}" \
        --project="${CONSUMER_PROJECT_ID}" \
        --quiet

      if [[ $? -ne 0 ]]; then
          echo "    WARNING: Failed to delete PSC Endpoint ${PSC_ENDPOINT_NAME}. It might already be gone or require manual deletion."
      else
          echo "    Successfully deleted PSC Endpoint ${PSC_ENDPOINT_NAME}."
      fi
  else
      # Forwarding rule not found, likely already deleted.
      echo "    PSC Endpoint ${PSC_ENDPOINT_NAME} not found, skipping deletion."
  fi

  # 3. Delete Static IP Address
  # Release the internal IP address reserved for the PSC endpoint.
  echo "  Attempting to delete Static IP Address '${PSC_IP_NAME}'..."
  # Check if it exists before trying to delete.
  set +e
  gcloud compute addresses describe "${PSC_IP_NAME}" --region="${REGION}" --project="${CONSUMER_PROJECT_ID}" > /dev/null 2>&1
  IP_EXISTS=$?
  set -e

  if [[ $IP_EXISTS -eq 0 ]]; then
      # IP Address exists, proceed with deletion.
      echo "    Static IP found. Deleting..."
      gcloud compute addresses delete "${PSC_IP_NAME}" \
        --region="${REGION}" \
        --project="${CONSUMER_PROJECT_ID}" \
        --quiet

      if [[ $? -ne 0 ]]; then
          echo "    WARNING: Failed to delete Static IP Address ${PSC_IP_NAME}. Check if it's still in use (e.g., by the Forwarding Rule if its deletion failed)."
      else
          echo "    Successfully deleted Static IP Address ${PSC_IP_NAME}."
      fi
  else
      # IP Address not found, likely already deleted.
      echo "    Static IP Address ${PSC_IP_NAME} not found, skipping deletion."
  fi

  echo "------------------------------------------"

done

echo "------------------------------------------"

# Check if jq is installed (required for parsing JSON output below)
if ! command -v jq &> /dev/null; then
    echo "WARNING: jq command not found. Skipping cleanup of remaining DNS records and zone deletion."
    echo "Consumer-Side PSC Resource Cleanup Attempt Completed (Zone/Remaining Records Skipped)."
    exit 0 # Exit gracefully without attempting further DNS cleanup
fi

echo "------------------------------------------"

# Delete ALL remaining DNS Record Sets (except NS/SOA) within the Managed Zone
echo "Attempting to delete any remaining DNS record sets in consumer zone '${CONSUMER_DNS_ZONE_NAME}'..."

# Check if the zone exists before trying to delete records
set +e
gcloud dns managed-zones describe "${CONSUMER_DNS_ZONE_NAME}" --project="${CONSUMER_PROJECT_ID}" --format="value(name)" > /dev/null 2>&1
ZONE_EXISTS_FINAL=$?
set -e

if [[ $ZONE_EXISTS_FINAL -eq 0 ]]; then
    # List all record sets in JSON, filter out NS and SOA
    mapfile -t record_lines < <(gcloud dns record-sets list \
        --zone="${CONSUMER_DNS_ZONE_NAME}" \
        --project="${CONSUMER_PROJECT_ID}" \
        --format="json" | \
        jq -c '.[] | select(.type != "NS" and .type != "SOA")')

    if [ ${#record_lines[@]} -eq 0 ]; then
        echo "No remaining deletable record sets found in zone '${CONSUMER_DNS_ZONE_NAME}'."
        TRANSACTION_NEEDED=false
    else
        echo "Found ${#record_lines[@]} remaining record set(s) to delete."
        TRANSACTION_NEEDED=true
        # Start transaction
        echo "Starting transaction to delete remaining record sets..."
        set +e
        gcloud dns record-sets transaction start --zone="${CONSUMER_DNS_ZONE_NAME}" --project="${CONSUMER_PROJECT_ID}"
        if [[ $? -ne 0 ]]; then
            echo "ERROR: Failed to start DNS transaction. Aborting remaining record deletion."
            TRANSACTION_NEEDED=false
        fi
        set -e
    fi

    if [ "$TRANSACTION_NEEDED" = true ]; then
        TRANSACTION_ABORTED=false
        for record_json in "${record_lines[@]}"; do
            # Extract fields using jq for accuracy
            name=$(echo "$record_json" | jq -r '.name')
            type=$(echo "$record_json" | jq -r '.type')
            ttl=$(echo "$record_json" | jq -r '.ttl')

            # Create an array to hold the arguments for the transaction remove command
            cmd_args=("--name" "$name" "--type" "$type" "--ttl" "$ttl" "--zone" "${CONSUMER_DNS_ZONE_NAME}" "--project" "${CONSUMER_PROJECT_ID}")

            # Read rrdatas into a bash array
            readarray -t rrdatas_array < <(echo "$record_json" | jq -r '.rrdatas[]')

            # Add each rrdata element prefixed by '--'
            for data in "${rrdatas_array[@]}"; do
                 # Trim leading/trailing whitespace which might come from jq/readarray processing
                 trimmed_data=$(echo -n "$data" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
                 if [[ -n "$trimmed_data" ]]; then # Ensure we don't add empty data
                    cmd_args+=("--" "$trimmed_data") # Use -- to separate data that might look like flags
                 fi
            done

            echo "Adding removal to transaction: gcloud dns record-sets transaction remove ${cmd_args[*]}"
            set +e
            gcloud dns record-sets transaction remove "${cmd_args[@]}"
            REMOVE_STATUS=$?
            set -e
            if [[ $REMOVE_STATUS -ne 0 ]]; then
                echo "ERROR: Failed to add record removal to transaction: Name: $name, Type: $type"
                echo "Aborting transaction..."
                gcloud dns record-sets transaction abort --zone="${CONSUMER_DNS_ZONE_NAME}" --project="${CONSUMER_PROJECT_ID}" > /dev/null 2>&1
                echo "Cleanup may be incomplete. Please check DNS zone '${CONSUMER_DNS_ZONE_NAME}'."
                TRANSACTION_ABORTED=true
                break # Exit loop on error
            fi
        done

        if [ "$TRANSACTION_ABORTED" = false ]; then
            # Execute transaction
            echo "Executing transaction for consumer zone..."
            set +e
            gcloud dns record-sets transaction execute --zone="${CONSUMER_DNS_ZONE_NAME}" --project="${CONSUMER_PROJECT_ID}"
            EXECUTE_STATUS=$?
            set -e
            if [[ $EXECUTE_STATUS -eq 0 ]]; then
                echo "Successfully deleted remaining record sets from consumer zone '${CONSUMER_DNS_ZONE_NAME}'."
            else
                echo "ERROR: Failed to execute transaction to delete remaining record sets from consumer zone '${CONSUMER_DNS_ZONE_NAME}'."
                # Continue to attempt zone deletion anyway
            fi
        fi
    fi # End if TRANSACTION_NEEDED

else
    echo "Consumer DNS zone '${CONSUMER_DNS_ZONE_NAME}' not found, skipping record set deletion and zone deletion."
fi

echo "------------------------------------------"

echo "Consumer-Side PSC Resource Cleanup Attempt Completed."