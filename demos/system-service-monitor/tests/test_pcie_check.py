"""Tests for pcie_check and clock_check modules."""

import subprocess
from unittest.mock import patch

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from checks.pcie_check import PCIeChecker, PCIeStatus
from checks.clock_check import ClockChecker, ClockStatus


class TestPCIeChecker:
    """Tests for PCIeChecker."""

    # Simulates 8x H100 GPUs on P5.48xlarge, all healthy at Gen5 x16
    _HEALTHY_OUTPUT = "\n".join(
        f"{i}, 5, 5, 16, 16" for i in range(8)
    )

    # GPU 3 downtraining: Gen5->Gen3, x16->x8
    _DEGRADED_OUTPUT = "\n".join(
        f"{i}, {'3' if i == 3 else '5'}, 5, {'8' if i == 3 else '16'}, 16"
        for i in range(8)
    )

    @patch("checks.pcie_check.subprocess.run")
    def test_all_healthy(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0, stdout=self._HEALTHY_OUTPUT, stderr="",
        )
        results = PCIeChecker().check()

        assert len(results) == 8
        for r in results:
            assert r.degraded is False
            assert r.link_gen_current == 5
            assert r.link_width_current == 16

    @patch("checks.pcie_check.subprocess.run")
    def test_degraded_link(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0, stdout=self._DEGRADED_OUTPUT, stderr="",
        )
        results = PCIeChecker().check()

        assert len(results) == 8
        degraded = [r for r in results if r.degraded]
        assert len(degraded) == 1
        assert degraded[0].gpu_index == 3
        assert degraded[0].link_gen_current == 3
        assert degraded[0].link_width_current == 8

    @patch("checks.pcie_check.subprocess.run")
    def test_nvidia_smi_not_found(self, mock_run):
        mock_run.side_effect = FileNotFoundError("nvidia-smi not found")
        results = PCIeChecker().check()
        assert results == []

    @patch("checks.pcie_check.subprocess.run")
    def test_nvidia_smi_timeout(self, mock_run):
        mock_run.side_effect = subprocess.TimeoutExpired(cmd="", timeout=15)
        results = PCIeChecker().check()
        assert results == []

    @patch("checks.pcie_check.subprocess.run")
    def test_malformed_output(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0,
            stdout="0, 5, 5, 16, 16\nbad line\n2, 5, 5, 16, 16\n",
            stderr="",
        )
        results = PCIeChecker().check()
        assert len(results) == 2  # skips bad line


class TestClockChecker:
    """Tests for ClockChecker."""

    _HEALTHY_CLOCKS = "\n".join(
        f"{i}, 1980, 1980, 2619, 2619" for i in range(8)
    )

    _THROTTLED_CLOCKS = "\n".join(
        f"{i}, {'1200' if i == 0 else '1980'}, 1980, 2619, 2619"
        for i in range(8)
    )

    @patch("checks.clock_check.ClockChecker._query_throttle_reasons")
    @patch("checks.clock_check.subprocess.run")
    def test_no_throttling(self, mock_run, mock_reasons):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0, stdout=self._HEALTHY_CLOCKS, stderr="",
        )
        mock_reasons.return_value = []
        results = ClockChecker(throttle_ratio=0.85).check()

        assert len(results) == 8
        for r in results:
            assert r.throttled is False
            assert r.clock_ratio == 1.0

    @patch("checks.clock_check.ClockChecker._query_throttle_reasons")
    @patch("checks.clock_check.subprocess.run")
    def test_throttled_gpu(self, mock_run, mock_reasons):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0, stdout=self._THROTTLED_CLOCKS, stderr="",
        )
        mock_reasons.return_value = [
            {"gpu_index": 0, "reasons": "SW Thermal Slowdown"},
        ]
        results = ClockChecker(throttle_ratio=0.85).check()

        throttled = [r for r in results if r.throttled]
        assert len(throttled) == 1
        assert throttled[0].gpu_index == 0
        assert throttled[0].clock_ratio < 0.85

    @patch("checks.clock_check.ClockChecker._query_throttle_reasons")
    @patch("checks.clock_check.subprocess.run")
    def test_custom_threshold(self, mock_run, mock_reasons):
        # GPU 0 at 1800/1980 = ~0.909 — above 0.85 but below 0.95
        output = "0, 1800, 1980, 2619, 2619"
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0, stdout=output, stderr="",
        )
        mock_reasons.return_value = []

        # With default 0.85 threshold — not throttled
        results = ClockChecker(throttle_ratio=0.85).check()
        assert results[0].throttled is False

        # With strict 0.95 threshold — throttled
        results = ClockChecker(throttle_ratio=0.95).check()
        assert results[0].throttled is True
