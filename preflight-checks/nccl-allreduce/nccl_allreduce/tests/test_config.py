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

from nccl_allreduce.config import (
    DEFAULT_PUBLISH_RETRY_WINDOW_SECONDS,
    Config,
    DirectPublisherConfig,
    _parse_float,
    _parse_int,
)


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

    def test_token_path_is_none_when_unset(self, base_env: dict[str, str]) -> None:
        with patch.dict(os.environ, base_env, clear=True):
            assert Config.from_env().token_path is None

    def test_token_path_loaded_from_env(self, base_env: dict[str, str]) -> None:
        env = {**base_env, "PLATFORM_CONNECTOR_TOKEN_PATH": "/var/run/secrets/nvsentinel/token"}
        with patch.dict(os.environ, env, clear=True):
            assert Config.from_env().token_path == "/var/run/secrets/nvsentinel/token"

    def test_empty_token_path_is_treated_as_unset(self, base_env: dict[str, str]) -> None:
        env = {**base_env, "PLATFORM_CONNECTOR_TOKEN_PATH": ""}
        with patch.dict(os.environ, env, clear=True):
            assert Config.from_env().token_path is None

    def test_publish_is_none_without_a_target(self, base_env: dict[str, str]) -> None:
        with patch.dict(os.environ, base_env, clear=True):
            assert Config.from_env().publish is None

    def test_publish_loaded_from_env(self, base_env: dict[str, str]) -> None:
        env = {
            **base_env,
            "HEALTH_PUBLISH_TARGET": "platform-connector-deployment.nvsentinel.svc:50051",
            "HEALTH_PUBLISH_INSECURE": "true",
            "HEALTH_PUBLISH_TOKEN_PATH": "/var/run/secrets/nvsentinel/token",
        }
        with patch.dict(os.environ, env, clear=True):
            cfg = Config.from_env()

        assert cfg.publish is not None
        assert cfg.publish.target == "platform-connector-deployment.nvsentinel.svc:50051"
        assert cfg.publish.token_path == "/var/run/secrets/nvsentinel/token"

    def test_invalid_publish_env_is_a_config_error(self, base_env: dict[str, str]) -> None:
        """A set target with a missing companion setting must not fall back to the socket."""
        env = {
            **base_env,
            "HEALTH_PUBLISH_TARGET": "platform-connector-deployment.nvsentinel.svc:50051",
            "HEALTH_PUBLISH_INSECURE": "true",
        }
        with patch.dict(os.environ, env, clear=True):
            with pytest.raises(ValueError, match="HEALTH_PUBLISH_TOKEN_PATH is required"):
                Config.from_env()


class TestDirectPublisherConfigFromEnv:
    """HEALTH_PUBLISH_* parsing, shared with the Go client and the gpu health monitor."""

    FULL_ENV = {
        "HEALTH_PUBLISH_TARGET": "platform-connector-deployment.nvsentinel.svc.cluster.local:50051",
        "HEALTH_PUBLISH_TLS_CA_FILE": "/etc/nvsentinel/platform-connector-deployment-ca/ca.crt",
        "HEALTH_PUBLISH_TLS_SERVER_NAME": "platform-connector-deployment.nvsentinel.svc",
        "HEALTH_PUBLISH_TOKEN_PATH": "/var/run/secrets/nvsentinel/token",
        "HEALTH_PUBLISH_RETRY_WINDOW": "1m30s",
    }

    def test_unset_target_means_none(self) -> None:
        assert DirectPublisherConfig.from_env({}) is None

    @pytest.mark.parametrize("target", ["", "   "])
    def test_blank_target_means_none(self, target: str) -> None:
        env = {**self.FULL_ENV, "HEALTH_PUBLISH_TARGET": target}
        assert DirectPublisherConfig.from_env(env) is None

    def test_full_env(self) -> None:
        cfg = DirectPublisherConfig.from_env(self.FULL_ENV)

        assert cfg == DirectPublisherConfig(
            target="platform-connector-deployment.nvsentinel.svc.cluster.local:50051",
            insecure=False,
            ca_file="/etc/nvsentinel/platform-connector-deployment-ca/ca.crt",
            server_name_override="platform-connector-deployment.nvsentinel.svc",
            token_path="/var/run/secrets/nvsentinel/token",
            retry_window_seconds=90.0,
        )

    def test_optional_settings_default(self) -> None:
        env = {
            "HEALTH_PUBLISH_TARGET": "host:50051",
            "HEALTH_PUBLISH_TLS_CA_FILE": "/ca.crt",
            "HEALTH_PUBLISH_TOKEN_PATH": "/token",
        }
        cfg = DirectPublisherConfig.from_env(env)

        assert cfg.insecure is False
        assert cfg.server_name_override is None
        assert cfg.retry_window_seconds == DEFAULT_PUBLISH_RETRY_WINDOW_SECONDS

    def test_ca_file_required_unless_insecure(self) -> None:
        env = {"HEALTH_PUBLISH_TARGET": "host:50051", "HEALTH_PUBLISH_TOKEN_PATH": "/token"}
        with pytest.raises(ValueError, match="HEALTH_PUBLISH_TLS_CA_FILE is required"):
            DirectPublisherConfig.from_env(env)

        cfg = DirectPublisherConfig.from_env({**env, "HEALTH_PUBLISH_INSECURE": "true"})
        assert cfg.insecure is True
        assert cfg.ca_file is None

    def test_ca_file_wins_over_insecure(self) -> None:
        env = {**self.FULL_ENV, "HEALTH_PUBLISH_INSECURE": "true"}
        cfg = DirectPublisherConfig.from_env(env)

        assert cfg.insecure is True
        assert cfg.ca_file == "/etc/nvsentinel/platform-connector-deployment-ca/ca.crt"

    @pytest.mark.parametrize("token_path", [None, "", "  "])
    def test_token_path_required(self, token_path: str | None) -> None:
        env = {**self.FULL_ENV}
        del env["HEALTH_PUBLISH_TOKEN_PATH"]
        if token_path is not None:
            env["HEALTH_PUBLISH_TOKEN_PATH"] = token_path
        with pytest.raises(ValueError, match="HEALTH_PUBLISH_TOKEN_PATH is required"):
            DirectPublisherConfig.from_env(env)

    @pytest.mark.parametrize(
        ("raw", "expected"),
        [("1", True), ("t", True), ("TRUE", True), (" true ", True), ("0", False), ("f", False), ("False", False)],
    )
    def test_insecure_parsed_like_go(self, raw: str, expected: bool) -> None:
        env = {**self.FULL_ENV, "HEALTH_PUBLISH_INSECURE": raw}
        assert DirectPublisherConfig.from_env(env).insecure is expected

    @pytest.mark.parametrize("raw", ["yes", "on", "2", "enabled"])
    def test_bad_boolean_is_rejected(self, raw: str) -> None:
        env = {**self.FULL_ENV, "HEALTH_PUBLISH_INSECURE": raw}
        with pytest.raises(ValueError, match="HEALTH_PUBLISH_INSECURE"):
            DirectPublisherConfig.from_env(env)

    @pytest.mark.parametrize(
        ("raw", "expected"),
        [("5m", 300.0), ("90s", 90.0), ("1m30s", 90.0), ("500ms", 0.5), ("1h", 3600.0), ("0.5m", 30.0)],
    )
    def test_retry_window_parsed_like_go(self, raw: str, expected: float) -> None:
        env = {**self.FULL_ENV, "HEALTH_PUBLISH_RETRY_WINDOW": raw}
        assert DirectPublisherConfig.from_env(env).retry_window_seconds == pytest.approx(expected)

    @pytest.mark.parametrize("raw", ["300", "5 m", "-5m", "5x", "abc"])
    def test_bad_retry_window_is_rejected(self, raw: str) -> None:
        env = {**self.FULL_ENV, "HEALTH_PUBLISH_RETRY_WINDOW": raw}
        with pytest.raises(ValueError, match="duration"):
            DirectPublisherConfig.from_env(env)

    def test_zero_retry_window_is_rejected(self) -> None:
        env = {**self.FULL_ENV, "HEALTH_PUBLISH_RETRY_WINDOW": "0s"}
        with pytest.raises(ValueError, match="must be a positive duration"):
            DirectPublisherConfig.from_env(env)

    def test_blank_retry_window_means_default(self) -> None:
        env = {**self.FULL_ENV, "HEALTH_PUBLISH_RETRY_WINDOW": "  "}
        assert DirectPublisherConfig.from_env(env).retry_window_seconds == DEFAULT_PUBLISH_RETRY_WINDOW_SECONDS
