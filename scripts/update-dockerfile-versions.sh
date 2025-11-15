#!/usr/bin/env bash
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

# Update Dockerfile base image versions to match .versions.yaml
#
# This script updates all Dockerfiles in the repository to use the Go and Python
# versions specified in .versions.yaml. This ensures consistency between local
# development, CI/CD, and container builds.
#
# Usage:
#   ./scripts/update-dockerfile-versions.sh        # Dry run (show changes)
#   ./scripts/update-dockerfile-versions.sh apply  # Apply changes

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERSIONS_FILE="${REPO_ROOT}/.versions.yaml"

# Check if yq is installed
if ! command -v yq >/dev/null 2>&1; then
    echo "❌ Error: yq is required but not installed."
    echo "   Install yq:"
    echo "   macOS:  brew install yq"
    echo "   Linux:  See https://github.com/mikefarah/yq"
    exit 1
fi

# Check if .versions.yaml exists
if [[ ! -f "${VERSIONS_FILE}" ]]; then
    echo "❌ Error: ${VERSIONS_FILE} not found"
    exit 1
fi

# Load versions from .versions.yaml
GO_VERSION=$(yq '.languages.go' "${VERSIONS_FILE}")
PYTHON_VERSION=$(yq '.languages.python' "${VERSIONS_FILE}")
POETRY_VERSION=$(yq '.build_tools.poetry' "${VERSIONS_FILE}")

echo "=== Dockerfile Version Update Tool ==="
echo "Go version:     ${GO_VERSION}"
echo "Python version: ${PYTHON_VERSION}"
echo "Poetry version: ${POETRY_VERSION}"
echo ""

# Determine mode
DRY_RUN=true
if [[ "${1:-}" == "apply" ]]; then
    DRY_RUN=false
    echo "Mode: APPLY (will modify files)"
else
    echo "Mode: DRY RUN (showing changes only)"
    echo "Run with 'apply' argument to make changes"
fi
echo ""

# Find all Dockerfiles
DOCKERFILES=$(find "${REPO_ROOT}" -type f \( -name "Dockerfile" -o -name "Dockerfile.*" \) ! -path "*/\.*" ! -path "*/vendor/*")

if [[ -z "${DOCKERFILES}" ]]; then
    echo "No Dockerfiles found"
    exit 0
fi

UPDATED_COUNT=0
SKIPPED_COUNT=0

# Process each Dockerfile
while IFS= read -r dockerfile; do
    RELATIVE_PATH="${dockerfile#"${REPO_ROOT}"/}"
    CHANGED=false
    
    # Check for Go base images
    if grep -q "FROM.*golang:" "${dockerfile}"; then
        CURRENT_GO=$(grep "FROM.*golang:" "${dockerfile}" | sed -E 's/.*golang:([0-9.]+).*/\1/' | head -1)
        
        if [[ "${CURRENT_GO}" != "${GO_VERSION}"* ]]; then
            echo "📝 ${RELATIVE_PATH}"
            echo "   Go: ${CURRENT_GO} → ${GO_VERSION}"
            CHANGED=true
            
            if [[ "${DRY_RUN}" == "false" ]]; then
                # Update Go version in Dockerfile
                # Match patterns like: golang:1.25-trixie, etc.
                sed -i.bak -E "s/(FROM.*golang:)[0-9]+\.[0-9]+(\.[0-9]+)?(-[a-z]+)?/\1${GO_VERSION}\3/g" "${dockerfile}"
                rm "${dockerfile}.bak"
            fi
        fi
    fi
    
    # Check for Python base images
    if grep -q "FROM.*python:" "${dockerfile}"; then
        CURRENT_PYTHON=$(grep "FROM.*python:" "${dockerfile}" | sed -E 's/.*python:([0-9.]+).*/\1/' | head -1)
        
        if [[ "${CURRENT_PYTHON}" != "${PYTHON_VERSION}"* ]]; then
            echo "📝 ${RELATIVE_PATH}"
            echo "   Python: ${CURRENT_PYTHON} → ${PYTHON_VERSION}"
            CHANGED=true
            
            if [[ "${DRY_RUN}" == "false" ]]; then
                # Update Python version in Dockerfile while preserving the base image variant
                # Examples: python:3.11-alpine → python:3.13-alpine, python:3.10-trixie → python:3.13-trixie
                # The pattern captures and preserves the variant suffix (e.g., -alpine, -trixie) via \3
                sed -i.bak -E "s/(FROM.*python:)[0-9]+\.[0-9]+(\.[0-9]+)?(-[a-z]+)?/\1${PYTHON_VERSION}\3/g" "${dockerfile}"
                rm "${dockerfile}.bak"
            fi
        fi
    fi
    
    # Check for Poetry installations
    if grep -q "pip install poetry==" "${dockerfile}"; then
        CURRENT_POETRY=$(grep "pip install poetry==" "${dockerfile}" | sed -E 's/.*poetry==([0-9.]+).*/\1/' | head -1)
        
        if [[ "${CURRENT_POETRY}" != "${POETRY_VERSION}" ]]; then
            echo "📝 ${RELATIVE_PATH}"
            echo "   Poetry: ${CURRENT_POETRY} → ${POETRY_VERSION}"
            CHANGED=true
            
            if [[ "${DRY_RUN}" == "false" ]]; then
                # Update Poetry version in pip install commands
                sed -i.bak -E "s/(pip install poetry==)[0-9]+\.[0-9]+(\.[0-9]+)?/\1${POETRY_VERSION}/g" "${dockerfile}"
                rm "${dockerfile}.bak"
            fi
        fi
    fi
    
    if [[ "${CHANGED}" == "true" ]]; then
        UPDATED_COUNT=$((UPDATED_COUNT + 1))
    else
        SKIPPED_COUNT=$((SKIPPED_COUNT + 1))
    fi
done <<< "${DOCKERFILES}"

echo ""
echo "=== Summary ==="
echo "Files to update: ${UPDATED_COUNT}"
echo "Files up to date: ${SKIPPED_COUNT}"

if [[ "${DRY_RUN}" == "true" ]] && [[ "${UPDATED_COUNT}" -gt 0 ]]; then
    echo ""
    echo "To apply these changes, run:"
    echo "  ./scripts/update-dockerfile-versions.sh apply"
fi

exit 0
