# NVIDIA Device API: Go Client Development

> [!IMPORTANT]
> This module relies heavily on **code generation**. For the high-level generation pipeline overview and the standard development loop, please refer to the [Root Development Guide](../DEVELOPMENT.md).

---

## Internal Structure

- `client/`: [Generated] The versioned Clientset.
- `listers/`: [Generated] Type-safe listers for cached lookups.
- `informers/`: [Generated] Shared Index Informers.
- `nvgrpc/`: **[Manual]** The gRPC transport layer
- `version/`: **[Manual]** Version injection functionality.

---

## Local Workflow

While global changes should be driven from the root Makefile, you can perform module-specific tasks here.

### Building & Testing

Unit tests in this directory focus on the manual logic in `nvgrpc` and the integrity of the generated clientset.

```bash
# Verify type safety of generated code and manual logic
make build

# Run unit tests
make test
```

### Modifying Generated Code

> [!WARNING]
> **Do not edit generated files directly.**
>
> Files in `client/`, `listers/`, and `informers/` contain a `DO NOT EDIT` header and are overwritten every time you run `make code-gen`.
>
> To change behavior:
>  - **API Definitions**: Modify the Proto or Go types in `../api`.
>  - **Client Logic**: Modify the templates in `../code-generator/cmd/client-gen`.
>  - **Transport & Connection**: Modify the gRPC logic in `nvgrpc`.

## Housekeeping

If your generated files are out of sync or contain stale data, you can purge the local generated artifacts:

```bash
# Removes generated code (client, listers, informers)
make clean
```

> [!TIP] For broader repository issues or environment-wise troubleshooting, please refer to the [Root Development Guide](../DEVELOPMENT.md).

---
