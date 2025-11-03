# GitHub Copilot Instructions for NVSentinel

## Project Overview

NVSentinel is a GPU Node Resilience System for Kubernetes that automatically detects, classifies, and remediates hardware and software faults in GPU nodes. It's designed for high-performance computing environments running NVIDIA GPUs.

**Status**: Experimental/Preview Release - APIs and configurations may change.

## Architecture & Technologies

### Core Technologies
- **Language**: Go 1.25+ (primary), Python 3.10+ (monitoring tools)
- **Container Platform**: Kubernetes 1.25+
- **Deployment**: Helm 3.0+, Tilt (development)
- **Storage**: MongoDB (event store with change streams)
- **Communication**: gRPC with Protocol Buffers
- **GPU Monitoring**: NVIDIA DCGM (Data Center GPU Manager)

### Project Structure
```
├── health-monitors/          # Pluggable health detection modules
│   ├── gpu-health-monitor/   # DCGM-based GPU monitoring (Python)
│   ├── csp-health-monitor/   # Cloud provider health checks (Go)
│   └── syslog-health-monitor/ # System log analysis (Go)
├── health-events-analyzer/   # Event classification and routing
├── fault-quarantine/  # Node isolation (cordon)
├── node-drainer/      # Workload eviction
├── fault-remediation/ # Break-fix automation
├── labeler/           # Node labeling (DCGM version, driver status, Kata detection)
├── janitor/                  # State cleanup and maintenance
├── platform-connectors/      # CSP integration (GCP, AWS, Azure)
├── commons/                  # Shared utilities
├── data-models/             # Protocol Buffer definitions
├── store-client/        # MongoDB client library
└── distros/kubernetes/      # Helm charts
```

## Coding Standards

### Go Code Guidelines

#### Module Organization
- Each service is a separate Go module with its own `go.mod`
- Use semantic import versioning
- Keep dependencies minimal and up-to-date
- Use `commons/` for shared utilities across modules

#### Code Style
- Follow standard Go conventions (gofmt, golint)
- Use structured logging via `log/slog`
- Error handling: wrap errors with context using `fmt.Errorf("context: %w", err)`
- Within `retry.RetryOnConflict` blocks, return errors **without wrapping** to preserve retry behavior
- Use meaningful variable names (`synced` over `ok` for cache sync checks)

#### Kubernetes Integration
- Use `client-go` for Kubernetes API interactions
- Prefer informers over direct API calls for watching resources
- Use `envtest` for testing Kubernetes controllers (not fake clients)
- Implement proper shutdown handling with context cancellation

#### Testing Requirements
- Use `testify/assert` and `testify/require` for assertions
- Write table-driven tests when testing multiple scenarios
- Use `envtest` for integration tests with real Kubernetes API
- Test coverage: aim for >80% on critical paths
- Name tests descriptively: `TestFunctionName_Scenario_ExpectedBehavior`

### Python Code Guidelines
- Use Poetry for dependency management
- Follow PEP 8 style guide
- Use Black for formatting
- Type hints required for all functions
- Use dataclasses for structured data

### Protobuf Guidelines
- Define messages in `data-models/protobufs/`
- Use semantic versioning for breaking changes
- Include comprehensive comments for all fields
- Generate code with: `make protos-generate`

## Development Workflows

### Building & Testing
```bash
# Lint and test all modules
make lint-test-all

# Lint specific module
cd labeler && make lint

# Test specific module
cd health-events-analyzer && make test

# Build container images (uses ko)
make images

# View all make targets
make help
```

### Version Management
- All tool versions centralized in `.versions.yaml`
- Use `yq` to read versions in scripts
- Update versions in one place, propagates everywhere
- Check versions: `make show-versions`

### Local Development with Tilt
```bash
cd tilt
tilt up  # Start local development environment
```

## Kata Containers Detection

The labeler implements Kata Containers detection:

### Detection Architecture
- **Input labels** (on nodes): `katacontainers.io/kata-runtime` (default) + optional custom label
- **Output label** (set by labeler): `nvsentinel.dgxc.nvidia.com/kata.enabled: "true"|"false"`
- **Truthy values**: `"true"`, `"enabled"`, `"1"`, `"yes"` (case-insensitive)
- **Lifecycle separation**: Pod events → DCGM/driver labels, Node events → kata labels

### DaemonSet Variants
- Separate DaemonSets for kata vs regular nodes
- Selection via `nodeAffinity` based on kata.enabled label
- Different volume mounts:
  - Regular: `/var/log` (file-based logs)
  - Kata: `/run/log/journal` and `/var/log/journal` (systemd journal)

## Important Patterns

### Error Handling in Retry Loops
```go
// ✅ CORRECT - Return error as-is for retry logic
err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
    _, err := client.Update(ctx, obj, metav1.UpdateOptions{})
    return err  // Don't wrap!
})

// ❌ WRONG - Wrapping breaks retry detection
err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
    _, err := client.Update(ctx, obj, metav1.UpdateOptions{})
    return fmt.Errorf("failed: %w", err)  // Breaks retry!
})
```

### Informer Usage
```go
// Use separate informers for different resource types
podInformer := factory.Core().V1().Pods().Informer()
nodeInformer := factory.Core().V1().Nodes().Informer()

// Extract event handler setup into helper methods
func (l *Labeler) registerPodEventHandlers() error {
    _, err := l.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
        // handlers...
    })
    return err
}
```

### Testing with envtest
```go
// Use real Kubernetes API server for tests
testEnv := &envtest.Environment{
    CRDDirectoryPaths: []string{},
}
cfg, err := testEnv.Start()
// Create clients from cfg
// Run tests
testEnv.Stop()
```

## Documentation Standards

### Code Comments
- Package-level godoc for all packages
- Function comments for exported functions
- Inline comments for complex logic only
- TODO comments should reference issues

### Helm Chart Documentation
- Document all values in `values.yaml` with inline comments
- Include examples for non-obvious configurations
- Note truthy value requirements where applicable
- Explain DaemonSet variant selection logic

## Security & Compliance

### Licensing
- All files must include Apache 2.0 license header
- Use `make license-headers-add` to add headers
- No GPL or viral licenses in dependencies

### Security
- Report security issues via SECURITY.md process
- Never commit secrets or credentials
- Use Kubernetes secrets for sensitive data
- Scan dependencies: automated via CI

## CI/CD Pipeline

### GitHub Actions Workflows
- **PR Checks**: lint, test, build for all modules
- **Release**: automated image builds and Helm chart publishing
- **Vulnerability Scanning**: daily Trivy scans uploaded to Security tab
- **Provenance**: SLSA attestation for all artifacts

### Container Images
- Built with `ko` (Go) and `docker` (Python)
- Multi-architecture support (amd64, arm64)
- Published to `ghcr.io/nvidia/nvsentinel/*`
- Signed with Sigstore cosign

## Common Tasks

### Adding a New Health Monitor
1. Create directory in `health-monitors/`
2. Implement gRPC service from `data-models/protobufs/`
3. Add Makefile with standard targets
4. Create Helm chart in `distros/kubernetes/nvsentinel/charts/`
5. Register in platform-connectors for CSP integration
6. Add to CI pipeline workflows

### Updating Dependencies
```bash
# Update Go dependencies
make gomod-tidy

# Update Python dependencies
cd health-monitors/gpu-health-monitor
poetry update
```

### Adding New Node Labels
1. Update labeler logic in `pkg/labeler/labeler.go`
2. Add tests in `labeler_test.go`
3. Document in Helm chart values
4. Update KATA_TESTING.md if kata-related

## Debugging Tips

### Local Development
- Use Tilt for rapid iteration
- Check logs: `kubectl logs -n nvsentinel <pod>`
- Port-forward for direct access: `kubectl port-forward -n nvsentinel svc/janitor 8080:8080`
- MongoDB Compass for event store inspection

### Common Issues
- **Informer not syncing**: Check RBAC permissions
- **Labels not updating**: Verify labeler is running and has node permissions
- **DaemonSet not scheduling**: Check node selectors and labels
- **Kata detection failing**: Verify input labels on nodes

## References

- [DEVELOPMENT.md](../DEVELOPMENT.md) - Detailed development guide
- [CONTRIBUTING.md](../CONTRIBUTING.md) - Contribution guidelines
- [README.md](../README.md) - Project overview
- [docs/](../docs/) - Architecture documentation
