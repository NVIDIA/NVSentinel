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
from time import sleep

import grpc
from google.protobuf.timestamp_pb2 import Timestamp

from .errors import get_error_name, get_recommended_action
from .protos import health_event_pb2 as pb
from .protos import health_event_pb2_grpc as pb_grpc

log = logging.getLogger(__name__)

MAX_RETRIES = 5
INITIAL_DELAY = 2.0
BACKOFF_FACTOR = 1.5
RPC_TIMEOUT = 30.0


class HealthReporter:
    AGENT = "preflight-dcgm-diag"
    COMPONENT_CLASS = "GPU"
    CHECK_NAME_PREFIX = "DcgmDiagnostic"

    def __init__(
        self,
        socket_path: str,
        node_name: str,
        processing_strategy: pb.ProcessingStrategy,
    ) -> None:
        self._socket_path = socket_path.removeprefix("unix://")
        self._node_name = node_name
        self._processing_strategy = processing_strategy

    def send_event(
        self,
        gpu_uuid: str,
        is_healthy: bool,
        is_fatal: bool,
        message: str,
        error_code: int = 0,
        test_name: str = "",
    ) -> None:
        """Send a single health event for one GPU."""
        if is_healthy:
            recommended_action = pb.RecommendedAction.NONE
        elif error_code:
            recommended_action = get_recommended_action(error_code)
        else:
            recommended_action = pb.RecommendedAction.CONTACT_SUPPORT

        # checkName: "DcgmDiagnostic" or "DcgmDiagnosticMemory" if test_name specified
        check_name = (
            f"{self.CHECK_NAME_PREFIX}{self._to_camel_case(test_name)}" if test_name else self.CHECK_NAME_PREFIX
        )

        # errorCode: use mnemonic like "DCGM_FR_CUDA_API" instead of numeric code
        error_name = get_error_name(error_code) if error_code else ""

        event = self._build_event(gpu_uuid, is_healthy, is_fatal, message, recommended_action, check_name, error_name)
        health_events = pb.HealthEvents(version=1, events=[event])

        log.info(
            "Sending health event",
            extra={
                "gpu": gpu_uuid,
                "check_name": check_name,
                "is_healthy": is_healthy,
                "is_fatal": is_fatal,
                "error_code": error_name or None,
                "recommended_action": pb.RecommendedAction.Name(recommended_action),
                "event_message": message,
            },
        )

        if not self._send_with_retries(health_events):
            raise RuntimeError(f"Failed to send health event after {MAX_RETRIES} retries")

    @staticmethod
    def _to_camel_case(text: str) -> str:
        """Convert 'memory' or 'pcie_test' to 'Memory' or 'PcieTest'."""
        return "".join(word.capitalize() for word in text.replace("-", "_").split("_"))

    def _build_event(
        self,
        gpu_uuid: str,
        is_healthy: bool,
        is_fatal: bool,
        message: str,
        recommended_action: int,
        check_name: str,
        error_name: str,
    ) -> pb.HealthEvent:
        entities = [pb.Entity(entityType="GPU_UUID", entityValue=gpu_uuid)] if gpu_uuid else []
        error_codes = [error_name] if error_name else []

        timestamp = Timestamp()
        timestamp.GetCurrentTime()

        return pb.HealthEvent(
            version=1,
            agent=self.AGENT,
            componentClass=self.COMPONENT_CLASS,
            checkName=check_name,
            isFatal=is_fatal,
            isHealthy=is_healthy,
            message=message,
            recommendedAction=recommended_action,
            errorCode=error_codes,
            entitiesImpacted=entities,
            generatedTimestamp=timestamp,
            nodeName=self._node_name,
            processingStrategy=self._processing_strategy,
        )

    def _send_with_retries(self, health_events: pb.HealthEvents) -> bool:
        delay = INITIAL_DELAY

        for attempt in range(MAX_RETRIES):
            try:
                with grpc.insecure_channel(f"unix://{self._socket_path}") as channel:
                    stub = pb_grpc.PlatformConnectorStub(channel)
                    stub.HealthEventOccurredV1(health_events, timeout=RPC_TIMEOUT)
                    log.info("Health event sent successfully")
                    return True
            except grpc.RpcError as e:
                log.warning(
                    "Failed to send health event",
                    extra={"attempt": attempt + 1, "max_retries": MAX_RETRIES, "error": str(e)},
                )
                if attempt < MAX_RETRIES - 1:
                    sleep(delay)
                    delay *= BACKOFF_FACTOR

        return False
