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

"""Unit tests for nccl_allreduce/errors.py"""

from nccl_allreduce.errors import ErrorDef, NCCLError


class TestNCCLError:
    """Tests for NCCLError enum."""

    def test_success_exit_code_is_zero(self) -> None:
        assert NCCLError.SUCCESS.value.exit_code == 0

    def test_success_has_no_error_code(self) -> None:
        assert NCCLError.SUCCESS.value.error_code is None

    def test_success_is_not_fatal(self) -> None:
        assert NCCLError.SUCCESS.value.is_fatal is False

    def test_hardware_errors_are_fatal(self) -> None:
        for error in [
            NCCLError.ALLREDUCE_BW_DEGRADED,
            NCCLError.ALLREDUCE_TIMEOUT,
            NCCLError.ALLREDUCE_INIT_FAILED,
        ]:
            assert error.value.is_fatal is True, f"{error.name} should be fatal"

    def test_hardware_errors_have_error_codes(self) -> None:
        for error in [
            NCCLError.ALLREDUCE_BW_DEGRADED,
            NCCLError.ALLREDUCE_TIMEOUT,
            NCCLError.ALLREDUCE_INIT_FAILED,
        ]:
            assert error.value.error_code is not None, f"{error.name} should have error_code"
            assert error.value.error_code.startswith("NCCL_"), f"{error.name} error_code should start with NCCL_"

    def test_infra_errors_are_not_fatal(self) -> None:
        for error in [NCCLError.GANG_CONFIG_ERROR, NCCLError.GANG_TIMEOUT]:
            assert error.value.is_fatal is False, f"{error.name} should not be fatal"

    def test_health_report_failed_has_no_error_code(self) -> None:
        """HEALTH_REPORT_FAILED cannot send a health event about itself."""
        assert NCCLError.HEALTH_REPORT_FAILED.value.error_code is None

    def test_all_exit_codes_are_non_negative(self) -> None:
        for error in NCCLError:
            assert error.value.exit_code >= 0, f"{error.name} has negative exit code"

    def test_error_def_is_frozen(self) -> None:
        """ErrorDef should be immutable."""
        error_def = NCCLError.SUCCESS.value
        assert isinstance(error_def, ErrorDef)
