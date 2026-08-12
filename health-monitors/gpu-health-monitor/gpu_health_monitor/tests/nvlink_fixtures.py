# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

"""Shared NVLink metadata fixtures for gpu-health-monitor tests.

The three GPU shapes mirror real hardware observed while diagnosing the
DCGM_FR_NVLINK_DOWN false positive:
  - A100_PCIE_UNBRIDGED: the live-repro shape — NVML GetNvLinkState returns
    SUCCESS/NOT_ACTIVE for the 12 bridge links of an unbridged A100 80GB PCIe.
  - H100_SXM_TRAINED: SXM part with all links trained by fabric manager.
  - L40_NO_NVLINK: no NVLink silicon; every link returns NOT_SUPPORTED.
"""

import json
from pathlib import Path
from typing import Final

from gpu_health_monitor.metadata import MetadataReader

A100_PCIE_UNBRIDGED: Final[dict] = {
    "gpu_id": 0,
    "uuid": "GPU-0",
    "device_name": "NVIDIA A100 80GB PCIe",
    "nvlinks": [],
    "nvlink_link_count": 12,
    "nvlink_active_link_count": 0,
}

H100_SXM_TRAINED: Final[dict] = {
    "gpu_id": 0,
    "uuid": "GPU-0",
    "device_name": "NVIDIA H100 80GB HBM3",
    "nvlinks": [],
    "nvlink_link_count": 18,
    "nvlink_active_link_count": 18,
}

L40_NO_NVLINK: Final[dict] = {
    "gpu_id": 0,
    "uuid": "GPU-0",
    "device_name": "NVIDIA L40",
    "nvlinks": [],
    "nvlink_link_count": 0,
    "nvlink_active_link_count": 0,
}


def make_metadata_reader(tmp_path: Path, gpus: list[dict]) -> MetadataReader:
    """Write a minimal gpu_metadata.json under tmp_path and return a reader for it."""
    metadata_path = tmp_path / "gpu_metadata.json"
    metadata_path.write_text(json.dumps({"gpus": gpus}))
    return MetadataReader(str(metadata_path))
