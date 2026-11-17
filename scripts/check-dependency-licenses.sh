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

# Fails when a third-party Go dependency of a core module has a license outside the allowed list
# in .dependency-licenses.yaml, unless an exception in that file covers it. Also fails on an
# exception that no module uses. See .dependency-licenses.yaml for the policy.
#
# go-licenses reports the packages that the module's non-test code imports, so test-only
# dependencies are not checked: they are not shipped.

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

GO_LICENSES=${GO_LICENSES:-go-licenses}
OWN_MODULE_PREFIX="github.com/nvidia/nvsentinel"

ROOT=$(git rev-parse --show-toplevel)
cd "$ROOT"

CONFIG=.dependency-licenses.yaml

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mapfile -t MODULES < <(yq '.modules[]' "$CONFIG")
yq '.allowed[]' "$CONFIG" >"$WORK/allowed.txt"
yq '.exceptions[] | [.module, .reported] | @tsv' "$CONFIG" >"$WORK/exceptions.tsv"

# One line per package: core module, package, license (several licenses joined with "+").
: >"$WORK/report.tsv"

for module in "${MODULES[@]}"; do
  echo "Checking dependency licenses of ${module}..."

  # -mod=readonly reads the module cache even when a vendor directory exists: protos-lint leaves
  # a partial, ignored vendor directory in data-models, and vendor mode cannot load from it.
  if ! (cd "$module" && GOFLAGS="${GOFLAGS:+$GOFLAGS }-mod=readonly" \
    "$GO_LICENSES" report ./... --ignore "$OWN_MODULE_PREFIX") \
    >"$WORK/${module}.csv" 2>"$WORK/${module}.log"; then
    echo -e "${RED}go-licenses failed for ${module}:${NC}"
    cat "$WORK/${module}.log"
    exit 1
  fi

  # go-licenses prints package,license URL,license names.
  awk -F, -v module="$module" -v OFS='\t' '
    NF >= 3 {
      license = $3
      for (i = 4; i <= NF; i++) license = license "+" $i
      print module, $1, license
    }' "$WORK/${module}.csv" >>"$WORK/report.tsv"
done

awk -F'\t' -v red="$RED" -v nc="$NC" '
  FILENAME == ARGV[1] { allowed[$1] = 1; next }
  FILENAME == ARGV[2] { exception[$1] = $2; used[$1] = 0; next }
  {
    module = $1; pkg = $2; license = $3

    ok = 1
    n = split(license, parts, "+")
    for (i = 1; i <= n; i++) {
      if (!(parts[i] in allowed)) { ok = 0; break }
    }
    if (ok) next

    covered = ""
    for (e in exception) {
      if (pkg == e || index(pkg, e "/") == 1) { covered = e; break }
    }

    if (covered == "") {
      printf "%s%s: %s has license %s, which is not allowed%s\n", red, module, pkg, license, nc
      failed = 1
    } else if (license != exception[covered]) {
      printf "%s%s: %s reports %s, but its exception expects %s%s\n", red, module, pkg, license, exception[covered], nc
      failed = 1
    } else {
      used[covered] = 1
    }
  }
  END {
    for (e in used) {
      if (!used[e]) {
        printf "%sexception for %s is not used by any core module; remove it%s\n", red, e, nc
        failed = 1
      }
    }
    exit failed
  }' "$WORK/allowed.txt" "$WORK/exceptions.tsv" "$WORK/report.tsv" || {
  echo
  echo "Add an allowed dependency to the exceptions in ${CONFIG} with a reason, or replace it."
  exit 1
}

echo -e "${GREEN}All dependency licenses of the core modules are allowed.${NC}"
