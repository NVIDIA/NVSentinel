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

"""Lists the packages that own the files that the traced processes loaded.

scripts/openvex/trace.sh runs this file inside the traced image, after the program ends, because the
package databases have to be the ones of that image. It reads:

- the records that sitecustomize.py wrote in /openvex-trace/out, one JSON file for each process, with
  the Python module files and shared libraries that the process loaded, for example
  {"argv": ["python3", "-m", "dcgm_diag"], "modules": [".../dcgm_diag/__main__.py"], "libs": ["/usr/lib/x86_64-linux-gnu/libz.so.1.3"],
   "sys_path": ["/opt/venv/lib/python3.12/site-packages", ...]}
- optional library paths on stdin, one on each line, as bare paths or as ldd output

It prints JSON, for example:

  {
   "files_loaded": 195,
   "python_dists_loaded": ["click==8.5.1", "prometheus_client==0.26.0"],
   "os_packages_loaded": ["libssl3", "musl", "zlib"],
   "os_packages_of_stdin_paths": []
  }

A package that is not in python_dists_loaded or os_packages_loaded was not loaded by any traced process.
"""

import glob
import json
import os
import re
import shutil
import subprocess
import sys
from importlib import metadata

RECORDS = "/openvex-trace/out/*.json"

# Some images link /lib, /lib64, /bin and /sbin to the same directories under /usr. dpkg can register a
# file under either spelling, while the records hold the resolved /usr path.
MERGED_USR_DIRS = (("/lib/", "/usr/lib/"), ("/lib64/", "/usr/lib64/"), ("/bin/", "/usr/bin/"), ("/sbin/", "/usr/sbin/"))


def read_records(pattern=RECORDS):
    """Returns the files that the traced processes loaded, and the directories they import from.

    The directories are the sys.path of each process. The mapper runs with python3 -I, which leaves some
    of them out of its own sys.path, for example a user site-packages directory, so it needs them to find
    the distributions that the processes loaded.

    A record that is not valid is skipped and reported, for example one that a process could not finish
    writing, or {"modules": [null], "libs": []}.

    Example: a record with "libs": ["/lib/x86_64-linux-gnu/libz.so.1"] adds
    "/usr/lib/x86_64-linux-gnu/libz.so.1.3" on an image where /lib links to /usr/lib.
    """
    loaded, search_path, read = set(), [], 0
    for record in sorted(glob.glob(pattern)):
        try:
            with open(record, encoding="utf-8") as f:
                data = json.load(f)
            if not isinstance(data["modules"], list) or not isinstance(data["libs"], list):
                raise ValueError("modules and libs are not lists")
            paths = data["modules"] + data["libs"]
            if not all(isinstance(p, str) for p in paths):
                raise ValueError("a path is not a string")
            loaded.update(os.path.realpath(p) for p in paths)
            search_path += [p for p in data.get("sys_path", []) if isinstance(p, str) and p not in search_path]
            read += 1
        except (OSError, ValueError, KeyError, TypeError) as e:
            print(f"skip {record}: {e}", file=sys.stderr)
    if read == 0:
        print("warning: no process record was read, so the result does not show what the program loads. "
              "The hook runs only in Python processes that read PYTHONPATH and import site, so not in a "
              "native program or in python3 -I or -S.", file=sys.stderr)
    return loaded, search_path


def library_paths_from_lines(lines):
    """Returns the resolved absolute paths in lines of bare paths or of ldd output.

    Example: these lines give {"/usr/lib/x86_64-linux-gnu/libc.so.6", "/usr/lib64/ld-linux-x86-64.so.2"}
    on an image with merged /usr, and skip linux-vdso.so.1, which is not a file:

      libc.so.6 => /lib/x86_64-linux-gnu/libc.so.6 (0x00007f...)
      /lib64/ld-linux-x86-64.so.2 (0x00007f...)
      linux-vdso.so.1 (0x00007ffd...)
    """
    paths = set()
    for line in lines:
        match = re.search(r"(?:=>\s*|^\s*)(/\S+)", line)
        if match:
            paths.add(os.path.realpath(match.group(1)))
    return paths


def python_distributions_owning(paths, search_path):
    """Returns "name==version" for each installed Python distribution that owns one of the paths.

    It reads the file list (RECORD) of every distribution in the directories of search_path, the
    directories that the traced processes import from, and in the sys.path of the mapper.

    Example: {"/usr/local/lib/python3.12/dist-packages/PIL/Image.py"} gives {"pillow==12.3.0"}.
    """
    owners = set()
    for dist in metadata.distributions(path=list(search_path) + sys.path):
        for file in dist.files or []:
            try:
                if os.path.realpath(str(dist.locate_file(file))) in paths:
                    owners.add(f"{dist.metadata['Name'].lower()}=={dist.version}")
                    break
            except (OSError, ValueError):
                continue
    return owners


def with_merged_usr_aliases(paths):
    """Returns the paths, plus the /lib, /lib64, /bin and /sbin spelling of each path under /usr.

    A spelling is added only when it is the same file, as on an image with merged /usr. Example:
    {"/usr/lib/x86_64-linux-gnu/libc.so.6"} gives
    {"/usr/lib/x86_64-linux-gnu/libc.so.6", "/lib/x86_64-linux-gnu/libc.so.6"} when /lib links to
    /usr/lib, and only the first path when /lib/x86_64-linux-gnu/libc.so.6 is another file.
    """
    aliases = set(paths)
    for path in paths:
        for short, full in MERGED_USR_DIRS:
            if path.startswith(full):
                alias = short + path[len(full):]
                if os.path.realpath(alias) == os.path.realpath(path):
                    aliases.add(alias)
    return aliases


def debian_packages_owning(paths):
    """Returns the dpkg packages that own the paths.

    Example: {"/usr/lib/x86_64-linux-gnu/libz.so.1.3"} gives {"zlib1g"}. A path that no package owns,
    for example a file of a pip install, gives nothing.
    """
    owners = set()
    paths = sorted(with_merged_usr_aliases(paths))
    for start in range(0, len(paths), 200):  # in batches, to keep the command line short
        output = subprocess.run(["dpkg", "-S", *paths[start:start + 200]], capture_output=True, text=True).stdout
        for line in output.splitlines():  # for example "zlib1g:amd64: /lib/x86_64-linux-gnu/libz.so.1.3"
            for package in line.split(": ", 1)[0].split(", "):
                owners.add(package.split(":")[0])
    return owners


def alpine_packages_owning(paths):
    """Returns the apk packages that own the paths.

    Example: {"/usr/lib/libz.so.1.3.2"} gives {"zlib"}.
    """
    owners = set()
    for path in sorted(paths):
        output = subprocess.run(["apk", "info", "-q", "--who-owns", path], capture_output=True, text=True).stdout
        owners.update(output.split())
    return owners


def os_packages_owning(paths):
    """Returns the OS packages that own the paths: dpkg packages on Debian and Ubuntu, apk packages on Alpine.

    Only system directories are checked, because OS packages install their files there.
    """
    system_paths = {p for p in paths if p.startswith(("/usr", "/lib", "/opt", "/bin", "/sbin"))}
    if shutil.which("dpkg"):
        return debian_packages_owning(system_paths)
    if shutil.which("apk"):
        return alpine_packages_owning(system_paths)
    print("no dpkg or apk in the image: OS packages are not listed", file=sys.stderr)
    return set()


def main():
    loaded, search_path = read_records()
    loaded.add(os.path.realpath(sys.executable))  # the interpreter, for example from python3.12-minimal
    stdin_paths = library_paths_from_lines(sys.stdin)
    search_path += [p for p in os.environ.get("OPENVEX_TRACE_PYTHONPATH", "").split(":") if p]
    print(json.dumps({
        "files_loaded": len(loaded),
        "python_dists_loaded": sorted(python_distributions_owning(loaded, search_path)),
        "os_packages_loaded": sorted(os_packages_owning(loaded)),
        "os_packages_of_stdin_paths": sorted(os_packages_owning(stdin_paths)),
    }, indent=1))


if __name__ == "__main__":
    main()
