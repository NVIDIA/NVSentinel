# Copilot Instructions for NVSentinel

This file provides repository-level instructions for GitHub Copilot to improve
code reviews and suggestions.

## Project Overview

NVSentinel is the NVIDIA Device API — a Protocol Buffer and gRPC-based API for
GPU device management. The codebase consists primarily of `.proto` definitions
and their generated Go code.

## Repository Structure

```
api/
├── proto/                    # Protocol Buffer source definitions (edit these)
│   └── device/v1alpha1/
│       └── gpu.proto
├── gen/go/                   # Generated Go code (DO NOT edit manually)
│   └── device/v1alpha1/
│       ├── gpu.pb.go
│       └── gpu_grpc.pb.go
├── go.mod
└── Makefile
```

## Code Review Guidelines

### Protocol Buffers

When reviewing `.proto` files:

- **Field naming**: Use `snake_case` for field names
- **Message/Service naming**: Use `CamelCase`
- **Documentation**: Every message, field, and RPC must have documentation
  comments explaining purpose and usage
- **Field numbers**: Never reuse or change field numbers in existing messages
- **Backwards compatibility**: New fields should be optional; avoid breaking
  changes to existing APIs
- **Style**: Follow [Google's Protocol Buffer Style Guide](https://protobuf.dev/programming-guides/style/)

### Generated Code

- Files in `api/gen/` are **auto-generated** — do not suggest edits to these
- If generated code appears outdated, suggest running `make protos-generate`

### Go Code

- Use `gofmt` formatting (enforced by golangci-lint)
- Documentation comments should wrap at 80 characters
- Follow standard Go idioms and error handling patterns
- No manual edits to `*.pb.go` or `*_grpc.pb.go` files

## Commit Standards

### Conventional Commits

All commits must follow conventional commit format:

```
feat: add new GPU condition type
fix: correct timestamp handling in conditions
docs: update API documentation
chore: update protoc-gen-go version
```

### DCO Sign-off

All commits **must** include a DCO sign-off line:

```
Signed-off-by: Name <email@example.com>
```

Use `git commit -s` to add this automatically.

## License Headers

All source files must include the Apache 2.0 license header:

```
Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
...
```

## What to Flag in Reviews

1. **Breaking API changes** without migration path
2. **Missing documentation** on proto messages/fields/RPCs
3. **Manual edits** to generated files
4. **Missing license headers**
5. **Unsigned commits** (missing DCO)
6. **Non-conventional commit messages**

## What NOT to Flag

1. Style issues in generated `*.pb.go` files
2. Import ordering in generated code
3. Line length in generated code

## Build Commands

```bash
make protos-generate  # Regenerate Go code from .proto files
make build            # Build the Go module
make lint             # Run go vet
make test             # Run tests
```

## Tool Versions

Tool versions are centralized in `.versions.yaml`. When suggesting dependency
updates, reference this file.
