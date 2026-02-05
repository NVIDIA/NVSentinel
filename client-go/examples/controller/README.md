# NVIDIA Device API: Controller Example

This example demonstrates how to use the NVIDIA Device API client with `controller-runtime` to drive standard Reconcilers.

## Concepts

- **Informer Injection**: Overriding `cache.Options` to inject a gRPC-backed `SharedIndexInformer` for specific Device types.
- **Hybrid Connectivity**: Setting up a `Manager` that maintains both a REST client (K8s API) and a gRPC client (Device API).
- **Transparent Caching**: Ensuring `mgr.GetClient()` calls are automatically routed to the local gRPC-backed cache for Device resources. 
- **Controller-Runtime Integration**: Using standard `builder` patterns to set up controllers that react to local hardware events as if they were standard Kubernetes objects.

---

## Running

1. Ensure the [Fake Server](../fake-server) is running.
2. Run the controller:

```bash
sudo go run main.go
```
To stop the controller, press `Ctrl+C`

> [!NOTE] `sudo` is required for the default socket path in `/var/run/`. Override the path with the `NVIDIA_DEVICE_API_TARGET` environment variable if using a custom location.

### Expected Output

```text
INFO    setup   starting manager
INFO    controller-runtime.metrics      Starting metrics server
INFO    Starting EventSource    {"controller": "gpu", "source": "kind source: *v1alpha1.GPU"}
INFO    Starting Controller     {"controller": "gpu"}
INFO    Starting workers        {"controller": "gpu", "worker count": 1}
INFO    Reconciled GPU  {"controller": "gpu", "name": "gpu-1", "uuid": "GPU-6bc0eaf0..."}
INFO    Reconciled GPU  {"controller": "gpu", "name": "gpu-2", "uuid": "GPU-0851fc7c..."}
...
```

---
