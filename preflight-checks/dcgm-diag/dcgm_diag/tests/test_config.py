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
    DEFAULT_STATUS_RETRY_MAX_ATTEMPTS,
    DEFAULT_STATUS_RETRY_INTERVAL_SECONDS,
    Config,
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
