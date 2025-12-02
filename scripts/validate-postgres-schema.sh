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

#===============================================================================
# PostgreSQL Schema Validation Script
#===============================================================================
#
# Purpose:
#   Ensures PostgreSQL schema consistency between:
#   1. docs/postgresql-schema.sql (canonical source)
#   2. distros/kubernetes/nvsentinel/values-tilt-postgresql.yaml (Helm initdb)
#
# Usage:
#   ./scripts/validate-postgres-schema.sh
#   make validate-postgres-schema
#
# Exit Codes:
#   0  - Schemas are in sync
#   1  - Schemas differ or validation failed
#
#===============================================================================

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Paths
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CANONICAL_SCHEMA="${REPO_ROOT}/docs/postgresql-schema.sql"
HELM_VALUES="${REPO_ROOT}/distros/kubernetes/nvsentinel/values-tilt-postgresql.yaml"

echo "==================================================================="
echo "PostgreSQL Schema Validation"
echo "==================================================================="
echo ""

# Check if files exist
if [[ ! -f "${CANONICAL_SCHEMA}" ]]; then
    echo -e "${RED}ERROR: Canonical schema not found: ${CANONICAL_SCHEMA}${NC}"
    exit 1
fi

if [[ ! -f "${HELM_VALUES}" ]]; then
    echo -e "${RED}ERROR: Helm values file not found: ${HELM_VALUES}${NC}"
    exit 1
fi

# Check if yq is installed
if ! command -v yq &> /dev/null; then
    echo -e "${RED}ERROR: yq is required but not installed${NC}"
    echo "Install yq:"
    echo "  macOS:  brew install yq"
    echo "  Linux:  https://github.com/mikefarah/yq#install"
    exit 1
fi

echo "✓ Found canonical schema: docs/postgresql-schema.sql"
echo "✓ Found Helm values:       distros/kubernetes/nvsentinel/values-tilt-postgresql.yaml"
echo ""

# Extract SQL from Helm values file
TEMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TEMP_DIR}"' EXIT

HELM_SCHEMA="${TEMP_DIR}/helm-schema.sql"
yq eval '.postgresql.primary.initdb.scripts."00-init.sql"' "${HELM_VALUES}" > "${HELM_SCHEMA}"

if [[ ! -s "${HELM_SCHEMA}" ]]; then
    echo -e "${RED}ERROR: Failed to extract schema from Helm values${NC}"
    exit 1
fi

echo "Comparing schemas..."
echo ""

# Normalize schemas for comparison (remove comments, empty lines, extra whitespace)
normalize_sql() {
    local file="$1"
    grep -v '^--' "$file" | \
        grep -v '^\s*$' | \
        sed 's/[[:space:]]\+/ /g' | \
        sed 's/^[[:space:]]*//' | \
        sed 's/[[:space:]]*$//' | \
        sort
}

CANONICAL_NORMALIZED="${TEMP_DIR}/canonical-normalized.sql"
HELM_NORMALIZED="${TEMP_DIR}/helm-normalized.sql"

normalize_sql "${CANONICAL_SCHEMA}" > "${CANONICAL_NORMALIZED}"
normalize_sql "${HELM_SCHEMA}" > "${HELM_NORMALIZED}"

# Compare normalized schemas
if diff -u "${CANONICAL_NORMALIZED}" "${HELM_NORMALIZED}" > "${TEMP_DIR}/schema-diff.txt" 2>&1; then
    echo -e "${GREEN}✅ SUCCESS: PostgreSQL schemas are in sync!${NC}"
    echo ""
    echo "Both files contain identical schema definitions:"
    echo "  • docs/postgresql-schema.sql"
    echo "  • distros/kubernetes/nvsentinel/values-tilt-postgresql.yaml"
    echo ""
    exit 0
else
    echo -e "${RED}❌ ERROR: PostgreSQL schemas are OUT OF SYNC!${NC}"
    echo ""
    echo "Differences found between:"
    echo "  • docs/postgresql-schema.sql (canonical)"
    echo "  • distros/kubernetes/nvsentinel/values-tilt-postgresql.yaml (Helm)"
    echo ""
    echo "Diff output:"
    echo "-------------------------------------------------------------------"
    cat "${TEMP_DIR}/schema-diff.txt"
    echo "-------------------------------------------------------------------"
    echo ""
    echo -e "${YELLOW}To fix this issue:${NC}"
    echo "  1. Update docs/postgresql-schema.sql with your schema changes"
    echo "  2. Run: make update-helm-postgres-schema"
    echo "  3. Commit both files together"
    echo ""
    exit 1
fi
