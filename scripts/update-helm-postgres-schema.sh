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
# PostgreSQL Schema Update Script
#===============================================================================
#
# Purpose:
#   Updates Helm values file with PostgreSQL schema from canonical source.
#   Ensures single source of truth: docs/postgresql-schema.sql
#
# Usage:
#   ./scripts/update-helm-postgres-schema.sh
#   make update-helm-postgres-schema
#
# What it does:
#   1. Reads canonical schema from docs/postgresql-schema.sql
#   2. Updates distros/kubernetes/nvsentinel/values-tilt-postgresql.yaml
#   3. Preserves YAML formatting and structure
#
# Exit Codes:
#   0  - Update successful
#   1  - Update failed
#
#===============================================================================

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Paths
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CANONICAL_SCHEMA="${REPO_ROOT}/docs/postgresql-schema.sql"
HELM_VALUES="${REPO_ROOT}/distros/kubernetes/nvsentinel/values-tilt-postgresql.yaml"
BACKUP_FILE="${HELM_VALUES}.backup.$(date +%Y%m%d_%H%M%S)"

echo "==================================================================="
echo "PostgreSQL Schema Update"
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

echo -e "${BLUE}Source:${NC}      docs/postgresql-schema.sql"
echo -e "${BLUE}Target:${NC}      distros/kubernetes/nvsentinel/values-tilt-postgresql.yaml"
echo ""

# Create backup
echo "Creating backup: $(basename "${BACKUP_FILE}")"
cp "${HELM_VALUES}" "${BACKUP_FILE}"

# Update Helm values file using yq
echo "Updating Helm values file..."

# Use yq to read the canonical schema file and insert it into the Helm values
# This avoids issues with escaping special characters
yq eval ".postgresql.primary.initdb.scripts.\"00-init.sql\" = load_str(\"${CANONICAL_SCHEMA}\")" "${HELM_VALUES}" > "${HELM_VALUES}.tmp"

# Replace original with updated version
mv "${HELM_VALUES}.tmp" "${HELM_VALUES}"

echo -e "${GREEN}✅ SUCCESS: Helm values file updated!${NC}"
echo ""
echo "Updated file:"
echo "  • distros/kubernetes/nvsentinel/values-tilt-postgresql.yaml"
echo ""
echo "Backup saved:"
echo "  • $(basename "${BACKUP_FILE}")"
echo ""
echo -e "${YELLOW}Next steps:${NC}"
echo "  1. Review the changes: git diff ${HELM_VALUES}"
echo "  2. Validate schemas match: make validate-postgres-schema"
echo "  3. Commit both files if changes look good"
echo ""
echo "To restore from backup if needed:"
echo "  cp ${BACKUP_FILE} ${HELM_VALUES}"
echo ""
