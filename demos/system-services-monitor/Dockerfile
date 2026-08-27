# python:3.13-slim pinned by digest so rebuilds of the same commit are
# reproducible (the tag alone is mutable). Digest is the multi-arch manifest
# list for python:3.13-slim as of 2026-08-25; to update, resolve the current
# one with: docker buildx imagetools inspect python:3.13-slim
FROM python:3.13-slim@sha256:7e3a6aca9d74f93cca21a91d86a8dad8c34749afd5b4a98ee481c9c47b9f5ed4

# nsenter is in util-linux (already in slim), but ensure it's available
RUN apt-get update && apt-get install -y --no-install-recommends \
    util-linux \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY config.py metrics.py monitor.py ./
COPY checks/ ./checks/

# nsenter requires root to enter host namespaces.
# The DaemonSet securityContext controls the actual privilege level.

EXPOSE 9101

ENTRYPOINT ["python", "monitor.py"]
