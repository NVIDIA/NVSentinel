# NVIDIA Device API: Watch Example

This example demonstrates how to set up and use the NVIDIA Device API client to react to asynchronous operations.

## Concepts

* **Manual Connection Management**: Constructing a connection with custom dialers for Unix domain sockets (UDS).
* **Middleware**: Injecting telemetry and structured logging into the gRPC transport layer.
* **Stream Processing**: Implementing long-lived `Watch()` streams with an asynchronous event-loop.
* **Context Handling**: Ensuring clean shutdowns by handling system signals (SIGTERM) and managing context-aware stream cancelation.

---

## Running

1. Ensure the [Fake Server](../fake-server) is running.
2. Run the example:

```bash
sudo go run main.go
```
To stop the application, press `Ctrl+C`

> [!NOTE] `sudo` is required for the default socket path in `/var/run/`. Override the path with the `NVIDIA_DEVICE_API_TARGET` environment variable if using a custom location.

### Expected Output

```text
"level"=0 "msg"="retrieved GPU list" "count"=8
"level"=0 "msg"="starting long-lived watch stream" "method"="/nvidia.nvsentinel.v1alpha1.GpuService/WatchGpus"
"level"=0 "msg"="watch stream established, waiting for events..."
"level"=0 "msg"="gpu status changed" "event"="ADDED" "name"="gpu-0" "uuid"="GPU-b56c1d18..." "status"="NotReady"
"level"=0 "msg"="gpu status changed" "event"="MODIFIED" "name"="gpu-0" "uuid"="GPU-b56c1d18..." "status"="Ready"
...
"level"=0 "msg"="gpu status changed" "event"="MODIFIED" "name"="gpu-1" "uuid"="GPU-2e6d5c15..." "status"="NotReady"
```

---
