# NVIDIA Device API: Code Generator

The `code-generator` module contains custom Golang code-generators used to implement [Kubernetes-style API types](https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md) backed by **gRPC transport**.

These generators enable the creation of native, versioned Go clients that communicate over Unix Domain Sockets (UDS) instead of the standard Kubernetes REST API.

---

## Structure

* **Orchestration Logic**: `kube_codegen.sh` is a bash library that provides a functional interface for managing the full generation lifecycle (Protos, DeepCopy, Goverter, and Clients).
* **Customized Generator**: A modified version of `client-gen` (located in `cmd/client-gen`) that replaces standard REST/JSON templates with gRPC-specific logic for the NVIDIA Device API.

---

## Usage

This module is primarily orchestrated by the root `Makefile` via `make code-gen`. The `kube_codegen.sh` script is designed to be **sourced** by other build scripts, not executed directly. See [client-go/hack/update-codegen.sh](../client-go/hack/update-codegen.sh) for an implementation example.

### Available Functions

- `kube::codegen::gen_proto_bindings`: Scans for `.proto` files and generates Go bindings (`.pb.go`) and gRPC interfaces (`_grpc.pb.go`).
- `kube::codegen::gen_helpers`: Runs upstream generators (`deepcopy`, `defaulter`, `validation`, `conversion`, [Goverter](https://github.com/jmattheis/goverter)).
- `kube::codegen::gen_client`: Compiles the customized `client-gen` binary and executes it to produce the Kubernetes-style stack: **Clientset**, **Listers**, and **Informers**.

---

## Configuration

Tool versions are managed in the central [`.versions.yaml`](../.versions.yaml) file at the repository root. See the [Configuration Guide](CONFIGURATION.md) for details on version pinning and environment variable overrides.

---

## Modifying the Generator

If you need to change the behavior of the generated client (e.g., adding default gRPC timeout logic or custom interceptors), modify the Go templates found in `cmd/client-gen/generators`. Detailed instructions can be found in the [client-gen README](./cmd/client-gen/README.md).

---
