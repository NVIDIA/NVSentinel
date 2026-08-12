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

"""Health check server that detects frozen reconciliation loops.

Serves /healthz alongside Prometheus /metrics on the same port.
Returns 503 if the reconciliation loop has not completed an iteration
within the staleness threshold. Uses time.monotonic() to avoid
false positives from NTP clock adjustments.
"""

import threading
import time
from http.server import ThreadingHTTPServer

from prometheus_client import MetricsHandler


class _last_reconcile:
    """Thread-safe tracker for the last successful reconcile timestamp.

    Uses time.monotonic() for NTP-immune elapsed time measurement.
    Module-scoped singleton — one tracker per process.
    """

    _lock = threading.Lock()
    _timestamp: float = time.monotonic()

    @classmethod
    def mark_alive(cls) -> None:
        with cls._lock:
            cls._timestamp = time.monotonic()

    @classmethod
    def seconds_since_last(cls) -> float:
        with cls._lock:
            return time.monotonic() - cls._timestamp


# Module-level staleness threshold (seconds). Set by start_server().
_staleness_threshold: float = 300.0


def mark_alive() -> None:
    """Record a successful reconcile loop iteration."""
    _last_reconcile.mark_alive()


class _HealthMetricsHandler(MetricsHandler):
    """HTTP handler serving both /metrics and /healthz."""

    def do_GET(self) -> None:
        if self.path == "/healthz":
            elapsed = _last_reconcile.seconds_since_last()
            if elapsed > _staleness_threshold:
                self.send_response(503)
                self.send_header("Content-Type", "text/plain; charset=utf-8")
                self.end_headers()
                self.wfile.write(
                    f"polling loop stale: last iteration {elapsed:.0f}s ago, "
                    f"threshold {_staleness_threshold:.0f}s\n".encode()
                )
            else:
                self.send_response(200)
                self.send_header("Content-Type", "text/plain; charset=utf-8")
                self.end_headers()
                self.wfile.write(b"ok\n")
        else:
            super().do_GET()


def start_server(port: int, staleness_seconds: float = 300.0) -> tuple[ThreadingHTTPServer, threading.Thread]:
    """Start an HTTP server serving /metrics and /healthz.

    Args:
        port: TCP port to listen on.
        staleness_seconds: Maximum seconds between reconcile iterations
            before /healthz returns 503.

    Returns:
        (ThreadingHTTPServer, daemon Thread) — same interface as
        prometheus_client.start_http_server().
    """
    global _staleness_threshold
    _staleness_threshold = staleness_seconds

    # Reset the grace period to start from server startup, not module import.
    _last_reconcile.mark_alive()

    httpd = ThreadingHTTPServer(("", port), _HealthMetricsHandler)
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()

    return httpd, t
