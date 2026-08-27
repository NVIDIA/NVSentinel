"""Tests for the main monitor module and config."""

import os
from unittest.mock import patch

import sys

import pytest
from prometheus_client import REGISTRY

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from config import MonitorConfig
from monitor import SystemServicesMonitor


class TestMonitorConfig:
    """Tests for configuration loading."""

    def test_defaults(self):
        config = MonitorConfig()
        assert config.check_interval == 30
        assert config.metrics_port == 9101
        assert config.boot_grace_period == 300
        assert config.enable_fabric_check is True
        assert config.enable_gpu_services_check is True
        # Fabric Manager has its own dedicated check; it must not also be in
        # the generic service list (it would be probed twice per cycle).
        assert "nvidia-fabricmanager" not in config.gpu_services
        # nv-hostengine is covered by gpu-health-monitor / DCGM and must
        # not appear in the default demo service list.
        assert "nv-hostengine" not in config.gpu_services
        # ...and the default must not silently become empty.
        assert "nvidia-persistenced" in config.gpu_services

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
            "ENABLE_GPU_SERVICES_CHECK": "false",
        }
        with patch.dict(os.environ, env, clear=False):
            config = MonitorConfig.from_env()

        assert config.check_interval == 60
        assert config.metrics_port == 9200
        assert config.log_level == "DEBUG"
        assert config.node_name == "test-node"
        assert config.boot_grace_period == 120
        assert config.flap_threshold == 5
        assert config.enable_fabric_check is True
        assert config.enable_gpu_services_check is False

    def test_check_interval_rejects_non_positive(self):
        # 0 or a negative interval would make the wait between cycles return
        # immediately and spin host checks in a tight loop.
        for bad in ("0", "-5"):
            with patch.dict(os.environ, {"CHECK_INTERVAL": bad}, clear=False):
                with pytest.raises(ValueError, match="CHECK_INTERVAL"):
                    MonitorConfig.from_env()

    def test_custom_gpu_services(self):
        with patch.dict(os.environ, {"GPU_SERVICES": "svc-a, svc-b , svc-c"}, clear=False):
            config = MonitorConfig.from_env()
        assert config.gpu_services == ["svc-a", "svc-b", "svc-c"]

    def test_bool_parsing(self):
        for truthy in ("true", "True", "TRUE", "1", "yes", "Yes"):
            with patch.dict(os.environ, {"ENABLE_FABRIC_CHECK": truthy}, clear=False):
                config = MonitorConfig.from_env()
                assert config.enable_fabric_check is True

        for falsy in ("false", "False", "0", "no", ""):
            with patch.dict(os.environ, {"ENABLE_FABRIC_CHECK": falsy}, clear=False):
                config = MonitorConfig.from_env()
                assert config.enable_fabric_check is False


def _node_health(node):
    return REGISTRY.get_sample_value("gpu_node_health_up", {"node": node})


def _fm_restarts(node):
    return REGISTRY.get_sample_value(
        "fabric_manager_restarts_total", {"node": node}
    ) or 0.0


class TestSystemServicesMonitor:
    """Tests for the monitor orchestrator."""

    def _make_monitor(self, **overrides):
        defaults = {
            "check_interval": 30,
            "metrics_port": 0,  # don't bind
            "node_name": "test-node",
            "boot_grace_period": 0,  # no grace period in tests
            "enable_fabric_check": True,
            "enable_gpu_services_check": True,
        }
        defaults.update(overrides)
        config = MonitorConfig(**defaults)
        return SystemServicesMonitor(config)

    @patch("monitor.ServiceChecker")
    def test_check_cycle_all_healthy(self, MockSvc):
        from checks.service_check import FabricManagerStatus, ServiceStatus

        mock_svc = MockSvc.return_value
        mock_svc.check_fabric_manager.return_value = FabricManagerStatus(
            name="nvidia-fabricmanager", active=True, load_state="loaded",
            sub_state="running",
        )
        mock_svc.check_all_gpu_services.return_value = {
            "nvidia-persistenced": ServiceStatus(
                name="nvidia-persistenced", active=True, load_state="loaded"),
        }

        monitor = self._make_monitor(node_name="node-healthy")
        monitor._service_checker = mock_svc
        monitor.run_check_cycle()

        # Verify service checks were invoked
        mock_svc.check_fabric_manager.assert_called_once()
        mock_svc.check_all_gpu_services.assert_called_once()
        assert _node_health("node-healthy") == 1

    @patch("monitor.ServiceChecker")
    def test_check_cycle_fabric_manager_down(self, MockSvc):
        from checks.service_check import FabricManagerStatus, ServiceStatus

        mock_svc = MockSvc.return_value
        mock_svc.check_fabric_manager.return_value = FabricManagerStatus(
            name="nvidia-fabricmanager", active=False, load_state="loaded",
            sub_state="failed",
        )
        mock_svc.check_all_gpu_services.return_value = {
            "nvidia-persistenced": ServiceStatus(
                name="nvidia-persistenced", active=True, load_state="loaded"),
        }

        monitor = self._make_monitor(node_name="node-fm-down")
        monitor._service_checker = mock_svc
        monitor.run_check_cycle()

        # Monitor should have flagged FM as down
        assert monitor._fabric_manager_down is True
        assert _node_health("node-fm-down") == 0

    @patch("monitor.ServiceChecker")
    def test_probe_error_marks_unhealthy_past_grace(self, MockSvc):
        """A failed probe must not leave the node reporting healthy."""
        from checks.service_check import FabricManagerStatus

        mock_svc = MockSvc.return_value
        mock_svc.check_fabric_manager.return_value = FabricManagerStatus(
            name="nvidia-fabricmanager", active=False,
            error="systemctl show timed out",
        )
        mock_svc.check_all_gpu_services.return_value = {}

        monitor = self._make_monitor(node_name="node-probe-err")
        monitor._service_checker = mock_svc
        monitor.run_check_cycle()

        assert _node_health("node-probe-err") == 0
        # UNKNOWN is not DOWN: the gauge must not claim FM is down.
        assert REGISTRY.get_sample_value(
            "fabric_manager_up", {"node": "node-probe-err"}) is None

    @patch("monitor.ServiceChecker")
    def test_check_exception_marks_unhealthy_past_grace(self, MockSvc):
        """An exception inside a check must not leave the node healthy."""
        mock_svc = MockSvc.return_value
        mock_svc.check_fabric_manager.side_effect = RuntimeError("nsenter exploded")
        mock_svc.check_all_gpu_services.return_value = {}

        monitor = self._make_monitor(node_name="node-check-exc")
        monitor._service_checker = mock_svc
        monitor.run_check_cycle()

        assert _node_health("node-check-exc") == 0

    @patch("monitor.ServiceChecker")
    def test_unit_not_found_is_skipped_not_down(self, MockSvc):
        """LoadState=not-found means the unit is absent, not failed."""
        from checks.service_check import FabricManagerStatus, ServiceStatus

        mock_svc = MockSvc.return_value
        mock_svc.check_fabric_manager.return_value = FabricManagerStatus(
            name="nvidia-fabricmanager", active=False, load_state="not-found",
        )
        mock_svc.check_all_gpu_services.return_value = {
            "nvidia-persistenced": ServiceStatus(
                name="nvidia-persistenced", active=False, load_state="not-found"),
        }

        monitor = self._make_monitor(node_name="node-not-found")
        monitor._service_checker = mock_svc
        monitor.run_check_cycle()

        assert monitor._fabric_manager_down is False
        assert _node_health("node-not-found") == 1
        assert REGISTRY.get_sample_value(
            "nvidia_service_up",
            {"node": "node-not-found", "service_name": "nvidia-persistenced"}) is None

    @patch("monitor.ServiceChecker")
    def test_restart_delta_increments_counter(self, MockSvc):
        """fabric_manager_restarts_total publishes positive NRestarts deltas."""
        from checks.service_check import FabricManagerStatus

        node = "node-restarts"
        mock_svc = MockSvc.return_value
        mock_svc.check_all_gpu_services.return_value = {}
        monitor = self._make_monitor(node_name=node)
        monitor._service_checker = mock_svc

        def fm(n):
            return FabricManagerStatus(
                name="nvidia-fabricmanager", active=True, load_state="loaded",
                sub_state="running", n_restarts=n,
            )

        # First observation only sets the baseline: restarts accumulated
        # before the monitor was deployed must not count.
        mock_svc.check_fabric_manager.return_value = fm(5)
        monitor.run_check_cycle()
        assert _fm_restarts(node) == 0.0

        # Two more restarts observed -> counter += 2.
        mock_svc.check_fabric_manager.return_value = fm(7)
        monitor.run_check_cycle()
        assert _fm_restarts(node) == 2.0

        # Unobserved NRestarts (None) leaves the baseline untouched...
        mock_svc.check_fabric_manager.return_value = fm(None)
        monitor.run_check_cycle()
        assert _fm_restarts(node) == 2.0

        # ...so the next real observation doesn't double-count.
        mock_svc.check_fabric_manager.return_value = fm(8)
        monitor.run_check_cycle()
        assert _fm_restarts(node) == 3.0

        # A counter reset (reset-failed / reboot) counts as one restart,
        # keeping the exported counter in step with flap tracking.
        mock_svc.check_fabric_manager.return_value = fm(1)
        monitor.run_check_cycle()
        assert _fm_restarts(node) == 4.0

    def test_grace_period_suppresses_unhealthy(self):
        monitor = self._make_monitor(
            boot_grace_period=9999,  # always in grace period
            enable_fabric_check=False,
        )
        assert monitor._in_grace_period() is True

    @patch("monitor.ServiceChecker")
    def test_grace_period_masks_down_service_in_gauge(self, MockSvc):
        """During boot grace a down service must not mark the node unhealthy."""
        from checks.service_check import FabricManagerStatus

        mock_svc = MockSvc.return_value
        mock_svc.check_fabric_manager.return_value = FabricManagerStatus(
            name="nvidia-fabricmanager", active=False, load_state="loaded",
            sub_state="failed",
        )
        mock_svc.check_all_gpu_services.return_value = {}

        monitor = self._make_monitor(node_name="node-grace-mask", boot_grace_period=9999)
        monitor._service_checker = mock_svc
        monitor.run_check_cycle()

        assert _node_health("node-grace-mask") == 1

    def test_no_grace_period(self):
        monitor = self._make_monitor(boot_grace_period=0)
        assert monitor._in_grace_period() is False
