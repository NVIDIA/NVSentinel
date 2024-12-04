# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

FROM python:3.10-bookworm

# Install protobuf compiler and related tools
RUN apt-get update && apt-get install -y \
    protobuf-compiler \
    python3-protobuf \
    && rm -rf /var/lib/apt/lists/*

# Create a working directory
WORKDIR /app

# Copy the proto file and the Python script
COPY platformconnector.proto .
COPY send_health_event.py .
COPY protos/ protos/

# Install grpcio-tools for generating Python code from .proto
RUN pip install grpcio-tools
