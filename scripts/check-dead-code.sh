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

# Fails when the repository contains dead Go code. Two checks run:
#
#   1. deadcode (golang.org/x/tools/cmd/deadcode) over every module at once,
#      through a temporary go.work, so a call from another module counts.
#      It reports functions that no binary or test reaches. Methods in test
#      and mock files are skipped: mocks often carry methods that exist only
#      to satisfy an interface.
#   2. golangci-lint's unused linter on each module with test files included,
#      for unused types, fields, constants and variables, which deadcode does
#      not report.
#
# Both run once per e2e build tag set. Code is reported only when it is dead
# under every tag set, since some helpers are used by one architecture only.

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

DEADCODE=${DEADCODE:-deadcode}
GOLANGCI_LINT=${GOLANGCI_LINT:-golangci-lint}
TAG_SETS=("amd64_group,mongodb,integration" "arm64_group,mongodb,integration")

ROOT=$(git rev-parse --show-toplevel)
cd "$ROOT"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mapfile -t MODULES < <(git ls-files '*go.mod' | xargs -n1 dirname | sort)

(cd "$WORK" && go work init "${MODULES[@]/#/$ROOT/}")

# Keeps only the findings present in every per-tag-set result file.
intersect() {
	local result=$1
	shift

	sort -u "$1" >"$result"
	shift

	for f in "$@"; do
		comm -12 "$result" <(sort -u "$f") >"$result.tmp"
		mv "$result.tmp" "$result"
	done
}

echo "Checking for functions no binary or test reaches (deadcode)..."

mapfile -t PACKAGES < <(GOWORK="$WORK/go.work" go list -m -f '{{.Path}}/...')

deadcode_results=()

for i in "${!TAG_SETS[@]}"; do
	out="$WORK/deadcode.$i"
	GOWORK="$WORK/go.work" "$DEADCODE" -test -tags "${TAG_SETS[$i]}" "${PACKAGES[@]}" >"$out.raw"
	sed -E 's/:[0-9]+:[0-9]+: unreachable func: /: /' "$out.raw" |
		{ grep -vE '(_test\.go|/mock[^/]*\.go|_mock\.go): [^ ]+\.[^ ]+$' || true; } >"$out"
	deadcode_results+=("$out")
done

intersect "$WORK/deadcode" "${deadcode_results[@]}"

echo "Checking for unused code, including test files (golangci-lint unused)..."

unused_results=()

for i in "${!TAG_SETS[@]}"; do
	out="$WORK/unused.$i"
	: >"$out"

	for module in "${MODULES[@]}"; do
		rc=0
		(
			cd "$module"
			GOWORK=off "$GOLANGCI_LINT" run \
				--config "$ROOT/.golangci.yml" \
				--tests \
				--enable-only unused \
				--build-tags "${TAG_SETS[$i]}" \
				--issues-exit-code 1 \
				--output.text.path stdout \
				--output.text.print-issued-lines=false \
				--output.code-climate.path "$WORK/code-climate.json" \
				./...
		) >"$WORK/golangci.out" 2>&1 || rc=$?

		# Exit code 1 means issues were found; anything else means the run itself failed.
		if [[ $rc -ne 0 && $rc -ne 1 ]]; then
			echo -e "${RED}golangci-lint failed on $module (exit $rc):${NC}"
			cat "$WORK/golangci.out"
			exit "$rc"
		fi

		# Paths are relative to the repository root, where .golangci.yml lives.
		{ grep -E '^[^ ]+\.go:[0-9]+:[0-9]+: ' "$WORK/golangci.out" || true; } >>"$out"
	done

	unused_results+=("$out")
done

intersect "$WORK/unused" "${unused_results[@]}"

if [[ -s "$WORK/deadcode" || -s "$WORK/unused" ]]; then
	if [[ -s "$WORK/deadcode" ]]; then
		echo -e "${RED}Functions that no binary or test reaches:${NC}"
		cat "$WORK/deadcode"
	fi

	if [[ -s "$WORK/unused" ]]; then
		echo -e "${RED}Unused code:${NC}"
		cat "$WORK/unused"
	fi

	echo -e "${RED}Dead code found. Remove it, or call it from a binary or test.${NC}"
	exit 1
fi

echo -e "${GREEN}No dead code found.${NC}"
