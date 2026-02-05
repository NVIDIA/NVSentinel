# NVIDIA Device API: API Definitions

The `api` module contains the canonical API definitions for the **NVIDIA Device API**. It serves as the single source of truth for resource schemas, gRPC wire formats, and Kubernetes-native Go types.

---

## Structure

* **`device/`**: Contains the **Kubernetes Resource Model (KRM)** definitions. These types implement the `runtime.Object` interface.
* **`proto/`**: Contains the **Language-Agnostic Definitions**. Protobuf messages and gRPC service definitions that define the node-local communication contract.
* **`gen/go/`**: Contains the **Bindings**. The output of the `protoc` compiler (`.pb.go` and `_grpc.pb.go`).

---

## Code Generation

This module relies on three distinct generation phases to maintain type safety:

1. **Protobuf**: Compiles `.proto` files into Go structs.
2. **DeepCopy**: Generates `zz_generated.deepcopy.go` to support Kubernetes object manipulation.
3. **Conversion**: Generates `zz_generated.goverter.go` to map between Protobuf messages and KRM Go types (using Goverter).

> [!NOTE]
> While the pipeline is automated, the **mapping** between Protobufs and Kubernetes types is manually defined in `api/device/${VERSION}/converter.go`. This file serves as the configuration for Goverter, allowing you to define custom transformation rules for fields. 

To run the full pipeline:

```bash
make code-gen
```

---

## Development

Refer to this module's [Development Guide](DEVELOPMENT.md) for instructions on adding new fields, defining resources, or updating the conversion mapping.

---
