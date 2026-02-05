# NVIDIA Device API: Go Client Examples

This directory contains examples demonstrating various use cases and functionality of the NVIDIA Device API Go client.

---

## Integration Patterns

| Example | Focus | Use Case |
| :--- | :--- | :--- |
| **[Basic Client](./client)** | **Point-in-Time Discovery** | CLI tools and scripts |
| **[Watch Monitor](./watch)** | **Asynchronous Operations** | Real-time monitoring and telemetry |
| **[Controller](./controller)** | **State Enforcement** | Kubernetes Operators and automation |
| **[Fake Client](./fake-client)** | **Unit Testing** | Mocking and event-driven validation |

---

## Prerequisites

All examples are designed to run locally using the [Fake Device API Server](./fake-server). It maintains an in-memory inventory of 8 GPUs and generates random status events (e.g., `Ready` toggles).

```bash
# Start the simulated device api server
sudo go run ./fake-server/main.go
```
To stop the server, press `Ctrl+C`

## Running Examples

With the server running in a dedicated terminal, navigate to any example and run it:

```bash
# Running the basic client example
cd client/
sudo go run main.go
```

> [!Note] `sudo` is required to access the default Unix domain socket (UDS) path in `/var/run/`. To run without root, override the socket path using the `NVIDIA_DEVICE_API_TARGET` environment variable.

---
