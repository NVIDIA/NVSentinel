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

from unittest import mock

import pytest

from file_server_cleanup import cleanup


@pytest.mark.parametrize(
    "env_value, expected_addr",
    [
        (None, "0.0.0.0"),
        ("::", "::"),
    ],
)
def test_main_metrics_bind_address(monkeypatch: pytest.MonkeyPatch, env_value: str | None, expected_addr: str) -> None:
    if env_value is None:
        monkeypatch.delenv("CLEANUP_METRICS_ADDR", raising=False)
    else:
        monkeypatch.setenv("CLEANUP_METRICS_ADDR", env_value)

    # Fail the server start so main() returns before its endless cleanup loop.
    with mock.patch.object(cleanup, "start_http_server", side_effect=OSError("stop")) as start:
        assert cleanup.main(metrics_port=9002) == 1

    start.assert_called_once_with(9002, addr=expected_addr)
