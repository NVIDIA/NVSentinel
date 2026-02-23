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

"""Gang coordination via ConfigMap for multi-node NCCL tests.

This module reads gang coordination data from a ConfigMap mounted as a volume.
The ConfigMap is created by the preflight webhook and contains:
  - expected_count: Number of pods in the gang
  - gang_id: Unique identifier for the gang
  - master_addr: IP address of rank 0 pod
  - master_port: Port for PyTorch distributed TCP bootstrap
  - peers: List of pod names, IPs, and ranks

Example ConfigMap data:
    expected_count: "2"
    gang_id: kubernetes-preflight-test-ns-test-workload-workers
    master_addr: 10.244.1.9
    master_port: "29500"
    peers: |-
        test-worker-0;10.244.1.9;0
        test-worker-1;10.244.3.6;1
"""

import logging
import os
import time
from dataclasses import dataclass

log = logging.getLogger(__name__)

KEY_EXPECTED_COUNT = "expected_count"
KEY_GANG_ID = "gang_id"
KEY_MASTER_ADDR = "master_addr"
KEY_MASTER_PORT = "master_port"
KEY_PEERS = "peers"

DEFAULT_MASTER_PORT = "29500"
DEFAULT_POLL_INTERVAL = 5.0


@dataclass
class PeerInfo:
    """Information about a gang peer.

    Attributes:
        pod_name: Kubernetes pod name.
        pod_ip: Pod IP address.
        rank: Distributed rank (0-indexed).
    """

    pod_name: str
    pod_ip: str
    rank: int


@dataclass
class GangConfig:
    """Gang coordination configuration.

    Attributes:
        expected_count: Number of pods expected in the gang.
        gang_id: Unique identifier for the gang.
        master_addr: IP address of the master (rank 0) pod.
        master_port: Port for PyTorch distributed TCP bootstrap.
        peers: List of all peers in the gang.
        my_rank: This pod's rank in the gang.
        my_pod_name: This pod's name.
    """

    expected_count: int
    gang_id: str
    master_addr: str
    master_port: str
    peers: list[PeerInfo]
    my_rank: int
    my_pod_name: str

    def get_torchrun_args(self, nprocs_per_node: int, script: str) -> list[str]:
        """Build torchrun command arguments.

        Args:
            nprocs_per_node: Number of GPUs per node.
            script: Path to the Python script to run.

        Returns:
            List of command-line arguments for torchrun.
        """
        return [
            "torchrun",
            f"--nnodes={self.expected_count}",
            f"--nproc_per_node={nprocs_per_node}",
            f"--node_rank={self.my_rank}",
            f"--master_addr={self.master_addr}",
            f"--master_port={self.master_port}",
            script,
        ]


class GangConfigReader:
    """Reads gang configuration from a mounted ConfigMap volume."""

    def __init__(self, config_dir: str) -> None:
        """Initialize the reader.

        Args:
            config_dir: Directory where the ConfigMap is mounted.
        """
        self._config_dir = config_dir

    def read(self, pod_name: str) -> GangConfig:
        """Read gang configuration from the ConfigMap.

        Args:
            pod_name: Name of this pod (used to determine rank).

        Returns:
            GangConfig with all coordination information.

        Raises:
            FileNotFoundError: If required ConfigMap files are missing.
            ValueError: If ConfigMap data is invalid.
        """
        expected_count = self._read_int(KEY_EXPECTED_COUNT)
        gang_id = self._read_string(KEY_GANG_ID, default="")
        master_addr = self._read_string(KEY_MASTER_ADDR, default="")
        master_port = self._read_string(KEY_MASTER_PORT, default=DEFAULT_MASTER_PORT)
        peers = self._read_peers()

        my_rank = self._find_rank(peers, pod_name)

        return GangConfig(
            expected_count=expected_count,
            gang_id=gang_id,
            master_addr=master_addr,
            master_port=master_port,
            peers=peers,
            my_rank=my_rank,
            my_pod_name=pod_name,
        )

    def _read_string(self, key: str, default: str | None = None) -> str:
        """Read a string value from the ConfigMap.

        Args:
            key: The ConfigMap data key.
            default: Default value if file doesn't exist.

        Returns:
            The string value.

        Raises:
            FileNotFoundError: If file doesn't exist and no default provided.
        """
        path = os.path.join(self._config_dir, key)
        try:
            with open(path, encoding="utf-8") as f:
                return f.read().strip()
        except FileNotFoundError:
            if default is not None:
                return default
            raise

    def _read_int(self, key: str) -> int:
        """Read an integer value from the ConfigMap.

        Args:
            key: The ConfigMap data key.

        Returns:
            The integer value.

        Raises:
            FileNotFoundError: If the file doesn't exist.
            ValueError: If the value is not a valid integer.
        """
        value = self._read_string(key)
        try:
            return int(value)
        except ValueError as err:
            raise ValueError(f"Invalid {key}: {value}") from err

    def _read_peers(self) -> list[PeerInfo]:
        """Read and parse the peers list from the ConfigMap.

        Format: "pod_name;pod_ip;rank" per line.

        Returns:
            List of PeerInfo objects.
        """
        try:
            data = self._read_string(KEY_PEERS, default="")
        except FileNotFoundError:
            return []

        if not data:
            return []

        peers = []
        for line in data.splitlines():
            line = line.strip()
            if not line:
                continue

            parts = line.split(";")
            if len(parts) < 2:
                log.warning("Invalid peer line, skipping", extra={"line": line})
                continue

            pod_name = parts[0].strip()
            pod_ip = parts[1].strip()

            rank = -1
            if len(parts) >= 3:
                try:
                    rank = int(parts[2].strip())
                except ValueError:
                    log.warning(
                        "Invalid rank in peer line, defaulting to -1",
                        extra={"line": line, "bad_rank": parts[2].strip()},
                    )

            peers.append(PeerInfo(pod_name=pod_name, pod_ip=pod_ip, rank=rank))

        return peers

    @staticmethod
    def _find_rank(peers: list[PeerInfo], pod_name: str) -> int:
        """Find the rank for a pod in the peers list.

        Args:
            peers: List of all peers.
            pod_name: Name of the pod to find.

        Returns:
            The rank of the pod, or -1 if not found.
        """
        for peer in peers:
            if peer.pod_name == pod_name:
                return peer.rank
        return -1


class GangWaiter:
    """Waits for gang formation to complete."""

    def __init__(
        self,
        config_dir: str,
        poll_interval: float = DEFAULT_POLL_INTERVAL,
    ) -> None:
        """Initialize the waiter.

        Args:
            config_dir: Directory where the ConfigMap is mounted.
            poll_interval: Seconds between polling attempts.
        """
        self._reader = GangConfigReader(config_dir)
        self._poll_interval = poll_interval

    def wait(self, pod_name: str, timeout_seconds: int) -> GangConfig:
        """Wait for gang formation to complete.

        Polls the ConfigMap until all expected peers have registered.

        Args:
            pod_name: Name of this pod.
            timeout_seconds: Maximum time to wait.

        Returns:
            GangConfig once all peers are registered.

        Raises:
            TimeoutError: If gang formation doesn't complete in time.
            ValueError: If ConfigMap data is invalid.
        """
        deadline = time.monotonic() + timeout_seconds

        log.info(
            "Waiting for gang formation",
            extra={
                "pod_name": pod_name,
                "timeout_seconds": timeout_seconds,
            },
        )

        while True:
            try:
                config = self._reader.read(pod_name)

                # Wait until expected_count is set (> 0) AND all peers registered.
                # The ConfigMap may be created empty initially and populated later
                # by the controller, so we must wait for expected_count > 0.
                if config.expected_count > 0 and len(config.peers) >= config.expected_count:
                    log.info(
                        "Gang formation complete",
                        extra={
                            "expected": config.expected_count,
                            "actual": len(config.peers),
                            "my_rank": config.my_rank,
                            "master_addr": config.master_addr,
                        },
                    )
                    return config

                if config.expected_count == 0:
                    log.info(
                        "Waiting for gang configuration",
                        extra={"status": "expected_count not set yet"},
                    )
                else:
                    log.info(
                        "Waiting for more peers",
                        extra={
                            "expected": config.expected_count,
                            "current": len(config.peers),
                            "remaining": config.expected_count - len(config.peers),
                        },
                    )

            except FileNotFoundError as err:
                log.debug(
                    "ConfigMap not ready yet",
                    extra={"error": str(err)},
                )
            except ValueError as err:
                log.warning(
                    "Error reading ConfigMap",
                    extra={"error": str(err)},
                )

            if time.monotonic() >= deadline:
                raise TimeoutError(f"Gang formation timeout after {timeout_seconds}s")

            time.sleep(self._poll_interval)
