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

import pytest

from dcgm_diag.config import (
    DEFAULT_PUBLISH_RETRY_WINDOW_SECONDS,
    DEFAULT_STATUS_RETRY_MAX_ATTEMPTS,
    DEFAULT_STATUS_RETRY_INTERVAL_SECONDS,
    Config,
    DirectPublisherConfig,
)
from dcgm_diag.protos import health_event_pb2 as pb


class TestConfigFromEnv:
    def test_valid_minimal_config(self, valid_env: None) -> None:
        cfg = Config.from_env()
        assert cfg.hostengine_addr == "nvidia-dcgm.gpu-operator.svc:5555"
        assert cfg.connector_socket == "/var/run/nvsentinel.sock"
        assert cfg.node_name == "test-node"
        assert cfg.diag_level == 2
        assert cfg.processing_strategy == pb.ProcessingStrategy.Value("EXECUTE_REMEDIATION")
        assert cfg.status_retry_max_attempts == DEFAULT_STATUS_RETRY_MAX_ATTEMPTS
        assert cfg.status_retry_interval_seconds == DEFAULT_STATUS_RETRY_INTERVAL_SECONDS

    def test_all_options(self, valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("DCGM_DIAG_LEVEL", "3")
        monkeypatch.setenv("DCGM_HOSTENGINE_ADDR", "localhost:5555")
        monkeypatch.setenv("PROCESSING_STRATEGY", "STORE_ONLY")
        monkeypatch.setenv("DCGM_DIAG_STATUS_RETRY_MAX_ATTEMPTS", "12")
        monkeypatch.setenv("DCGM_DIAG_STATUS_RETRY_INTERVAL_SECONDS", "5")

        cfg = Config.from_env()
        assert cfg.diag_level == 3
        assert cfg.hostengine_addr == "localhost:5555"
        assert cfg.processing_strategy == pb.ProcessingStrategy.Value("STORE_ONLY")
        assert cfg.status_retry_max_attempts == 12
        assert cfg.status_retry_interval_seconds == 5

    def test_missing_hostengine_addr(self, clean_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("PLATFORM_CONNECTOR_SOCKET", "/sock")
        monkeypatch.setenv("NODE_NAME", "test-node")
        with pytest.raises(ValueError, match="DCGM_HOSTENGINE_ADDR is required"):
            Config.from_env()

    def test_missing_connector_socket(self, clean_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("DCGM_HOSTENGINE_ADDR", "localhost:5555")
        monkeypatch.setenv("NODE_NAME", "test-node")
        with pytest.raises(ValueError, match="PLATFORM_CONNECTOR_SOCKET is required"):
            Config.from_env()

    def test_missing_node_name(self, clean_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("DCGM_HOSTENGINE_ADDR", "localhost:5555")
        monkeypatch.setenv("PLATFORM_CONNECTOR_SOCKET", "/sock")
        with pytest.raises(ValueError, match="NODE_NAME is required"):
            Config.from_env()

    @pytest.mark.parametrize("level", ["0", "5", "-1", "99"])
    def test_invalid_diag_level(self, valid_env: None, monkeypatch: pytest.MonkeyPatch, level: str) -> None:
        monkeypatch.setenv("DCGM_DIAG_LEVEL", level)
        with pytest.raises(ValueError, match="DCGM_DIAG_LEVEL must be 1-4"):
            Config.from_env()

    @pytest.mark.parametrize("level", ["1", "2", "3", "4"])
    def test_valid_diag_levels(self, valid_env: None, monkeypatch: pytest.MonkeyPatch, level: str) -> None:
        monkeypatch.setenv("DCGM_DIAG_LEVEL", level)
        cfg = Config.from_env()
        assert cfg.diag_level == int(level)

    def test_invalid_processing_strategy(self, valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("PROCESSING_STRATEGY", "INVALID")
        with pytest.raises(ValueError, match="Invalid PROCESSING_STRATEGY"):
            Config.from_env()

    def test_invalid_status_retry_max_attempts(self, valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("DCGM_DIAG_STATUS_RETRY_MAX_ATTEMPTS", "0")
        with pytest.raises(ValueError, match="DCGM_DIAG_STATUS_RETRY_MAX_ATTEMPTS must be >= 1"):
            Config.from_env()

    def test_invalid_status_retry_interval(self, valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("DCGM_DIAG_STATUS_RETRY_INTERVAL_SECONDS", "0")
        with pytest.raises(ValueError, match="DCGM_DIAG_STATUS_RETRY_INTERVAL_SECONDS must be > 0"):
            Config.from_env()

    def test_token_path_is_none_when_unset(self, valid_env: None) -> None:
        assert Config.from_env().token_path is None

    def test_token_path_loaded_from_env(self, valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("PLATFORM_CONNECTOR_TOKEN_PATH", "/var/run/secrets/nvsentinel/token")
        assert Config.from_env().token_path == "/var/run/secrets/nvsentinel/token"

    def test_empty_token_path_is_treated_as_unset(self, valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("PLATFORM_CONNECTOR_TOKEN_PATH", "")
        assert Config.from_env().token_path is None

    def test_publish_is_none_without_a_target(self, valid_env: None) -> None:
        assert Config.from_env().publish is None

    def test_publish_loaded_from_env(self, valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
        monkeypatch.setenv("HEALTH_PUBLISH_TARGET", "platform-connector-deployment.nvsentinel.svc:50051")
        monkeypatch.setenv("HEALTH_PUBLISH_INSECURE", "true")
        monkeypatch.setenv("HEALTH_PUBLISH_TOKEN_PATH", "/var/run/secrets/nvsentinel/token")

        cfg = Config.from_env()

        assert cfg.publish is not None
        assert cfg.publish.target == "platform-connector-deployment.nvsentinel.svc:50051"
        assert cfg.publish.token_path == "/var/run/secrets/nvsentinel/token"

    def test_invalid_publish_env_is_a_config_error(self, valid_env: None, monkeypatch: pytest.MonkeyPatch) -> None:
        """A set target with a missing companion setting must not fall back to the socket."""
        monkeypatch.setenv("HEALTH_PUBLISH_TARGET", "platform-connector-deployment.nvsentinel.svc:50051")
        monkeypatch.setenv("HEALTH_PUBLISH_INSECURE", "true")
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
