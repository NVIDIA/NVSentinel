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

echo "--- Starting Manifest Generation and Publishing ---"

required_vars=("CI_PROJECT_DIR" "HELM_URL" "NGC_ORG" "NGC_USER_NAME" "NGC_PASSWORD" "CI_API_V4_URL" "CI_PROJECT_ID" "CI_JOB_TOKEN")
for var in "${required_vars[@]}"; do
  if [ -z "${!var}" ]; then
    echo "Error: Required environment variable $var is not set."
    exit 1
  fi
done

cd "$CI_PROJECT_DIR/distros/kubernetes" || { echo "Error: Failed to cd into distros/kubernetes"; exit 1; }

echo "Adding Helm repositories..."
helm repo add --force-update mongodb "${HELM_URL}/${NGC_ORG}" --username="${NGC_USER_NAME}" --password="${NGC_PASSWORD}"

echo "Updating Helm dependencies..."
cd nvsentinel/charts/mongodb-store || { echo "Error: Failed to cd into mongodb-store chart"; exit 1; }
helm dependency update
cd ../../..
cd nvsentinel || { echo "Error: Failed to cd into nvsentinel parent chart"; exit 1; }
helm dependency update
cd ..

CHART_PATH="$CI_PROJECT_DIR/distros/kubernetes/nvsentinel"
RELEASE_NAME="nvsentinel"
NAMESPACE="dgxc-system"
OVERRIDE_VALUES_TMP_FILE="override-values.yaml"

# Create override values file
echo "Creating override values file ($OVERRIDE_VALUES_TMP_FILE)..."
envsubst < "$CI_PROJECT_DIR/mcp-override-values.yaml" > "$OVERRIDE_VALUES_TMP_FILE"

# Run the split manifests script
echo "Running manifest split script..."
"$CI_PROJECT_DIR/split_manifests.sh" "${CHART_PATH}" "${RELEASE_NAME}" "${NAMESPACE}" "$OVERRIDE_VALUES_TMP_FILE"

# Validate generated manifests with kubeconform
DATREE_SCHEMA_LOCATION="https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"
echo "Validating mgmt/resources.yaml..."
if grep -q -v -e '^[[:space:]]*$' -e '^[[:space:]]*#' mgmt/resources.yaml; then
  echo "File mgmt/resources.yaml contains definitions, validating with kubeconform..."
  kubeconform -summary -schema-location default -schema-location "${DATREE_SCHEMA_LOCATION}" mgmt/resources.yaml
else
  echo "Skipping validation for mgmt/resources.yaml as it appears empty or contains only comments."
fi
echo "Validating tenant/resources.yaml..."
if grep -q -v -e '^[[:space:]]*$' -e '^[[:space:]]*#' tenant/resources.yaml; then
  echo "File tenant/resources.yaml contains definitions, validating with kubeconform..."
  kubeconform -summary -schema-location default -schema-location "${DATREE_SCHEMA_LOCATION}" tenant/resources.yaml
else
  echo "Skipping validation for tenant/resources.yaml as it appears empty or contains only comments."
fi

# Copy manifests to the artifact directory
ARTIFACT_DIR="$CI_PROJECT_DIR/manifests"
echo "Copying manifests to artifact directory ($ARTIFACT_DIR)..."
mkdir -p "$ARTIFACT_DIR/mgmt" "$ARTIFACT_DIR/tenant"
cp mgmt/resources.yaml "$ARTIFACT_DIR/mgmt/"
cp tenant/resources.yaml "$ARTIFACT_DIR/tenant/"

echo "Listing contents of generated manifests:"
ls -la "$ARTIFACT_DIR/mgmt/" "$ARTIFACT_DIR/tenant/"

rm "$OVERRIDE_VALUES_TMP_FILE"

echo "--- Starting Package Registry Upload ---"

if [ -n "${CI_COMMIT_TAG:-}" ]; then
  PACKAGE_VERSION=${CI_COMMIT_TAG}
  echo "Detected tag build. Package version: $PACKAGE_VERSION"
else
  if [ -z "$SAFE_REF_NAME" ]; then
    echo "Error: SAFE_REF_NAME is not set. It should be passed via dotenv artifact from prepare-vars job."
    exit 1
  fi
  PACKAGE_VERSION="${SAFE_REF_NAME}"
  echo "Detected branch build. Package version: $PACKAGE_VERSION"

  # Find and delete existing package ID for this version
  echo "Searching for existing package ID for version ${PACKAGE_VERSION}..."

  PACKAGE_ID=$(curl --silent --show-error --fail-with-body --header "JOB-TOKEN: $CI_JOB_TOKEN" "${CI_API_V4_URL}/projects/${CI_PROJECT_ID}/packages?package_name=nvsentinel-manifests&package_version=${PACKAGE_VERSION}" | jq --raw-output '.[0].id // ""' )

  if [ -n "$PACKAGE_ID" ]; then
    echo "Found existing package ID: ${PACKAGE_ID}. Deleting..."

    curl --request DELETE --show-error --fail-with-body --header "JOB-TOKEN: $CI_JOB_TOKEN" "${CI_API_V4_URL}/projects/${CI_PROJECT_ID}/packages/${PACKAGE_ID}" -w "\nHTTP Status (delete by ID): %{http_code}\n"
    echo "Deletion request sent. Waiting briefly..."
    sleep 2
  else
    echo "No existing package found with version ${PACKAGE_VERSION} to delete, or failed to query packages."
  fi
fi

# Prepare package files for upload
echo "Preparing package files for upload..."
cd "$CI_PROJECT_DIR" || { echo "Error: Failed to cd to $CI_PROJECT_DIR"; exit 1; }
TEMP_PACKAGE_DIR="package_tmp"
rm -rf "$TEMP_PACKAGE_DIR" nvsentinel-manifests.zip
mkdir -p "$TEMP_PACKAGE_DIR"
cp -r "$ARTIFACT_DIR/mgmt" "$TEMP_PACKAGE_DIR/"
cp -r "$ARTIFACT_DIR/tenant" "$TEMP_PACKAGE_DIR/"
cd "$TEMP_PACKAGE_DIR" || { echo "Error: Failed to cd into $TEMP_PACKAGE_DIR"; exit 1; }
zip -r ../nvsentinel-manifests.zip ./*
cd ..

# Upload files to GitLab Package Registry
PACKAGE_NAME="nvsentinel-manifests"
PACKAGE_REGISTRY_URL="${CI_API_V4_URL}/projects/${CI_PROJECT_ID}/packages/generic/${PACKAGE_NAME}/${PACKAGE_VERSION}"
ZIP_FILE_NAME="nvsentinel-manifests.zip"
MGMT_FILE_NAME="mgmt-resources.yaml"
TENANT_FILE_NAME="tenant-resources.yaml"

echo "Uploading $ZIP_FILE_NAME to ${PACKAGE_REGISTRY_URL}/${ZIP_FILE_NAME}"
curl --show-error --fail-with-body --header "JOB-TOKEN: $CI_JOB_TOKEN" --upload-file "$ZIP_FILE_NAME" "${PACKAGE_REGISTRY_URL}/${ZIP_FILE_NAME}"

echo "Uploading $MGMT_FILE_NAME to ${PACKAGE_REGISTRY_URL}/${MGMT_FILE_NAME}"
curl --show-error --fail-with-body --header "JOB-TOKEN: $CI_JOB_TOKEN" --upload-file "$ARTIFACT_DIR/mgmt/resources.yaml" "${PACKAGE_REGISTRY_URL}/${MGMT_FILE_NAME}"

echo "Uploading $TENANT_FILE_NAME to ${PACKAGE_REGISTRY_URL}/${TENANT_FILE_NAME}"
curl --show-error --fail-with-body --header "JOB-TOKEN: $CI_JOB_TOKEN" --upload-file "$ARTIFACT_DIR/tenant/resources.yaml" "${PACKAGE_REGISTRY_URL}/${TENANT_FILE_NAME}"

rm -rf "$TEMP_PACKAGE_DIR" "$ZIP_FILE_NAME"

echo "--- Manifest Generation and Publishing Complete ---"