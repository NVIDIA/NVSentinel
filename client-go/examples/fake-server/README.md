# NVIDIA Device API: Fake Device API Server

This program simulates a running NVIDIA Device API server. It periodically modifies GPU readiness to provide a dynamic data source for testing without requiring a physical GPU node.

---

## Usage

Run the server in a dedicated terminal window:

```bash
sudo go run main.go
```
To stop the server, press `Ctrl+C`

> [!NOTE] `sudo` is required for the default socket path in `/var/run/`. Override the path with `NVIDIA_DEVICE_API_TARGET` to run without root (e.g., `unix:///tmp/device-api.sock`).

---
