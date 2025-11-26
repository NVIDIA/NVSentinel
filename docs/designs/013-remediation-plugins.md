# ADR-013: Architecture — Remediation Plugins

## Context

NVSentinel maintains a list of `RecommendedAction`'s in the health event protobuf definition.

Janitor currently uses an [internal package](https://github.com/NVIDIA/NVSentinel/tree/64277d288b60074e96b921d9e22107ea038960f1/janitor/pkg/csp) for providing remediation workflows. We cannot enumerate every possible cloud service provider's implementation of RebootNode, TerminateNode, etc. Therefore, we need to allow for NVSentinel end users to provide a "backend" for Janitor controllers to call out to do the requested action.

## Decision

Change the CSP interface package to an external gRPC service.

## Implementation

- The `janitor/pkg/csp` package will be re-implemented as a gRPC service
  - The [interface](https://github.com/NVIDIA/NVSentinel/blob/64277d288b60074e96b921d9e22107ea038960f1/janitor/pkg/model/csp.go#L29-L39) will stay the same
- Janitor will include a new config field `cspPluginHost`

```yaml
global:
  cspPluginHost: csp-plugin-default.nvsentinel.svc.cluster.local
rebootNodeController:
  cspPluginHost: csp-plugin-reboot-specific.nvsentinel.svc.cluster.local
```

- This repository will include usable plugins (code, build pipelines, artifacts) for:
  - AWS
  - Azure
  - GCP
  - OCI

- <Describe how the decision will be implemented operationally and/or in code>
- <Call out key locations such as directories, services, or pipelines>
- <Explain how this integrates with existing tooling and processes>

## Rationale

- We want to enable end users to define their own remediation behavior
- gRPC provides a strong contractual interface for multiple projects to coordinate communication
- Publishing protobuf definitions via this repository gives end users strong codegen ability for developing their own plugins

- <Reason 1 (e.g., operational simplicity)>
- <Reason 2 (e.g., security/compliance)>
- <Reason 3 (e.g., developer experience)>

## Consequences

### Positive
- End users can define remediation behavior in accordance to their running environment while still maintaining parity with upstream NVSentinel developments in control loops

### Negative
- Requires a build/deploy pipeline for new plugins
- External plugins are defined outside of this repository, making deploy bundling (e.g. this repository's helm chart) slightly more difficult

### Mitigations
- Maintain the gRPC proto definitions in `NVIDIA/NVSentinel` so end users have codegen capabilities

## Alternatives Considered

### <Alternative A>
**Rejected** because: <reasons>

### <Alternative B>
**Rejected** because: <reasons>

## Notes

- <Any additional notes, clarifications, or non-goals>

## References (Optional)

- <Links to related ADRs or external docs>
