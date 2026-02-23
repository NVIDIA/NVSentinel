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

"""Unit tests for nccl_allreduce/config.py"""

import os
from unittest.mock import patch

import pytest

from nccl_allreduce.config import Config, _parse_float, _parse_int


class TestParseFloat:
    """Tests for _parse_float helper."""

    def test_returns_default_when_unset(self) -> None:
        with patch.dict(os.environ, {}, clear=True):
            assert _parse_float("MISSING_VAR", 42.0) == 42.0

    def test_parses_valid_float(self) -> None:
        with patch.dict(os.environ, {"TEST_VAR": "3.14"}):
            assert _parse_float("TEST_VAR", 0.0) == 3.14

    def test_rejects_non_numeric(self) -> None:
        with patch.dict(os.environ, {"TEST_VAR": "abc"}):
            with pytest.raises(ValueError, match="Invalid TEST_VAR"):
                _parse_float("TEST_VAR", 0.0)

    def test_rejects_zero(self) -> None:
        with patch.dict(os.environ, {"TEST_VAR": "0"}):
            with pytest.raises(ValueError, match="must be positive"):
                _parse_float("TEST_VAR", 1.0)

    def test_rejects_negative(self) -> None:
        with patch.dict(os.environ, {"TEST_VAR": "-5.0"}):
            with pytest.raises(ValueError, match="must be positive"):
                _parse_float("TEST_VAR", 1.0)


class TestParseInt:
    """Tests for _parse_int helper."""

    def test_returns_default_when_unset(self) -> None:
        with patch.dict(os.environ, {}, clear=True):
            assert _parse_int("MISSING_VAR", 10) == 10

    def test_parses_valid_int(self) -> None:
        with patch.dict(os.environ, {"TEST_VAR": "20"}):
            assert _parse_int("TEST_VAR", 0) == 20

    def test_rejects_non_numeric(self) -> None:
        with patch.dict(os.environ, {"TEST_VAR": "xyz"}):
            with pytest.raises(ValueError, match="Invalid TEST_VAR"):
                _parse_int("TEST_VAR", 0)

    def test_rejects_zero(self) -> None:
        with patch.dict(os.environ, {"TEST_VAR": "0"}):
            with pytest.raises(ValueError, match="must be >= 1"):
                _parse_int("TEST_VAR", 1)


class TestConfigFromEnv:
    """Tests for Config.from_env()."""

    @pytest.fixture()
    def base_env(self) -> dict[str, str]:
        """Minimal valid environment for Config.from_env()."""
        return {
            "PLATFORM_CONNECTOR_SOCKET": "unix:///var/run/nvsentinel.sock",
            "NODE_NAME": "test-node",
            "POD_NAME": "test-pod-0",
            "PROCESSING_STRATEGY": "EXECUTE_REMEDIATION",
        }

    def test_loads_with_defaults(self, base_env: dict[str, str]) -> None:
        with patch.dict(os.environ, base_env, clear=True):
            cfg = Config.from_env()
            assert cfg.bw_threshold_gbps == 100.0
            assert cfg.gang_timeout_seconds == 600
            assert cfg.message_sizes == "4G,8G"
            assert cfg.benchmark_iters == 20
            assert cfg.warmup_iters == 5
            assert cfg.skip_bandwidth_check is False

    def test_loads_custom_values(self, base_env: dict[str, str]) -> None:
        env = {
            **base_env,
            "BW_THRESHOLD_GBPS": "200",
            "MESSAGE_SIZES": "1G",
            "BENCHMARK_ITERS": "10",
            "WARMUP_ITERS": "3",
            "SKIP_BANDWIDTH_CHECK": "true",
        }
        with patch.dict(os.environ, env, clear=True):
            cfg = Config.from_env()
            assert cfg.bw_threshold_gbps == 200.0
            assert cfg.message_sizes == "1G"
            assert cfg.benchmark_iters == 10
            assert cfg.warmup_iters == 3
            assert cfg.skip_bandwidth_check is True

    def test_raises_without_connector_socket(self, base_env: dict[str, str]) -> None:
        env = {**base_env}
        del env["PLATFORM_CONNECTOR_SOCKET"]
        with patch.dict(os.environ, env, clear=True):
            with pytest.raises(ValueError, match="PLATFORM_CONNECTOR_SOCKET"):
                Config.from_env()

    def test_raises_without_node_name(self, base_env: dict[str, str]) -> None:
        env = {**base_env}
        del env["NODE_NAME"]
        with patch.dict(os.environ, env, clear=True):
            with pytest.raises(ValueError, match="NODE_NAME"):
                Config.from_env()

    def test_raises_without_pod_name(self, base_env: dict[str, str]) -> None:
        env = {**base_env}
        del env["POD_NAME"]
        with patch.dict(os.environ, env, clear=True):
            with pytest.raises(ValueError, match="POD_NAME"):
                Config.from_env()

    def test_raises_with_invalid_processing_strategy(self, base_env: dict[str, str]) -> None:
        env = {**base_env, "PROCESSING_STRATEGY": "INVALID_STRATEGY"}
        with patch.dict(os.environ, env, clear=True):
            with pytest.raises(ValueError, match="Invalid PROCESSING_STRATEGY"):
                Config.from_env()
