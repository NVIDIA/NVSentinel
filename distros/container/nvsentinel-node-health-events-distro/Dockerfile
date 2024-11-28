
#Platform connector build
FROM golang:1.22 AS builder

RUN mkdir -p /app
COPY platform-connectors /app/platform-connectors
WORKDIR /app/platform-connectors

ENV CGO_ENABLED=0

RUN mkdir -p /etc/config

RUN go mod download
RUN go mod tidy

RUN go build -o platform-connector main.go

# gpu-health-monitor build dependencies requirements
FROM python:3.10-bookworm AS build

RUN mkdir -p /app
COPY health-monitors/gpu-health-monitor /app/gpu-health-monitor
WORKDIR /app/gpu-health-monitor
RUN pip install poetry==1.8.2

ENV POETRY_NO_INTERACTION=1 \
    POETRY_VIRTUALENVS_IN_PROJECT=1 \
    POETRY_VIRTUALENVS_CREATE=1 \
    POETRY_CACHE_DIR=/tmp/poetry_cache

RUN poetry build --format wheel
RUN poetry export --format requirements.txt --output constraints.txt --without-hashes

# Final stage application for gpu_health_monitor and platform-connectors
FROM nvcr.io/nvidia/cloud-native/dcgm:3.3.7-1-ubuntu22.04

WORKDIR /app

# Update package list and install Python and pip
RUN apt-get update && \
    apt-get install -y python3-pip && \
    rm -rf /var/lib/apt/lists/*

ENV PYTHONPATH=/usr/local/dcgm/bindings/python3 \
    PYTHONUNBUFFERED=1

COPY --from=build /app/gpu-health-monitor/dist/*.whl /app/
COPY --from=build /app/gpu-health-monitor/constraints.txt /app/

# Install gpu-health-monitor
RUN pip install /app/gpu_health_monitor*.whl --constraint /app/constraints.txt

# Copy the built Go application from the builder stage
COPY --from=builder /app/platform-connectors/platform-connector /app/
