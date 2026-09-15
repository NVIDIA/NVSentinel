# Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

"""Run the unchanged monitor with host DCGM and optional systemd supervision."""

from __future__ import annotations

import ctypes
import importlib
from importlib.metadata import version
import json
import os
from pathlib import Path
import socket
import sys
from threading import Event, Thread
import urllib.error
import urllib.request


def check_runtime() -> dict[str, str]:
    """Load host DCGM and installed dependencies without connecting to a GPU."""
    bindings = Path(os.environ.get("DCGM_PYTHON_PATH", "/usr/share/datacenter-gpu-manager-4/bindings/python3"))
    if not bindings.is_dir():
        raise RuntimeError(f"DCGM bindings not found at {bindings}; set DCGM_PYTHON_PATH to the host DCGM 4 bindings")
    sys.path.insert(0, str(bindings.resolve()))
    ctypes.CDLL("libdcgm.so.4")
    for name in (
        "dcgm_structs",
        "dcgm_agent",
        "dcgm_errors",
        "dcgm_fields",
        "dcgmvalue",
        "pydcgm",
        "gpu_health_monitor.cli",
    ):
        importlib.import_module(name)
    return {
        "version": version("gpu-health-monitor"),
        "python": sys.version.split()[0],
        "executable": sys.executable,
        "pid": str(os.getpid()),
        "dcgmBindings": str(bindings),
    }


def notify(address: str, message: str) -> None:
    """Send one systemd notification to a filesystem or abstract Unix socket."""
    if address.startswith("@"):
        address = "\0" + address[1:]
    with socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM) as client:
        client.settimeout(1)
        client.connect(address)
        client.sendall(message.encode())


def supervise(address: str, port: int, interval: float, stop: Event) -> None:
    """Notify readiness and keep the watchdog alive only while /healthz is healthy.

    A stale monitor or an unreachable endpoint receives no heartbeat. systemd,
    not this thread, then performs the bounded process restart.
    """
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    ready = False
    while not stop.is_set():
        try:
            with opener.open(f"http://127.0.0.1:{port}/healthz", timeout=min(interval, 2)) as response:
                healthy = response.status == 200 and response.read(16).strip() == b"ok"
            if healthy:
                notify(address, "WATCHDOG=1" if ready else "READY=1\nWATCHDOG=1")
                ready = True
        except (OSError, urllib.error.URLError):
            # Startup, a stale loop, and a stopped HTTP server all mean no heartbeat.
            pass
        stop.wait(interval)


def main() -> None:
    """Validate the private runtime, then start the normal monitor CLI."""
    try:
        runtime = check_runtime()
        if sys.argv[1:] == ["--check-runtime"]:
            print(json.dumps(runtime, sort_keys=True))
            return
        if not os.environ.get("NODE_NAME") and "--help" not in sys.argv[1:]:
            raise RuntimeError("NODE_NAME is required")
        address = os.environ.get("NOTIFY_SOCKET")
        watchdog_usec = int(os.environ.get("WATCHDOG_USEC", "0"))
        watchdog_pid = int(os.environ.get("WATCHDOG_PID", str(os.getpid())))
        if address and (watchdog_usec <= 0 or watchdog_pid != os.getpid()):
            raise RuntimeError("The native systemd unit requires WatchdogSec and the correct WATCHDOG_PID")
        port = int(os.environ.get("GPU_HEALTH_MONITOR_PORT", "2114"))
        if not 1 <= port <= 65535:
            raise RuntimeError("GPU_HEALTH_MONITOR_PORT must be between 1 and 65535")
    except (OSError, ValueError, RuntimeError, ImportError) as error:
        raise SystemExit(f"GPU monitor native startup failed: {error}") from error

    from gpu_health_monitor.cli import cli

    print(
        json.dumps({"event": "native_runtime_validated", **runtime}, sort_keys=True),
        file=sys.stderr,
        flush=True,
    )
    stop = Event()
    watcher = None
    if address:
        watcher = Thread(
            target=supervise,
            args=(address, port, min(watchdog_usec / 3_000_000, 5), stop),
            name="systemd-watchdog",
            daemon=True,
        )
        watcher.start()
    try:
        cli()
    finally:
        stop.set()
        if watcher is not None:
            watcher.join(timeout=3)


if __name__ == "__main__":
    main()
