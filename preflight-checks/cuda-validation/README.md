# preflight-cuda-validation

One-shot CUDA validation probe that runs as a Pod **init-container**
before the workload starts. Sibling of `dcgm-diag`,
`nccl-allreduce`, and `nccl-loopback` under `preflight-checks/`.

## What it does

For every CUDA device visible to the container, the probe:

1. Calls `torch.cuda.set_device(i)`.
2. Allocates a small float tensor (`torch.randn(N)` where
   `N = CUDA_VALIDATION_TENSOR_SIZE`, default `1024` elements).
3. Runs a trivial on-device kernel (`tensor.sum()`).
4. Calls `torch.cuda.synchronize(i)`.
5. Verifies the result is finite.
6. Frees the allocation (`del tensor; torch.cuda.empty_cache()`).

If every visible GPU passes, the container exits `0` and the workload
pod proceeds. If any GPU fails the probe, the container exits non-zero
and the workload pod stays gated.

## What it gates

The probe answers: *can the CUDA runtime allocate, launch a kernel, and
synchronize on every GPU this container sees?* Failures here mean the
workload would hit the same problem on first device touch — better to
fail fast in an init-container than after model load.

It does **not** replace the deeper checks in the sibling components:

* `dcgm-diag` — DCGM-driven hardware diagnostics
* `nccl-allreduce` — multi-node bandwidth + collective correctness
* `nccl-loopback` — single-node NCCL collective sanity

## Why an init-container, not a daemon checker

This probe was originally proposed inside the
`system-services-monitor` daemon as a poll-loop checker. Reviewer
feedback ([#891](https://github.com/NVIDIA/NVSentinel/pull/891)
comment-4404974860) flagged that allocating CUDA tensors from a
steady-state daemon competes with running-workload memory and risks
OOMing user jobs. The init-container pattern eliminates that concern:
the probe runs **once** before the workload starts, then exits.

## Exit codes

| Code | Meaning |
|------|---------|
| `0`  | All visible GPUs passed the probe. |
| `1`  | At least one GPU failed (allocation, kernel, sync, or non-finite result). |
| `2`  | Environment problem: PyTorch missing, CUDA runtime unavailable, or zero GPUs visible. Treated as infrastructure, not per-GPU fault. |

## Environment variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `CUDA_VALIDATION_TENSOR_SIZE` | `1024` | float32 element count to allocate per GPU. Kept tiny on purpose — the probe must not be able to OOM the workload that follows. |
| `LOG_LEVEL` | `info` | One of `debug`, `info`, `warn`, `error`. Logs go to stderr; init-container logs are how you debug failures. |

## Build

From the repository root:

```sh
make -C preflight-checks/cuda-validation docker-build
```

This produces `ghcr.io/nvidia/nvsentinel/preflight-cuda-validation:<branch>`.

To override the PyTorch base image:

```sh
make -C preflight-checks/cuda-validation docker-build PYTORCH_VERSION=26.04-py3
```

## Use as an init-container

```yaml
apiVersion: v1
kind: Pod
spec:
  initContainers:
    - name: cuda-validation
      image: ghcr.io/nvidia/nvsentinel/preflight-cuda-validation:main
      env:
        - name: LOG_LEVEL
          value: info
      resources:
        limits:
          nvidia.com/gpu: 8   # match workload GPU request
  containers:
    - name: workload
      # ...
```

A non-zero exit from the init-container blocks the workload from starting.

## Design notes

* **Single-file probe.** The script is a standalone Python module with
  `if __name__ == "__main__"`; no package layout, no Poetry, no protos.
  This keeps the diff small and reviewable. Future iterations can grow
  the structure to match `dcgm-diag` (Poetry package, gRPC health
  events to Platform Connector, structured logging) once the probe's
  scope is settled.
* **No HealthReporter integration.** Unlike sibling preflight checks,
  this probe currently signals only via exit code. Init-container exit
  codes already gate the workload pod; emitting a `HealthEvent` to
  Platform Connector is a follow-up once the daemon-side consumer is
  finalised.
* **Tiny allocation.** Probe footprint is intentionally negligible
  (`1024` float32 elements ≈ 4 KiB per GPU) so it cannot starve the
  workload of memory. The original daemon-side checker was rejected
  precisely because steady-state allocations competed with workload
  memory.
