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

from .errors import get_error_name, resolve_recommended_action
from .protos import health_event_pb2 as pb
from .protos import health_event_pb2_grpc as pb_grpc

log = logging.getLogger(__name__)

MAX_RETRIES = 5
INITIAL_DELAY = 2.0
BACKOFF_FACTOR = 1.5
RPC_TIMEOUT = 30.0
# Only transport-level failures are worth another attempt. Every other status
# is a deterministic verdict from platform-connector (PERMISSION_DENIED,
# UNAUTHENTICATED, INVALID_ARGUMENT, ...) that will come back identical on the
# next attempt, so retrying it only delays the workload behind this preflight
# check without changing the outcome.
RETRYABLE_STATUS_CODES = frozenset(
    {
        grpc.StatusCode.UNAVAILABLE,
        grpc.StatusCode.DEADLINE_EXCEEDED,
    }
)


def _rpc_status_code(error: grpc.RpcError) -> grpc.StatusCode | None:
    """The status code carried by a gRPC failure, or None when it carries none.

    Failures raised by a live channel are ``grpc.Call`` instances and always
    carry a code. A bare ``grpc.RpcError`` does not; it expresses no verdict
    either way, so callers keep treating it as retryable.
    """
    code_getter = getattr(error, "code", None)
    if not callable(code_getter):
        return None
    code = code_getter()
    return code if isinstance(code, grpc.StatusCode) else None


class HealthReporter:
    AGENT = "preflight-dcgm-diag"
    COMPONENT_CLASS = "GPU"
    CHECK_NAME_PREFIX = "DcgmDiagnostic"

    def __init__(
        self,
        socket_path: str,
        node_name: str,
        processing_strategy: pb.ProcessingStrategy,
        token_path: str | None = None,
    ) -> None:
        self._socket_path = socket_path.removeprefix("unix://")
        self._node_name = node_name
        self._processing_strategy = processing_strategy
        # Projected ServiceAccount token presented as a bearer credential on
        # every send, used exactly as given. The config layer already resolves
        # PLATFORM_CONNECTOR_TOKEN_PATH; resolving it again here would let
        # ambient process state override an explicitly empty argument.
        self._token_path = token_path

    def send_event(
        self,
        gpu_uuid: str,
        is_healthy: bool,
        is_fatal: bool,
        message: str,
        error_code: int = 0,
        error_code_name: str = "",
        test_name: str = "",
        recommended_action: int | None = None,
    ) -> None:
        """Send a single health event for one GPU.

        ``is_fatal`` is emitted as given; the caller is responsible for deciding
        fatality. The recommended action shown in the event is resolved from the
        result so it stays consistent with that decision.
        """
        # DCGM_ST_* execution/status failures do not carry a DCGM_FR_* diagnostic
        # code, so callers may pass the non-actionable recommendation explicitly.
        if recommended_action is None:
            recommended_action = resolve_recommended_action(is_healthy, error_code)

        # checkName: "DcgmDiagnostic" or "DcgmDiagnosticMemory" if test_name specified
        check_name = (
            f"{self.CHECK_NAME_PREFIX}{self._to_camel_case(test_name)}" if test_name else self.CHECK_NAME_PREFIX
        )

        # errorCode: use mnemonic like "DCGM_FR_CUDA_API" or status like "DCGM_ST_IN_USE".
        error_name = error_code_name or (get_error_name(error_code) if error_code else "")

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

    def _token_metadata(self) -> list[tuple[str, str]] | None:
        """Bearer-token call metadata from the projected token file, or None.

        The kubelet rewrites the projected token file periodically, so the file
        is re-read on every call rather than cached. When a token path is
        configured but unreadable, this raises RuntimeError rather than sending
        without one: a reporter configured to present a token must not silently
        fall back to publishing anonymously.
        """
        if not self._token_path:
            return None
        try:
            with open(self._token_path) as token_file:
                token = token_file.read()
        except OSError as e:
            log.error("Failed to read platform-connector token from %s: %s", self._token_path, e)
            # Raised as RuntimeError because that is the failure mode the public
            # send methods document and the only one callers catch. Letting OSError
            # escape would bypass their handling and end the check with a traceback
            # instead of the mapped exit code.
            raise RuntimeError(f"cannot read platform-connector token from {self._token_path}: {e}") from e
        # An empty file is a broken mount, not a credential. Sending "Bearer "
        # gets a generic authentication error back from the server and sends
        # whoever debugs it looking at RBAC and audiences; failing here names
        # the actual problem.
        if not token:
            log.error("Platform-connector token file %s is empty", self._token_path)
            raise RuntimeError(f"platform-connector token file {self._token_path} is empty")
        return [("authorization", "Bearer " + token)]

    def _send_with_retries(self, health_events: pb.HealthEvents) -> bool:
        """Send health events with exponential backoff retries.

        When a token path is configured, every attempt re-reads the projected
        token file and attaches it as bearer metadata; a failed token read
        raises out of this method instead of sending without the token.

        Only ``RETRYABLE_STATUS_CODES`` are retried. Any other gRPC status is a
        deterministic rejection, so the loop stops on the first one and reports
        the failure immediately.
        """
        delay = INITIAL_DELAY

        for attempt in range(MAX_RETRIES):
            try:
                with grpc.insecure_channel(f"unix://{self._socket_path}") as channel:
                    stub = pb_grpc.PlatformConnectorStub(channel)
                    stub.HealthEventOccurredV1(health_events, timeout=RPC_TIMEOUT, metadata=self._token_metadata())
                    log.info("Health event sent successfully")
                    return True
            except grpc.RpcError as e:
                log.warning(
                    "Failed to send health event",
                    extra={"attempt": attempt + 1, "max_retries": MAX_RETRIES, "error": str(e)},
                )
                code = _rpc_status_code(e)
                if code is not None and code not in RETRYABLE_STATUS_CODES:
                    # The same request will earn the same status next time, so
                    # stop here instead of holding the workload behind the
                    # remaining backoff.
                    log.error(
                        "Platform-connector returned non-retryable status; abandoning retries",
                        extra={"status": code.name, "attempt": attempt + 1, "max_retries": MAX_RETRIES},
                    )
                    return False
                if attempt < MAX_RETRIES - 1:
                    sleep(delay)
                    delay *= BACKOFF_FACTOR

        return False
