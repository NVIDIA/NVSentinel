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

# Loaded by scripts/openvex/trace.sh through PYTHONPATH: records the Python modules and shared libraries of each Python process.
import atexit
import json
import os
import sys
import threading
import time


def _dump(reason):
    try:
        mods = sorted({getattr(m, "__file__", None) or "" for m in list(sys.modules.values())} - {""})
        libs = set()
        with open("/proc/self/maps") as f:
            for line in f:
                p = line.split()[-1]
                if p.startswith("/") and ".so" in p:
                    libs.add(os.path.realpath(p))
        with open(f"/openvex-trace/out/{os.getpid()}-{reason}.json", "w") as f:
            json.dump({"argv": getattr(sys, "orig_argv", sys.argv), "modules": mods, "libs": sorted(libs)}, f)
    except Exception as e:
        sys.stderr.write(f"trace dump failed: {e}\n")


atexit.register(_dump, "exit")


def _periodic():
    while True:
        time.sleep(2)
        _dump("periodic")


def _start():
    threading.Thread(target=_periodic, daemon=True).start()


_start()
# A forked child, such as a multiprocessing worker, has no copy of the thread, and ends with os._exit,
# which skips atexit. Restart the thread in the child, and record before os._exit.
os.register_at_fork(after_in_child=_start)
_orig_exit = os._exit


def _exit(code, _orig=_orig_exit):
    _dump("exit")
    _orig(code)


os._exit = _exit

for _n in ("execv", "execve"):
    _orig = getattr(os, _n)

    def _wrap(*a, _orig=_orig, **k):
        _dump("exec")
        return _orig(*a, **k)

    setattr(os, _n, _wrap)

# Chain to the image's own sitecustomize, if any.
_here = os.path.dirname(os.path.abspath(__file__))
_rest = [p for p in sys.path if os.path.abspath(p or ".") != _here]
import importlib.machinery  # noqa: E402
import importlib.util  # noqa: E402

_spec = importlib.machinery.PathFinder.find_spec("sitecustomize", _rest)
if _spec:
    _m = importlib.util.module_from_spec(_spec)
    _spec.loader.exec_module(_m)
