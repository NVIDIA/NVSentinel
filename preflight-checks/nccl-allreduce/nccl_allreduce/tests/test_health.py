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

"""Unit tests for nccl_allreduce/health.py"""

import pytest

from nccl_allreduce.errors import NCCLError
from nccl_allreduce.health import HealthReporter
from nccl_allreduce.protos import health_event_pb2 as pb


@pytest.fixture()
def reporter() -> HealthReporter:
    return HealthReporter(
        socket_path="unix:///tmp/test.sock",
        node_name="test-node",
        processing_strategy=pb.ProcessingStrategy.EXECUTE_REMEDIATION,
    )


class TestBuildEvent:
    """Tests for HealthReporter event building."""

    def test_build_success_event(self, reporter: HealthReporter) -> None:
        event = reporter._build_event(
            is_healthy=True,
            is_fatal=False,
            message="Test passed",
            recommended_action=pb.RecommendedAction.NONE,
            error_code=None,
        )

        assert event.isHealthy is True
        assert event.isFatal is False
        assert event.message == "Test passed"
        assert event.agent == "preflight-nccl-allreduce"
        assert event.componentClass == "Node"
        assert event.checkName == "NCCLAllReduceTest"
        assert event.nodeName == "test-node"
        assert len(event.errorCode) == 0

    def test_build_failure_event(self, reporter: HealthReporter) -> None:
        event = reporter._build_event(
            is_healthy=False,
            is_fatal=True,
            message="BW degraded",
            recommended_action=pb.RecommendedAction.CONTACT_SUPPORT,
            error_code="NCCL_ALLREDUCE_BW_DEGRADED",
        )

        assert event.isHealthy is False
        assert event.isFatal is True
        assert event.errorCode == ["NCCL_ALLREDUCE_BW_DEGRADED"]
        assert event.recommendedAction == pb.RecommendedAction.CONTACT_SUPPORT

    def test_build_event_has_timestamp(self, reporter: HealthReporter) -> None:
        event = reporter._build_event(
            is_healthy=True,
            is_fatal=False,
            message="test",
            recommended_action=pb.RecommendedAction.NONE,
            error_code=None,
        )
        assert event.generatedTimestamp.seconds > 0

    def test_socket_path_strips_unix_prefix(self) -> None:
        r = HealthReporter(
            socket_path="unix:///var/run/nvsentinel.sock",
            node_name="node",
            processing_strategy=pb.ProcessingStrategy.EXECUTE_REMEDIATION,
        )
        assert r._socket_path == "/var/run/nvsentinel.sock"


class TestSendFailure:
    """Tests for send_failure validation."""

    def test_raises_for_error_without_error_code(self, reporter: HealthReporter) -> None:
        """Errors with no error_code (like HEALTH_REPORT_FAILED) cannot send events."""
        with pytest.raises(ValueError, match="does not generate health events"):
            reporter.send_failure(NCCLError.HEALTH_REPORT_FAILED, "test")

    def test_raises_for_success_error_code(self, reporter: HealthReporter) -> None:
        """SUCCESS has no error_code, so send_failure should reject it."""
        with pytest.raises(ValueError, match="does not generate health events"):
            reporter.send_failure(NCCLError.SUCCESS, "test")
