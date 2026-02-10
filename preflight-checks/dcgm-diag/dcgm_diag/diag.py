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

import logging
from dataclasses import dataclass

import dcgm_structs
import pydcgm

from .gpu import GPUDiscovery

log = logging.getLogger(__name__)


@dataclass
class DiagResult:
    test_name: str
    status: str
    gpu_index: int
    gpu_uuid: str
    error_code: int
    error_message: str


class DCGMDiagnostic:
    DIAG_LEVELS = {
        1: dcgm_structs.DCGM_DIAG_LVL_SHORT,
        2: dcgm_structs.DCGM_DIAG_LVL_MED,
        3: dcgm_structs.DCGM_DIAG_LVL_LONG,
        4: dcgm_structs.DCGM_DIAG_LVL_XLONG,
    }

    def __init__(self, hostengine_addr: str) -> None:
        self._hostengine_addr = hostengine_addr
        self._handle: pydcgm.DcgmHandle | None = None
        self._gpu_discovery = GPUDiscovery()

    def run(self, level: int) -> list[DiagResult]:
        self._connect()
        try:
            return self._run_diagnostic(level)
        finally:
            self._disconnect()

    def get_all_gpu_uuids(self) -> list[str]:
        return self._gpu_discovery.get_all_uuids()

    def _connect(self) -> None:
        log.info(f"Connecting to DCGM hostengine at {self._hostengine_addr}")
        self._handle = pydcgm.DcgmHandle(
            ipAddress=self._hostengine_addr,
            opMode=dcgm_structs.DCGM_OPERATION_MODE_AUTO,
        )

    def _disconnect(self) -> None:
        if self._handle:
            try:
                self._handle.Shutdown()
            except Exception as e:
                log.warning(f"Failed to shutdown DCGM handle: {e}")
            self._handle = None

    def _run_diagnostic(self, level: int) -> list[DiagResult]:
        gpu_indices = self._gpu_discovery.get_allocated_gpus()
        if not gpu_indices:
            raise RuntimeError("No GPUs allocated to this container")

        log.info(f"Running DCGM diagnostic level={level} gpus={gpu_indices}")

        group = self._create_gpu_group(gpu_indices)
        try:
            diag_level = self.DIAG_LEVELS.get(level, dcgm_structs.DCGM_DIAG_LVL_SHORT)
            response = group.action.RunDiagnostic(diag_level)
            return self._parse_response(response, gpu_indices)
        finally:
            group.Delete()

    def _create_gpu_group(self, gpu_indices: list[int]) -> pydcgm.DcgmGroup:
        group = pydcgm.DcgmGroup(
            self._handle,
            groupName="nvsentinel-preflight-diag",
            groupType=dcgm_structs.DCGM_GROUP_EMPTY,
        )
        for idx in gpu_indices:
            group.AddGpu(idx)
        return group

    def _parse_response(
        self, response: dcgm_structs.c_dcgmDiagResponse_v12, gpu_indices: list[int]
    ) -> list[DiagResult]:
        """Parse DCGM v12 diagnostic response structure.

        v12 structure:
        - tests[0..numTests-1]: c_dcgmDiagTestRun_v2 with name, result, resultIndices
        - results[0..numResults-1]: c_dcgmDiagEntityResult_v1 with entity.entityId, result, testId
        - errors[0..numErrors-1]: c_dcgmDiagError_v1 with entity, msg, testId
        """
        results = []
        gpu_set = set(gpu_indices)

        error_lookup: dict[tuple[int, int], tuple[int, str]] = {}
        for i in range(response.numErrors):
            err = response.errors[i]
            key = (err.testId, err.entity.entityId)
            error_lookup[key] = (err.code, self._decode_string(err.msg))

        for test_idx in range(response.numTests):
            test = response.tests[test_idx]
            test_name = self._decode_string(test.name)

            for j in range(test.numResults):
                result_idx = test.resultIndices[j]
                entity_result = response.results[result_idx]
                gpu_idx = entity_result.entity.entityId

                if gpu_idx not in gpu_set:
                    continue

                status = self._status_to_string(entity_result.result)
                error_code, error_msg = error_lookup.get((entity_result.testId, gpu_idx), (0, ""))

                results.append(
                    DiagResult(
                        test_name=test_name,
                        status=status,
                        gpu_index=gpu_idx,
                        gpu_uuid=self._gpu_discovery.get_uuid(gpu_idx),
                        error_code=error_code,
                        error_message=error_msg,
                    )
                )

        return results

    @staticmethod
    def _decode_string(value) -> str:
        if isinstance(value, bytes):
            return value.decode("utf-8").strip("\x00")
        if isinstance(value, str):
            return value.strip("\x00")
        return str(value) if value else ""

    @staticmethod
    def _status_to_string(status: int) -> str:
        status_map = {
            dcgm_structs.DCGM_DIAG_RESULT_PASS: "pass",
            dcgm_structs.DCGM_DIAG_RESULT_SKIP: "skip",
            dcgm_structs.DCGM_DIAG_RESULT_WARN: "warn",
            dcgm_structs.DCGM_DIAG_RESULT_FAIL: "fail",
            dcgm_structs.DCGM_DIAG_RESULT_NOT_RUN: "not_run",
        }
        return status_map.get(status, "unknown")
