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

import dcgm_structs, dcgm_errors, dcgm_fields, dcgmvalue, pydcgm, bisect
import logging as log
from . import types, metrics
from threading import Event
from ctypes import *
from functools import partial
from concurrent.futures import ThreadPoolExecutor
import subprocess
import time
import os

DELAY, MULTIPLIER, MAX_DELAY = 2, 1.5, 120
DCGM_4_PYTHON_PATH = "/usr/share/datacenter-gpu-manager-4/bindings/python3"


class DCGMWatcher:
    def __init__(
        self,
        addr: str,
        poll_interval_seconds: int,
        callbacks: list[types.CallbackInterface],
        dcgm_k8s_service_enabled: bool,
    ) -> None:
        self._addr = addr
        self._poll_interval_seconds = poll_interval_seconds
        self._callbacks = callbacks

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

    def _get_dcgm_handle(self) -> pydcgm.DcgmHandle:

        try:
            if self._dcgm_k8s_service_enabled:
                log.info(f"DCGM k8s service enabled. Using {self._addr}")
            else:
                log.info(f"DCGM k8s service disabled. Using {self._addr}")
            dcgm_handle = pydcgm.DcgmHandle(ipAddress=self._addr, opMode=dcgm_structs.DCGM_OPERATION_MODE_AUTO)
            log.info("Successfully created DCGM handle")
            return dcgm_handle
        except Exception as e:
            log.error(f"Error creating DCGM handle: {e}")
            metrics.dcgm_api_failures.labels("ErrorInitDCGMHandle").inc()
            return None

    def _initialize_dcgm_monitoring(self, dcgm_handle: pydcgm.DcgmHandle) -> tuple:
        """Initialize DCGM monitoring components.

        Returns:
            A tuple of (dcgm_group, gpu_ids, gpu_serials)
        """
        dcgm_group = self._create_dcgm_group_with_all_entities(dcgm_handle)
        with metrics.dcgm_api_latency.labels("group_health_set").time():
            dcgm_group.health.Set(dcgm_structs.DCGM_HEALTH_WATCH_ALL)

        gpu_ids = dcgm_group.GetGpuIds()
        gpu_serials = self._get_gpu_serial_numbers(dcgm_handle)
        log.info(f"dcgm gpu_id are {gpu_ids}")

        return dcgm_group, gpu_ids, gpu_serials

    def _cleanup_dcgm_resources(
        self,
        dcgm_group: pydcgm.DcgmGroup,
        dcgm_handle: pydcgm.DcgmHandle,
    ):
        """Clean up DCGM resources safely."""
        try:
            if dcgm_group:
                dcgm_group.Delete()
                dcgm_group = None
            if dcgm_handle:
                # Clean up the handle
                dcgm_handle.Shutdown()
                del dcgm_handle
        except Exception as e:
            log.error(f"Error cleaning up DCGM handle: {e}")

    def start(self, fields_to_monitor: list[str], exit: Event) -> None:
        dcgm_handle = None
        dcgm_group = None
        gpu_ids = []

        # Initial DCGM handle and monitoring setup
        while not exit.is_set():
            with metrics.overall_reconcile_loop_time.time():
                if dcgm_handle is None:
                    try:
                        dcgm_handle = self._get_dcgm_handle()
                        dcgm_group, gpu_ids, gpu_serials = self._initialize_dcgm_monitoring(dcgm_handle)
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
                        dcgm_handle = None
                        dcgm_group = None
                        gpu_ids = []
                    else:
                        log.debug("Publish DCGM health checks")
                        self._fire_callback_funcs(
                            types.CallbackInterface.health_event_occurred.__name__,
                            [health_status, gpu_ids],
                        )

            log.debug("Waiting till next cycle")
            exit.wait(self._poll_interval_seconds)

        # Cleanup on exit
        self._cleanup_dcgm_resources(dcgm_group, dcgm_handle)

        self._callback_thread_pool.shutdown(cancel_futures=True)
