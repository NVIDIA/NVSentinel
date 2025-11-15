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

import json
import logging as log
import threading
from typing import Optional


class MetadataReader:
    """Lazy-loading, thread-safe GPU metadata reader.

    This class reads GPU metadata from a JSON file and provides
    thread-safe access to GPU UUID and chassis serial information.
    The metadata is loaded lazily on first access.
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

    def _ensure_loaded(self):
        """Load metadata on first use (lazy loading).

        This method uses double-checked locking to ensure thread-safe
        lazy initialization of the metadata.
        """
        if self._loaded:
            return

        with self._lock:
            if self._loaded:
                return

            try:
                with open(self._path, "r") as f:
                    self._metadata = json.load(f)
                self._loaded = True
                gpu_count = len(self._metadata.get("gpus", []))
                chassis = self._metadata.get("chassis_serial")
                log.info(
                    f"GPU metadata loaded from {self._path}: "
                    f"{gpu_count} GPUs, chassis_serial={'present' if chassis else 'absent'}"
                )
            except FileNotFoundError:
                log.warning(f"Metadata file not found: {self._path}, continuing without metadata enrichment")
                self._metadata = {}
                self._loaded = True
            except Exception as e:
                # Handles JSON decode errors, permission errors, etc.
                log.error(f"Error loading metadata from {self._path}: {e}")
                self._metadata = {}
                self._loaded = True

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
