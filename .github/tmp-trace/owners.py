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

# TEMPORARY (#2388): maps the files that the traced processes loaded, and extra library paths given
# on stdin, to their Python distributions and Debian packages.
import glob
import json
import os
import subprocess
import sys
from importlib import metadata

loaded = set()
for f in glob.glob("/trace/out/*.json"):
    d = json.load(open(f))
    for p in d["modules"] + d["libs"]:
        loaded.add(os.path.realpath(p))
extra = {os.path.realpath(line.strip()) for line in sys.stdin if line.strip()}

py = {}
for dist in metadata.distributions():
    name = f"{dist.metadata['Name'].lower()}=={dist.version}"
    for f in dist.files or []:
        try:
            p = os.path.realpath(str(dist.locate_file(f)))
        except Exception:
            continue
        if p in loaded:
            py[name] = py.get(name, 0) + 1


def deb_owners(paths):
    out = {}
    paths = sorted(p for p in paths if p.startswith(("/usr", "/lib", "/opt")))
    for i in range(0, len(paths), 200):
        res = subprocess.run(["dpkg", "-S", *paths[i:i + 200]], capture_output=True, text=True).stdout
        for line in res.splitlines():
            for pkg in line.split(": ", 1)[0].split(", "):
                pkg = pkg.split(":")[0]
                out[pkg] = out.get(pkg, 0) + 1
    return out


python_exe = os.path.realpath(sys.executable)
print(json.dumps({
    "files_loaded": len(loaded),
    "python_dists_loaded": sorted(py),
    "deb_packages_loaded": sorted(deb_owners(loaded | {python_exe})),
    "deb_packages_linked_by_dcgm": sorted(deb_owners(extra)),
}, indent=1))
