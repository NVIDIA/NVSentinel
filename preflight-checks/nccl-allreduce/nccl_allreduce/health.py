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

"""Health event reporting to Platform Connector."""

import logging
from time import sleep

import grpc
from google.protobuf.timestamp_pb2 import Timestamp

from .errors import NCCLError
from .protos import health_event_pb2 as pb
from .protos import health_event_pb2_grpc as pb_grpc

log = logging.getLogger(__name__)

MAX_RETRIES = 5
INITIAL_DELAY = 2.0
BACKOFF_FACTOR = 1.5
RPC_TIMEOUT = 30.0


class HealthReporter:
    """Reports health events to the Platform Connector."""

    AGENT = "preflight-nccl-allreduce"
    COMPONENT_CLASS = "Node"
    CHECK_NAME = "NCCLAllReduceTest"

    def __init__(
        self,
        socket_path: str,
        node_name: str,
        processing_strategy: int,
    ) -> None:
        """Initialize the reporter.

        Args:
            socket_path: Unix socket path for Platform Connector.
            node_name: Kubernetes node name for health events.
            processing_strategy: ProcessingStrategy enum value.
        """
        self._socket_path = socket_path.removeprefix("unix://")
        self._node_name = node_name
        self._processing_strategy = processing_strategy

    def send_success(self, message: str) -> None:
        """Send a successful health event.

        Args:
            message: Success message describing the result.

        Raises:
            RuntimeError: If the event cannot be sent after retries.
        """
        event = self._build_event(
            is_healthy=True,
            is_fatal=False,
            message=message,
            recommended_action=pb.RecommendedAction.NONE,
            error_code=None,
        )
        self._send(event)

    def send_failure(self, error: NCCLError, message: str) -> None:
        """Send a failure health event.

        Args:
            error: The NCCL error that occurred.
            message: Error message describing the failure.

        Raises:
            RuntimeError: If the event cannot be sent after retries.
            ValueError: If the error type doesn't support health events.
        """
        error_def = error.value

        if error_def.error_code is None:
            raise ValueError(f"Error {error.name} does not generate health events")

        event = self._build_event(
            is_healthy=False,
            is_fatal=error_def.is_fatal,
            message=message,
            recommended_action=error_def.recommended_action,
            error_code=error_def.error_code,
        )
        self._send(event)

    def _build_event(
        self,
        is_healthy: bool,
        is_fatal: bool,
        message: str,
        recommended_action: int,
        error_code: str | None,
    ) -> pb.HealthEvent:
        """Build a health event protobuf message.

        Args:
            is_healthy: Whether the check passed.
            is_fatal: Whether the failure is fatal.
            message: Event message.
            recommended_action: RecommendedAction enum value.
            error_code: Error code mnemonic (None for success).

        Returns:
            HealthEvent protobuf message.
        """
        timestamp = Timestamp()
        timestamp.GetCurrentTime()

        error_codes = [error_code] if error_code else []

        return pb.HealthEvent(
            version=1,
            agent=self.AGENT,
            componentClass=self.COMPONENT_CLASS,
            checkName=self.CHECK_NAME,
            isFatal=is_fatal,
            isHealthy=is_healthy,
            message=message,
            recommendedAction=recommended_action,
            errorCode=error_codes,
            entitiesImpacted=[],
            generatedTimestamp=timestamp,
            nodeName=self._node_name,
            processingStrategy=self._processing_strategy,
        )

    def _send(self, event: pb.HealthEvent) -> None:
        """Send a health event with retries.

        Args:
            event: The health event to send.

        Raises:
            RuntimeError: If the event cannot be sent after retries.
        """
        health_events = pb.HealthEvents(version=1, events=[event])

        log.info(
            "Sending health event",
            extra={
                "is_healthy": event.isHealthy,
                "is_fatal": event.isFatal,
                "error_code": event.errorCode[0] if event.errorCode else None,
                "recommended_action": pb.RecommendedAction.Name(event.recommendedAction),
                "event_message": event.message,
            },
        )

        if not self._send_with_retries(health_events):
            raise RuntimeError(f"Failed to send health event after {MAX_RETRIES} retries")

    def _send_with_retries(self, health_events: pb.HealthEvents) -> bool:
        """Send health events with exponential backoff retries.

        Args:
            health_events: The health events to send.

        Returns:
            True if sent successfully, False otherwise.
        """
        delay = INITIAL_DELAY

        for attempt in range(MAX_RETRIES):
            try:
                with grpc.insecure_channel(f"unix://{self._socket_path}") as channel:
                    stub = pb_grpc.PlatformConnectorStub(channel)
                    stub.HealthEventOccurredV1(
                        health_events,
                        timeout=RPC_TIMEOUT,
                    )
                    log.info("Health event sent successfully")
                    return True
            except grpc.RpcError as err:
                log.warning(
                    "Failed to send health event",
                    extra={
                        "attempt": attempt + 1,
                        "max_retries": MAX_RETRIES,
                        "error": str(err),
                    },
                )
                if attempt < MAX_RETRIES - 1:
                    sleep(delay)
                    delay *= BACKOFF_FACTOR

        return False
