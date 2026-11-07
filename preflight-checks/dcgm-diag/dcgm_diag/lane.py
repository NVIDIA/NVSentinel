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

"""DCGM client lane selection.

DCGM 4.7.1 clients and hostengines older than 4.7.1 cannot run diagnostics
against each other. The image ships two client lanes: the default lane
(DCGM 4.7.1, base image paths) and a legacy lane (DCGM 4.7.0, under
LEGACY_LANE_DIR). One process can load only one libdcgm, so switching to the
legacy lane re-executes the process with the legacy library and bindings.
"""

import logging
import os
import re
import sys

from .diag import DCGMDiagnostic

log = logging.getLogger(__name__)

LEGACY_LANE_DIR = "/opt/dcgm-4.7.0"
LANE_ENV = "DCGM_DIAG_CLIENT_LANE"
DEFAULT_LANE_MIN_VERSION = (4, 7, 1)

_VERSION_RE = re.compile(r"version:(\d+)\.(\d+)\.(\d+)")


def parse_hostengine_version(build_info: str) -> tuple[int, int, int] | None:
    match = _VERSION_RE.search(build_info or "")
    if not match:
        return None
    return int(match.group(1)), int(match.group(2)), int(match.group(3))


def needs_legacy_lane(hostengine_version: tuple[int, int, int] | None) -> bool:
    return hostengine_version is not None and hostengine_version < DEFAULT_LANE_MIN_VERSION


def select_lane(diag: DCGMDiagnostic) -> None:
    """Re-execute in the legacy lane if the hostengine is older than 4.7.1; otherwise return."""
    if os.environ.get(LANE_ENV):
        log.info("DCGM client lane already selected", extra={"lane": os.environ[LANE_ENV]})
        return

    try:
        build_info = diag.get_hostengine_build_info()
    except Exception as e:
        log.warning("Unable to read DCGM hostengine version; using default lane", extra={"error": str(e)})
        return

    hostengine_version = parse_hostengine_version(build_info)
    if hostengine_version is None:
        log.warning("Unable to parse DCGM hostengine version; using default lane", extra={"build_info": build_info})
        return

    version_str = ".".join(map(str, hostengine_version))
    if not needs_legacy_lane(hostengine_version):
        log.info("Using default DCGM client lane", extra={"hostengine_version": version_str})
        return

    if not os.path.isdir(LEGACY_LANE_DIR):
        log.warning(
            "Legacy DCGM client lane not found; using default lane",
            extra={"hostengine_version": version_str, "lane_dir": LEGACY_LANE_DIR},
        )
        return

    env = dict(os.environ)
    env[LANE_ENV] = "legacy"
    env["PYTHONPATH"] = os.path.join(LEGACY_LANE_DIR, "bindings")
    lib_dir = os.path.join(LEGACY_LANE_DIR, "lib")
    env["LD_LIBRARY_PATH"] = os.pathsep.join(p for p in (lib_dir, env.get("LD_LIBRARY_PATH")) if p)

    log.info("Switching to legacy DCGM client lane", extra={"hostengine_version": version_str})
    argv = [sys.executable, "-m", "dcgm_diag", *sys.argv[1:]]
    os.execve(sys.executable, argv, env)
