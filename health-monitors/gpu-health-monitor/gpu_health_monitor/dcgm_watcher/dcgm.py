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
from gpu_health_monitor.metadata import MetadataReader
from threading import Event
from functools import partial
from concurrent.futures import ThreadPoolExecutor
import os

DELAY, MULTIPLIER, MAX_DELAY = 2, 1.5, 120
DCGM_4_PYTHON_PATH = "/usr/share/datacenter-gpu-manager-4/bindings/python3"
DCGM_CONNECTION_TYPE_TCP = 1


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
DCGM_FIELDS_MONITORING: dict[str, types.DCGMFieldMonitor] = {}
_gpu_temp_limit_field_id = getattr(dcgm_fields, "DCGM_FI_DEV_GPU_TEMP_TLIMIT", None)
if _gpu_temp_limit_field_id is not None:
    DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"] = types.DCGMFieldMonitor(
        field_id=_gpu_temp_limit_field_id,
        watch_name="DCGM_HEALTH_WATCH_THERMAL_MARGIN",
        violation_code="GPU_TEMP_HW_SLOWDOWN_VIOLATION",
    )


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
        self._metadata_reader = metadata_reader
        self._field_group = None
        self._dcgm_mode = dcgm_mode

        self._health_watches = self._get_available_health_watches()
        log.debug(f"Got available health watches {self._health_watches}")
        metrics.num_health_watches.set(len(self._health_watches))

        self._error_codes = self._get_available_error_codes()
        log.debug(f"Got available error codes {self._error_codes}")

        self._callback_thread_pool = ThreadPoolExecutor()
        self._dcgm_k8s_service_enabled = dcgm_k8s_service_enabled

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

                health_status[watch_name].status = types.HealthStatus(int(incident.health))
                gpu_id = incident.entityInfo.entityId
                fallback_error_code = self._error_codes.get(dcgm_errors.DCGM_FR_UNKNOWN, "DCGM_FR_UNKNOWN")
                error_code = self._error_codes.get(incident.error.code, fallback_error_code)
                if error_code == fallback_error_code:
                    log.warning(f"Unknown DCGM error code {incident.error.code} for entity {gpu_id}")
                error_msg = incident.error.msg

                log.debug(f"incident.error.code is {incident.error.code} and error msg is {error_msg}")

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

            if self._thermal_margin_enabled and self._metadata_reader is not None:
                self._field_group = pydcgm.DcgmFieldGroup(
                    dcgm_handle, "gpu_temp_limit", [DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"].field_id]
                )
                update_freq_usec = self._poll_interval_seconds * 1_000_000
                # We only read GetLatest, so retain a single most-recent field-153
                # sample: max_keep_age=0.0 (no time bound), max_keep_samples=1.
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
                    "Watching DCGM field %s (GPU T.Limit) for %s at %ss interval",
                    DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"].field_id,
                    DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"].watch_name,
                    self._poll_interval_seconds,
                )
            elif self._thermal_margin_enabled:
                log.warning("GpuThermalMarginWatch enabled but no metadata reader configured; skipping field watch")

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
    ):
        """Clean up DCGM resources safely.

        Group deletion and handle shutdown are in separate try blocks so that
        a failure in Delete() does not prevent Shutdown() from running.
        """
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

        # Initial DCGM handle and monitoring setup
        try:
            while not exit.is_set():
                # Wait for poll interval to allow DCGM initialization
                log.debug("Waiting till next cycle")
                if exit.wait(self._poll_interval_seconds):
                    break

                with metrics.overall_reconcile_loop_time.time():
                    if dcgm_handle is None:
                        try:
                            dcgm_handle = self._get_dcgm_handle()
                            if dcgm_handle is None:
                                self._fire_callback_funcs(types.CallbackInterface.dcgm_connectivity_failed.__name__, [])
                                self._cleanup_dcgm_resources(dcgm_group, dcgm_handle)
                                continue
                            dcgm_group, gpu_ids, _gpu_serials = self._initialize_dcgm_monitoring(dcgm_handle)
                        except Exception as e:
                            log.error(f"Error getting DCGM handle: {e}")
                            self._fire_callback_funcs(types.CallbackInterface.dcgm_connectivity_failed.__name__, [])
                            self._cleanup_dcgm_resources(dcgm_group, dcgm_handle)
                            dcgm_handle = None
                            dcgm_group = None
                            gpu_ids = []
                    else:
                        log.debug("Running health check")
                        health_status, connectivity_success = self._perform_health_check(dcgm_group)

                        if not connectivity_success:
                            log.warning("DCGM connectivity failure detected")
                            self._cleanup_dcgm_resources(dcgm_group, dcgm_handle)
                            self._fire_callback_funcs(types.CallbackInterface.dcgm_connectivity_failed.__name__, [])
                            dcgm_handle = None
                            dcgm_group = None
                            gpu_ids = []
                        else:
                            margin_details = self._evaluate_gpu_thermal_margin(dcgm_group, gpu_ids)
                            if margin_details is not None:
                                health_status[DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"].watch_name] = (
                                    margin_details
                                )
                            self._suppress_configured_error_codes(health_status)
                            log.debug("Publish DCGM health checks")
                            self._fire_callback_funcs(
                                types.CallbackInterface.health_event_occurred.__name__,
                                [health_status, gpu_ids],
                            )
        finally:
            # Shutdown() stops the embedded hostengine and its loopback server.
            try:
                self._cleanup_dcgm_resources(dcgm_group, dcgm_handle)
            finally:
                self._callback_thread_pool.shutdown(cancel_futures=True)
