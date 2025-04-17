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

# === CONFIGURATION ===
# Define variables for creating DNS resources and configuring ExternalDNS
# in the producer/management project and cluster.

# --- Cloud DNS Zone Configuration ---
DNS_ZONE_NAME="psc-internal-zone"      # A name for the managed zone resource itself (e.g., psc-internal-zone)
DNS_DOMAIN_NAME="psc.gcp.internal."   # The actual DNS domain name this zone will manage (note trailing dot!)
VPC_NETWORK="mgmt"                    # The VPC network in the PRODUCER project this private zone should be visible to
PROJECT_ID="proj-dgxc-runai-np-msti07-mgmt" # The PRODUCER project ID

# --- Google Service Account (GSA) for ExternalDNS ---
GSA_NAME="external-dns-manager" # Name for the GSA that ExternalDNS will use
GSA_EMAIL="${GSA_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"

# --- Kubernetes Service Account (KSA) and Namespace for ExternalDNS Pod ---
K8S_NAMESPACE="external-dns-gcp" # Namespace where ExternalDNS will be deployed in the K8s cluster
K8S_SERVICE_ACCOUNT="external-dns" # Name of the KSA used by the ExternalDNS pod (defined in Helm chart)

# --- Helm Configuration ---
HELM_RELEASE_NAME="external-dns" # Name for the Helm release of ExternalDNS
# === END CONFIGURATION ===

# Exit script immediately if any command fails
set -e

# --- Step 1: Create Private Cloud DNS Managed Zone ---
# This zone will hold the DNS records automatically created by ExternalDNS.
# It's made private and visible to the specified producer VPC network.
echo "Step 1: Creating Private Cloud DNS Zone '${DNS_ZONE_NAME}' for domain '${DNS_DOMAIN_NAME}'..."
# Check if zone exists first to make script idempotent
set +e
gcloud dns managed-zones describe "${DNS_ZONE_NAME}" --project="${PROJECT_ID}" > /dev/null 2>&1
ZONE_EXISTS=$?
set -e

if [[ ${ZONE_EXISTS} -ne 0 ]]; then
  echo "  Zone not found. Creating..."
  gcloud dns managed-zones create "${DNS_ZONE_NAME}" \
    --description="Private zone for GKE services managed by ExternalDNS" \
    --dns-name="${DNS_DOMAIN_NAME}" \
    --visibility=private `# Makes the zone resolvable only within specified VPCs` \
    --networks="${VPC_NETWORK}" `# Network(s) that can resolve records in this zone` \
    --project="${PROJECT_ID}"
  if [[ $? -ne 0 ]]; then echo "ERROR: Failed to create DNS zone."; exit 1; fi
  echo "  Successfully created private DNS zone '${DNS_ZONE_NAME}'."
else
  echo "  DNS Zone '${DNS_ZONE_NAME}' already exists."
fi

# --- Step 2: Create Google Service Account (GSA) for ExternalDNS ---
# ExternalDNS needs permissions to interact with the Cloud DNS API.
# We create a dedicated GSA for this purpose.
echo "
Step 2: Creating Google Service Account '${GSA_NAME}'..."
set +e
gcloud iam service-accounts describe "${GSA_EMAIL}" --project="${PROJECT_ID}" > /dev/null 2>&1
GSA_EXISTS=$?
set -e

if [[ ${GSA_EXISTS} -ne 0 ]]; then
  echo "  GSA not found. Creating..."
  gcloud iam service-accounts create "${GSA_NAME}" \
    --display-name="ExternalDNS Manager Service Account" \
    --project="${PROJECT_ID}"
  if [[ $? -ne 0 ]]; then echo "ERROR: Failed to create GSA."; exit 1; fi
  echo "  Successfully created GSA: ${GSA_EMAIL}"
else
  echo "  GSA '${GSA_EMAIL}' already exists."
fi

# --- Step 3: Grant GSA DNS Permissions ---
# Grant the created GSA the necessary IAM role (dns.admin) to manage DNS records
# within the specified project.
echo "
Step 3: Granting DNS Admin role to GSA '${GSA_EMAIL}'..."
# Note: It's generally safe to run add-iam-policy-binding multiple times; it's additive.
gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
  --member="serviceAccount:${GSA_EMAIL}" \
  --role="roles/dns.admin" `# Allows managing DNS zones and records`
if [[ $? -ne 0 ]]; then echo "ERROR: Failed to grant DNS Admin role."; exit 1; fi
echo "  Successfully granted roles/dns.admin to ${GSA_EMAIL} on project ${PROJECT_ID}"

# --- Step 4: Configure Workload Identity Binding ---
# Securely links the Kubernetes Service Account (KSA) used by the ExternalDNS pod
# to the Google Service Account (GSA). This allows the pod to authenticate to GCP APIs
# as the GSA without needing to mount service account key files.
echo "
Step 4: Binding KSA [${K8S_NAMESPACE}/${K8S_SERVICE_ACCOUNT}] to GSA [${GSA_EMAIL}]..."
# Allow the KSA (identified by its namespace and name within the GKE cluster)
# to impersonate the GSA.
gcloud iam service-accounts add-iam-policy-binding "${GSA_EMAIL}" \
  --role="roles/iam.workloadIdentityUser" `# Role required for Workload Identity` \
  --member="serviceAccount:${PROJECT_ID}.svc.id.goog[${K8S_NAMESPACE}/${K8S_SERVICE_ACCOUNT}]" `# Format: serviceAccount:PROJECT_ID.svc.id.goog[K8S_NAMESPACE/KSA_NAME]` \
  --project="${PROJECT_ID}"
if [[ $? -ne 0 ]]; then echo "ERROR: Failed to bind KSA to GSA."; exit 1; fi
echo "  Successfully bound KSA to GSA."

# --- Step 5: Deploy ExternalDNS using Helm ---
# Installs the ExternalDNS application into the Kubernetes cluster using Helm.
# Configuration is passed inline via a heredoc, including Workload Identity setup.
echo "
Step 5: Deploying ExternalDNS Helm chart..."
# Add the required Helm repository if it doesn't exist
if ! helm repo list | grep -q bitnami; then
    echo "  Adding Bitnami Helm repository..."
    helm repo add bitnami https://charts.bitnami.com/bitnami
fi
helm repo update

# Deploy ExternalDNS using inline values via heredoc for clarity
helm upgrade --install "${HELM_RELEASE_NAME}" bitnami/external-dns \
  --namespace "${K8S_NAMESPACE}" \
  --create-namespace `# Create the namespace if it doesn't exist` \
  -f - <<EOF
# Configure ExternalDNS behavior

sources:
  - service # Watch Kubernetes Service objects for annotations
  # - ingress # Optionally watch Ingress objects too

provider: google # Specify Google Cloud DNS as the provider

google:
  project: "${PROJECT_ID}" # Tell ExternalDNS which GCP project to manage
  zoneVisibility: "private" # Specify we are working with private zones

# Filter domains: Only manage records within this specific domain
domainFilters:
  - "${DNS_DOMAIN_NAME%?}" # Domain name managed by the zone (remove trailing dot)

policy: upsert-only # Create or update records, don't delete records managed by others

# Use TXT records for ownership tracking
registry: "txt"
txtOwnerId: "k8s-producer-mongo" # Unique ID for this instance to own its TXT records

# Create Kubernetes RBAC roles needed by ExternalDNS
rbac:
  create: true

# Configure the Kubernetes Service Account for the ExternalDNS pod
serviceAccount:
  create: true # Helm chart should create the KSA
  name: "${K8S_SERVICE_ACCOUNT}" # Must match the name used in Workload Identity binding
  # Add the Workload Identity annotation to link KSA to GSA
  annotations:
    iam.gke.io/gcp-service-account: ${GSA_EMAIL} # The GSA email created earlier

# Optional: Resource limits/requests for the ExternalDNS pod
# resources:
#   limits:
#     memory: 100Mi
#   requests:
#     memory: 50Mi
#     cpu: 10m

EOF

# Check Helm deployment status
if [[ $? -eq 0 ]]; then
  echo "Successfully deployed ExternalDNS release '${HELM_RELEASE_NAME}' in namespace '${K8S_NAMESPACE}'."
  echo "Verify pod status with: kubectl get pods -n ${K8S_NAMESPACE}"
  echo "Verify logs with: kubectl logs -n ${K8S_NAMESPACE} -l app.kubernetes.io/name=external-dns -f"
else
  echo "ERROR: Helm deployment failed for ExternalDNS."
  exit 1
fi