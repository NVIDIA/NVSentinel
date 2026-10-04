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

# Runs the whole demo, start to finish.
#
#   ./demo.sh            build the cluster, break the GPU, watch NVSentinel fix it
#   ./demo.sh cleanup    delete the cluster
#
# The steps this runs live in scripts/ and can be run one at a time instead, if
# you would rather read what each one does before it does it. See README.md.

# shellcheck source-path=SCRIPTDIR source=scripts/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/scripts/common.sh"

STEPS=(
    00-setup.sh
    01-show-cluster.sh
    02-inject-fault.sh
    03-watch-remediation.sh
    04-recover.sh
)

usage() {
    cat <<EOF
NVSentinel local fault injection demo

  ./demo.sh            Run every step end to end. About ten minutes once the images are cached, longer on the first run.
  ./demo.sh cleanup    Delete the demo cluster.
  ./demo.sh help       Show this message.

To step through instead, run these in order and read as you go:

$(printf '  ./scripts/%s\n' "${STEPS[@]}")
  ./scripts/99-cleanup.sh
EOF
}

run_all() {
    # Tells each step not to suggest running the next one by hand, so the run
    # reads as one sequence rather than five scripts in a trenchcoat.
    export DEMO_ONESHOT=1

    section "NVSentinel local fault injection demo"
    log "Build a cluster, break a GPU, and watch NVSentinel detect the fault, protect the workload, and remediate the node."

    local step
    for step in "${STEPS[@]}"; do
        if ! "$DEMO_DIR/scripts/$step"; then
            echo
            fail "Step ${step} failed. Nothing was cleaned up, so you can investigate the cluster as it stands.
When you are done:  ./demo.sh cleanup"
        fi
    done

    section "Done"
    log "Delete the cluster when you have finished poking at it:  ./demo.sh cleanup"
}

main() {
    case "${1:-run}" in
    run)
        run_all
        ;;
    cleanup)
        "$DEMO_DIR/scripts/99-cleanup.sh"
        ;;
    help | -h | --help)
        usage
        ;;
    *)
        usage >&2
        echo >&2
        fail "Unknown argument: $1"
        ;;
    esac
}

main "$@"
