# Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

"""Check the exported inventory of field constants, independently of active watches."""

from types import SimpleNamespace

from prometheus_client import REGISTRY, generate_latest
import pytest

from gpu_health_monitor.dcgm_watcher import dcgm, metrics


@pytest.mark.parametrize("field_count", [0, 1, 2])
@pytest.mark.parametrize("thermal_enabled", [False, True])
def test_number_of_fields_reports_binding_inventory(
    monkeypatch: pytest.MonkeyPatch, field_count: int, thermal_enabled: bool
) -> None:
    """Count available DCGM_FI_DEV constants even before field watches start."""
    available_fields = {
        "DCGM_FI_DEV_GPU_TEMP_MARGIN_CELSIUS": 153,
        "DCGM_FI_DEV_CLOCKS_EVENT_REASONS": 112,
    }
    binding = dict(list(available_fields.items())[:field_count])
    binding["DCGM_FE_GPU"] = 1
    monkeypatch.setattr(dcgm, "dcgm_fields", SimpleNamespace(**binding))
    # The gauge is process-global. An empty inventory must replace a prior value.
    previous_value = REGISTRY.get_sample_value("number_of_fields")
    metrics.num_fields.set(42)
    watcher = None
    try:
        watcher = dcgm.DCGMWatcher(
            dcgm.types.DCGMWatcherConfig(
                addr="localhost:5555",
                poll_interval_seconds=10,
                dcgm_k8s_service_enabled=False,
                thermal_margin_enabled=thermal_enabled,
            ),
            callbacks=[],
        )

        assert watcher._field_group is None
        assert f"number_of_fields {field_count}.0\n" in generate_latest(REGISTRY).decode()
    finally:
        if watcher is not None:
            watcher._callback_thread_pool.shutdown(wait=True)
        metrics.num_fields.set(previous_value)
