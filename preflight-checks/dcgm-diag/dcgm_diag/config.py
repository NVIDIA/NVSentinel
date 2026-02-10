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
from dataclasses import dataclass

from .protos import health_event_pb2 as pb


@dataclass
class Config:
    diag_level: int
    hostengine_addr: str
    connector_socket: str
    node_name: str
    processing_strategy: pb.ProcessingStrategy

    @classmethod
    def from_env(cls) -> "Config":
        diag_level = int(os.getenv("DCGM_DIAG_LEVEL", "2"))
        hostengine_addr = os.getenv("DCGM_HOSTENGINE_ADDR", "")
        connector_socket = os.getenv("PLATFORM_CONNECTOR_SOCKET", "")
        node_name = os.getenv("NODE_NAME", "")
        strategy_str = os.getenv("PROCESSING_STRATEGY", "EXECUTE_REMEDIATION")

        if not hostengine_addr:
            raise ValueError("DCGM_HOSTENGINE_ADDR is required")

        if not connector_socket:
            raise ValueError("PLATFORM_CONNECTOR_SOCKET is required")

        if not node_name:
            raise ValueError("NODE_NAME is required")

        if diag_level < 1 or diag_level > 4:
            raise ValueError(f"DCGM_DIAG_LEVEL must be 1-4, got {diag_level}")

        try:
            processing_strategy = pb.ProcessingStrategy.Value(strategy_str)
        except ValueError:
            raise ValueError(f"Invalid PROCESSING_STRATEGY: {strategy_str}")

        return cls(
            diag_level=diag_level,
            hostengine_addr=hostengine_addr,
            connector_socket=connector_socket,
            node_name=node_name,
            processing_strategy=processing_strategy,
        )
