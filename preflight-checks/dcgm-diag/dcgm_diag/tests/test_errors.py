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

from unittest.mock import patch

from dcgm_diag.errors import get_recommended_action, resolve_recommended_action
from dcgm_diag.protos import health_event_pb2 as pb


class TestGetRecommendedAction:
    def test_unknown_code_returns_contact_support(self) -> None:
        assert get_recommended_action(99999) == pb.RecommendedAction.CONTACT_SUPPORT


class TestResolveRecommendedAction:
    def test_healthy_returns_none(self) -> None:
        assert resolve_recommended_action(is_healthy=True, error_code=0) == pb.RecommendedAction.NONE

    def test_failure_without_code_returns_contact_support(self) -> None:
        assert resolve_recommended_action(is_healthy=False, error_code=0) == pb.RecommendedAction.CONTACT_SUPPORT

    @patch("dcgm_diag.errors.get_recommended_action", return_value=pb.RecommendedAction.NONE)
    def test_non_actionable_code_returns_none(self, _mock: object) -> None:
        # e.g. DCGM_FR_XID_ERROR maps to NONE in the CSV
        assert resolve_recommended_action(is_healthy=False, error_code=1234) == pb.RecommendedAction.NONE

    @patch("dcgm_diag.errors.get_recommended_action", return_value=pb.RecommendedAction.RESTART_VM)
    def test_actionable_code_returns_action(self, _mock: object) -> None:
        assert resolve_recommended_action(is_healthy=False, error_code=1234) == pb.RecommendedAction.RESTART_VM
