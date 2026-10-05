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

# Reports each test of a bash test script on its own, as JUnit XML: the format
# that CI systems and TestGrid read.
#
# Source this file, run each test with junit_test, and end with junit_finish:
#
#   source "${SCRIPT_DIR}/junit.sh"
#   JUNIT_SUITE=my-tests             # the suite's name; the script's name if unset
#   JUNIT_CLEANUP=clean_up           # optional: runs after each test, in its shell
#   junit_test test_install          # runs the function test_install as a test
#   junit_test upgrade helm upgrade --wait my-release ./chart   # or a command
#   junit_finish                     # returns 1 if any test failed
#
# Each test runs in a subshell with errexit (set -e). It stops at its first
# failing command, or when it calls exit, and the next test still runs. The
# variables a test sets don't reach the tests after it, so JUNIT_CLEANUP runs
# in the test's own subshell, where it can see them. A test can call
# junit_skip_test "<reason>" to stop and report itself as skipped. Call
# junit_test as a command of its own, not in an if or a && or || list: bash
# turns errexit off there, also in the test.
#
# When ARTIFACTS is set, the report goes to $ARTIFACTS/junit_<suite>.xml, as
# Prow and TestGrid expect. It is written again after each test, so the tests
# that finished are reported even if the script is killed. A failed test's
# message is the last line of its output that mentions an error, and the lines
# before it are the details. The whole output stays in the log.

JUNIT_SUITE=${JUNIT_SUITE:-$(basename "$0" .sh)}
JUNIT_CLEANUP=${JUNIT_CLEANUP:-}

_junit_dir=""
_junit_started=""
_junit_cases=""
_junit_summary=""
_junit_tests=0
_junit_failures=0
_junit_skipped=0
_junit_seconds=0

# junit_test NAME [COMMAND [ARG...]]
# Runs COMMAND, or the function NAME, as the test NAME, and reports its result.
# It returns 0 even when the test fails, so that the script carries on.
junit_test() {
    local name=$1
    shift
    if [[ $# -eq 0 ]]; then
        set -- "$name"
    fi
    if [[ -z "$_junit_dir" ]]; then
        _junit_dir=$(mktemp -d)
        _junit_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    fi
    rm -f "$_junit_dir/output" "$_junit_dir/skipped"
    echo "=== RUN   $name"

    # errexit only works in the subshell if the subshell isn't part of an if or
    # a || list, so turn it off here instead, and read the status afterwards.
    local errexit="" status started seconds
    if [[ $- == *e* ]]; then
        errexit=true
    fi
    started=$(date +%s)
    set +e
    (
        set -e
        if [[ -n "$JUNIT_CLEANUP" ]]; then
            # shellcheck disable=SC2064 # JUNIT_CLEANUP is the command to run.
            trap "$JUNIT_CLEANUP" EXIT
        fi
        "$@"
    ) 2>&1 | tee "$_junit_dir/output"
    status=${PIPESTATUS[0]}
    if [[ -n "$errexit" ]]; then
        set -e
    fi
    seconds=$(($(date +%s) - started))

    local result message="" details=""
    if [[ $status -ne 0 ]]; then
        result=FAILED
        _junit_failures=$((_junit_failures + 1))
        _junit_failure "$status"
    elif [[ -f "$_junit_dir/skipped" ]]; then
        result=SKIPPED
        _junit_skipped=$((_junit_skipped + 1))
        message=$(cut -c 1-300 "$_junit_dir/skipped")
    else
        result=PASSED
    fi
    _junit_tests=$((_junit_tests + 1))
    _junit_seconds=$((_junit_seconds + seconds))

    local outcome=""
    case $result in
    FAILED) outcome="<failure message=\"$(_junit_escape <<<"$message")\">$(_junit_escape <<<"$details")</failure>" ;;
    SKIPPED) outcome="<skipped message=\"$(_junit_escape <<<"$message")\"/>" ;;
    esac
    local testcase line
    printf -v testcase '    <testcase classname="%s" name="%s" time="%d">%s</testcase>\n' \
        "$(_junit_escape <<<"$JUNIT_SUITE")" "$(_junit_escape <<<"$name")" "$seconds" "$outcome"
    _junit_cases+=$testcase
    printf -v line '  %-7s  %s (%ds)%s\n' "$result" "$name" "$seconds" "${message:+: $message}"
    _junit_summary+=$line

    echo "--- $result  $name (${seconds}s)"
    _junit_write
}

# junit_skip_test REASON
# Stops the running test, and reports it as skipped. Call it from a test.
junit_skip_test() {
    if [[ -z "$_junit_dir" ]]; then
        echo "junit_skip_test: call it from a test that junit_test runs" >&2
        return 1
    fi
    echo "Skipping: $*"
    printf '%s\n' "$*" >"$_junit_dir/skipped"
    exit 0
}

# junit_finish
# Prints a summary of the tests, and returns 1 if any of them failed.
junit_finish() {
    echo "$JUNIT_SUITE: $((_junit_tests - _junit_failures - _junit_skipped)) passed, $_junit_failures failed, $_junit_skipped skipped"
    printf '%s' "$_junit_summary"
    if [[ -n "$_junit_dir" ]]; then
        rm -rf "$_junit_dir"
    fi
    [[ $_junit_failures -eq 0 ]]
}

# _junit_failure STATUS
# Sets message and details from the failed test's output: the last line that
# mentions an error, or that is one of bash's own errors, and the lines that led
# to it.
_junit_failure() {
    local output="$_junit_dir/output" error
    error=$(grep -naiE '(^|[^[:alpha:]])error([^[:alpha:]]|$)|: line [0-9]+: ' "$output" | tail -n 1) || true
    if [[ -n "$error" ]]; then
        message=$(cut -d : -f 2- <<<"$error" | cut -c 1-300)
        details=$(head -n "${error%%:*}" "$output" | tail -n 10 | cut -c 1-300)
    else
        message="exited with status $1"
        details=$(tail -n 10 "$output" | cut -c 1-300)
    fi
}

# _junit_write
# Writes the report of the tests so far, if ARTIFACTS is set.
_junit_write() {
    if [[ -z "${ARTIFACTS:-}" ]]; then
        return 0
    fi
    mkdir -p "$ARTIFACTS"
    local report="$ARTIFACTS/junit_${JUNIT_SUITE//[^A-Za-z0-9_.-]/_}.xml"
    {
        printf '<?xml version="1.0" encoding="UTF-8"?>\n<testsuites>\n'
        printf '  <testsuite name="%s" tests="%d" failures="%d" errors="0" skipped="%d" time="%d" timestamp="%s">\n' \
            "$(_junit_escape <<<"$JUNIT_SUITE")" "$_junit_tests" "$_junit_failures" "$_junit_skipped" "$_junit_seconds" "$_junit_started"
        printf '%s' "$_junit_cases"
        printf '  </testsuite>\n</testsuites>\n'
    } >"$report.tmp"
    mv "$report.tmp" "$report"
}

# _junit_escape
# Makes stdin safe in XML: drops the control characters XML doesn't allow, and
# escapes the characters it reserves.
_junit_escape() {
    tr -d '\000-\010\013\014\016-\037' | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g'
}
