#!/usr/bin/env python3
#
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

"""Update public install docs to the latest released NVSentinel Helm version.

Contract:
- argv[1] must be a stable release tag in the form vMAJOR.MINOR.PATCH.
- Only public install docs in DOC_FILES are updated.
- Historical docs, release notes, and test reports are intentionally ignored.
"""

import re
import sys
from pathlib import Path


DOC_FILES = [
    Path("README.md"),
    Path("docs/OVERVIEW.md"),
    Path("distros/kubernetes/README.md"),
]

VERSION_RE = re.compile(r"^v\d+\.\d+\.\d+$")
ENV_RE = re.compile(r"^(NVSENTINEL_VERSION=)v\d+\.\d+\.\d+(\s*)$")
HELM_VERSION_RE = re.compile(r"(--version\s+)v\d+\.\d+\.\d+")


def update_file(path: Path, version: str) -> bool:
    lines = path.read_text(encoding="utf-8").splitlines(keepends=True)
    changed = False
    updated: list[str] = []

    for line in lines:
        original = line
        line = ENV_RE.sub(rf"\g<1>{version}\2", line)

        recent_context = "".join(updated[-4:]) + line
        if "oci://ghcr.io/nvidia/nvsentinel" in recent_context:
            line = HELM_VERSION_RE.sub(rf"\g<1>{version}", line)

        if line != original:
            changed = True
        updated.append(line)

    if changed:
        path.write_text("".join(updated), encoding="utf-8")
    return changed


def main() -> int:
    if len(sys.argv) != 2 or not VERSION_RE.fullmatch(sys.argv[1]):
        print("usage: update-docs-helm-version.py vMAJOR.MINOR.PATCH", file=sys.stderr)
        return 2

    version = sys.argv[1]
    changed = [str(path) for path in DOC_FILES if update_file(path, version)]

    if changed:
        print("Updated Helm version references:")
        for path in changed:
            print(f"- {path}")
    else:
        print(f"Docs already reference {version}")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
