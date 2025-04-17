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

# Variables (should match the ones used in configure_dns_mgmt.sh)
DNS_ZONE_NAME="psc-internal-zone"
DNS_DOMAIN_NAME="psc.gcp.internal." # Used for informational messages
VPC_NETWORK="mgmt" # Used for informational messages
PROJECT_ID="proj-dgxc-runai-np-msti07-mgmt"
GSA_NAME="external-dns-manager"
GSA_EMAIL="${GSA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
K8S_NAMESPACE="external-dns-gcp"
K8S_SERVICE_ACCOUNT="external-dns"
HELM_RELEASE_NAME="external-dns"

# --- Check for required tools ---
if ! command -v jq &> /dev/null; then
    echo "ERROR: jq command not found. Please install jq (e.g., 'sudo apt-get install jq' or 'brew install jq')."
    exit 1
fi
# --- End Check ---

echo "Starting cleanup of ExternalDNS resources for project ${PROJECT_ID}..."

# 1. Uninstall Helm Release
echo "Attempting to uninstall Helm release '${HELM_RELEASE_NAME}' in namespace '${K8S_NAMESPACE}'..."
helm uninstall "${HELM_RELEASE_NAME}" --namespace "${K8S_NAMESPACE}" > /dev/null 2>&1 # Suppress helm output unless error
if [ $? -eq 0 ]; then
  echo "Successfully uninstalled Helm release '${HELM_RELEASE_NAME}'."
else
  # Try finding the release if uninstall failed (maybe different name/namespace?)
  if helm status "${HELM_RELEASE_NAME}" --namespace "${K8S_NAMESPACE}" > /dev/null 2>&1; then
      echo "Helm uninstall failed for '${HELM_RELEASE_NAME}'. Please check manually."
  else
      echo "Helm release '${HELM_RELEASE_NAME}' not found in namespace '${K8S_NAMESPACE}'."
  fi
fi

# 2. Remove Workload Identity Binding (KSA -> GSA)
echo "Attempting to remove Workload Identity binding for KSA [${K8S_NAMESPACE}/${K8S_SERVICE_ACCOUNT}] from GSA [${GSA_EMAIL}]..."
# Check if GSA exists before attempting removal
if gcloud iam service-accounts describe "${GSA_EMAIL}" --project="${PROJECT_ID}" --format="value(name)" > /dev/null 2>&1; then
    gcloud iam service-accounts remove-iam-policy-binding "${GSA_EMAIL}" \
      --role="roles/iam.workloadIdentityUser" \
      --member="serviceAccount:${PROJECT_ID}.svc.id.goog[${K8S_NAMESPACE}/${K8S_SERVICE_ACCOUNT}]" \
      --project="${PROJECT_ID}" \
      --quiet
    if [ $? -eq 0 ]; then
        echo "Successfully removed Workload Identity binding."
    else
        echo "Failed to remove Workload Identity binding (might not exist or permissions issue)."
    fi
else
    echo "GSA [${GSA_EMAIL}] not found, skipping Workload Identity binding removal."
fi


# 3. Remove IAM Policy Binding (GSA DNS Admin Role)
echo "Attempting to remove DNS Admin role binding for GSA [${GSA_EMAIL}] from project [${PROJECT_ID}]..."
# Check if GSA exists before attempting removal
if gcloud iam service-accounts describe "${GSA_EMAIL}" --project="${PROJECT_ID}" --format="value(name)" > /dev/null 2>&1; then
  gcloud projects remove-iam-policy-binding "${PROJECT_ID}" \
    --member="serviceAccount:${GSA_EMAIL}" \
    --role="roles/dns.admin" \
    --condition=None \
    --quiet # Added --condition=None as it's often required for removals
  if [ $? -eq 0 ]; then
      echo "Successfully removed DNS Admin role binding."
  else
      # Check if the binding actually exists before declaring failure
      if gcloud projects get-iam-policy "${PROJECT_ID}" --format=json | jq -e --arg role "roles/dns.admin" --arg member "serviceAccount:${GSA_EMAIL}" '.bindings[] | select(.role == $role) | .members[] | select(. == $member)' > /dev/null; then
         echo "Failed to remove DNS Admin role binding (permissions issue?)."
      else
         echo "DNS Admin role binding for GSA [${GSA_EMAIL}] does not exist, skipping removal."
      fi
  fi
else
    echo "GSA [${GSA_EMAIL}] not found, skipping DNS Admin role binding removal."
fi

# 4. Delete Google Service Account (GSA)
echo "Attempting to delete Google Service Account [${GSA_EMAIL}]..."
if gcloud iam service-accounts describe "${GSA_EMAIL}" --project="${PROJECT_ID}" --format="value(name)" > /dev/null 2>&1; then
  gcloud iam service-accounts delete "${GSA_EMAIL}" \
    --project="${PROJECT_ID}" \
    --quiet
  if [ $? -eq 0 ]; then
      echo "Successfully deleted GSA [${GSA_EMAIL}]."
  else
      echo "Failed to delete GSA [${GSA_EMAIL}]."
  fi
else
    echo "GSA [${GSA_EMAIL}] not found, skipping deletion."
fi

# 5. Delete DNS Record Sets within the Managed Zone
echo "Attempting to delete DNS record sets in zone '${DNS_ZONE_NAME}'..."

# Check if the zone exists before trying to delete records
if gcloud dns managed-zones describe "${DNS_ZONE_NAME}" --project="${PROJECT_ID}" --format="value(name)" > /dev/null 2>&1; then
    # List all record sets in JSON, filter out NS and SOA, format for transaction remove
    # Read into an array for safer iteration
    mapfile -t record_lines < <(gcloud dns record-sets list \
        --zone="${DNS_ZONE_NAME}" \
        --project="${PROJECT_ID}" \
        --format="json" | \
        jq -c '.[] | select(.type != "NS" and .type != "SOA")')

    if [ ${#record_lines[@]} -eq 0 ]; then
        echo "No deletable record sets found in zone '${DNS_ZONE_NAME}'."
        TRANSACTION_NEEDED=false
    else
        echo "Found ${#record_lines[@]} record set(s) to delete."
        TRANSACTION_NEEDED=true
        # Start transaction
        echo "Starting transaction to delete record sets..."
        if ! gcloud dns record-sets transaction start --zone="${DNS_ZONE_NAME}" --project="${PROJECT_ID}"; then
            echo "ERROR: Failed to start DNS transaction. Aborting record deletion."
            TRANSACTION_NEEDED=false # Prevent trying to execute/abort later
            # Optionally exit here if this is critical
            # exit 1
        fi
    fi

    if [ "$TRANSACTION_NEEDED" = true ]; then
        TRANSACTION_ABORTED=false
        for record_json in "${record_lines[@]}"; do
            # Extract fields using jq
            name=$(echo "$record_json" | jq -r '.name')
            type=$(echo "$record_json" | jq -r '.type')
            ttl=$(echo "$record_json" | jq -r '.ttl')
            # rrdatas is an array
            # Create an array to hold the arguments for the transaction remove command
            cmd_args=("--name" "$name" "--type" "$type" "--ttl" "$ttl" "--zone" "${DNS_ZONE_NAME}" "--project" "${PROJECT_ID}")

            # Read rrdatas into a bash array, ensuring proper handling of spaces/special chars if any
            readarray -t rrdatas_array < <(echo "$record_json" | jq -r '.rrdatas[]')

            # Add each rrdata element prefixed by '--'
            for data in "${rrdatas_array[@]}"; do
                 # Trim leading/trailing whitespace which might come from jq/readarray processing
                 trimmed_data=$(echo -n "$data" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
                 if [[ -n "$trimmed_data" ]]; then # Ensure we don't add empty data
                    cmd_args+=("--" "$trimmed_data")
                 fi
            done

            echo "Adding removal to transaction: gcloud dns record-sets transaction remove ${cmd_args[*]}"
            if ! gcloud dns record-sets transaction remove "${cmd_args[@]}"; then
                echo "ERROR: Failed to add record removal to transaction: Name: $name, Type: $type"
                echo "Aborting transaction..."
                gcloud dns record-sets transaction abort --zone="${DNS_ZONE_NAME}" --project="${PROJECT_ID}"
                echo "Cleanup may be incomplete. Please check DNS zone '${DNS_ZONE_NAME}'."
                TRANSACTION_ABORTED=true
                break # Exit loop on error
            fi
        done

        if [ "$TRANSACTION_ABORTED" = false ]; then
            # Execute transaction
            echo "Executing transaction..."
            if gcloud dns record-sets transaction execute --zone="${DNS_ZONE_NAME}" --project="${PROJECT_ID}"; then
                echo "Successfully deleted record sets from zone '${DNS_ZONE_NAME}'."
            else
                echo "ERROR: Failed to execute transaction to delete record sets."
                echo "You may need to manually delete records from zone '${DNS_ZONE_NAME}'."
                # Zone deletion might still fail below
            fi
        fi
    fi # End if TRANSACTION_NEEDED

    # 6. Delete Cloud DNS Managed Zone (only if the zone existed initially)
    echo "Attempting to delete DNS managed zone '${DNS_ZONE_NAME}'..."
    gcloud dns managed-zones delete "${DNS_ZONE_NAME}" \
      --project="${PROJECT_ID}" \
      --quiet
    if [ $? -eq 0 ]; then
        echo "Successfully deleted DNS zone '${DNS_ZONE_NAME}'."
    else
        echo "ERROR: Failed to delete DNS zone '${DNS_ZONE_NAME}'. It might still contain records or other issues. Please check manually."
    fi
else
    echo "DNS zone '${DNS_ZONE_NAME}' not found, skipping record set deletion and zone deletion."
fi

echo "Cleanup process finished." 