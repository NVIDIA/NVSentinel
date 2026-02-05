# NVIDIA Device API: Client Generator

This is a modified version of the Kubernetes [client-gen](https://github.com/kubernetes/code-generator/tree/master/cmd/client-gen).

It generates a typed, versioned Go **Clientset**. Unlike the standard generator which defaults to REST/HTTP, this version creates clients that use **gRPC transport** to communicate with the node-local NVIDIA Device API.

---

## Workflow

### 1. Tagging API Types

In your API definition files (e.g., `api/device/v1alpha1/types.go`), mark the types (e.g., `GPU`) that you want to generate clients for using `// +genclient`:

* `// +genclient` - Generate default client verb functions (`create`, `update`, `delete`, `get`, `list`, `patch`, `watch`, and `updateStatus`).
* `// +genclient:nonNamespaced` - Generate verb functions without namespace parameters.
* `// +genclient:onlyVerbs=<verb>,<verb>` - Generate **only** the listed verbs.
* `// +genclient:skipVerbs=<verb>,<verb>` - Generate all default verbs **except** the listed ones.
* `// +genclient:noStatus` - Skip `updateStatus` verb even if the `.Status` struct field exists.
* `// +groupName=policy.authorization.k8s.io` – Overrides the API group name (defaults to the package name).
* `// +groupGoName=AuthorizationPolicy` – Sets a custom Golang identifier to de-conflict groups (defaults to the upper-case first segment of the group name).

### 2. Running the Generator

> [!NOTE]
> This binary is typically invoked via [kube_codegen.sh](../../README.md).

When invoked, the generator resolves packages by combining `--input-base` (Go types) and `--proto-base` (gRPC stubs) to create unified transport logic.

### 3. Adding Expansion Methods

`client-gen` generates standard methods. To add custom logic (e.g., specialized filters) to a client, create a file named `${TYPE}_expansion.go` in the generated directory. The generator will automatically detect and embed the `${TYPE}Expansion` interface into the client.

---

## Output Structure

The generator produces a tiered directory structure. By default, the layout follows this pattern:

- **Clientset**: Found at `${output-dir}/${versioned-name}/${clientset-name}/`. The primary entry point for consumers.
- **Typed Clients**: Found at `${output-dir}/${versioned-name}/${clientset-name}/typed/${group}/${version}/`. Contains the actual gRPC-backed implementation for each resource type.
- **Fake Clientset**: Found at `${output-dir}/${versioned-name}/${clientset-name}/fake/`. Used for unit testing without a running gRPC server.
- **Listers**: Found at `${output-dir}/listers/`. Provides a read-only, cached view of resources for high-speed lookups.
- **Informers**: Found at `${output-dir}/informers/`. Keeps the local cache updated via gRPC watch streams and provides event triggers for controllers.

---

## Flags

| Flag | Required | Description |
|------|----------|-------------|
| `--proto-base` | **Yes** | The base Go import path for the generated Protobuf stubs (e.g., `github.com/org/repo/api/gen/go`). |
| `--input` | **Yes** | Comma-separated list of groups/versions to generate (e.g., `device/v1alpha1,networking/v1`). |
| `--input-base` | **Yes** | Base import path for the API types (e.g., `github.com/org/repo/api`). |
| `--output-pkg` | **Yes** | Go package path for the generated files (e.g., `github.com/org/repo/client`). |
| `--output-dir` | **Yes** | Base directory for the output on disk (e.g., `./client`). |
| `--boilerplate` | No | Path to a header file (copyright/license) to prepend to generated files. Default: `hack/boilerplate.go.txt`. |
| `--clientset-name` | No | Name of the generated package/directory. Default: `clientset`. |
| `--versioned-name` | No | Name of the versioned clientset directory. Default: `versioned`. |
| `--plural-exceptions`| No | Comma-separated list of `Type:PluralizedType` overrides. |
| `--fake-clientset` | No | Generate a fake clientset that can be used in tests. Default: `true` |
| `--prefers-protobuf` | **N/A** | **Removed** This generator assumes Protobuf/gRPC support is always enabled. |
| `--apply-configuration-package` | **N/A** | **Not Implemented** |

---
