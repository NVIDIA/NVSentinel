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

import contextlib
import enum
import json
import logging as log
import os
import threading
from collections.abc import Iterator
from typing import Optional


class NVLinkDownExpectation(enum.Enum):
    """Why (or whether) an all-NVLink-links-down report is expected for a GPU.

    NO_NVLINK_HARDWARE and UNBRIDGED_PCIE both mean the down state is
    expected, but they differ in ambiguity: no-silicon is unambiguous,
    while an unbridged bridge-capable PCIe card is indistinguishable from
    a card whose bridge was dead at metadata-collection time, so callers
    must require explicit operator opt-in before acting on UNBRIDGED_PCIE.
    """

    NO_NVLINK_HARDWARE = "no_nvlink_hardware"
    UNBRIDGED_PCIE = "unbridged_pcie"
    NVLINK_IN_USE = "nvlink_in_use"
    UNKNOWN = "unknown"


def _version_of(st: os.stat_result) -> tuple[int, int, int, int]:
    """Device, inode, size and mtime of the file. A change in any of them means a new copy."""
    return (st.st_dev, st.st_ino, st.st_size, st.st_mtime_ns)


class MetadataReader:
    """Lazy-loading, thread-safe GPU metadata reader.

    This class reads GPU metadata from a JSON file and provides
    thread-safe access to GPU UUID and chassis serial information.
    The metadata is loaded lazily on first access and read again
    when the file changes.
    """

    def __init__(self, metadata_path: str):
        """Initialize the metadata reader.

        Args:
            metadata_path: Path to the GPU metadata JSON file.
        """
        self._path = metadata_path
        self._metadata = None
        self._lock = threading.RLock()
        self._loaded = False
        self._missing_warned = False
        # _version_of() for the copy read last, whether it parsed or not.
        self._file_version: Optional[tuple[int, int, int, int]] = None
        self._pinned = 0

    @contextlib.contextmanager
    def pinned(self) -> Iterator[None]:
        """Check the file once, then serve that copy to every lookup in the block.

        A health poll looks up every GPU twice per DCGM watch, once for the PCI
        address and once for the UUID. Pinning the copy for the poll keeps that
        to one stat of the file, and every event of the poll names its GPUs
        from the same copy. Lookups made from other threads
        while the block runs get that copy too.
        """
        with self._lock:
            self._ensure_loaded()
            self._pinned += 1
        try:
            yield
        finally:
            with self._lock:
                self._pinned -= 1

    def _ensure_loaded(self):
        """Load metadata on first use and read it again when the file changed.

        metadata-collector replaces the file with a rename when its pod starts,
        and the file outlives a reboot on the host, so the copy read first can
        be the previous boot's. Outside pinned(), every lookup compares the
        file's device, inode, size and mtime with the copy read last, like the
        syslog monitor's reader does.
        """
        with self._lock:
            if self._pinned:
                return

            version = None
            try:
                version = _version_of(os.stat(self._path))
                if version == self._file_version:
                    return

                with open(self._path, "r") as f:
                    # Stat the open file, so the version always matches what was read.
                    version = _version_of(os.fstat(f.fileno()))
                    metadata = json.load(f)
                gpu_count = len(metadata.get("gpus", []))
                chassis = metadata.get("chassis_serial")
                reloaded = self._loaded
                self._metadata = metadata
                self._file_version = version
                self._loaded = True
                self._missing_warned = False
                log.info(
                    f"GPU metadata {'reloaded' if reloaded else 'loaded'} from {self._path}: "
                    f"{gpu_count} GPUs, chassis_serial={'present' if chassis else 'absent'}"
                )
            except FileNotFoundError:
                # The metadata file may not exist yet if gpu-health-monitor starts before
                # the metadata-collector writes it. Treat as transient: serve empty metadata
                # for now but stay "unloaded" so a later access reloads it once the file
                # appears (otherwise we permanently lose enrichment/visibility on this node).
                if not self._missing_warned:
                    log.warning(f"Metadata file not found: {self._path}, will retry on next access")
                    self._missing_warned = True
                if self._loaded:
                    # Keep the copy read before; metadata-collector never deletes the file.
                    return
                self._metadata = {}
                # Intentionally do NOT set self._loaded = True, so the next access retries.
                return
            except Exception as e:
                # Handles JSON decode errors, permission errors, etc. The version is kept,
                # so this copy is not read again but the next one is. Only a copy that
                # parsed counts as loaded, and it stays in use until a good one replaces it.
                self._file_version = version
                if self._loaded:
                    log.error(f"Error loading metadata from {self._path}, keeping the copy read before: {e}")
                    return
                log.error(f"Error loading metadata from {self._path}: {e}")
                self._metadata = {}

    def get_gpu_uuid(self, gpu_id: int) -> Optional[str]:
        """Get GPU UUID by DCGM GPU ID.

        Args:
            gpu_id: The DCGM GPU ID (0, 1, 2, ...).

        Returns:
            The GPU UUID string if found, None otherwise.
        """
        self._ensure_loaded()

        if not self._metadata:
            return None

        gpus = self._metadata.get("gpus", [])
        for gpu in gpus:
            if gpu.get("gpu_id") == gpu_id:
                uuid = gpu.get("uuid")
                if uuid:
                    log.debug(f"Found GPU UUID for GPU {gpu_id}: {uuid}")
                    return uuid
                else:
                    log.warning(f"GPU {gpu_id} found in metadata but has no UUID")
                    return None

        log.debug(f"GPU {gpu_id} not found in metadata")
        return None

    def get_pci_address(self, gpu_id: int) -> Optional[str]:
        """Get PCI address by DCGM GPU ID.

        Args:
            gpu_id: The DCGM GPU ID (0, 1, 2, ...).

        Returns:
            The PCI address string if found, None otherwise.
        """
        self._ensure_loaded()

        if not self._metadata:
            return None

        gpus = self._metadata.get("gpus", [])
        for gpu in gpus:
            if gpu.get("gpu_id") == gpu_id:
                pci_address = gpu.get("pci_address")
                if pci_address:
                    log.debug(f"Found PCI address for GPU {gpu_id}: {pci_address}")
                    return pci_address
                else:
                    log.warning(f"GPU {gpu_id} found in metadata but has no PCI address")
                    return None

        log.debug(f"GPU {gpu_id} not found in metadata")
        return None

    def get_slowdown_tlimit_c(self, gpu_id: int) -> Optional[int]:
        """Return NVML slowdown T.Limit offset (°C) for the GPU, if published."""
        self._ensure_loaded()

        if not self._metadata:
            return None

        gpus = self._metadata.get("gpus", [])
        for gpu in gpus:
            if gpu.get("gpu_id") == gpu_id:
                raw = gpu.get("slowdown_tlimit_c")
                if raw is None:
                    log.warning(f"GPU {gpu_id} has no slowdown_tlimit_c")
                    return None
                try:
                    return int(raw)
                except (ValueError, TypeError):
                    log.warning(
                        "GPU %s slowdown_tlimit_c value %r is not a valid integer; treating as missing",
                        gpu_id,
                        raw,
                    )
                    return None

        return None

    @staticmethod
    def _as_link_count(value: object, gpu_id: int, field: str) -> Optional[int]:
        """Validate a link-count field: only a non-negative int is accepted.

        Anything else (bool, float, string, negative) is malformed producer
        output and must read as unknown, never as a confirmed count.
        """
        if value is None:
            return None
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            log.warning(
                "GPU %s %s value %r is not a non-negative integer; treating as unknown",
                gpu_id,
                field,
                value,
            )
            return None
        return value

    def classify_nvlink_down(self, gpu_id: int) -> NVLinkDownExpectation:
        """Classify whether an all-NVLink-links-down report is expected
        steady state for this GPU rather than a fault.

        DCGM's NVLink health watch fires DCGM_FR_NVLINK_DOWN whenever a GPU
        has NVLink hardware whose links are down. That is normal for:
          - GPUs with no NVLink silicon at all (L40, A40): nothing to be up.
          - NVLink-bridge-capable PCIe cards with no bridge installed
            (A100/H100 PCIe): NVML reports the bridge links (SUCCESS) but
            they are permanently inactive.

        Decision rule, using metadata collected at startup:
          - nvlink_active_link_count > 0 → NVLINK_IN_USE: links going down
            is a genuine fault.
          - nvlink_active_link_count == 0 and nvlink_link_count == 0
            → NO_NVLINK_HARDWARE: unambiguous, nothing could ever be up.
          - nvlink_active_link_count == 0 and device_name contains "PCIe"
            → UNBRIDGED_PCIE: consistent with an unbridged bridge-capable
            card, but indistinguishable from a card whose bridge was dead
            at collection time — callers must require explicit operator
            opt-in before suppressing on this value.
          - nvlink_active_link_count == 0 otherwise → UNKNOWN: on SXM/HGX
            systems links train via fabric manager after boot, so a zero
            reading may just mean metadata was collected too early.

        Fails closed on the active count: a missing or malformed
        nvlink_active_link_count always yields UNKNOWN. The hardware count
        is corroborating evidence only: when it is missing or malformed, a
        PCIe device name with a zero active count still classifies as
        UNBRIDGED_PCIE.

        Args:
            gpu_id: The DCGM GPU ID (0, 1, 2, ...).

        Returns:
            An NVLinkDownExpectation. UNKNOWN is returned when the GPU is
            not found, metadata is unavailable, or the expectation cannot
            be established safely — callers must never suppress on UNKNOWN.
        """
        self._ensure_loaded()

        if not self._metadata:
            return NVLinkDownExpectation.UNKNOWN

        gpus = self._metadata.get("gpus", [])
        for gpu in gpus:
            if gpu.get("gpu_id") == gpu_id:
                active = self._as_link_count(gpu.get("nvlink_active_link_count"), gpu_id, "nvlink_active_link_count")
                if active is None:
                    return NVLinkDownExpectation.UNKNOWN
                if active > 0:
                    return NVLinkDownExpectation.NVLINK_IN_USE

                hardware = self._as_link_count(gpu.get("nvlink_link_count"), gpu_id, "nvlink_link_count")
                if hardware == 0:
                    return NVLinkDownExpectation.NO_NVLINK_HARDWARE

                device_name = gpu.get("device_name")
                if isinstance(device_name, str) and "PCIe" in device_name:
                    return NVLinkDownExpectation.UNBRIDGED_PCIE

                return NVLinkDownExpectation.UNKNOWN

        log.debug(f"GPU {gpu_id} not found in metadata")
        return NVLinkDownExpectation.UNKNOWN

    def get_chassis_serial(self) -> Optional[str]:
        """Get chassis serial number.

        Returns:
            The chassis serial number if available, None otherwise.
        """
        self._ensure_loaded()

        if not self._metadata:
            return None

        chassis_serial = self._metadata.get("chassis_serial")
        if chassis_serial:
            log.debug(f"Found chassis serial: {chassis_serial}")
        else:
            log.debug("No chassis serial in metadata")

        return chassis_serial
