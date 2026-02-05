# NVIDIA Device API: Client Example

This example demonstrates how to use the NVIDIA Device API client to interact with node-local resources.

## Concepts

* **Client Initialization**: Setting up a gRPC connection over a Unix domain socket (UDS) to communicate with the local device API.
* **K8s-Native Verbs**: Using standard `Get` and `List` operations to retrieve point-in-time resource snapshots.
* **Status Evaluation**: Using standard Kubernetes `meta` helpers to check resource readiness and conditions.

---

## Running

1. Ensure the [Fake Server](../fake-server) is running.
2. Run the example:

```bash
sudo go run main.go
```

> [!NOTE] `sudo` is required for the default socket path in `/var/run/`. Override the path with the `NVIDIA_DEVICE_API_TARGET` environment variable if using a custom location.

### Expected Output

```text
"level"=0 "msg"="discovered GPUs" "count"=8 "target"="unix:///tmp/nvidia-device-api.sock"
"level"=0 "msg"="details" "name"="gpu-0" "uuid"="GPU-6e5b6a57..."
"level"=0 "msg"="status" "name"="gpu-0" "uuid"="GPU-6e5b6a57..." "status"="Ready"
"level"=0 "msg"="status" "name"="gpu-1" "uuid"="GPU-2b418863..." "status"="Ready"
"level"=0 "msg"="status" "name"="gpu-2" "uuid"="GPU-4e4e629e..." "status"="Ready"
...
"level"=0 "msg"="status" "name"="gpu-7" "uuid"="GPU-66ba2ccd..." "status"="NotReady"
```

---
