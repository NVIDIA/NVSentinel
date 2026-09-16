# ADR-058: Authentication — Host-Native Metadata Collector

Status: Proposed for upstream review. The contributor approved this implementation scope; maintainer acceptance is pending.

## Context

The metadata collector writes hardware inventory before it starts pod-to-GPU mapping. A host process then fails because mapper initialization requires in-cluster credentials. The kubelet client also requires a projected ServiceAccount token.

The Kubernetes API and kubelet are separate authentication boundaries. They can use different credentials, server names, and certificate authorities.

## Decision

Add explicit kubeconfigs for the Kubernetes API and kubelet HTTPS clients. Preserve the existing in-cluster defaults when each flag is absent.

Keep the local PodResources socket and pod-to-GPU annotation flow. Do not add an inventory-only mode.

## Implementation

- Add `--kubeconfig` and `--kubelet-kubeconfig` in `metadata-collector/main.go`.
- Load explicit files with client-go. Do not select a user's default kubeconfig implicitly.
- Require authenticated, verified HTTPS for explicit configurations.
- Read the kubelet endpoint from its kubeconfig. Do not copy API server credentials or trust settings to it.
- Use client-go transports for token-file reload and client certificate handling.
- Keep PodResources socket checks, gRPC request behavior, and the consecutive-poll failure limit unchanged.
- Keep inventory, annotation schemas, and mapping decisions unchanged.

## Rationale

Separate kubeconfigs keep endpoint trust and credentials together. Existing client-go handling avoids a second credential implementation. Explicit paths prevent accidental use of administrator credentials from a home directory.

## Consequences

### Positive

- A host process can retain full pod-to-GPU mapping.
- Existing DaemonSet flags, credentials, and endpoint selection remain unchanged.
- Host connections verify server identity.

### Negative

- Operators must provision and rotate two sets of connection settings.
- File-token reload is periodic, not immediate.
- Credentials do not grant permissions by themselves.

### Mitigations

- Document required pod patch and kubelet permissions.
- Test credential separation, credential validation, TLS errors, and authorized annotation updates.
- Retain existing retries and the failure threshold.

## Alternatives Considered

### Reuse one kubeconfig for both endpoints

Rejected because API server trust and credentials need not work against the kubelet.

### Copy ServiceAccount files into their pod paths

Rejected because it hides the authentication contract and leaves certificate verification unresolved.

### Disable pod mapping

Rejected because inventory alone cannot provide workload attribution.

## Notes

This extends deployment options without replacing the DaemonSet design in ADR-010. It does not change publisher authentication from ADR-052. Credential provisioning, RBAC changes, packaging, and remediation remain out of scope.

Restart the collector after changes to kubeconfig contents or CA trust. File credentials follow client-go reload behavior.

## References

- [Issue #1836](https://github.com/NVIDIA/NVSentinel/issues/1836)
- [Platform connector out-of-cluster support](https://github.com/NVIDIA/NVSentinel/pull/1359)
- [ADR-010: Metadata Retrieval](010-metadata-retrieval.md)
- [ADR-052: Deployment Platform Connector](052-deployment-platform-connector.md)
- [Kubelet authentication and authorization](https://kubernetes.io/docs/reference/access-authn-authz/kubelet-authn-authz/)
