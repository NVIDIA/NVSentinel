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

import dcgm_agent, dcgm_structs, dcgm_errors, dcgm_fields, dcgmvalue, pydcgm, bisect
import logging as log
from . import types, metrics
from gpu_health_monitor.metadata import MetadataReader, NVLinkDownExpectation
from threading import Event, Lock, Thread
from functools import partial
from concurrent.futures import ThreadPoolExecutor
from gpu_health_monitor.healthz import mark_alive as _mark_alive
from collections.abc import Callable, Iterator
import contextlib
from contextlib import AbstractContextManager
import os
import time

DELAY, MULTIPLIER, MAX_DELAY = 2, 1.5, 120
DCGM_4_PYTHON_PATH = "/usr/share/datacenter-gpu-manager-4/bindings/python3"
DCGM_CONNECTION_TYPE_TCP = 1

# How often the watchdog thread checks for an overdue probe.
PROBE_WATCHDOG_INTERVAL_SECONDS = 1.0


def _run_dcgm_server(port: int, bind_address: str) -> None:
    """Expose the embedded hostengine over TCP for local DCGM clients.

    DCGM 4.x ships dcgmServerRun in its Python bindings. DCGM 3.3.7 exports
    the same dcgmEngineRun API but does not include that Python wrapper.
    """
    server_run = getattr(dcgm_agent, "dcgmServerRun", None)
    if server_run is not None:
        server_run(port, bind_address, DCGM_CONNECTION_TYPE_TCP)
        return

    fn = dcgm_agent.dcgmFP("dcgmEngineRun")
    ret = fn(port, bind_address.encode("utf-8"), DCGM_CONNECTION_TYPE_TCP)
    dcgm_structs._dcgmCheckReturn(ret)


# Registry of DCGM field monitors, keyed by config key.
# To add a new field monitor:
# 1. Add a new entry here (key = config key from [dcgmfieldsmonitoring])
# 2. Add the key to configmap.yaml under [dcgmfieldsmonitoring]
# 3. Add evaluation logic in _evaluate_* method if needed
def _first_defined(module: object, *names: str) -> int | None:
    """Return the first attribute that exists on ``module``, or None.

    DCGM renamed the clocks-throttle family to "clocks event" in 4.x. Looking up
    both spellings keeps a field monitor working across that rename in either
    direction. ``getattr`` chaining with ``or`` is avoided deliberately: a
    legitimate value of 0 would be skipped.
    """
    for name in names:
        value = getattr(module, name, None)
        if value is not None:
            return value
    return None


DCGM_FIELDS_MONITORING: dict[str, types.DCGMFieldMonitor] = {}
_gpu_temp_limit_field_id = getattr(dcgm_fields, "DCGM_FI_DEV_GPU_TEMP_TLIMIT", None)
if _gpu_temp_limit_field_id is not None:
    DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"] = types.DCGMFieldMonitor(
        field_id=_gpu_temp_limit_field_id,
        watch_name="DCGM_HEALTH_WATCH_THERMAL_MARGIN",
        violation_code="GPU_TEMP_HW_SLOWDOWN_VIOLATION",
    )

_clocks_event_reasons_field_id = _first_defined(
    dcgm_fields,
    "DCGM_FI_DEV_CLOCKS_EVENT_REASONS",
    "DCGM_FI_DEV_CLOCK_THROTTLE_REASONS",
)
if _clocks_event_reasons_field_id is not None:
    DCGM_FIELDS_MONITORING["gpupowerbrakemonitoringenabled"] = types.DCGMFieldMonitor(
        field_id=_clocks_event_reasons_field_id,
        watch_name="DCGM_HEALTH_WATCH_POWER_BRAKE",
        violation_code="GPU_HW_POWER_BRAKE_VIOLATION",
    )

# An asserted external hardware power brake, i.e. the power delivery path telling
# the GPU to drop clocks. Deliberately not bit 0x04 (SW power cap), which is normal
# capping under load and carries no fault information, nor bit 0x40 (HW thermal
# slowdown), which the thermal margin monitor already covers.
HW_POWER_BRAKE_REASON_BIT = (
    _first_defined(
        dcgm_fields,
        "DCGM_CLOCKS_EVENT_REASON_HW_POWER_BRAKE",
        "DCGM_CLOCKS_THROTTLE_REASON_HW_POWER_BRAKE",
    )
    or 0x0000000000000080
)


class ProbeWatchdog:
    """Reports DCGM probes that stop returning instead of failing.

    A wedged NVIDIA driver parks its callers in uninterruptible sleep, so a
    blocked DCGM call never raises and never times out — it simply never comes
    back. The poll loop therefore cannot report its own hang: it is stuck
    before the point where it would publish anything, and the liveness probe
    only sees a frozen loop and restarts the container, which discards the
    evidence and hangs again.

    This watchdog closes that gap by treating "the probe did not return" as a
    positive finding. It runs on its own thread so the driver cannot block it.
    Delivery is retried until ``on_hang`` succeeds: a hung poll loop has no
    "next cycle" to fall back on, so a single failed publish would otherwise
    lose the event for the life of the process.

    ``on_hang`` must complete within a bounded time. ``poll_once`` holds the
    watchdog lock across the callback so a concurrent probe return cannot emit
    recovery before the unhealthy event is committed. The platform connector
    bounds critical delivery with ``CRITICAL_EVENT_DELIVERY_TIMEOUT_SECONDS``.
    """

    def __init__(self, deadline_seconds: float, on_hang: Callable[[str, float], bool | None]) -> None:
        self._deadline_seconds = deadline_seconds
        self._on_hang = on_hang
        self._lock = Lock()
        self._operation: str | None = None
        self._started_at = 0.0
        self._detected = False
        self._reported = False

    @contextlib.contextmanager
    def probe(self, operation: str) -> Iterator[None]:
        """Mark a blocking DCGM call as in flight for the duration of the block."""
        with self._lock:
            self._operation = operation
            self._started_at = time.monotonic()
            self._detected = False
            self._reported = False
        try:
            yield
        finally:
            with self._lock:
                self._operation = None

    def poll_once(self) -> bool:
        """Attempt to report an overdue probe. Returns True once delivery succeeds."""
        with self._lock:
            if self._operation is None or self._reported:
                return False
            elapsed = time.monotonic() - self._started_at
            if elapsed < self._deadline_seconds:
                return False
            operation = self._operation
            if not self._detected:
                self._detected = True
                log.error(
                    f"DCGM probe {operation} has not returned after {elapsed:.1f}s "
                    f"(deadline {self._deadline_seconds:.1f}s); treating the DCGM probe as unresponsive"
                )
                # This metric records detection, not successful event delivery.
                metrics.dcgm_probe_hangs.labels(operation).inc()

            # False means "try again later". True/None means the hang was handled
            # (published, or already cached as active). Keep the lock while the
            # bounded callback runs: if the probe returns concurrently, its
            # finally block waits here, then emits recovery only after the
            # unhealthy event is committed.
            delivered = self._on_hang(operation, elapsed)
            if delivered is False:
                log.warning(
                    f"DCGM probe {operation} unresponsive for {elapsed:.1f}s but "
                    "the health event was not published; will retry"
                )
                return False

            self._reported = True
            return True

    def run(self, exit: Event, interval_seconds: float = PROBE_WATCHDOG_INTERVAL_SECONDS) -> None:
        """Poll for overdue probes until exit is set."""
        while not exit.wait(interval_seconds):
            try:
                self.poll_once()
            except Exception as e:
                log.exception(f"Probe watchdog iteration failed: {e}")


class DCGMWatcher:
    def __init__(
        self,
        addr: str,
        poll_interval_seconds: int,
        callbacks: list[types.CallbackInterface],
        dcgm_k8s_service_enabled: bool,
        thermal_margin_enabled: bool = False,
        metadata_reader: MetadataReader | None = None,
        dcgm_mode: str = "remote",
        suppressed_error_codes: frozenset[str] | None = None,
        suppress_unbridged_pcie_nvlink_down: bool = False,
        probe_deadline_seconds: float = 0.0,
        power_brake_enabled: bool = False,
        power_brake_min_consecutive_polls: int = 1,
    ) -> None:
        self._addr = addr
        self._poll_interval_seconds = poll_interval_seconds
        self._callbacks = callbacks
        self._suppressed_error_codes = frozenset(suppressed_error_codes or ())
        if self._suppressed_error_codes:
            log.info(f"Suppressing DCGM incidents for error codes: {sorted(self._suppressed_error_codes)}")
        thermal_margin_supported = "gputemplimitmonitoringenabled" in DCGM_FIELDS_MONITORING
        self._thermal_margin_enabled = thermal_margin_enabled and thermal_margin_supported
        if thermal_margin_enabled and not thermal_margin_supported:
            log.warning(
                "GpuThermalMarginWatch requested but DCGM_FI_DEV_GPU_TEMP_TLIMIT (field 153) is unavailable; "
                "disabling the optional monitor"
            )
        power_brake_supported = "gpupowerbrakemonitoringenabled" in DCGM_FIELDS_MONITORING
        self._power_brake_enabled = power_brake_enabled and power_brake_supported
        if power_brake_enabled and not power_brake_supported:
            log.warning(
                "GpuPowerBrakeWatch requested but neither DCGM_FI_DEV_CLOCKS_EVENT_REASONS nor "
                "DCGM_FI_DEV_CLOCK_THROTTLE_REASONS is available; disabling the optional monitor"
            )
        # A brake asserted for a single poll can be a load transient. Requiring N
        # consecutive observations before failing keeps that out of the event stream
        # without hiding a sustained assertion, which is the actionable case.
        self._power_brake_min_consecutive_polls = max(1, power_brake_min_consecutive_polls)
        self._power_brake_streaks: dict[int, int] = {}
        if self._power_brake_enabled:
            log.info(
                "GpuPowerBrakeWatch enabled: field %s, bit 0x%x, %d consecutive poll(s) to fail",
                DCGM_FIELDS_MONITORING["gpupowerbrakemonitoringenabled"].field_id,
                HW_POWER_BRAKE_REASON_BIT,
                self._power_brake_min_consecutive_polls,
            )
        self._metadata_reader = metadata_reader
        self._suppress_unbridged_pcie_nvlink_down = suppress_unbridged_pcie_nvlink_down
        if suppress_unbridged_pcie_nvlink_down:
            log.info(
                "Operator opted in to suppressing DCGM_FR_NVLINK_DOWN on unbridged "
                "bridge-capable PCIe GPUs (zero active NVLink links by design)"
            )
        self._field_group = None
        self._dcgm_mode = dcgm_mode

        self._health_watches = self._get_available_health_watches()
        log.debug(f"Got available health watches {self._health_watches}")
        metrics.num_health_watches.set(len(self._health_watches))

        self._error_codes = self._get_available_error_codes()
        log.debug(f"Got available error codes {self._error_codes}")

        self._callback_thread_pool = ThreadPoolExecutor()
        self._dcgm_k8s_service_enabled = dcgm_k8s_service_enabled

        self._probe_watchdog: ProbeWatchdog | None = None
        if probe_deadline_seconds > 0:
            self._probe_watchdog = ProbeWatchdog(probe_deadline_seconds, self._report_probe_unresponsive)
            log.info(f"DCGM probe watchdog enabled with a {probe_deadline_seconds:.1f}s deadline")
        else:
            log.warning("DCGM probe watchdog disabled; a driver that hangs instead of erroring will not be reported")

    def _get_available_health_watches(self) -> dict[int, str]:
        health_watches = {}
        for var in dir(dcgm_structs):
            if (
                var.startswith("DCGM_HEALTH_WATCH")
                and not "_COUNT_" in var
                and not "DCGM_GROUP_MAX_ENTITIES" in var
                and not "DCGM_HEALTH_WATCH_MAX_INCIDENTS" in var
            ):
                health_watches[getattr(dcgm_structs, var)] = var
        log.info(f"dcgm_health_watches {health_watches}")
        return health_watches

    def _get_available_error_codes(self) -> dict[int, str]:
        error_codes = {}
        for var in dir(dcgm_errors):
            if (
                var.startswith("DCGM_FR")
                and not var.startswith("DCGM_FR_EC_")
                and not var.endswith("MSG")
                and not var.endswith("NEXT")
            ):

                val = getattr(dcgm_errors, var)
                """
                TODO : Fix it https://nvbugspro.nvidia.com/bug/4803080
                This is to handle a special case of error code DCGM_FR_PCIE_H_REPLAY_VIOLATION. What is happening here
                is error code DCGM_FR_PCIE_H_REPLAY_VIOLATION is present twice in dcgm_errors.py as seen below.
                DCGM_FR_PCIE_H_REPLAY_VIOLATION             = 98 # Host PCIe replay count violation
                DCGM_FR_PCIE_H_REPLAY_VIOLATION       = "GPU %u host-side correctable PCIe replay count violation, see dmesg for more information."
                Ideally, the second occurance should have MSG suffix appended to it. Due to this, the first occurance of
                this will be written by the second occurance. Since this comes from dcgm, hence  they should correct it.
                For the time being ignore this DCGM error  as only second occurance is getting considered which we don't
                want.This is due to the behaviour of how dictionary works in python.
                Will fix this code later.
                """
                if str(val).startswith("GPU"):
                    continue
                if str(val).startswith("(") and str(val).endswith(")"):
                    val = str(val)[1:-2]
                error_codes[int(val)] = var
        log.info(f"error_codes {error_codes}")
        return error_codes

    def _get_available_fields(self) -> dict[str, int]:
        fields = {}
        for var in dir(dcgm_fields):
            if var.startswith("DCGM_FI_DEV"):
                fields[var] = getattr(dcgm_fields, var)
        return fields

    def _get_health_status_dict(self) -> dict[str, types.HealthDetails]:
        health_status = {}
        for system_name in self._health_watches.values():
            health_status[system_name] = types.HealthDetails(status=types.HealthStatus.PASS, entity_failures={})
        return health_status

    def _suppress_configured_error_codes(self, health_status: dict[str, types.HealthDetails]) -> None:
        if not self._suppressed_error_codes:
            return

        for watch_name, details in health_status.items():
            suppressed_gpu_ids = [
                gpu_id
                for gpu_id, failure in details.entity_failures.items()
                if failure.code in self._suppressed_error_codes
            ]
            for gpu_id in suppressed_gpu_ids:
                error_code = details.entity_failures[gpu_id].code
                log.debug(
                    f"Suppressing incident for watch={watch_name} entity={gpu_id} "
                    f"error_code={error_code}: high-frequency non-actionable event"
                )
                metrics.dcgm_health_check_suppressed_incidents.labels(error_code).inc()
                del details.entity_failures[gpu_id]

            # A watch with no remaining failures is healthy again.
            if suppressed_gpu_ids and not details.entity_failures:
                details.status = types.HealthStatus.PASS

    def _is_nvlink_down_false_positive(self, watch_name: str, gpu_id: int, error_code: str) -> bool:
        """Return True when a DCGM_FR_NVLINK_DOWN incident is a false positive
        because all NVLink links down is expected steady state for this GPU.

        Two expected-down cases exist (see MetadataReader.classify_nvlink_down):
          - NO_NVLINK_HARDWARE (L40, A40): unambiguous, always suppressed.
          - UNBRIDGED_PCIE (A100/H100 PCIe with zero active links): from
            metadata alone this is indistinguishable from a card whose NVLink
            bridge was already dead at collection time, so it is suppressed
            ONLY when the operator has explicitly asserted the fleet runs
            unbridged PCIe cards (--suppress-nvlink-down-unbridged-pcie).

        The check runs per incident (not on the aggregated entity failure) so
        a genuine non-NVLINK_DOWN incident on the same GPU and watch is never
        dropped alongside the false positive.

        Fails closed: when the expectation cannot be established (no metadata
        reader, metadata unavailable, GPU not found, malformed counts, or an
        SXM system whose links may simply not have trained yet) the incident
        is never suppressed.
        """
        if watch_name != "DCGM_HEALTH_WATCH_NVLINK" or error_code != "DCGM_FR_NVLINK_DOWN":
            return False

        if self._metadata_reader is None:
            return False

        expectation = self._metadata_reader.classify_nvlink_down(gpu_id)

        if expectation is NVLinkDownExpectation.NVLINK_IN_USE:
            return False

        if expectation is NVLinkDownExpectation.UNKNOWN:
            log.warning(
                f"Cannot determine whether NVLink-down is expected for GPU {gpu_id}; "
                f"not suppressing DCGM_FR_NVLINK_DOWN"
            )
            return False

        if expectation is NVLinkDownExpectation.UNBRIDGED_PCIE and not self._suppress_unbridged_pcie_nvlink_down:
            log.warning(
                f"GPU {gpu_id} looks like an unbridged bridge-capable PCIe card (zero active NVLink "
                f"links), but suppressing DCGM_FR_NVLINK_DOWN for this case requires operator opt-in "
                f"(--suppress-nvlink-down-unbridged-pcie); not suppressing"
            )
            return False

        log.info(f"Suppressing DCGM_FR_NVLINK_DOWN for GPU {gpu_id}: {expectation.value}")
        metrics.dcgm_health_check_suppressed_incidents.labels(f"DCGM_FR_NVLINK_DOWN_{expectation.value.upper()}").inc()

        return True

    def _fire_callback_funcs(self, func_name: str, args: list[any]):
        def done_callback(class_name: str, func_name: str, future):
            e = future.exception()
            if e is not None:
                log.exception(e)
                metrics.callback_failures.labels(class_name, func_name).inc()
            else:
                metrics.callback_success.labels(class_name, func_name).inc()

        for callback in self._callbacks:
            log.debug(f"Invoking callback {func_name} on {callback.__class__.__name__}")
            self._callback_thread_pool.submit(getattr(callback, func_name), *args).add_done_callback(
                partial(done_callback, callback.__class__.__name__, func_name)
            )

    def _invoke_callback_funcs_sync(self, func_name: str, args: list[object]) -> bool:
        """Invoke critical callbacks directly, outside the shared executor.

        DCGM cleanup can block forever. Queueing a callback before cleanup is
        insufficient because slow callbacks can saturate the executor and leave
        the critical event queued until liveness kills the process.
        """
        delivered = True
        for callback in self._callbacks:
            try:
                result = getattr(callback, func_name)(*args)
                if result is False:
                    delivered = False
                    metrics.callback_failures.labels(callback.__class__.__name__, func_name).inc()
                else:
                    metrics.callback_success.labels(callback.__class__.__name__, func_name).inc()
            except Exception as e:
                delivered = False
                log.exception(e)
                metrics.callback_failures.labels(callback.__class__.__name__, func_name).inc()
        return delivered

    def _report_connectivity_failed(self) -> bool:
        """Deliver connectivity failure before entering potentially hung cleanup."""
        delivered = self._invoke_callback_funcs_sync(types.CallbackInterface.dcgm_connectivity_failed.__name__, [])
        if not delivered:
            log.warning("Failed to publish DCGM connectivity failure before cleanup; will retry on next cycle")
        return delivered

    def _report_probe_unresponsive(self, operation: str, elapsed_seconds: float) -> bool:
        """Publish the hang synchronously so the watchdog can retry on failure."""
        return self._invoke_callback_funcs_sync(
            types.CallbackInterface.dcgm_probe_unresponsive.__name__,
            [operation, elapsed_seconds, self._dcgm_mode],
        )

    def _probe(self, operation: str) -> AbstractContextManager[None]:
        """Track a blocking DCGM call, when the watchdog is enabled."""
        if self._probe_watchdog is None:
            return contextlib.nullcontext()
        return self._probe_watchdog.probe(operation)

    def _create_dcgm_group_with_all_entities(self, dcgm_handle: pydcgm.DcgmHandle) -> pydcgm.DcgmGroup:
        dcgm_system = dcgm_handle.GetSystem()

        with metrics.dcgm_api_latency.labels("discovery_get_entity_group_entities").time():
            supported_gpus = dcgm_system.discovery.GetEntityGroupEntities(dcgm_fields.DCGM_FE_GPU, True)

        log.info(f"supported gpus are {supported_gpus}")
        with metrics.dcgm_api_latency.labels("discovery_get_entity_group_entities").time():
            supported_switches = dcgm_system.discovery.GetEntityGroupEntities(dcgm_fields.DCGM_FE_SWITCH, True)
        log.info(f"supported switches are {supported_switches}")

        dcgm_group = pydcgm.DcgmGroup(dcgm_handle, groupName="dcgm_health", groupType=dcgm_structs.DCGM_GROUP_EMPTY)
        for gpu in supported_gpus:
            with metrics.dcgm_api_latency.labels("discovery_group_add_entity").time():
                dcgm_group.AddEntity(dcgm_fields.DCGM_FE_GPU, gpu)
        for switch in supported_switches:
            with metrics.dcgm_api_latency.labels("discovery_group_add_entity").time():
                dcgm_group.AddEntity(dcgm_fields.DCGM_FE_SWITCH, switch)

        return dcgm_group

    def _get_gpu_serial_numbers(self, dcgm_handle: pydcgm.DcgmHandle) -> dict[int, str]:
        dcgm_system = dcgm_handle.GetSystem()
        gpu_serials = {}

        with metrics.dcgm_api_latency.labels("discovery_get_entity_group_entities").time():
            supported_gpus = dcgm_system.discovery.GetEntityGroupEntities(dcgm_fields.DCGM_FE_GPU, True)

        # Get serial numbers for each GPU
        for gpu in supported_gpus:
            with metrics.dcgm_api_latency.labels("get_latest_values").time():
                serial = dcgm_system.discovery.GetGpuAttributes(gpu).identifiers.serial
                gpu_serials[gpu] = serial

        return gpu_serials

    def _perform_health_check(self, dcgm_group: pydcgm.DcgmGroup) -> tuple[dict[str, types.HealthDetails], bool]:
        """
        Perform DCGM health check.

        Returns:
            A tuple of (health_status, connectivity_success)
            - health_status: dict of health details for each watch
            - connectivity_success: True if DCGM connection is successful, False otherwise
        """
        try:
            with metrics.dcgm_api_latency.labels("health_check").time():
                health_details = dcgm_group.health.Check()
            log.debug(f"initial health status is {health_details}")

            health_status = self._get_health_status_dict()
            # Temporary dict to accumulate multiple failures per GPU
            gpu_failures_accumulator = {}

            log.debug(
                f"Health check returned: overallHealth={health_details.overallHealth}, "
                f"incidentCount={health_details.incidentCount}"
            )

            for i in range(health_details.incidentCount):
                incident = health_details.incidents[i]
                log.debug(
                    f"Incident[{i}]: system={incident.system} (known={incident.system in self._health_watches}), "
                    f"health={incident.health}, error.code={incident.error.code}, "
                    f"entityGroupId={incident.entityInfo.entityGroupId}, "
                    f"entityId={incident.entityInfo.entityId}, "
                    f"error.msg={incident.error.msg}"
                )

                watch_name = self._health_watches.get(incident.system)
                if watch_name is None:
                    log.warning(
                        f"Unknown health watch system value {incident.system} "
                        f"for entity {incident.entityInfo.entityId}, skipping incident"
                    )
                    metrics.dcgm_health_check_unknown_system_skipped.inc()
                    continue

                gpu_id = incident.entityInfo.entityId
                fallback_error_code = self._error_codes.get(dcgm_errors.DCGM_FR_UNKNOWN, "DCGM_FR_UNKNOWN")
                error_code = self._error_codes.get(incident.error.code, fallback_error_code)
                if error_code == fallback_error_code:
                    log.warning(f"Unknown DCGM error code {incident.error.code} for entity {gpu_id}")
                error_msg = incident.error.msg

                log.debug(f"incident.error.code is {incident.error.code} and error msg is {error_msg}")

                # Per-incident suppression: a suppressed incident must neither
                # degrade the watch status nor land in the accumulator, while
                # other incidents on the same GPU and watch are kept.
                if self._is_nvlink_down_false_positive(watch_name, gpu_id, error_code):
                    continue

                health_status[watch_name].status = types.HealthStatus(int(incident.health))

                # Create a key for accumulating failures per GPU per watch
                accumulator_key = (watch_name, gpu_id)

                if accumulator_key not in gpu_failures_accumulator:
                    gpu_failures_accumulator[accumulator_key] = {"code": error_code, "messages": []}

                # Accumulate all error messages for this GPU and watch type
                gpu_failures_accumulator[accumulator_key]["messages"].append(error_msg)

            # Now consolidate accumulated failures into health_status
            for (watch_name, gpu_id), failure_data in gpu_failures_accumulator.items():
                # Combine all messages with semicolon separator
                combined_message = "; ".join(failure_data["messages"])
                health_status[watch_name].entity_failures[gpu_id] = types.ErrorDetails(
                    message=combined_message, code=failure_data["code"]
                )

            log.debug(f"filled in health details is {health_status}")
            return health_status, True
        except dcgm_structs.DCGMError_Timeout as e:
            log.error(f"DCGM health check timed out: {e}. Indicating connectivity failure.")
            metrics.dcgm_api_failures.labels("health_check_timeout").inc()
            # Return empty health status with connectivity failure flag
            return self._get_health_status_dict(), False
        except Exception as e:
            log.error(f"Unexpected error during DCGM health check: {e}. Indicating connectivity failure.")
            metrics.dcgm_api_failures.labels("health_check_error").inc()
            # Return empty health status with connectivity failure flag
            return self._get_health_status_dict(), False

    def _evaluate_gpu_thermal_margin(
        self,
        dcgm_group: pydcgm.DcgmGroup,
        gpu_ids: list[int],
    ) -> types.HealthDetails | None:
        """Evaluate the GPU thermal margin (DCGM field 153) against the per-GPU
        HW slowdown T.Limit threshold from metadata.

        For each GPU it reads the live margin and fails ``GpuThermalMarginWatch``
        when ``margin < slowdown_threshold``; GPUs missing the threshold or a
        usable margin sample are skipped. Returns ``HealthDetails`` for the
        evaluated GPUs, or ``None`` when the watch is disabled, the field group
        is unset, or no metadata reader is configured.
        """
        if not self._thermal_margin_enabled or self._field_group is None or self._metadata_reader is None:
            return None

        monitor = DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"]
        margin_details = types.HealthDetails(status=types.HealthStatus.PASS, entity_failures={})

        try:
            with metrics.dcgm_api_latency.labels("dcgm_field_153_get_latest").time():
                field_values = dcgm_group.samples.GetLatest(self._field_group)
        except Exception as e:
            log.error("Error getting latest DCGM field 153 values for GpuThermalMarginWatch: %s", e)
            metrics.dcgm_api_failures.labels("dcgm_field_153_get_latest").inc()
            return None

        evaluated = False
        for gpu_id in gpu_ids:
            slowdown_threshold = self._metadata_reader.get_slowdown_tlimit_c(gpu_id)
            if slowdown_threshold is None:
                log.warning(
                    "GPU %s missing slowdown TLIMIT threshold metadata; GpuThermalMarginWatch not active",
                    gpu_id,
                )
                metrics.gpu_temp_limit_slowdown_threshold_missing.inc()
                continue

            field_samples = field_values.values.get(gpu_id, {}).get(monitor.field_id, [])
            if not field_samples:
                log.warning("GPU %s field 153 margin unavailable; skipping thermal margin evaluation", gpu_id)
                metrics.gpu_temp_limit_margin_blank.inc()
                continue

            raw_margin = field_samples[0].value
            try:
                margin_c = int(raw_margin)
            except (ValueError, TypeError):
                log.warning(
                    "GPU %s thermal margin value %r is not a valid integer; skipping thermal margin evaluation",
                    gpu_id,
                    raw_margin,
                )
                continue
            evaluated = True

            if margin_c < slowdown_threshold:
                log.debug(
                    "GPU %s thermal margin %s°C below HW slowdown T.Limit (slowdown=%s°C) for GpuThermalMarginWatch",
                    gpu_id,
                    margin_c,
                    slowdown_threshold,
                )
                margin_details.status = types.HealthStatus.FAIL
                margin_details.entity_failures[gpu_id] = types.ErrorDetails(
                    message=f"GPU {gpu_id} thermal margin {margin_c}°C below HW slowdown T.Limit (slowdown={slowdown_threshold}°C)",
                    code=monitor.violation_code,
                )
            else:
                log.debug(
                    "GPU %s thermal margin %s°C at or above HW slowdown T.Limit (slowdown=%s°C) for GpuThermalMarginWatch",
                    gpu_id,
                    margin_c,
                    slowdown_threshold,
                )

        if not evaluated:
            return None

        return margin_details

    def _evaluate_gpu_power_brake(
        self,
        dcgm_group: pydcgm.DcgmGroup,
        gpu_ids: list[int],
    ) -> types.HealthDetails | None:
        """Evaluate the clocks-event-reasons mask for an asserted HW power brake.

        Fails ``GpuPowerBrakeWatch`` for a GPU whose mask has
        ``HW_POWER_BRAKE_REASON_BIT`` set on at least
        ``power_brake_min_consecutive_polls`` consecutive polls. GPUs without a
        usable sample are skipped and leave their streak untouched, so a gap in
        DCGM data neither raises nor clears a finding.

        This exists because DCGM's POWER health watch does not report the brake:
        its dominant code, ``DCGM_FR_CLOCK_THROTTLE_POWER``, tracks power-capped
        clock throttling, maps to ``NONE``, and is documented as a non-actionable
        flap. A sustained brake is a power delivery fault, so it needs its own
        signal rather than sharing that one.

        Returns ``None`` when the watch is disabled, the field group is unset, or
        no GPU produced a usable sample.
        """
        if not self._power_brake_enabled or self._field_group is None:
            return None

        monitor = DCGM_FIELDS_MONITORING["gpupowerbrakemonitoringenabled"]
        brake_details = types.HealthDetails(status=types.HealthStatus.PASS, entity_failures={})

        try:
            with metrics.dcgm_api_latency.labels("dcgm_clocks_event_reasons_get_latest").time():
                field_values = dcgm_group.samples.GetLatest(self._field_group)
        except Exception as e:
            log.error("Error getting latest DCGM clocks-event-reasons values for GpuPowerBrakeWatch: %s", e)
            metrics.dcgm_api_failures.labels("dcgm_clocks_event_reasons_get_latest").inc()
            return None

        evaluated = False
        for gpu_id in gpu_ids:
            field_samples = field_values.values.get(gpu_id, {}).get(monitor.field_id, [])
            if not field_samples:
                # Debug, not warning: a GPU that never reports this field would log
                # on every poll. The counter below keeps it observable.
                log.debug("GPU %s clocks-event-reasons unavailable; skipping power brake evaluation", gpu_id)
                metrics.gpu_power_brake_reasons_blank.inc()
                continue

            raw_reasons = field_samples[0].value
            try:
                reasons_mask = int(raw_reasons)
            except (ValueError, TypeError):
                log.warning(
                    "GPU %s clocks-event-reasons value %r is not a valid integer; skipping power brake evaluation",
                    gpu_id,
                    raw_reasons,
                )
                continue

            # DCGM encodes "no data" as int64 sentinels (DCGM_INT64_BLANK and
            # friends, 0x7ffffffffffffff0..f3) whose low byte has bit 0x80 set,
            # so an unchecked blank would count as an asserted brake. Treat it
            # like a missing sample: skip, keep the streak.
            if dcgmvalue.DCGM_INT64_IS_BLANK(reasons_mask):
                # Debug, not warning: a GPU whose field is unsupported returns a
                # blank on every poll, which would flood the log. The counter keeps
                # it observable without the noise.
                log.debug(
                    "GPU %s clocks-event-reasons value is blank (0x%x); skipping power brake evaluation",
                    gpu_id,
                    reasons_mask,
                )
                metrics.gpu_power_brake_reasons_blank.inc()
                continue
            evaluated = True

            if reasons_mask & HW_POWER_BRAKE_REASON_BIT:
                streak = self._power_brake_streaks.get(gpu_id, 0) + 1
                self._power_brake_streaks[gpu_id] = streak
                if streak < self._power_brake_min_consecutive_polls:
                    log.debug(
                        "GPU %s HW power brake asserted (mask=0x%x), %d/%d consecutive polls; not failing yet",
                        gpu_id,
                        reasons_mask,
                        streak,
                        self._power_brake_min_consecutive_polls,
                    )
                    continue
                log.debug(
                    "GPU %s HW power brake asserted (mask=0x%x) for %d consecutive polls",
                    gpu_id,
                    reasons_mask,
                    streak,
                )
                brake_details.status = types.HealthStatus.FAIL
                brake_details.entity_failures[gpu_id] = types.ErrorDetails(
                    message=(
                        f"GPU {gpu_id} hardware power brake asserted for {streak} consecutive "
                        f"poll(s) (clocks event reasons mask 0x{reasons_mask:x})"
                    ),
                    code=monitor.violation_code,
                )
            else:
                self._power_brake_streaks.pop(gpu_id, None)

        if not evaluated:
            return None

        return brake_details

    def _create_dcgm_handle(self) -> pydcgm.DcgmHandle:
        if self._dcgm_mode == "local-managed":
            host, port = self._parse_local_dcgm_addr()
            log.info("Starting in-process embedded DCGM hostengine (local-managed mode)")
            dcgm_handle = pydcgm.DcgmHandle(opMode=dcgm_structs.DCGM_OPERATION_MODE_AUTO)
            try:
                _run_dcgm_server(port, host)
            except Exception as e:
                metrics.dcgm_api_failures.labels("dcgm_engine_run").inc()
                log.error("Error starting embedded DCGM hostengine: %s", e)
                dcgm_handle.Shutdown()
                raise
            log.info(f"Successfully started embedded DCGM hostengine listening on {host}:{port}")
            return dcgm_handle

        if self._dcgm_k8s_service_enabled:
            log.info(f"DCGM k8s service enabled. Using {self._addr}")
        else:
            log.info(f"DCGM k8s service disabled. Using {self._addr}")
        dcgm_handle = pydcgm.DcgmHandle(ipAddress=self._addr, opMode=dcgm_structs.DCGM_OPERATION_MODE_AUTO)
        log.info("Successfully created DCGM handle")
        return dcgm_handle

    def _parse_local_dcgm_addr(self) -> tuple[str, int]:
        if ":" not in self._addr:
            raise ValueError(f"DCGM address must be host:port, got {self._addr}")

        host, port_text = self._addr.rsplit(":", 1)
        host = host.strip("[]")
        if host == "localhost":
            host = "127.0.0.1"
        if host not in ("127.0.0.1", "::1"):
            raise ValueError(f"local-managed mode requires a loopback DCGM address, got {self._addr}")

        port = int(port_text)
        if not 1 <= port <= 65535:
            raise ValueError(f"DCGM port must be between 1 and 65535, got {port}")
        return host, port

    def _get_dcgm_handle(self) -> pydcgm.DcgmHandle | None:
        try:
            return self._create_dcgm_handle()
        except Exception as e:
            log.error(f"Error creating DCGM handle: {e}")
            metrics.dcgm_api_failures.labels("ErrorInitDCGMHandle").inc()
            return None

    def _initialize_dcgm_monitoring(self, dcgm_handle: pydcgm.DcgmHandle) -> tuple:
        """Initialize DCGM monitoring components.

        Returns:
            A tuple of (dcgm_group, gpu_ids, gpu_serials)

        If any step after group creation fails the group is deleted before the
        exception propagates so that it does not leak on the DCGM server.
        """
        dcgm_group = self._create_dcgm_group_with_all_entities(dcgm_handle)
        self._field_group = None
        try:
            with metrics.dcgm_api_latency.labels("group_health_set").time():
                dcgm_group.health.Set(dcgm_structs.DCGM_HEALTH_WATCH_ALL)

            gpu_ids = dcgm_group.GetGpuIds()
            gpu_serials = self._get_gpu_serial_numbers(dcgm_handle)
            log.info(f"dcgm gpu_id are {gpu_ids}")

            # One field group covers every enabled field monitor; each evaluator
            # reads back only its own field id from the samples.
            watched_fields: list[int] = []
            watched_descriptions: list[str] = []
            if self._thermal_margin_enabled and self._metadata_reader is not None:
                thermal_monitor = DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"]
                watched_fields.append(thermal_monitor.field_id)
                watched_descriptions.append(
                    f"{thermal_monitor.field_id} (GPU T.Limit) for {thermal_monitor.watch_name}"
                )
            elif self._thermal_margin_enabled:
                log.warning("GpuThermalMarginWatch enabled but no metadata reader configured; skipping field watch")

            if self._power_brake_enabled:
                brake_monitor = DCGM_FIELDS_MONITORING["gpupowerbrakemonitoringenabled"]
                watched_fields.append(brake_monitor.field_id)
                watched_descriptions.append(
                    f"{brake_monitor.field_id} (clocks event reasons) for {brake_monitor.watch_name}"
                )

            if watched_fields:
                self._field_group = pydcgm.DcgmFieldGroup(dcgm_handle, "nvsentinel_field_monitors", watched_fields)
                update_freq_usec = self._poll_interval_seconds * 1_000_000
                # We only read GetLatest, so retain a single most-recent sample per
                # field: max_keep_age=0.0 (no time bound), max_keep_samples=1.
                max_keep_age_seconds = 0.0
                max_keep_samples = 1
                with metrics.dcgm_api_latency.labels("field_watch_fields").time():
                    dcgm_group.samples.WatchFields(
                        self._field_group,
                        update_freq_usec,
                        max_keep_age_seconds,
                        max_keep_samples,
                    )
                log.info(
                    "Watching DCGM field(s) %s at %ss interval",
                    "; ".join(watched_descriptions),
                    self._poll_interval_seconds,
                )

            return dcgm_group, gpu_ids, gpu_serials
        except Exception as e:
            log.warning(f"DCGM monitoring initialization failed, rolling back group: {e}")
            if self._field_group is not None:
                try:
                    dcgm_group.samples.UnwatchFields(self._field_group)
                except Exception as unwatch_err:
                    log.warning(f"Error unwatching GPU temp limit field watch during rollback: {unwatch_err}")
                try:
                    self._field_group.Delete()
                except Exception as delete_err:
                    log.warning(f"Error deleting GPU temp limit field group during rollback: {delete_err}")
                self._field_group = None
            try:
                dcgm_group.Delete()
            except Exception as del_err:
                log.warning(f"Failed to delete DCGM group during init rollback: {del_err}")
                metrics.dcgm_api_failures.labels("init_group_rollback").inc()
            raise

    def _cleanup_dcgm_resources(
        self,
        dcgm_group: pydcgm.DcgmGroup,
        dcgm_handle: pydcgm.DcgmHandle,
        *,
        track_probe: bool = True,
    ):
        """Clean up DCGM resources safely.

        Group deletion and handle shutdown are in separate try blocks so that
        a failure in Delete() does not prevent Shutdown() from running. When
        ``track_probe`` is true the sequence is watchdog-tracked because every
        call reaches the driver and Shutdown() in particular can block. Intentional
        loop teardown passes ``track_probe=False`` so a slow cleanup during
        rolling upgrades cannot publish a false GpuDcgmUnresponsive.
        """
        probe = self._probe("dcgm_cleanup") if track_probe else contextlib.nullcontext()
        with probe:
            if dcgm_group and self._field_group is not None:
                try:
                    with metrics.dcgm_api_latency.labels("field_unwatch_fields").time():
                        dcgm_group.samples.UnwatchFields(self._field_group)
                except Exception as e:
                    log.warning(f"Error unwatching GPU temp limit field watch: {e}")
                    metrics.dcgm_api_failures.labels("field_unwatch_fields").inc()
                try:
                    self._field_group.Delete()
                except Exception as e:
                    log.warning(f"Error deleting GPU temp limit field group: {e}")
                    metrics.dcgm_api_failures.labels("field_group_delete_error").inc()
                self._field_group = None

            if dcgm_group:
                try:
                    dcgm_group.Delete()
                except Exception as e:
                    log.warning(f"Error deleting DCGM group (will still shut down handle): {e}")
                    metrics.dcgm_api_failures.labels("group_delete_error").inc()

            if dcgm_handle:
                dcgm_handle.Shutdown()

    def start(self, fields_to_monitor: list[str], exit: Event) -> None:
        dcgm_handle = None
        dcgm_group = None
        gpu_ids = []

        # Tied to loop teardown rather than to the process exit event: on SIGTERM
        # during a hang the loop cannot return, and the stuck probe still needs
        # reporting.
        watchdog_exit = Event()
        watchdog_thread = None
        if self._probe_watchdog is not None:
            watchdog_thread = Thread(
                target=self._probe_watchdog.run, args=(watchdog_exit,), name="dcgm-probe-watchdog", daemon=True
            )
            watchdog_thread.start()

        # Initial DCGM handle and monitoring setup
        try:
            while not exit.is_set():
                # Wait for poll interval to allow DCGM initialization
                log.debug("Waiting till next cycle")
                if exit.wait(self._poll_interval_seconds):
                    break

                with metrics.overall_reconcile_loop_time.time():
                    # Mark the loop as alive on every iteration, regardless of
                    # DCGM connectivity. The liveness probe detects a frozen
                    # loop, not a failed dependency.
                    _mark_alive()

                    if dcgm_handle is None:
                        try:
                            with self._probe("dcgm_connect"):
                                dcgm_handle = self._get_dcgm_handle()
                            if dcgm_handle is None:
                                self._report_connectivity_failed()
                                self._cleanup_dcgm_resources(dcgm_group, dcgm_handle)
                                continue
                            with self._probe("dcgm_initialize_monitoring"):
                                dcgm_group, gpu_ids, _gpu_serials = self._initialize_dcgm_monitoring(dcgm_handle)
                        except Exception as e:
                            log.error(f"Error getting DCGM handle: {e}")
                            self._report_connectivity_failed()
                            self._cleanup_dcgm_resources(dcgm_group, dcgm_handle)
                            dcgm_handle = None
                            dcgm_group = None
                            gpu_ids = []
                    else:
                        log.debug("Running health check")
                        with self._probe("dcgm_health_check"):
                            health_status, connectivity_success = self._perform_health_check(dcgm_group)

                        if not connectivity_success:
                            log.warning("DCGM connectivity failure detected")
                            # Publish before cleaning up: Shutdown() calls into
                            # DCGM as well, so an unresponsive driver would block
                            # here and the event would never be sent.
                            self._report_connectivity_failed()
                            self._cleanup_dcgm_resources(dcgm_group, dcgm_handle)
                            dcgm_handle = None
                            dcgm_group = None
                            gpu_ids = []
                        else:
                            with self._probe("dcgm_thermal_margin"):
                                margin_details = self._evaluate_gpu_thermal_margin(dcgm_group, gpu_ids)
                            if margin_details is not None:
                                health_status[DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"].watch_name] = (
                                    margin_details
                                )
                            with self._probe("dcgm_power_brake"):
                                brake_details = self._evaluate_gpu_power_brake(dcgm_group, gpu_ids)
                            if brake_details is not None:
                                health_status[DCGM_FIELDS_MONITORING["gpupowerbrakemonitoringenabled"].watch_name] = (
                                    brake_details
                                )
                            self._suppress_configured_error_codes(health_status)
                            log.debug("Publish DCGM health checks")
                            self._fire_callback_funcs(
                                types.CallbackInterface.health_event_occurred.__name__,
                                [health_status, gpu_ids],
                            )
        finally:
            # Stop the watchdog before teardown cleanup. A slow Shutdown() during
            # rolling upgrades / DCGM restarts must not publish GpuDcgmUnresponsive.
            # Mid-loop cleanups after connectivity failure remain probe-tracked.
            watchdog_exit.set()
            if watchdog_thread is not None:
                watchdog_thread.join(timeout=PROBE_WATCHDOG_INTERVAL_SECONDS * 2)
            try:
                self._cleanup_dcgm_resources(dcgm_group, dcgm_handle, track_probe=False)
            finally:
                self._callback_thread_pool.shutdown(cancel_futures=True)
