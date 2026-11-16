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

# Run by scripts/openvex/trace.sh: maps the files that the traced processes loaded, and the library paths given
# on stdin (one path, or ldd output, on each line), to their Python distributions and dpkg or apk packages.
import glob
import json
import os
import re
import shutil
import subprocess
import sys
from importlib import metadata

loaded = set()
for f in glob.glob("/openvex-trace/out/*.json"):
    # Skip a record that is not valid, for example one that a process could not finish writing.
    try:
        with open(f) as fh:
            d = json.load(fh)
        paths = list(d["modules"]) + list(d["libs"])
    except (OSError, ValueError, KeyError, TypeError) as e:
        print(f"skip {f}: {e}", file=sys.stderr)
        continue
    for p in paths:
        loaded.add(os.path.realpath(p))
# Accept bare paths and ldd lines such as "libc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x...)".
extra = set()
for line in sys.stdin:
    m = re.search(r"(?:=>\s*|^\s*)(/\S+)", line)
    if m:
        extra.add(os.path.realpath(m.group(1)))

# The mapper runs with -I, so add the PYTHONPATH of the image to find the distributions installed there.
search = [p for p in os.environ.get("OPENVEX_TRACE_PYTHONPATH", "").split(":") if p] + sys.path
py = {}
for dist in metadata.distributions(path=search):
    name = f"{dist.metadata['Name'].lower()}=={dist.version}"
    for f in dist.files or []:
        try:
            p = os.path.realpath(str(dist.locate_file(f)))
        except Exception:
            continue
        if p in loaded:
            py[name] = py.get(name, 0) + 1


def os_owners(paths):
    """Maps file paths to the Debian (dpkg) or Alpine (apk) packages that own them."""
    out = {}
    paths = sorted(p for p in paths if p.startswith(("/usr", "/lib", "/opt", "/bin", "/sbin")))
    if shutil.which("dpkg"):
        # dpkg records some files under /lib, /bin or /sbin, which are links to /usr on merged-/usr images,
        # and the records hold the resolved /usr path. Query both spellings.
        aliases = set(paths)
        for p in paths:
            for short, full in (("/lib/", "/usr/lib/"), ("/lib64/", "/usr/lib64/"), ("/bin/", "/usr/bin/"), ("/sbin/", "/usr/sbin/")):
                if p.startswith(full):
                    aliases.add(short + p[len(full):])
        paths = sorted(aliases)
        for i in range(0, len(paths), 200):
            res = subprocess.run(["dpkg", "-S", *paths[i:i + 200]], capture_output=True, text=True).stdout
            for line in res.splitlines():
                for pkg in line.split(": ", 1)[0].split(", "):
                    out[pkg.split(":")[0]] = 1
    elif shutil.which("apk"):
        for path in paths:
            res = subprocess.run(["apk", "info", "-q", "--who-owns", path], capture_output=True, text=True).stdout
            for pkg in res.split():
                out[pkg] = 1
    return out


python_exe = os.path.realpath(sys.executable)
print(json.dumps({
    "files_loaded": len(loaded),
    "python_dists_loaded": sorted(py),
    "os_packages_loaded": sorted(os_owners(loaded | {python_exe})),
    "os_packages_of_stdin_paths": sorted(os_owners(extra)),
}, indent=1))
