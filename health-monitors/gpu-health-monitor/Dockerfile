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

FROM python:3.10.14-slim-bookworm AS runtime
RUN apt-get update && apt-get install -y wget && \
    wget https://developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/cuda-keyring_1.1-1_all.deb && \
    dpkg -i cuda-keyring_1.1-1_all.deb && rm cuda-keyring_1.1-1_all.deb && \
    apt-get update && apt-get install -y datacenter-gpu-manager=1:3.3.6 && \
    apt-get clean

ENV PYTHONPATH=/usr/local/dcgm/bindings/python3 \
    PYTHONUNBUFFERED=1

COPY --from=build /app/dist/*.whl ./
COPY --from=build /app/constraints.txt ./
RUN pip install ./gpu_health_monitor*.whl --constraint constraints.txt

ENTRYPOINT ["gpu_health_monitor"]
