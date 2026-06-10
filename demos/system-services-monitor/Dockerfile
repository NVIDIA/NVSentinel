FROM python:3.13-slim

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
