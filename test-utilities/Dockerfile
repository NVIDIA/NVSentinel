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
