# NVIDIA Device API: Code Generator Configuration

The code generation pipeline relies on specific versions of upstream tools. The source of truth for these versions is the root [/.versions.yaml](../../.versions.yaml).

---

## Version Precedence

1. **Override**: Environment variables set in the shell.
2. **Default**: Values defined in `.versions.yaml`.
3. **Fallback**: Tool-specific defaults.

---

## Overriding Versions

You can override defaults by setting environment variables before running the generation pipeline (e.g., `make code-gen`).

| Environment Variable      | Tool Category                | Key in `.versions.yaml` |
|---------------------------|------------------------------|-------------------------|
| `KUBE_CODEGEN_TAG`        | Kubernetes Generators        | `kubernetes_code_gen`   |
| `PROTOC_GEN_GO_TAG`       | Protobuf Go Plugin           | `protoc_gen_go`         |
| `PROTOC_GEN_GO_GRPC_TAG`  | gRPC Go Plugin               | `protoc_gen_go_grpc`    |
| `GOVERTER_TAG`            | Proto-to-Go Mapper           | `goverter`              |

### Example: Testing a Different Kubernetes Version

If you need to verify the generator against a different version of the Kubernetes upstream code-generator:

```bash
export KUBE_CODEGEN_TAG="v0.32.0"
./hack/update-codegen.sh
```

---
