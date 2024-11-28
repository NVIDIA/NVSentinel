FROM python:3.10-bookworm AS build

RUN pip install poetry==1.8.2

ENV POETRY_NO_INTERACTION=1 \
    POETRY_VIRTUALENVS_IN_PROJECT=1 \
    POETRY_VIRTUALENVS_CREATE=1 \
    POETRY_CACHE_DIR=/tmp/poetry_cache

WORKDIR /app

COPY . .
RUN poetry build --format wheel 
RUN poetry export --format requirements.txt --output constraints.txt --without-hashes

FROM nvcr.io/nvidia/cloud-native/dcgm:3.3.7-1-ubuntu22.04 AS runtime

RUN apt-get update && \
    apt-get install -y python3-pip && \
    rm -rf /var/lib/apt/lists/*

ENV PYTHONPATH=/usr/local/dcgm/bindings/python3 \
    PYTHONUNBUFFERED=1

COPY --from=build /app/dist/*.whl ./
COPY --from=build /app/constraints.txt ./
RUN pip install ./gpu_health_monitor*.whl --constraint constraints.txt

# Copy the proto file and the Python script
COPY gpu_health_monitor/clear_xid_errors/clear_xid_error_health_event.py .
COPY gpu_health_monitor/clear_xid_errors/protos/ protos/

ENTRYPOINT ["gpu_health_monitor"]
