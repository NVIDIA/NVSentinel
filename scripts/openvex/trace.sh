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

# Runs a Python program in its image and lists the packages that its processes load.
#
# Usage: scripts/openvex/trace.sh [-o DIR] [-l LIBS] IMAGE [DOCKER-RUN-OPTION...] [-- ARG...]
#   -o DIR   where to keep the records of the processes, in a new subdirectory for each run
#            (default: a new temporary directory). Use a directory that other users cannot write.
#   -l LIBS  a file with more library paths to map to packages, for example the output of ldd
#   IMAGE    the image to run, which needs python3
#   Options for docker run follow the image. The arguments after -- go after the image, as the
#   command. For a program that runs until it is stopped, use timeout, for example:
#   IMAGE --entrypoint timeout -- -s INT 45 gpu_health_monitor --dcgm-addr 127.0.0.1:5555
#
# A sitecustomize hook records the Python modules and shared libraries of every Python process,
# when the process exits, before it calls exec, and every 2 seconds. The script then prints, as
# JSON, the Python distributions and the OS packages (dpkg or apk) that own those files.
#
# The trace covers only the code that runs. Run the program as far as it gets without a GPU or a
# cluster, and state in the pull request which code paths the trace did not reach.

set -euo pipefail

usage() {
  sed -n '/^# Usage:/,/^#$/p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

out=""
libs=/dev/null
while getopts "o:l:h" opt; do
  case "${opt}" in
    o) out=${OPTARG} ;;
    l) libs=${OPTARG} ;;
    *) usage ;;
  esac
done
shift $((OPTIND - 1))
[ "$#" -ge 1 ] || usage
image=$1
shift
docker_opts=()
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do
  docker_opts+=("$1")
  shift
done
if [ "$#" -gt 0 ]; then shift; fi

hook="$(cd "$(dirname "$0")" && pwd)/trace"
out=${out:-$(mktemp -d)}
mkdir -p "${out}"
# A new directory for each run, so that the report covers only this run. The program can run as a
# user other than yours, for example the USER of the image, so the directory is writable by all.
# The sticky bit keeps each user's records safe from the others.
records=$(cd "$(mktemp -d "${out}/run.XXXXXX")" && pwd)
chmod 1777 "${records}"

docker image inspect "${image}" >/dev/null 2>&1 || docker pull -q "${image}" >/dev/null

# Keep the PYTHONPATH of the image, so the program still finds its modules.
image_pythonpath=$(docker image inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "${image}" \
  | sed -n 's/^PYTHONPATH=//p')
pythonpath="/openvex-trace/hook${image_pythonpath:+:${image_pythonpath}}"

set +e
docker run --rm -v "${hook}:/openvex-trace/hook:ro" -v "${records}:/openvex-trace/out" \
  -e PYTHONPATH="${pythonpath}" ${docker_opts[@]+"${docker_opts[@]}"} "${image}" "$@" 2>&1 | tail -40
echo "(the program exited with ${PIPESTATUS[0]})"
set -e

# -I keeps the hook out of the mapper. The mapper gets the PYTHONPATH of the image separately, to find
# the distributions installed there.
docker run --rm -i --entrypoint python3 -v "${hook}:/openvex-trace/hook:ro" -v "${records}:/openvex-trace/out:ro" \
  -e OPENVEX_TRACE_PYTHONPATH="${image_pythonpath}" "${image}" -I /openvex-trace/hook/owners.py < "${libs}"
echo "Process records: ${records}" >&2
