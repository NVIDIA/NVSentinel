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
from pathlib import Path
import tempfile
import threading
import pytest
from gpu_health_monitor.metadata import MetadataReader, NVLinkDownExpectation
from gpu_health_monitor.tests.nvlink_fixtures import (
    A100_PCIE_UNBRIDGED,
    H100_SXM_TRAINED,
    L40_NO_NVLINK,
    make_metadata_reader,
)


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


def test_file_appears_after_initial_miss(sample_metadata):
    """Metadata written after the first (missing) access must be picked up.

    Regression for the startup race where gpu-health-monitor reads the metadata
    file before metadata-collector has written it. A missing file must NOT latch
    the reader as 'loaded'; a later access should reload once the file appears.
    """
    with tempfile.TemporaryDirectory() as tmpdir:
        path = os.path.join(tmpdir, "gpu_metadata.json")

        reader = MetadataReader(path)

        # First access: file absent -> graceful None, and reader stays unloaded.
        assert reader.get_gpu_uuid(0) is None
        assert reader._loaded is False

        # metadata-collector writes the file after the monitor already started.
        with open(path, "w") as f:
            json.dump(sample_metadata, f)

        # Next access must reload and surface the real data.
        assert reader.get_gpu_uuid(0) == "GPU-00000000-0000-0000-0000-000000000000"
        assert reader._loaded is True
        assert reader.get_slowdown_tlimit_c(0) is None  # not present in sample, but no crash


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


# --- classify_nvlink_down tests ---


def test_classify_unbridged_pcie_card(tmp_path: Path) -> None:
    """A100 PCIe with bridge links present but zero active: UNBRIDGED_PCIE.

    This is the live-repro shape: NVML GetNvLinkState returns SUCCESS for
    links 0-11 (state NOT_ACTIVE) on an unbridged A100 80GB PCIe."""
    reader = make_metadata_reader(tmp_path, [A100_PCIE_UNBRIDGED])
    assert reader.classify_nvlink_down(0) is NVLinkDownExpectation.UNBRIDGED_PCIE


def test_classify_no_nvlink_silicon(tmp_path: Path) -> None:
    """L40-class GPU with zero hardware links: NO_NVLINK_HARDWARE."""
    reader = make_metadata_reader(tmp_path, [L40_NO_NVLINK])
    assert reader.classify_nvlink_down(0) is NVLinkDownExpectation.NO_NVLINK_HARDWARE


def test_classify_active_links_in_use(tmp_path: Path) -> None:
    """SXM GPU with trained links: NVLINK_IN_USE — a down report is genuine."""
    reader = make_metadata_reader(tmp_path, [H100_SXM_TRAINED])
    assert reader.classify_nvlink_down(0) is NVLinkDownExpectation.NVLINK_IN_USE


def test_classify_unknown_sxm_zero_active(tmp_path: Path) -> None:
    """SXM GPU with hardware links but zero active (fabric manager may not
    have trained links when metadata was collected): UNKNOWN, fail closed."""
    gpu = dict(H100_SXM_TRAINED, nvlink_active_link_count=0)
    reader = make_metadata_reader(tmp_path, [gpu])
    assert reader.classify_nvlink_down(0) is NVLinkDownExpectation.UNKNOWN


def test_classify_pcie_name_without_hardware_count(tmp_path: Path) -> None:
    """PCIe card with zero active links and no hardware count (partial
    metadata): the PCIe device name still classifies as UNBRIDGED_PCIE."""
    gpu = {k: v for k, v in A100_PCIE_UNBRIDGED.items() if k != "nvlink_link_count"}
    reader = make_metadata_reader(tmp_path, [gpu])
    assert reader.classify_nvlink_down(0) is NVLinkDownExpectation.UNBRIDGED_PCIE


def test_classify_unknown_missing_active_count(tmp_path: Path) -> None:
    """Old metadata-collector without nvlink_active_link_count: UNKNOWN."""
    gpu = {k: v for k, v in A100_PCIE_UNBRIDGED.items() if k != "nvlink_active_link_count"}
    reader = make_metadata_reader(tmp_path, [gpu])
    assert reader.classify_nvlink_down(0) is NVLinkDownExpectation.UNKNOWN


@pytest.mark.parametrize("bad_value", [-1, 0.5, True, "12", "invalid"])
def test_classify_unknown_malformed_active_count(tmp_path: Path, bad_value: object) -> None:
    """Malformed nvlink_active_link_count values: UNKNOWN, fail closed."""
    gpu = dict(A100_PCIE_UNBRIDGED, nvlink_active_link_count=bad_value)
    reader = make_metadata_reader(tmp_path, [gpu])
    assert reader.classify_nvlink_down(0) is NVLinkDownExpectation.UNKNOWN


def test_classify_malformed_hardware_count_pcie_name_still_wins(tmp_path: Path) -> None:
    """Malformed hardware count reads as unknown, but a PCIe device name with
    zero active links still classifies as UNBRIDGED_PCIE."""
    gpu = dict(A100_PCIE_UNBRIDGED, nvlink_link_count="bogus")
    reader = make_metadata_reader(tmp_path, [gpu])
    assert reader.classify_nvlink_down(0) is NVLinkDownExpectation.UNBRIDGED_PCIE


def test_classify_unknown_zero_active_no_pcie_name_no_hardware_count(tmp_path: Path) -> None:
    """Zero active links with neither a zero hardware count nor a PCIe name:
    UNKNOWN, fail closed."""
    gpu = {
        "gpu_id": 0,
        "uuid": "GPU-0",
        "device_name": "NVIDIA H100 NVL",
        "nvlinks": [],
        "nvlink_active_link_count": 0,
    }
    reader = make_metadata_reader(tmp_path, [gpu])
    assert reader.classify_nvlink_down(0) is NVLinkDownExpectation.UNKNOWN


def test_classify_unknown_gpu_not_found(tmp_path: Path) -> None:
    """Non-existent GPU returns UNKNOWN."""
    reader = make_metadata_reader(tmp_path, [A100_PCIE_UNBRIDGED])
    assert reader.classify_nvlink_down(99) is NVLinkDownExpectation.UNKNOWN


def test_classify_unknown_metadata_unavailable() -> None:
    """Missing metadata file returns UNKNOWN."""
    reader = MetadataReader("/nonexistent/file.json")
    assert reader.classify_nvlink_down(0) is NVLinkDownExpectation.UNKNOWN


# --- metadata-collector rewriting the file ---


def write_like_metadata_collector(path: str, metadata: dict) -> None:
    """Write the file the way metadata-collector does: a temporary file renamed over the old one."""
    tmp_path = path + ".tmp"
    with open(tmp_path, "w") as f:
        json.dump(metadata, f)
    os.chmod(tmp_path, 0o644)
    os.rename(tmp_path, path)


def test_rereads_file_replaced_by_metadata_collector(tmp_path: Path, sample_metadata: dict) -> None:
    """The file lives on a hostPath that survives a reboot, so the monitor can read the previous
    boot's copy before metadata-collector replaces it. Later lookups must serve the new file."""
    path = str(tmp_path / "gpu_metadata.json")
    write_like_metadata_collector(path, sample_metadata)
    reader = MetadataReader(path)
    assert reader.get_gpu_uuid(0) == "GPU-00000000-0000-0000-0000-000000000000"
    assert reader.get_pci_address(1) == "0001:00:00.0"
    inode_read_first = os.stat(path).st_ino

    sample_metadata["gpus"][0]["uuid"] = "GPU-22222222-2222-2222-2222-222222222222"
    sample_metadata["gpus"][1]["pci_address"] = "0002:00:00.0"
    write_like_metadata_collector(path, sample_metadata)
    assert os.stat(path).st_ino != inode_read_first

    assert reader.get_gpu_uuid(0) == "GPU-22222222-2222-2222-2222-222222222222"
    assert reader.get_pci_address(1) == "0002:00:00.0"


def test_rereads_file_rewritten_in_place(tmp_path: Path, sample_metadata: dict) -> None:
    """A rewrite in place keeps the inode, and here the size too, so the newer mtime is what shows it."""
    path = tmp_path / "gpu_metadata.json"
    path.write_text(json.dumps(sample_metadata))
    reader = MetadataReader(str(path))
    assert reader.get_gpu_uuid(0) == "GPU-00000000-0000-0000-0000-000000000000"
    before = os.stat(path)

    sample_metadata["gpus"][0]["uuid"] = "GPU-22222222-2222-2222-2222-222222222222"
    path.write_text(json.dumps(sample_metadata))
    os.utime(path, ns=(before.st_atime_ns, before.st_mtime_ns + 1_000_000_000))
    after = os.stat(path)
    assert (after.st_ino, after.st_size) == (before.st_ino, before.st_size)

    assert reader.get_gpu_uuid(0) == "GPU-22222222-2222-2222-2222-222222222222"


def test_rereads_file_that_failed_to_parse(tmp_path: Path, sample_metadata: dict) -> None:
    """A file that could not be parsed must not leave the reader empty after metadata-collector
    writes a good one."""
    path = tmp_path / "gpu_metadata.json"
    path.write_text('{"version": "1.0", "gpus": [}')
    reader = MetadataReader(str(path))
    assert reader.get_gpu_uuid(0) is None

    write_like_metadata_collector(str(path), sample_metadata)

    assert reader.get_gpu_uuid(0) == "GPU-00000000-0000-0000-0000-000000000000"
    assert reader.get_chassis_serial() == "CHASSIS-12345"


def test_malformed_rewrite_keeps_the_last_good_metadata(tmp_path: Path, sample_metadata: dict) -> None:
    """A copy that fails to parse must not wipe the GPU identities read before, and the next good
    copy is picked up."""
    path = str(tmp_path / "gpu_metadata.json")
    write_like_metadata_collector(path, sample_metadata)
    reader = MetadataReader(path)
    assert reader.get_gpu_uuid(0) == "GPU-00000000-0000-0000-0000-000000000000"

    bad_copy = path + ".tmp"
    with open(bad_copy, "w") as f:
        f.write('{"version": "1.0", "gpus": [}')
    os.rename(bad_copy, path)

    assert reader.get_gpu_uuid(0) == "GPU-00000000-0000-0000-0000-000000000000"
    assert reader.get_pci_address(1) == "0001:00:00.0"
    assert reader.get_chassis_serial() == "CHASSIS-12345"

    sample_metadata["gpus"][0]["uuid"] = "GPU-22222222-2222-2222-2222-222222222222"
    write_like_metadata_collector(path, sample_metadata)

    assert reader.get_gpu_uuid(0) == "GPU-22222222-2222-2222-2222-222222222222"


def test_pinned_checks_the_file_once(tmp_path: Path, sample_metadata: dict, monkeypatch: pytest.MonkeyPatch) -> None:
    """Inside pinned() the file is checked once however many lookups run, and a copy written
    meanwhile is served from the first lookup after the block."""
    path = str(tmp_path / "gpu_metadata.json")
    write_like_metadata_collector(path, sample_metadata)
    reader = MetadataReader(path)
    stat_calls = []
    real_stat = os.stat

    def counting_stat(stat_path: str, *args: object, **kwargs: object) -> os.stat_result:
        if stat_path == path:
            stat_calls.append(stat_path)
        return real_stat(stat_path, *args, **kwargs)

    monkeypatch.setattr(os, "stat", counting_stat)

    with reader.pinned():
        for _ in range(3):
            for gpu_id in (0, 1):
                assert reader.get_gpu_uuid(gpu_id) is not None
                assert reader.get_pci_address(gpu_id) is not None
        assert reader.get_chassis_serial() == "CHASSIS-12345"

        sample_metadata["gpus"][0]["uuid"] = "GPU-22222222-2222-2222-2222-222222222222"
        write_like_metadata_collector(path, sample_metadata)
        assert reader.get_gpu_uuid(0) == "GPU-00000000-0000-0000-0000-000000000000"
    assert len(stat_calls) == 1

    assert reader.get_gpu_uuid(0) == "GPU-22222222-2222-2222-2222-222222222222"


def test_concurrent_lookups_while_metadata_collector_rewrites(tmp_path: Path, sample_metadata: dict) -> None:
    """Lookups from several threads while the file is replaced over and over always see a whole
    copy, and the reader ends on the last one."""
    path = str(tmp_path / "gpu_metadata.json")
    uuids = ("GPU-00000000-0000-0000-0000-000000000000", "GPU-22222222-2222-2222-2222-222222222222")
    write_like_metadata_collector(path, sample_metadata)
    reader = MetadataReader(path)
    errors = []
    stop = threading.Event()

    def look_up() -> None:
        try:
            while not stop.is_set():
                uuid = reader.get_gpu_uuid(0)
                if uuid not in uuids:
                    errors.append(f"unexpected UUID {uuid!r}")
                    return
        except Exception as e:
            errors.append(e)

    threads = [threading.Thread(target=look_up) for _ in range(8)]
    for thread in threads:
        thread.start()
    for i in range(50):
        sample_metadata["gpus"][0]["uuid"] = uuids[i % 2]
        write_like_metadata_collector(path, sample_metadata)
    stop.set()
    for thread in threads:
        thread.join()
    assert errors == []

    # A last copy of another size, so it can't look like a copy the reader read before.
    sample_metadata["gpus"][0]["uuid"] = "GPU-last"
    write_like_metadata_collector(path, sample_metadata)
    assert reader.get_gpu_uuid(0) == "GPU-last"


def test_pinned_poll_that_raises_releases_the_pin(tmp_path: Path, sample_metadata: dict) -> None:
    """An exception inside a pinned poll must not leave the pin set, or the file would never be checked again."""
    path = str(tmp_path / "gpu_metadata.json")
    write_like_metadata_collector(path, sample_metadata)
    reader = MetadataReader(path)

    with pytest.raises(RuntimeError):
        with reader.pinned():
            assert reader.get_gpu_uuid(0) == sample_metadata["gpus"][0]["uuid"]
            raise RuntimeError("publish failed")

    sample_metadata["gpus"][0]["uuid"] = "GPU-after-the-failed-poll"
    write_like_metadata_collector(path, sample_metadata)
    assert reader.get_gpu_uuid(0) == "GPU-after-the-failed-poll"


def test_rereads_after_the_file_went_missing(tmp_path: Path, sample_metadata: dict) -> None:
    """metadata-collector never deletes the file, so a missing file keeps the copy read before until a new one is written."""
    path = str(tmp_path / "gpu_metadata.json")
    write_like_metadata_collector(path, sample_metadata)
    reader = MetadataReader(path)
    uuid = sample_metadata["gpus"][0]["uuid"]
    pci = sample_metadata["gpus"][0]["pci_address"]
    assert reader.get_gpu_uuid(0) == uuid

    os.unlink(path)
    assert reader.get_gpu_uuid(0) == uuid
    assert reader.get_pci_address(0) == pci

    sample_metadata["gpus"][0]["uuid"] = "GPU-written-again-later"
    write_like_metadata_collector(path, sample_metadata)
    assert reader.get_gpu_uuid(0) == "GPU-written-again-later"


WRONG_SHAPE_COPIES = [
    pytest.param('"abc"', id="string_instead_of_object"),
    pytest.param('{"version": "1.0", "gpus": "abc"}', id="gpus_is_a_string"),
    pytest.param('{"version": "1.0", "gpus": [1]}', id="gpu_entry_is_a_number"),
    pytest.param('{"version": "1.0", "gpus": {}}', id="gpus_is_an_object"),
]


@pytest.mark.parametrize("contents", WRONG_SHAPE_COPIES)
def test_wrong_shape_rewrite_keeps_the_last_good_metadata(tmp_path: Path, sample_metadata: dict, contents: str) -> None:
    """A copy that parses as JSON but has the wrong shape is rejected like one that doesn't parse, so
    lookups keep the identities read before instead of raising on every poll."""
    path = str(tmp_path / "gpu_metadata.json")
    write_like_metadata_collector(path, sample_metadata)
    reader = MetadataReader(path)
    assert reader.get_gpu_uuid(0) == "GPU-00000000-0000-0000-0000-000000000000"

    bad_copy = path + ".tmp"
    with open(bad_copy, "w") as f:
        f.write(contents)
    os.rename(bad_copy, path)

    assert reader.get_gpu_uuid(0) == "GPU-00000000-0000-0000-0000-000000000000"
    assert reader.get_pci_address(1) == "0001:00:00.0"
    assert reader.get_chassis_serial() == "CHASSIS-12345"

    sample_metadata["gpus"][0]["uuid"] = "GPU-after-the-wrong-shape"
    write_like_metadata_collector(path, sample_metadata)
    assert reader.get_gpu_uuid(0) == "GPU-after-the-wrong-shape"


@pytest.mark.parametrize("contents", WRONG_SHAPE_COPIES)
def test_wrong_shape_first_copy_reads_as_empty_until_a_good_one(
    tmp_path: Path, sample_metadata: dict, contents: str
) -> None:
    """A first copy with the wrong shape serves empty metadata, without raising, until a good copy is written."""
    path = tmp_path / "gpu_metadata.json"
    path.write_text(contents)
    reader = MetadataReader(str(path))
    assert reader.get_gpu_uuid(0) is None
    assert reader.get_pci_address(0) is None
    assert reader.classify_nvlink_down(0) is NVLinkDownExpectation.UNKNOWN

    write_like_metadata_collector(str(path), sample_metadata)
    assert reader.get_gpu_uuid(0) == "GPU-00000000-0000-0000-0000-000000000000"
