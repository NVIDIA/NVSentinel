#!/bin/bash
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

# Prints the go test -skip pattern for one E2E CI job: the tests in e2e-shards.yaml that are not in
# the list of this job or in "all". Prints nothing if no test is skipped.
#
# Usage: e2e-skip-pattern.sh <E2E_JOB_ID>
#
# The script fails if e2e-shards.yaml is not complete and correct:
# - Each key is "all" or a job in the E2E workflow matrix, and each job in the matrix has a key.
# - Each test function in tests/ is listed, each listed test exists, and no test is listed twice for
#   a job.
# - Each job builds the file of each of its tests with its build tags, and each test runs on each
#   architecture that builds its file.
# Otherwise a test can run in every job, or in no job of an architecture.

set -euo pipefail

job=${1:?usage: $0 <E2E_JOB_ID>}
dir=$(cd "$(dirname "$0")" && pwd -P)
shards="$dir/e2e-shards.yaml"
workflow="$dir/../.github/workflows/e2e-test.yml"

# One "<job> <arch> <tags>" line for each job in the matrix. The same expressions as E2E_JOB_ID and
# TEST_TAGS in the workflow.
matrix=$(yq '.jobs["e2e-test"].strategy.matrix.include[] | [.arch, .datastore, .platform_connector // ""] | join(" ")' "$workflow" |
    awk '{ print $1 "-" $2 ($3 == "deployment" ? "-deployment-pc" : ""), $1, $1 "_group" ($2 ~ /mongodb/ ? ",mongodb" : "") }')

if ! awk -v job="$job" '$1 == job { found = 1 } END { exit !found }' <<< "$matrix"; then
    echo "Error: job $job is not in the matrix of $workflow" >&2
    exit 1
fi

# One "<tags> <file>" line for each test file that go builds with the tags of a job.
built=$(awk '{ print $3 }' <<< "$matrix" | sort -u | while read -r tags; do
    (cd "$dir" && go list -tags "$tags" -f '{{.Dir}} {{join .TestGoFiles " "}}' ./...) |
        awk -v tags="$tags" '{ for (i = 2; i <= NF; i++) print tags, $1 "/" $i }'
done)

# One "<file> <test>" line for each top-level test function in all packages of tests/. gofmt sets
# the spacing, so only the parameter name can change.
tests=$(grep -rHoE '^func Test[A-Za-z0-9_]*\([A-Za-z0-9_]+ \*testing\.T\)' --include='*_test.go' "$dir" |
    sed -E 's/^([^:]+):func (Test[A-Za-z0-9_]*).*/\1 \2/')

# One "job <key> <type>" line for each key, and one "test <key> <number of items that are not
# names> <tests>" line for each list. yq keeps a duplicate key, so the checks below find it.
entries=$(yq 'to_entries[] | (("job " + .key + " " + (.value | tag)), (select(.value | tag == "!!seq") | "test " + .key + " " + (.value | map(select(tag != "!!str")) | length | tostring) + " " + (.value | map(select(tag == "!!str")) | join(" "))))' "$shards")

awk -v matrix="$matrix" -v built="$built" -v tests="$tests" -v job="$job" -v shards="$shards" '
    function fail(msg) {
        printf "Error: %s: %s\n", shards, msg > "/dev/stderr"
        failed = 1
        exit 1
    }
    BEGIN {
        njobs = split(matrix, lines, "\n")
        for (i = 1; i <= njobs; i++) {
            split(lines[i], f, " ")
            jobs[i] = f[1]; arch[f[1]] = f[2]; tags[f[1]] = f[3]; known[f[1]] = 1
        }
        known["all"] = 1
        n = split(built, lines, "\n")
        for (i = 1; i <= n; i++) { split(lines[i], f, " "); builds[f[1], f[2]] = 1 }
        ntests = split(tests, lines, "\n")
        for (i = 1; i <= ntests; i++) { split(lines[i], f, " "); defined[i] = f[2]; file[f[2]] = f[1] }
    }
    $1 == "job" {
        if (!($2 in known)) fail($2 " is not \"all\" or a job in the E2E matrix")
        if ($2 in keys) fail($2 " is listed twice")
        if ($3 != "!!seq") fail($2 " is not a list of tests")
        keys[$2] = 1
    }
    $1 == "test" {
        if ($3 != 0) fail($2 " has an item that is not a test name")
        for (i = 4; i <= NF; i++) {
            if (!($i in file)) fail($i " in " $2 " is not a test function in tests/")
            if (($2, $i) in pairs) fail($i " is listed twice in " $2)
            if ($2 != "all" && !((tags[$2], file[$i]) in builds)) fail($i " is in " $2 ", but the tags of " $2 " do not build its file")
            pairs[$2, $i] = 1
            if ($2 == "all") inall[$i] = 1; else injob[$i] = 1
            if ($2 == "all" || $2 == job) runs[$i] = 1
        }
    }
    END {
        if (failed) exit 1
        for (i = 1; i <= njobs; i++) {
            if (!(jobs[i] in keys)) fail(jobs[i] " has no key. Add it with its tests")
        }
        for (i = 1; i <= ntests; i++) {
            t = defined[i]
            if (!(t in inall) && !(t in injob)) fail(t " is not listed. Add it to its jobs, or to \"all\"")
            if ((t in inall) && (t in injob)) fail(t " is in \"all\" and in a job. Keep it in one of them")
            # The architectures that build the file of the test, and the architectures that run it.
            delete needed
            delete covered
            for (j = 1; j <= njobs; j++) {
                jb = jobs[j]
                if (!((tags[jb], file[t]) in builds)) continue
                needed[arch[jb]] = 1
                if ((t in inall) || ((jb, t) in pairs)) covered[arch[jb]] = 1
            }
            for (a in needed) {
                if (!(a in covered)) fail(t " does not run on " a ". Add it to a job of " a)
            }
            if (!(t in runs)) skip = skip (skip == "" ? "" : "|") t
        }
        if (skip != "") print "^(" skip ")$"
    }
' <<< "$entries"
