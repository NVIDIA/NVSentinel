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
import os
import tempfile
import threading
import pytest
from gpu_health_monitor.metadata import MetadataReader


@pytest.fixture
def sample_metadata():
    """Sample GPU metadata for testing."""
    return {
        "version": "1.0",
        "timestamp": "2025-11-07T10:00:00Z",
        "node_name": "test-node",
        "chassis_serial": "CHASSIS-12345",
        "gpus": [
            {
                "gpu_id": 0,
                "uuid": "GPU-00000000-0000-0000-0000-000000000000",
                "pci_address": "0000:17:00.0",
                "serial_number": "SN-GPU-0",
                "device_name": "NVIDIA A100",
                "nvlinks": [],
            },
            {
                "gpu_id": 1,
                "uuid": "GPU-11111111-1111-1111-1111-111111111111",
                "pci_address": "0001:00:00.0",
                "serial_number": "SN-GPU-1",
                "device_name": "NVIDIA A100",
                "nvlinks": [],
            },
        ],
        "nvswitches": [],
    }


@pytest.fixture
def metadata_file(sample_metadata):
    """Create a temporary metadata file for testing."""
    with tempfile.NamedTemporaryFile(mode="w", delete=False, suffix=".json") as f:
        json.dump(sample_metadata, f)
        temp_path = f.name
    yield temp_path
    os.unlink(temp_path)


def test_lazy_loading(metadata_file):
    """Test that metadata is not loaded until first access."""
    reader = MetadataReader(metadata_file)

    assert not reader._loaded
    assert reader._metadata is None

    uuid = reader.get_gpu_uuid(0)

    assert reader._loaded
    assert reader._metadata is not None
    assert uuid == "GPU-00000000-0000-0000-0000-000000000000"


def test_get_gpu_uuid_found(metadata_file):
    """Test successful GPU UUID lookup."""
    reader = MetadataReader(metadata_file)

    uuid0 = reader.get_gpu_uuid(0)
    assert uuid0 == "GPU-00000000-0000-0000-0000-000000000000"

    uuid1 = reader.get_gpu_uuid(1)
    assert uuid1 == "GPU-11111111-1111-1111-1111-111111111111"


def test_get_gpu_uuid_not_found(metadata_file):
    """Test GPU UUID lookup for non-existent GPU ID."""
    reader = MetadataReader(metadata_file)

    uuid = reader.get_gpu_uuid(99)
    assert uuid is None


def test_get_pci_address_found(metadata_file):
    """Test successful PCI address lookup."""
    reader = MetadataReader(metadata_file)

    pci0 = reader.get_pci_address(0)
    assert pci0 == "0000:17:00.0"

    pci1 = reader.get_pci_address(1)
    assert pci1 == "0001:00:00.0"


def test_get_pci_address_not_found(metadata_file):
    """Test PCI address lookup for non-existent GPU ID."""
    reader = MetadataReader(metadata_file)

    pci = reader.get_pci_address(99)
    assert pci is None


def test_get_chassis_serial(metadata_file):
    """Test chassis serial retrieval."""
    reader = MetadataReader(metadata_file)

    serial = reader.get_chassis_serial()
    assert serial == "CHASSIS-12345"


def test_get_chassis_serial_missing(sample_metadata):
    """Test chassis serial retrieval when not present."""
    del sample_metadata["chassis_serial"]

    with tempfile.NamedTemporaryFile(mode="w", delete=False, suffix=".json") as f:
        json.dump(sample_metadata, f)
        temp_path = f.name

    try:
        reader = MetadataReader(temp_path)
        serial = reader.get_chassis_serial()
        assert serial is None
    finally:
        os.unlink(temp_path)


def test_file_not_found():
    """Test graceful handling of missing metadata file."""
    reader = MetadataReader("/nonexistent/file.json")

    uuid = reader.get_gpu_uuid(0)
    assert uuid is None

    pci = reader.get_pci_address(0)
    assert pci is None

    serial = reader.get_chassis_serial()
    assert serial is None


def test_malformed_json():
    """Test graceful handling of malformed JSON."""
    with tempfile.NamedTemporaryFile(mode="w", delete=False, suffix=".json") as f:
        f.write('{"version": "1.0", "gpus": [}')
        temp_path = f.name

    try:
        reader = MetadataReader(temp_path)

        uuid = reader.get_gpu_uuid(0)
        assert uuid is None

        serial = reader.get_chassis_serial()
        assert serial is None
    finally:
        os.unlink(temp_path)


def test_empty_metadata():
    """Test handling of empty metadata."""
    with tempfile.NamedTemporaryFile(mode="w", delete=False, suffix=".json") as f:
        json.dump({}, f)
        temp_path = f.name

    try:
        reader = MetadataReader(temp_path)

        uuid = reader.get_gpu_uuid(0)
        assert uuid is None

        serial = reader.get_chassis_serial()
        assert serial is None
    finally:
        os.unlink(temp_path)


def test_gpu_without_uuid(sample_metadata):
    """Test handling of GPU entry without UUID field."""
    del sample_metadata["gpus"][0]["uuid"]

    with tempfile.NamedTemporaryFile(mode="w", delete=False, suffix=".json") as f:
        json.dump(sample_metadata, f)
        temp_path = f.name

    try:
        reader = MetadataReader(temp_path)

        uuid = reader.get_gpu_uuid(0)
        assert uuid is None

        uuid = reader.get_gpu_uuid(1)
        assert uuid == "GPU-11111111-1111-1111-1111-111111111111"
    finally:
        os.unlink(temp_path)


def test_concurrent_access(metadata_file):
    """Test thread-safe concurrent access to metadata."""
    reader = MetadataReader(metadata_file)
    results = []
    errors = []

    def read_uuid(gpu_id):
        try:
            uuid = reader.get_gpu_uuid(gpu_id)
            results.append((gpu_id, uuid))
        except Exception as e:
            errors.append(e)

    threads = []
    for _ in range(10):
        for gpu_id in [0, 1]:
            t = threading.Thread(target=read_uuid, args=(gpu_id,))
            threads.append(t)
            t.start()

    for t in threads:
        t.join()

    assert len(errors) == 0
    assert len(results) == 20

    gpu0_results = [uuid for gid, uuid in results if gid == 0]
    gpu1_results = [uuid for gid, uuid in results if gid == 1]

    assert all(uuid == "GPU-00000000-0000-0000-0000-000000000000" for uuid in gpu0_results)
    assert all(uuid == "GPU-11111111-1111-1111-1111-111111111111" for uuid in gpu1_results)


def test_load_called_once(metadata_file):
    """Test that metadata file is loaded only once even with multiple accesses."""
    reader = MetadataReader(metadata_file)

    # Count how many times we actually acquire the lock and load
    load_count = 0
    original_open = open

    def counting_open(*args, **kwargs):
        nonlocal load_count
        # Only count opens of the metadata file
        if args and args[0] == metadata_file:
            load_count += 1
        return original_open(*args, **kwargs)

    # Temporarily replace open to count file loads
    import builtins

    builtins.open = counting_open

    try:
        reader.get_gpu_uuid(0)  # Should load file
        reader.get_gpu_uuid(1)  # Should NOT load (already loaded)
        reader.get_chassis_serial()  # Should NOT load (already loaded)
        reader.get_gpu_uuid(0)  # Should NOT load (already loaded)
    finally:
        builtins.open = original_open

    # File should only be opened once despite 4 method calls
    assert load_count == 1, f"Expected 1 file load, got {load_count}"


def test_concurrent_initial_load(metadata_file):
    """Test that concurrent initial loads don't cause race conditions."""
    reader = MetadataReader(metadata_file)
    results = []

    def load_and_read():
        uuid = reader.get_gpu_uuid(0)
        results.append(uuid)

    threads = [threading.Thread(target=load_and_read) for _ in range(50)]

    for t in threads:
        t.start()

    for t in threads:
        t.join()

    assert len(results) == 50
    assert all(uuid == "GPU-00000000-0000-0000-0000-000000000000" for uuid in results)
    assert reader._loaded
