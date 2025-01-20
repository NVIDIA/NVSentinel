# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

XID_CALLBACK = CFUNCTYPE(None, c_void_p)


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
        self._dcgm_k8s_service_url = "nvidia-dcgm.gpu-operator.svc:5555"

    def _get_available_health_watches(self) -> dict[int, str]:
        health_watches = {}
        for var in dir(dcgm_structs):
            if (
                var.startswith("DCGM_HEALTH_WATCH")
                and not var.endswith("ALL")
                and not "_COUNT_" in var
                and not "DCGM_GROUP_MAX_ENTITIES" in var
                and not "DCGM_HEALTH_WATCH_MAX_INCIDENTS" in var
            ):
                health_watches[getattr(dcgm_structs, var)] = var
        log.info(f"dcgm_health_watches {health_watches}")
        return health_watches

    def _get_available_error_codes(self) -> dict[int, str]:
        error_codes = {}
        lines = []
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
            log.info(f"Invoking callback {func_name} on {callback.__class__.__name__}")
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

    def _perform_health_check(self, dcgm_group: pydcgm.DcgmGroup) -> dict[str, types.HealthDetails]:
        with metrics.dcgm_api_latency.labels("health_check").time():
            health_details = dcgm_group.health.Check()
        log.info(f"initial health status is {health_details}")

        health_status = self._get_health_status_dict()
        for i in range(health_details.incidentCount):
            incident = health_details.incidents[i]
            health_status[self._health_watches[incident.system]].status = types.HealthStatus(int(incident.health))
            health_status[self._health_watches[incident.system]].entity_failures[incident.entityInfo.entityId] = (
                types.ErrorDetails(message=incident.error.msg, code=self._error_codes[incident.error.code])
            )
            log.debug(f"incident.error.code is {incident.error.code} and error msg is {incident.error.msg}")
        log.debug(f"filled in health details is {health_status}")
        return health_status

    def _xid_event_callback_func(self, gpu_id, data):
        callbackData = dcgm_structs.c_dcgmPolicyCallbackResponse_v1()
        memmove(addressof(callbackData), data, callbackData.FieldsSizeof())
        xid_error = int(callbackData.val.xid.errnum)
        log.info(f"detected xid error {xid_error} on {gpu_id}")
        self._fire_callback_funcs(types.CallbackInterface.xid_event_occurred.__name__, [gpu_id, xid_error])

    def _register_xid_callbacks_on_all_gpus(self, dcgm_handle: pydcgm.DcgmHandle) -> list[pydcgm.DcgmGroup]:
        dcgm_system = dcgm_handle.GetSystem()

        with metrics.dcgm_api_latency.labels("discovery_get_entity_group_entities").time():
            supported_gpus = dcgm_system.discovery.GetEntityGroupEntities(dcgm_fields.DCGM_FE_GPU, True)

        dcgm_groups = []
        # hold a reference to the xid callback functions so that it does not get garbage collected resulting in
        # segmentation fault
        self._xid_callback_funcs = []
        newPolicy = dcgm_structs.c_dcgmPolicy_v1()
        newPolicy.version = dcgm_structs.dcgmPolicy_version1
        newPolicy.condition = dcgm_structs.DCGM_POLICY_COND_XID
        newPolicy.parms[dcgm_structs.DCGM_POLICY_COND_IDX_XID].tag = 0
        newPolicy.parms[dcgm_structs.DCGM_POLICY_COND_IDX_XID].val.boolean = True

        for gpu in supported_gpus:
            dcgm_group = pydcgm.DcgmGroup(dcgm_handle, groupName="dcgm_health", groupType=dcgm_structs.DCGM_GROUP_EMPTY)
            with metrics.dcgm_api_latency.labels("group_add_gpu").time():
                dcgm_group.AddGpu(gpu)
            dcgm_group.policy.Set(newPolicy)
            log.info("setting the policy")
            log.info(f"Registering XID callback for GPU {gpu}")
            _xid_event_callback_func = XID_CALLBACK(partial(self._xid_event_callback_func, gpu))
            with metrics.dcgm_api_latency.labels("policy_register").time():
                returnVal = dcgm_group.policy.Register(
                    condition=dcgm_structs.DCGM_POLICY_COND_XID, beginCallback=_xid_event_callback_func
                )
                log.info(f"dcgm XID error  notification register with returnValue {returnVal}")
            self._xid_callback_funcs.append(_xid_event_callback_func)

            dcgm_groups.append(dcgm_group)

        return dcgm_groups

    def _unregister_xid_callbacks(self, dcgm_groups: list[pydcgm.DcgmGroup]):

        # Since there is no python SDK API to clear a dcgmi policy, hence directly using the dcgmi policy command to do
        # that. In case Python SDK API is found, we can move the below command to DCGM Api
        if self._dcgm_k8s_service_enabled:
            command = f"/bin/bash -c 'dcgmi policy --host {self._dcgm_k8s_service_url} --clear'"
        else:
            command = "/bin/bash -c 'dcgmi policy --clear'"
        try:
            subprocess.run(command, shell=True, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        except subprocess.CalledProcessError as e:
            log.fatal(f"Command failed with exit status {e.returncode} and error {e.stderr}")
        for dcgm_group in dcgm_groups:
            log.info(f"Unregistering XID callback from {dcgm_group.GetId()}")
            dcgm_group.policy.Unregister(condition=dcgm_structs.DCGM_POLICY_COND_XID)

    def start(self, fields_to_monitor: list[str], exit: Event) -> None:
        if self._dcgm_k8s_service_enabled:
            log.info(f"DCGM k8s service enabled. Using {self._dcgm_k8s_service_url}")
            dcgm_handle = pydcgm.DcgmHandle(
                ipAddress=self._dcgm_k8s_service_url, opMode=dcgm_structs.DCGM_OPERATION_MODE_AUTO
            )
        else:
            log.info(f"DCGM k8s service disabled. Using {self._addr}")
            dcgm_handle = pydcgm.DcgmHandle(ipAddress=self._addr, opMode=dcgm_structs.DCGM_OPERATION_MODE_AUTO)
        dcgm_system = dcgm_handle.GetSystem()

        dcgm_group = self._create_dcgm_group_with_all_entities(dcgm_handle)
        with metrics.dcgm_api_latency.labels("group_health_set").time():
            dcgm_group.health.Set(dcgm_structs.DCGM_HEALTH_WATCH_ALL)

        gpu_ids = dcgm_group.GetGpuIds()
        log.info(f"dcgm gpu_id are {gpu_ids}")
        dcgm_groups_with_xid_policy = self._register_xid_callbacks_on_all_gpus(dcgm_handle)
        older_field_values = {}
        while not exit.is_set():
            with metrics.overall_reconcile_loop_time.time():
                log.info("Running health check")
                health_status = self._perform_health_check(dcgm_group)
                self._fire_callback_funcs(
                    types.CallbackInterface.health_event_occurred.__name__, [health_status, gpu_ids]
                )

            log.info("Waiting till next cycle")
            exit.wait(self._poll_interval_seconds)

        self._unregister_xid_callbacks(dcgm_groups_with_xid_policy)
        self._callback_thread_pool.shutdown(cancel_futures=True)
