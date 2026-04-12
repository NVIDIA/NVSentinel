"""Tests for the main monitor module and config."""

import os
from unittest.mock import patch, MagicMock

import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from config import MonitorConfig
from monitor import FabricManagerMonitor


class TestMonitorConfig:
    """Tests for configuration loading."""

    def test_defaults(self):
        config = MonitorConfig()
        assert config.check_interval == 30
        assert config.metrics_port == 9101
        assert config.boot_grace_period == 300
        assert config.enable_fabric_check is True
        assert config.enable_cuda_validation is False
        assert config.clock_throttle_ratio == 0.85
        assert "nvidia-fabricmanager" in config.gpu_services

    def test_from_env(self):
        env = {
            "CHECK_INTERVAL": "60",
            "METRICS_PORT": "9200",
            "LOG_LEVEL": "DEBUG",
            "NODE_NAME": "test-node",
            "BOOT_GRACE_PERIOD": "120",
            "FLAP_WINDOW": "300",
            "FLAP_THRESHOLD": "5",
            "ENABLE_FABRIC_CHECK": "true",
            "ENABLE_PCIE_CHECK": "false",
            "ENABLE_CLOCK_CHECK": "true",
            "ENABLE_NVLINK_CHECK": "false",
            "ENABLE_CUDA_VALIDATION": "true",
            "CUDA_VALIDATION_INTERVAL": "1200",
            "DCGM_EXPORTER_URL": "http://dcgm:9400",
            "CLOCK_THROTTLE_RATIO": "0.90",
        }
        with patch.dict(os.environ, env, clear=False):
            config = MonitorConfig.from_env()

        assert config.check_interval == 60
        assert config.metrics_port == 9200
        assert config.log_level == "DEBUG"
        assert config.node_name == "test-node"
        assert config.boot_grace_period == 120
        assert config.flap_threshold == 5
        assert config.enable_pcie_check is False
        assert config.enable_nvlink_check is False
        assert config.enable_cuda_validation is True
        assert config.cuda_validation_interval == 1200
        assert config.dcgm_exporter_url == "http://dcgm:9400"
        assert config.clock_throttle_ratio == 0.90

    def test_custom_gpu_services(self):
        with patch.dict(os.environ, {"GPU_SERVICES": "svc-a, svc-b , svc-c"}, clear=False):
            config = MonitorConfig.from_env()
        assert config.gpu_services == ["svc-a", "svc-b", "svc-c"]

    def test_bool_parsing(self):
        for truthy in ("true", "True", "TRUE", "1", "yes", "Yes"):
            with patch.dict(os.environ, {"ENABLE_CUDA_VALIDATION": truthy}, clear=False):
                config = MonitorConfig.from_env()
                assert config.enable_cuda_validation is True

        for falsy in ("false", "False", "0", "no", ""):
            with patch.dict(os.environ, {"ENABLE_CUDA_VALIDATION": falsy}, clear=False):
                config = MonitorConfig.from_env()
                assert config.enable_cuda_validation is False


class TestFabricManagerMonitor:
    """Tests for the monitor orchestrator."""

    def _make_monitor(self, **overrides):
        defaults = {
            "check_interval": 30,
            "metrics_port": 0,  # don't bind
            "node_name": "test-node",
            "boot_grace_period": 0,  # no grace period in tests
            "enable_fabric_check": True,
            "enable_pcie_check": False,
            "enable_clock_check": False,
            "enable_nvlink_check": False,
            "enable_cuda_validation": False,
        }
        defaults.update(overrides)
        config = MonitorConfig(**defaults)
        return FabricManagerMonitor(config)

    @patch("monitor.NVLinkFabricChecker")
    @patch("monitor.ClockChecker")
    @patch("monitor.PCIeChecker")
    @patch("monitor.ServiceChecker")
    def test_check_cycle_all_healthy(self, MockSvc, MockPCIe, MockClk, MockNVL):
        from checks.service_check import FabricManagerStatus, ServiceStatus

        mock_svc = MockSvc.return_value
        mock_svc.check_fabric_manager.return_value = FabricManagerStatus(
            name="nvidia-fabricmanager", active=True, sub_state="running",
        )
        mock_svc.check_all_gpu_services.return_value = {
            "nvidia-fabricmanager": ServiceStatus(name="nvidia-fabricmanager", active=True),
            "nvidia-persistenced": ServiceStatus(name="nvidia-persistenced", active=True),
            "nv-hostengine": ServiceStatus(name="nv-hostengine", active=True),
        }

        monitor = self._make_monitor()
        monitor._service_checker = mock_svc
        monitor.run_check_cycle()

        # Verify fabric_manager_up was called (metrics are global singletons)
        mock_svc.check_fabric_manager.assert_called_once()
        mock_svc.check_all_gpu_services.assert_called_once()

    @patch("monitor.NVLinkFabricChecker")
    @patch("monitor.ClockChecker")
    @patch("monitor.PCIeChecker")
    @patch("monitor.ServiceChecker")
    def test_check_cycle_fabric_manager_down(self, MockSvc, MockPCIe, MockClk, MockNVL):
        from checks.service_check import FabricManagerStatus, ServiceStatus

        mock_svc = MockSvc.return_value
        mock_svc.check_fabric_manager.return_value = FabricManagerStatus(
            name="nvidia-fabricmanager", active=False, sub_state="failed",
        )
        mock_svc.check_all_gpu_services.return_value = {
            "nvidia-fabricmanager": ServiceStatus(name="nvidia-fabricmanager", active=False),
        }

        monitor = self._make_monitor()
        monitor._service_checker = mock_svc
        monitor.run_check_cycle()

        # Monitor should have flagged FM as down
        assert monitor._fabric_manager_down is True

    def test_grace_period_suppresses_unhealthy(self):
        monitor = self._make_monitor(
            boot_grace_period=9999,  # always in grace period
            enable_fabric_check=False,
        )
        assert monitor._in_grace_period() is True

    def test_no_grace_period(self):
        monitor = self._make_monitor(boot_grace_period=0)
        assert monitor._in_grace_period() is False
