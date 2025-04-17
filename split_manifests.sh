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
set -euo pipefail

if [ "$#" -ne 5 ]; then
    echo "Usage: $0 <helm-chart-path> <release-name> <namespace> <mgmt-override-file> <tenant-override-file>"
    exit 1
fi

HELM_CHART_PATH="$1"
RELEASE_NAME="$2"
NAMESPACE="$3"
MGMT_OVERRIDE_FILE="$4"
TENANT_OVERRIDE_FILE="$5"
OUTPUT_MGMT_TMP="all-manifests-mgmt.yaml"
OUTPUT_TENANT_TMP="all-manifests-tenant.yaml"
MGMT_RESOURCES_TMP="mgmt-temp.yaml"
TENANT_RESOURCES_TMP="tenant-temp.yaml"

# Export RELEASE_NAME for yq and envsubst
export RELEASE_NAME

# Ensure mgmt and tenant directories exist
mkdir -p mgmt tenant

# Check if override files exist (basic check, generate_manifests.sh does detailed check)
if [ ! -f "$MGMT_OVERRIDE_FILE" ]; then echo "Error: Mgmt override file '$MGMT_OVERRIDE_FILE' not found."; exit 1; fi
if [ ! -f "$TENANT_OVERRIDE_FILE" ]; then echo "Error: Tenant override file '$TENANT_OVERRIDE_FILE' not found."; exit 1; fi

# --- Management Resources --- 

echo "Rendering manifests for Management resources..."

# Render ALL manifests using MGMT override values
helm template "$RELEASE_NAME" "$HELM_CHART_PATH" --namespace "$NAMESPACE" \
    -f "$MGMT_OVERRIDE_FILE" > "$OUTPUT_MGMT_TMP"

# Select only mgmt resources
yq eval-all 'select(.metadata.annotations."dgxc.nvidia.com/resource-category"? | test("(^|,)\\s*management\\s*(,|$)"))' "$OUTPUT_MGMT_TMP" > "$MGMT_RESOURCES_TMP"

# Process mgmt resources step 1: Add SAN entry
MGMT_RESOURCES_TMP_SAN=$(mktemp)
yq eval-all '
  select(.kind == "Certificate" and .metadata.name | test("mongo-server-cert-.*")) |=
  (
    .spec.dnsNames += [strenv(RELEASE_NAME) + "-mongodb-" + (.metadata.name | sub("mongo-server-cert-"; "")) + ".psc.gcp.internal"]
  )
' "$MGMT_RESOURCES_TMP" > "$MGMT_RESOURCES_TMP_SAN"

# Process mgmt resources step 2: Remove nodeSelector
MGMT_RESOURCES_TMP_FINAL=$(mktemp)
yq eval-all '
  (.. | select(has("nodeSelector") and .nodeSelector.nodeGroup? == "system-cpu")) |= del(.nodeSelector)
' "$MGMT_RESOURCES_TMP_SAN" > "$MGMT_RESOURCES_TMP_FINAL"

# Process mgmt resources step 3: Remove comments
sed '/^[[:space:]]*#!/b; /^[[:space:]]*#/d' "$MGMT_RESOURCES_TMP_FINAL" > mgmt/resources.yaml

rm "$MGMT_RESOURCES_TMP"
rm "$MGMT_RESOURCES_TMP_SAN"
rm "$MGMT_RESOURCES_TMP_FINAL"
rm "$OUTPUT_MGMT_TMP"
echo "Management resources generated."

# --- Tenant Resources --- 

echo "Rendering manifests for Tenant resources..."

# Render ALL manifests using the TENANT override values
helm template "$RELEASE_NAME" "$HELM_CHART_PATH" --namespace "$NAMESPACE" \
    -f "$TENANT_OVERRIDE_FILE" > "$OUTPUT_TENANT_TMP"

# Select only tenant resources
yq eval-all 'select(.metadata.annotations."dgxc.nvidia.com/resource-category"? | test("(^|,)\\s*tenant\\s*(,|$)"))' "$OUTPUT_TENANT_TMP" > "$TENANT_RESOURCES_TMP"

# Process tenant resources: remove comments
sed '/^[[:space:]]*#!/b; /^[[:space:]]*#/d' "$TENANT_RESOURCES_TMP" > tenant/resources.yaml

rm "$TENANT_RESOURCES_TMP"
rm "$OUTPUT_TENANT_TMP"
echo "Tenant resources generated."

echo "Manifests have been split into mgmt/ and tenant/ directories using separate override files."