#!/usr/bin/env bash

# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

# Checks the Renovate setup for .versions.yaml:
#   1. Every pin has a "# renovate:" annotation on the line above it, whose depType is the
#      pin's section, unless the pin is in UNTRACKED. Renovate skips a pin without one silently.
#   2. Every annotated section, and every depTypeTemplate of a custom manager, has a packageRules
#      entry with a groupName in .github/renovate.json5.
#   3. A custom manager reads each pin outside .versions.yaml, in PINNED_FILES, and its
#      matchStrings find the version in the file.
#   4. renovate-config-validator accepts .github/renovate.json5 as repository config, using the
#      pinned Renovate image. Given no path, the validator finds the file and does not treat it
#      as global config.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERSIONS_FILE="${REPO_ROOT}/.versions.yaml"

# Pins that Renovate does not update, as section.key:
#   - go, python: toolchain upgrades are their own change.
#   - docker_buildx: 'latest' is not a version.
#   - renovate: tag@digest, which the annotation regex does not read.
UNTRACKED="languages.go languages.python container_tools.docker_buildx container_tools.renovate"

errors=0

echo "Checking Renovate annotations in .versions.yaml..."

annotation_errors=$(awk -v untracked="${UNTRACKED}" '
  BEGIN {
    n = split(untracked, list, " ")
    for (i = 1; i <= n; i++) skip[list[i]] = 1
  }
  # A section is a top-level key with no value. A pin is any key under it with a scalar value.
  /^[A-Za-z0-9_-]+:[[:space:]]*(#.*)?$/ { section = $1; sub(/:$/, "", section); prev = ""; next }
  /^  [A-Za-z0-9_-]+:[[:space:]]*[^[:space:]#]/ {
    key = $1; sub(/:$/, "", key)
    id = section "." key
    annotated = (prev ~ /^  # renovate: datasource=[^ ]+ depName=[^ ]+ depType=[^ ]+$/)
    if (skip[id]) {
      if (annotated) printf "%s:%d: %s is in UNTRACKED but has an annotation\n", FILENAME, NR, id
    } else if (!annotated) {
      printf "%s:%d: %s has no \"# renovate:\" annotation on the line above\n", FILENAME, NR, id
    } else {
      want = "depType=" section
      if (substr(prev, length(prev) - length(want) + 1) != want)
        printf "%s:%d: %s annotation must end with %s\n", FILENAME, NR, id, want
      if ($0 !~ /^  [^:]+:[[:space:]]*["\x27]/)
        printf "%s:%d: %s value must be quoted, or the annotation regex does not read it\n", FILENAME, NR, id
      else if ($0 ~ /^  [^:]+:[[:space:]]*["\x27][^"\x27]*:/)
        printf "%s:%d: %s value contains \":\", which the annotation regex does not read\n", FILENAME, NR, id
    }
  }
  prev ~ /^  # renovate:/ && $0 !~ /^  [A-Za-z0-9_-]+:[[:space:]]*[^[:space:]#]/ {
    printf "%s:%d: annotation is not followed by a pin\n", FILENAME, NR - 1
  }
  { prev = $0 }
' "${VERSIONS_FILE}")

if [ -n "${annotation_errors}" ]; then
  echo "${annotation_errors}" >&2
  errors=1
fi

RENOVATE_IMAGE="ghcr.io/renovatebot/renovate:$(yq '.container_tools.renovate' "${VERSIONS_FILE}")"

echo "Checking the group rules and the pinned files..."

# Files outside .versions.yaml that pin a tool version. The tool reads its version from the file.
PINNED_FILES="fern/fern.config.json"

# Parse the config with the json5 package that ships in the Renovate image, so comments and
# rules without a groupName cannot pass. The check fails if the image no longer has json5.
mapfile -t dep_types < <(grep -oE 'depType=[A-Za-z0-9_-]+' "${VERSIONS_FILE}" | cut -d= -f2 | sort -u)

if ! docker run --rm \
  -v "${REPO_ROOT}:/usr/src/app:ro" \
  --entrypoint node \
  "${RENOVATE_IMAGE}" \
  -e '
    const fs = require("fs");
    const JSON5 = require(require.resolve("json5", { paths: ["/usr/local/renovate"] }));
    const config = JSON5.parse(fs.readFileSync(".github/renovate.json5", "utf8"));
    const rules = config.packageRules || [];
    const managers = config.customManagers || [];
    const [pinnedFiles, ...versionsDepTypes] = process.argv.slice(1);
    const depTypes = new Set(versionsDepTypes);
    for (const m of managers) {
      if (m.depTypeTemplate) depTypes.add(m.depTypeTemplate);
    }
    let failed = false;
    for (const depType of depTypes) {
      const grouped = rules.some((r) => (r.matchDepTypes || []).includes(depType) && r.groupName);
      if (!grouped) {
        console.error(".github/renovate.json5: no packageRules entry with a groupName for depType " + depType);
        failed = true;
      }
    }
    // A pattern is "/regex/" or a glob. The pinned files use only the regex form.
    const matches = (p, file) => p.startsWith("/") && p.endsWith("/") && new RegExp(p.slice(1, -1)).test(file);
    // Renovate takes the version from the currentValue group of a match string.
    const extracts = (m, text) => (m.matchStrings || []).some((s) => new RegExp(s).exec(text)?.groups?.currentValue);
    for (const file of pinnedFiles.split(" ").filter(Boolean)) {
      const readers = managers.filter((m) => (m.managerFilePatterns || []).some((p) => matches(p, file)));
      if (readers.length === 0) {
        console.error(".github/renovate.json5: no custom manager reads " + file);
        failed = true;
      } else if (!readers.some((m) => extracts(m, fs.readFileSync(file, "utf8")))) {
        console.error(".github/renovate.json5: no matchStrings of the custom managers for " + file + " finds a version in it");
        failed = true;
      }
    }
    process.exit(failed ? 1 : 0);
  ' "${PINNED_FILES}" "${dep_types[@]}"; then
  errors=1
fi

echo "Validating .github/renovate.json5 with renovate-config-validator..."

if ! docker run --rm \
  -v "${REPO_ROOT}/.github:/usr/src/app/.github:ro" \
  "${RENOVATE_IMAGE}" \
  renovate-config-validator --strict; then
  errors=1
fi

if [ "${errors}" -ne 0 ]; then
  echo "Renovate configuration check failed." >&2
  exit 1
fi

echo "Renovate configuration is valid."
