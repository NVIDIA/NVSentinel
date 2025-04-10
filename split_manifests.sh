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

if [ "$#" -ne 4 ]; then
    echo "Usage: $0 <helm-chart-path> <release-name> <namespace> <override-values-file>"
    exit 1
fi

HELM_CHART_PATH="$1"
RELEASE_NAME="$2"
NAMESPACE="$3"
OVERRIDE_VALUES_FILE="$4"
OUTPUT_FILE="all-manifests.yaml" # Temporary file for all manifests
MGMT_TEMP_FILE="mgmt-temp.yaml" # Temporary file for mgmt resources

# Ensure mgmt and tenant directories exist
mkdir -p mgmt tenant

# Check if override values file exists
if [ ! -f "$OVERRIDE_VALUES_FILE" ]; then
    echo "Error: Override values file '$OVERRIDE_VALUES_FILE' does not exist."
    exit 1
fi

echo "Using override values from $OVERRIDE_VALUES_FILE"
# Render all manifests using the override values file
helm template "$RELEASE_NAME" "$HELM_CHART_PATH" --namespace "$NAMESPACE" \
    --set mongodb.global.namespaceOverride="$NAMESPACE" \
    -f "$OVERRIDE_VALUES_FILE" > "$OUTPUT_FILE"


# Step 1: Select only mgmt resources into a temporary file
# Check the annotation for "management" (allowing comma separation)
yq eval-all 'select(.metadata.annotations."dgxc.nvidia.com/resource-category"? | test("(^|,)\\s*management\\s*(,|$)"))' "$OUTPUT_FILE" > "$MGMT_TEMP_FILE"

# Step 2: Process the temporary file to remove the specific nodeSelector
# Find any object (..) that has a nodeSelector field containing nodeGroup: system-cpu.
# Update the parent object by deleting (.nodeSelector) its nodeSelector field.
# Then remove comments and save
yq eval-all '(.. | select(has("nodeSelector") and .nodeSelector.nodeGroup? == "system-cpu")) |= del(.nodeSelector)' "$MGMT_TEMP_FILE" | sed '/^[[:space:]]*#!/b; /^[[:space:]]*#/d' > mgmt/resources.yaml

rm "$MGMT_TEMP_FILE"

# Select tenant resources from the original full output, remove comments and save
yq eval-all 'select(.metadata.annotations."dgxc.nvidia.com/resource-category"? | test("(^|,)\\s*tenant\\s*(,|$)"))' "$OUTPUT_FILE" | sed '/^[[:space:]]*#!/b; /^[[:space:]]*#/d' > tenant/resources.yaml

rm "$OUTPUT_FILE"

echo "Manifests have been split into mgmt/ and tenant/ directories."