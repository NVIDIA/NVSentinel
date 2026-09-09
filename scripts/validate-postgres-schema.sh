#!/usr/bin/env bash

# Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

# Validates the repository contract for manually applied PostgreSQL migrations.
# This script does not connect to a database or apply DDL.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
MIGRATION_DIR="${REPO_ROOT}/store-client/pkg/datastore/providers/postgresql/migrations"
SCHEMA_VERSION_SOURCE="${REPO_ROOT}/store-client/pkg/datastore/providers/postgresql/schema_version.go"
DATASTORE_SOURCE="${REPO_ROOT}/store-client/pkg/datastore/providers/postgresql/datastore.go"
HELM_VALUES=(
    "${REPO_ROOT}/distros/kubernetes/nvsentinel/values-tilt-postgresql.yaml"
    "${REPO_ROOT}/distros/kubernetes/nvsentinel/values-postgresql.yaml"
)

fail() {
    echo "ERROR: $*" >&2
    exit 1
}

[[ -d "${MIGRATION_DIR}" ]] || fail "migration directory not found: ${MIGRATION_DIR}"
[[ -f "${SCHEMA_VERSION_SOURCE}" ]] || fail "schema version source not found"

shopt -s nullglob
migration_files=("${MIGRATION_DIR}"/[0-9][0-9][0-9][0-9][0-9]_*.sql)
(( ${#migration_files[@]} > 0 )) || fail "no PostgreSQL migrations found"

expected_version=1
for migration_file in "${migration_files[@]}"; do
    filename="$(basename "${migration_file}")"
    version_text="${filename%%_*}"
    version=$((10#${version_text}))

    (( version == expected_version )) ||
        fail "expected migration version ${expected_version}, found ${filename}"

    grep -Eq '^BEGIN;$' "${migration_file}" ||
        fail "${filename} must start a transaction"
    grep -Eq '^COMMIT;$' "${migration_file}" ||
        fail "${filename} must commit its transaction"
    grep -q 'INSERT INTO nvsentinel_schema_migrations' "${migration_file}" ||
        fail "${filename} must record its applied version"
    grep -Eq "VALUES[[:space:]]*\\(${version}," "${migration_file}" ||
        fail "${filename} records a version that does not match its filename"

    expected_version=$((expected_version + 1))
done

latest_version=$((expected_version - 1))
required_version="$(
    awk '/const RequiredSchemaVersion int64 =/ { print $NF }' "${SCHEMA_VERSION_SOURCE}"
)"

[[ "${required_version}" == "${latest_version}" ]] ||
    fail "RequiredSchemaVersion=${required_version:-missing}, latest migration=${latest_version}"

if grep -Eq \
    'CREATE[[:space:]]+(TABLE|INDEX|TRIGGER|EXTENSION)|ALTER[[:space:]]+TABLE|DROP[[:space:]]+TRIGGER|CREATE[[:space:]]+OR[[:space:]]+REPLACE[[:space:]]+FUNCTION' \
    "${DATASTORE_SOURCE}"; then
    fail "application datastore code must not contain PostgreSQL DDL"
fi

for values_file in "${HELM_VALUES[@]}"; do
    [[ -f "${values_file}" ]] || fail "Helm values file not found: ${values_file}"
    if grep -q '00-init.sql' "${values_file}"; then
        fail "$(basename "${values_file}") must not embed the PostgreSQL schema"
    fi
done

echo "PostgreSQL migrations are valid (latest version: ${latest_version})"
