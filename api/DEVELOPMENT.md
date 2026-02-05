# NVIDIA Device API: API Development

---

## Workflow

Follow these steps to add new resources or update existing fields:

1. **Proto Definitions**: Update `proto/device/${VERSION}/${TYPE}.proto`. Ensure Protobuf field numbers are never reused or changed once released.
2. **Go Definitions**: Update `device/${VERSION}/${TYPE}_types.go`. Ensure you include the necessary marker comments (e.g., `// +k8s:deepcopy-gen`) required by the generators.
3. **Registration**: If adding a new **Kind**, register it in `device/${VERSION}/register.go` within the `addKnownTypes` function.
4. **Conversion Logic**: Update the mapping interface in `device/${VERSION}/converter.go`. See [Goverter](https://github.com/jmattheis/goverter) documentation for additional details.
5. **Generate**: Run `make code-gen`. This orchestrates `protoc`, `deepcopy-gen`, and `goverter` to refresh all artifacts.

---

## Conventions

### Kubernetes Resource Model (KRM)

- **Spec vs Status**: Use `Spec` for intended state and `Status` for observed state. Please refer to the [Kubernetes Resource Model](https://github.com/kubernetes/design-proposals-archive/blob/main/architecture/resource-management.md) for additional details.
- **Conditions**: Use the standard `metav1.Condition` slice in the Status block.

### Protocol Buffer Guidelines

When modifying protocol buffer definitions, adhere to the following:

- **Naming**: Use `snake_case` for field names and `CamelCase` for messages/services.
- **Documentation**: Every message and field must have a comment describing its purpose.
- **Style**: Follow the [Google Protocol Buffer Style Guide](https://protobuf.dev/programming-guides/style/)

---

## Housekeeping

If your generated files are out of sync or contain stale data, you can purge the local generated artifacts:

```bash
# Removes generated code (bindings, deepcopy, goverter)
make clean
```

> [!TIP] For broader repository issues or environment-wise troubleshooting, please refer to the [Root Development Guide](../DEVELOPMENT.md).
