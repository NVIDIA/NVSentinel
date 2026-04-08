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

import os
from pathlib import Path

import pytest

from nccl_allreduce.gang import GangConfig, GangConfigReader, GangWaiter, PeerInfo


def write_configmap(tmpdir: str, data: dict[str, str]) -> None:
    """Write ConfigMap-style files into a directory."""
    for key, value in data.items():
        with open(os.path.join(tmpdir, key), "w", encoding="utf-8") as f:
            f.write(value)


class TestGangConfigReader:
    """Tests for reading gang coordination data from a mounted ConfigMap volume.

    Covers: full happy-path parsing, malformed peer ranks, missing required
    keys, empty peer lists, and rank lookup for unknown pods.
    """

    def test_read_parses_configmap(self, tmp_path: Path) -> None:
        config_dir = str(tmp_path)
        write_configmap(
            config_dir,
            {
                "expected_count": "3",
                "gang_id": "volcano-default-pg1",
                "master_addr": "10.0.0.1",
                "master_port": "29500",
                "peers": "pod-0;10.0.0.1;0\npod-1;10.0.0.2;1\npod-2;10.0.0.3;2",
            },
        )

        reader = GangConfigReader(config_dir)
        cfg = reader.read("pod-1")

        assert cfg.expected_count == 3
        assert cfg.gang_id == "volcano-default-pg1"
        assert cfg.master_addr == "10.0.0.1"
        assert cfg.master_port == "29500"
        assert len(cfg.peers) == 3
        assert cfg.my_rank == 1

    def test_read_peers_with_invalid_rank(self, tmp_path: Path) -> None:
        config_dir = str(tmp_path)
        write_configmap(
            config_dir,
            {
                "expected_count": "2",
                "peers": "pod-0;10.0.0.1;bad-rank\npod-1;10.0.0.2;1",
            },
        )

        reader = GangConfigReader(config_dir)
        cfg = reader.read("pod-1")

        assert len(cfg.peers) == 2
        assert cfg.peers[0].rank == -1  # bad rank defaults to -1
        assert cfg.peers[1].rank == 1

    def test_read_missing_expected_count(self, tmp_path: Path) -> None:
        config_dir = str(tmp_path)
        # No expected_count file
        write_configmap(config_dir, {"peers": "pod-0;10.0.0.1;0"})

        reader = GangConfigReader(config_dir)
        with pytest.raises(FileNotFoundError):
            reader.read("pod-0")

    def test_read_empty_peers(self, tmp_path: Path) -> None:
        config_dir = str(tmp_path)
        write_configmap(
            config_dir,
            {"expected_count": "2", "peers": ""},
        )

        reader = GangConfigReader(config_dir)
        cfg = reader.read("pod-0")

        assert len(cfg.peers) == 0
        assert cfg.my_rank == -1

    def test_find_rank_not_found(self, tmp_path: Path) -> None:
        config_dir = str(tmp_path)
        write_configmap(
            config_dir,
            {"expected_count": "2", "peers": "pod-0;10.0.0.1;0"},
        )

        reader = GangConfigReader(config_dir)
        cfg = reader.read("unknown-pod")
        assert cfg.my_rank == -1


class TestPeerFormat:
    """Tests for parsing peer format with check_names field."""

    def test_parse_four_field_format(self, tmp_path: Path) -> None:
        config_dir = str(tmp_path)
        write_configmap(
            config_dir,
            {
                "expected_count": "2",
                "peers": (
                    "pod-0;10.0.0.1;0;preflight-dcgm-diag,preflight-nccl-allreduce\n"
                    "pod-1;10.0.0.2;1;preflight-dcgm-diag,preflight-nccl-allreduce"
                ),
            },
        )

        reader = GangConfigReader(config_dir)
        cfg = reader.read("pod-0")

        assert len(cfg.peers) == 2
        assert cfg.peers[0].check_names == "preflight-dcgm-diag,preflight-nccl-allreduce"
        assert cfg.peers[1].check_names == "preflight-dcgm-diag,preflight-nccl-allreduce"

    def test_backward_compatible_three_field_format(self, tmp_path: Path) -> None:
        config_dir = str(tmp_path)
        write_configmap(
            config_dir,
            {
                "expected_count": "2",
                "peers": "pod-0;10.0.0.1;0\npod-1;10.0.0.2;1",
            },
        )

        reader = GangConfigReader(config_dir)
        cfg = reader.read("pod-0")

        assert len(cfg.peers) == 2
        assert cfg.peers[0].check_names == ""
        assert cfg.peers[1].check_names == ""

    def test_mixed_old_new_format(self, tmp_path: Path) -> None:
        config_dir = str(tmp_path)
        write_configmap(
            config_dir,
            {
                "expected_count": "2",
                "peers": "pod-0;10.0.0.1;0\npod-1;10.0.0.2;1;preflight-dcgm-diag",
            },
        )

        reader = GangConfigReader(config_dir)
        cfg = reader.read("pod-0")

        assert len(cfg.peers) == 2
        assert cfg.peers[0].check_names == ""
        assert cfg.peers[1].check_names == "preflight-dcgm-diag"


class TestValidatePeers:
    """Tests for GangConfig.validate_peers() check consistency validation."""

    @staticmethod
    def _make_config(peers: list[PeerInfo]) -> GangConfig:
        return GangConfig(
            expected_count=len(peers),
            gang_id="test-gang",
            master_addr="10.0.0.1",
            master_port="29500",
            peers=peers,
            my_rank=0,
            my_pod_name="pod-0",
        )

    def test_same_checks(self) -> None:
        peers = [
            PeerInfo("pod-0", "10.0.0.1", 0, "preflight-dcgm-diag,preflight-nccl-allreduce"),
            PeerInfo("pod-1", "10.0.0.2", 1, "preflight-dcgm-diag,preflight-nccl-allreduce"),
        ]
        cfg = self._make_config(peers)
        assert cfg.validate_peers() is None

    def test_check_mismatch(self) -> None:
        peers = [
            PeerInfo("pod-0", "10.0.0.1", 0, "preflight-dcgm-diag,preflight-nccl-allreduce"),
            PeerInfo("pod-1", "10.0.0.2", 1, "preflight-nccl-allreduce"),
        ]
        cfg = self._make_config(peers)
        result = cfg.validate_peers()
        assert result is not None
        assert "mismatch" in result.lower()

    def test_all_default_no_checks(self) -> None:
        peers = [
            PeerInfo("pod-0", "10.0.0.1", 0),
            PeerInfo("pod-1", "10.0.0.2", 1),
        ]
        cfg = self._make_config(peers)
        assert cfg.validate_peers() is None

    def test_single_peer(self) -> None:
        peers = [PeerInfo("pod-0", "10.0.0.1", 0, "preflight-dcgm-diag")]
        cfg = self._make_config(peers)
        assert cfg.validate_peers() is None

    def test_empty_peers(self) -> None:
        cfg = self._make_config([])
        assert cfg.validate_peers() is None

    def test_mismatch_error_includes_pod_details(self) -> None:
        peers = [
            PeerInfo("pod-0", "10.0.0.1", 0, "a,b"),
            PeerInfo("pod-1", "10.0.0.2", 1, "a,b"),
            PeerInfo("pod-2", "10.0.0.3", 2, "a"),
        ]
        cfg = self._make_config(peers)
        result = cfg.validate_peers()
        assert result is not None
        assert "pod-0" in result
        assert "pod-2" in result
        assert "[a,b]" in result
        assert "[a]" in result

    def test_mixed_default_and_explicit_mismatch(self) -> None:
        peers = [
            PeerInfo("pod-0", "10.0.0.1", 0),
            PeerInfo("pod-1", "10.0.0.2", 1, "preflight-dcgm-diag"),
        ]
        cfg = self._make_config(peers)
        result = cfg.validate_peers()
        assert result is not None
        assert "mismatch" in result.lower()


class TestGangConfig:
    """Tests for building torchrun CLI arguments from gang coordination info."""

    def test_get_torchrun_args(self) -> None:
        cfg = GangConfig(
            expected_count=4,
            gang_id="test-gang",
            master_addr="10.0.0.1",
            master_port="29500",
            peers=[],
            my_rank=2,
            my_pod_name="worker-2",
        )

        args = cfg.get_torchrun_args(nprocs_per_node=8, script="train.py")

        assert args == [
            "torchrun",
            "--nnodes=4",
            "--nproc_per_node=8",
            "--node_rank=2",
            "--master_addr=10.0.0.1",
            "--master_port=29500",
            "train.py",
        ]


class TestGangWaiter:
    """Tests for polling the ConfigMap until gang formation completes.

    Covers: immediate success when all peers are present, timeout when
    peers are missing, and timeout when the ConfigMap doesn't exist yet.
    """

    def test_wait_succeeds_when_all_peers_present(self, tmp_path: Path) -> None:
        config_dir = str(tmp_path)
        write_configmap(
            config_dir,
            {
                "expected_count": "2",
                "gang_id": "test",
                "master_addr": "10.0.0.1",
                "master_port": "29500",
                "peers": "pod-0;10.0.0.1;0\npod-1;10.0.0.2;1",
            },
        )

        waiter = GangWaiter(config_dir, poll_interval=0.01)
        cfg = waiter.wait("pod-0", timeout_seconds=5)

        assert cfg.expected_count == 2
        assert len(cfg.peers) == 2

    def test_wait_timeout_when_peers_missing(self, tmp_path: Path) -> None:
        config_dir = str(tmp_path)
        write_configmap(
            config_dir,
            {
                "expected_count": "3",
                "peers": "pod-0;10.0.0.1;0",
            },
        )

        waiter = GangWaiter(config_dir, poll_interval=0.01)

        with pytest.raises(TimeoutError, match="timeout"):
            waiter.wait("pod-0", timeout_seconds=0.05)

    def test_wait_timeout_when_configmap_missing(self, tmp_path: Path) -> None:
        config_dir = str(tmp_path)
        # Empty directory - no ConfigMap files

        waiter = GangWaiter(config_dir, poll_interval=0.01)

        with pytest.raises(TimeoutError, match="timeout"):
            waiter.wait("pod-0", timeout_seconds=0.05)
