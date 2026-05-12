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

"""OpenTelemetry metrics for preflight checks.

Pushes metrics to the OTLP gRPC receiver (typically the Datadog Agent
on port 4317).  Reads ``OTEL_EXPORTER_OTLP_ENDPOINT`` from the
environment.  No-ops silently when the exporter is unavailable.
"""

import logging
import os

from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import (
    OTLPMetricExporter,
)
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource

log = logging.getLogger(__name__)

_DEFAULT_ENDPOINT = "http://datadog-agent.datadog.svc.cluster.local:4317"


def emit_check_result(check_name: str, passed: bool, attributes: dict[str, str] | None = None) -> None:
    """Record a preflight check result and flush to the collector.

    Creates a short-lived meter provider, records the counter, flushes,
    and shuts down.  Designed for init containers that emit one metric
    and exit.  Best-effort — never raises.
    """
    try:
        _emit(check_name, passed, attributes or {})
    except Exception:
        log.debug("Failed to emit OTel metric", exc_info=True)


def _emit(check_name: str, passed: bool, extra_attrs: dict[str, str]) -> None:
    endpoint = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", _DEFAULT_ENDPOINT)
    service_name = os.environ.get("DD_SERVICE", "nvsentinel-preflight")

    resource = Resource.create({"service.name": service_name})
    exporter = OTLPMetricExporter(endpoint=endpoint, insecure=True)
    reader = PeriodicExportingMetricReader(exporter, export_interval_millis=1000)
    provider = MeterProvider(resource=resource, metric_readers=[reader])

    try:
        meter = provider.get_meter("nvsentinel.preflight")
        counter = meter.create_counter(
            "nvsentinel.preflight.check.completed",
            description="Preflight check completions",
        )

        attrs = {"check": check_name, "result": "pass" if passed else "fail"}
        attrs.update(extra_attrs)
        counter.add(1, attrs)

        provider.force_flush(timeout_millis=5000)
    finally:
        provider.shutdown()
