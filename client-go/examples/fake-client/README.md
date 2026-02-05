# NVIDIA Device API: Fake Client Example

This example demonstrates how to use the NVIDIA Device API generated **fake versioned client** with a `SharedInformerFactory` in tests.

## Concepts

* **Fake Clientset**: Implements the `versioned.Interface` via an in-memory `ObjectTracker` to simulate the gRPC server state for unit testing.
* **Watch Reactors**: Using `PrependWatchReactor` to synchronize the transition from `LIST` to `WATCH`, preventing race conditions during event injection.
* **Tracker Injection**: Directly modifying the `ObjectTracker` to simulate "server-side" events, such as a discovery agent reporting a new device.

---

## Running
```bash
go test -v -run TestGPUInformerWithFakeClient
```

---
