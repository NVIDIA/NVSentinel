# ADR-029: Security — gRPC TLS and Authentication for Janitor-Provider Connection

## Context

The janitor controller communicates with the janitor-provider over gRPC to execute
CSP operations (reboot, terminate, readiness checks):

```text
janitor controller → gRPC :50051 → janitor-provider
```

These components may be deployed in the same namespace or different namespaces
depending on the deployment model. Regardless of namespace topology, the gRPC
channel should be secured — not all environments support Kubernetes NetworkPolicies
(e.g., OCI), and even where they are available, defense in depth is good practice.

The gRPC connection was initially implemented with `insecure.NewCredentials()` — no
encryption, no authentication. Without application-layer security, any pod in the
cluster could connect to the provider and trigger CSP operations (reboots,
terminations) without authorization.

Two security properties are needed:

1. **Encryption in transit** — prevent eavesdropping on the gRPC channel
2. **Client authentication** — verify that only the janitor controller can invoke
   provider operations

### Options Evaluated

**TLS approaches:**

| Option | Description |
|--------|-------------|
| A. cert-manager server TLS | cert-manager issues a server certificate; client verifies via CA bundle |
| B. Mutual TLS (mTLS) | Both sides get certificates; client identity via cert CN/SAN |
| C. Manual/static certificates | Pre-generated certs mounted as Secrets |
| D. Service mesh sidecar | Transparent mTLS via sidecar proxy (e.g., Linkerd, Istio) |

**Authentication approaches:**

| Option | Description |
|--------|-------------|
| A. Kubernetes ServiceAccount token | Client sends projected SA token; server validates via TokenReview API |
| B. mTLS client identity | Client cert CN/SAN as identity (coupled to TLS option B) |
| C. Shared secret / API key | Static secret shared between client and server |
| D. OIDC / external IdP | External identity provider issues tokens |

## Recommendation

This document recommends **server-side TLS via cert-manager** (TLS option A) combined
with **Kubernetes ServiceAccount token authentication** (auth option A). The client
(janitor) verifies the server's certificate via a CA bundle and attaches a projected
SA token as a Bearer token in gRPC metadata on every call. The server
(janitor-provider) validates the token against the Kubernetes TokenReview API.

## Implementation

### Architecture

```text
┌──────────────────────────────────┐          ┌──────────────────────────────────────┐
│  janitor controller              │          │  janitor-provider                    │
│                                  │          │                                      │
│  1. Load CA bundle               │          │  1. Load TLS cert+key                │
│  2. Read SA token from disk      │          │  2. Extract Bearer token             │
│  3. Attach Bearer header         │───TLS───▶│  3. TokenReview API call             │
│  4. gRPC call                    │          │  4. Validate .status.authenticated   │
│                                  │          │  5. Execute CSP operation            │
│                                  │          │                                      │
│  Projected SA token volume:      │          │  cert-manager Certificate:           │
│  /var/run/secrets/nvsentinel/    │          │  /etc/nvsentinel/janitor-provider/   │
│    csp-provider/token            │          │    tls/{tls.crt, tls.key}            │
│                                  │          │                                      │
│  CA bundle (from Secret):        │          │  RBAC:                               │
│  /etc/nvsentinel/janitor/        │          │  - create TokenReview                │
│    csp-ca/ca.crt                 │          │                                      │
└──────────────────────────────────┘          └──────────────────────────────────────┘
```

### Client Side — Janitor Controller

#### TLS Credentials (`janitor/pkg/client/grpc_tls.go`)

Reads the CA bundle from disk and creates TLS transport credentials. Because the
client creates a fresh gRPC connection for each CSP operation (see
[Client-Side CA Bundle Rotation](#client-side-ca-bundle-rotation)), the CA bundle
is read fresh every time — no watcher or caching needed.

```go
func NewCSPProviderDialOptions(caPath string, insecureMode bool) ([]grpc.DialOption, error) {
    if insecureMode {
        return []grpc.DialOption{
            grpc.WithTransportCredentials(insecure.NewCredentials()),
        }, nil
    }

    caPEM, err := os.ReadFile(caPath)
    // ... error handling ...

    certPool := x509.NewCertPool()
    certPool.AppendCertsFromPEM(caPEM)

    tlsCfg := &tls.Config{
        RootCAs:    certPool,
        MinVersion: tls.VersionTLS12,
    }

    return []grpc.DialOption{
        grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
    }, nil
}
```

Uses `crypto/tls` and `crypto/x509` from the standard library, and
`google.golang.org/grpc/credentials` already in `go.mod`. No new dependencies.

#### Token Interceptor (`janitor/pkg/client/grpc_auth.go`)

A gRPC unary client interceptor that reads the projected SA token on every call:

```go
func TokenInterceptor(tokenPath string) grpc.UnaryClientInterceptor {
    return func(ctx context.Context, method string, req, reply any,
        cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
        opts ...grpc.CallOption) error {

        tokenBytes, err := os.ReadFile(tokenPath)
        // ... error handling ...

        token := strings.TrimSpace(string(tokenBytes))
        ctx = metadata.AppendToOutgoingContext(ctx,
            "authorization", "Bearer "+token)

        return invoker(ctx, method, req, reply, cc, opts...)
    }
}
```

Key design choices:

| Choice | Rationale |
|--------|-----------|
| Re-read token on every call | Handles K8s automatic token rotation without caching logic |
| `"authorization"` header | Standard HTTP/gRPC convention for Bearer tokens |
| `strings.TrimSpace` | Defensive against trailing newlines in projected token files |

#### Controller Integration (`rebootnode_controller.go`, `terminatenode_controller.go`)

Each controller creates a fresh gRPC connection per reconciliation. This ensures
the CA bundle and SA token are read from disk on every call, so certificate and
token rotation are picked up automatically without watchers or caching (see
[Client-Side CA Bundle Rotation](#client-side-ca-bundle-rotation)).

```go
func (r *RebootNodeReconciler) dialProvider(ctx context.Context) (pb.CSPProviderClient, func(), error) {
    dialOpts, err := grpcclient.NewCSPProviderDialOptions(
        r.Config.CSPProviderCAPath,
        r.Config.CSPProviderInsecure)
    if err != nil {
        return nil, nil, fmt.Errorf("create dial options: %w", err)
    }

    if !r.Config.CSPProviderInsecure {
        tokenPath := r.Config.CSPProviderTokenPath
        if tokenPath == "" {
            tokenPath = grpcclient.DefaultSATokenPath
        }
        dialOpts = append(dialOpts,
            grpc.WithUnaryInterceptor(grpcclient.TokenInterceptor(tokenPath)))
    }

    conn, err := grpc.NewClient(r.Config.CSPProviderHost, dialOpts...)
    if err != nil {
        return nil, nil, fmt.Errorf("dial csp-provider: %w", err)
    }

    return pb.NewCSPProviderClient(conn), func() { conn.Close() }, nil
}
```

The caller uses the returned cleanup function to close the connection after use:

```go
client, cleanup, err := r.dialProvider(ctx)
if err != nil { ... }
defer cleanup()

resp, err := client.RebootNode(ctx, req)
```

Auth is only added when not in insecure mode, preserving the development workflow.

#### Configuration (`janitor/pkg/config/config.go`)

Three fields added to `GlobalConfig`, cascaded to controller-specific configs:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `CSPProviderCAPath` | `string` | `""` | Path to CA bundle for server verification |
| `CSPProviderInsecure` | `bool` | `false` | Skip TLS (development only) |
| `CSPProviderTokenPath` | `string` | `""` | Path to projected SA token (falls back to default SA mount) |

### Server Side — Janitor-Provider

#### TLS Server (`janitor-provider/main.go`)

Uses `certwatcher` from `controller-runtime` to watch the TLS certificate and key
files, enabling hot-reload when cert-manager rotates the certificate (see
[Server-Side Cert Rotation](#server-side-cert-rotation)):

```go
certPath, keyPath := os.Getenv("TLS_CERT_PATH"), os.Getenv("TLS_KEY_PATH")

if tlsEnabled {
    watcher, err := certwatcher.New(certPath, keyPath)
    // ... error handling ...
    go watcher.Start(ctx)

    tlsCfg := &tls.Config{
        GetCertificate: watcher.GetCertificate,
        MinVersion:     tls.VersionTLS12,
    }
    serverOpts = append(serverOpts,
        grpc.Creds(credentials.NewTLS(tlsCfg)))
}
```

#### TokenReview Interceptor (`janitor-provider/pkg/auth/token_review.go`)

A gRPC unary server interceptor that validates incoming tokens:

```go
func TokenReviewInterceptor(
    k8sClient kubernetes.Interface,
    audiences []string,
) grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req any,
        info *grpc.UnaryServerInfo,
        handler grpc.UnaryHandler) (any, error) {

        token, err := extractBearerToken(ctx)  // from gRPC metadata
        if err != nil {
            return nil, err  // codes.Unauthenticated
        }

        if err := validateToken(ctx, k8sClient, token, audiences); err != nil {
            return nil, err  // codes.Unauthenticated or codes.Internal
        }

        return handler(ctx, req)
    }
}
```

Token validation submits a `TokenReview` to the Kubernetes API server:

```go
review := &authv1.TokenReview{
    Spec: authv1.TokenReviewSpec{
        Token:     token,
        Audiences: audiences,
    },
}
result, err := k8sClient.AuthenticationV1().TokenReviews().Create(
    ctx, review, metav1.CreateOptions{})
```

The server checks `result.Status.Authenticated` and logs the authenticated user.

TLS and auth are independently configurable. In environments where a service mesh
already provides transport encryption, auth can be enabled without TLS at the
application layer. When neither TLS nor a mesh is present, enabling both is
recommended to avoid sending tokens over an unencrypted channel.

### Helm Chart Configuration

#### Janitor (client) — `charts/janitor/values.yaml`

```yaml
config:
  cspProvider:
    tls:
      enabled: true
      caSecretName: "janitor-provider-grpc-cert"  # Matches provider's cert secret
      insecure: false       # For local dev only
    auth:
      enabled: true
      audience: "nvsentinel-csp-provider"
      expirationSeconds: 3600
```

When `tls.enabled=true`, the chart mounts the CA Secret and injects
`cspProviderCAPath` into the config. When `auth.enabled=true`, it creates a projected
ServiceAccount token volume with the configured audience and expiration.

#### Janitor-Provider (server) — `charts/janitor-provider/values.yaml`

```yaml
tls:
  enabled: true
  certDir: "/etc/nvsentinel/janitor-provider/tls"
  issuerName: "janitor-selfsigned-issuer"
  secretName: ""  # Name of the TLS Secret; defaults to "<fullname>-grpc-cert" if empty

auth:
  enabled: true
  audiences:
    - "nvsentinel-csp-provider"
```

When `tls.enabled=true`, the chart creates a cert-manager `Certificate` resource
and mounts the resulting Secret. The `issuerName` defaults to the janitor's existing
self-signed Issuer, reusing NVSentinel's existing PKI infrastructure. The `secretName`
field allows overriding the name of the TLS Secret created by the `Certificate`
resource (or referencing a pre-existing Secret when using externally managed
certificates). This keeps the server-side Secret name in sync with the client's
`caSecretName` — without it, users who bring their own certificates would have no
way to tell janitor-provider which Secret to mount.

### Certificate Rotation

cert-manager automatically rotates leaf certificates before expiry. The design needs
to account for how each component picks up rotated credentials without manual
intervention.

**SA tokens** are the simplest case: because the client interceptor re-reads the
token file on every gRPC call, kubelet's automatic token refresh is picked up
immediately with no additional machinery.

**TLS certificates** require more consideration. A naive implementation that loads
certs once at startup would serve stale credentials after cert-manager rotates the
certificate, requiring a pod restart to pick up the new cert. The approaches below
avoid that.

#### Server-Side Cert Rotation

The janitor-provider should use the `certwatcher` package from `controller-runtime`
to watch certificate files and hot-reload them via the `tls.Config.GetCertificate`
callback. This is the same pattern the janitor already uses for webhook and metrics
TLS:

```go
watcher, err := certwatcher.New(certPath, keyPath)
go watcher.Start(ctx)  // starts filesystem watch

tlsConfig := &tls.Config{
    GetCertificate: watcher.GetCertificate,
    MinVersion:     tls.VersionTLS12,
}
```

Since the janitor-provider does not use `controller-runtime`'s manager, the
`certwatcher` goroutine would be started manually (it implements `Runnable` with a
`Start(ctx)` method). This reuses a well-tested package already in the dependency
tree.

An alternative is a standalone `fsnotify` watcher, but `certwatcher` is preferred
for consistency with the janitor's existing pattern.

#### Client-Side CA Bundle Rotation

The client needs to trust the CA that signed the server's certificate. If the CA
itself rotates (e.g., the self-signed Issuer regenerates its CA key), the client
must pick up the new CA bundle or it will reject the new server cert.

Rather than introducing a file watcher or periodic reload goroutine, this design
takes a simpler approach: **create a fresh gRPC connection for each CSP operation**.
Each call to `NewCSPProviderDialOptions` reads the CA bundle from disk, so the
client naturally picks up any rotated CA bundle without additional machinery.

This works well because:

- **CSP operations are infrequent** — reboot and terminate reconciliations happen on
  the order of minutes, not milliseconds. The overhead of a new TLS handshake per
  call is negligible relative to the CSP operation itself.
- **No persistent connection to manage** — no need to handle reconnection logic,
  connection health checks, or stale connection cleanup.
- **CA rotation is automatic** — when cert-manager rotates the CA and the kubelet
  updates the mounted Secret, the next reconciliation reads the new CA bundle. No
  watcher, no atomic swap, no goroutines.
- **SA token is already read per-call** — the token interceptor re-reads the
  projected token file on every gRPC call (see above), so a per-call connection
  model is consistent with the auth approach.

See the `dialProvider()` helper in [Controller Integration](#controller-integration-rebootnode_controllergo-terminatenode_controllergo)
for the full implementation. Each reconciliation calls `dialProvider()`, which reads
the CA bundle and SA token fresh from disk, dials the provider, and returns a client
with a cleanup function.

If profiling later shows that connection overhead is a concern (unlikely given the
call frequency), a connection pool or persistent connection with a CA watcher can be
introduced as an optimization. For now, simplicity wins.

### Test Compatibility

Existing unit tests use `bufconn` with insecure credentials. The `CSPProviderInsecure`
flag preserves this:

- Tests set `CSPProviderInsecure: true` in config
- Token interceptor is skipped when insecure
- No test rewrites needed

## Rationale

- **No new dependencies** — cert-manager is already deployed across NVSentinel
  (preflight, janitor webhooks, mongodb, postgresql). The Go code already imports
  `certwatcher` for webhook TLS hot-reload. `authenticationv1.TokenReview` is a
  native K8s API via `client-go`.
- **SA tokens are free** — Kubernetes automatically mounts projected tokens into
  pods, handles rotation, and scopes them to specific audiences. No Secrets to
  create, rotate, or distribute.
- **Simpler than mTLS** — One certificate to issue (server-side only). The client
  trusts the server via CA bundle; the server trusts the client via token validation.
  mTLS requires two certificates, doubling the cert-manager resources and rotation
  surface.
- **Established K8s pattern** — This is how Kubernetes aggregated API servers and
  webhook backends authenticate. The TokenReview API is purpose-built for this use
  case.
- **Audience scoping** — Projected SA tokens are scoped to a specific audience
  (`nvsentinel-csp-provider`), so a token stolen from another volume mount cannot be
  replayed against the provider. Note that audience scoping prevents cross-service
  token replay but does not prevent a pod with its own projected volume targeting the
  same audience — RBAC controls on pod creation are the primary gate for that.

## Consequences

### Positive

- gRPC channel encrypted with TLS 1.2+
- Client identity verified on every request via K8s-native TokenReview
- Token rotation handled automatically by Kubernetes (default 1h expiry,
  kubelet refreshes at 80% lifetime)
- No manual Secret management — cert-manager and projected SA tokens are fully
  automated
- Insecure fallback preserved for local development (Tilt, kind clusters)
- All existing tests pass without modification

### Negative

- **TokenReview latency** — each gRPC call makes a round-trip to the K8s API server
  for token validation
- **cert-manager dependency** — requires cert-manager to be installed in the cluster
  (already a requirement for NVSentinel)
- **No authorization** — any valid SA token with the correct audience is accepted;
  there is no RBAC-level check on which ServiceAccount is calling

### Mitigations

- **TokenReview latency**: In practice, janitor operations are infrequent (triggered
  by node faults, not high-throughput). A few milliseconds of auth overhead per call
  is negligible against CSP API latency (seconds). If this becomes a concern, a
  token-caching layer with short TTL can be added to the server interceptor without
  changing the client.
- **cert-manager**: Already a hard dependency for NVSentinel. No additional
  operational burden.
- **Authorization**: The projected SA token is scoped to the janitor controller's
  ServiceAccount and audience. In practice, only pods with the correct SA and
  volume projection can produce a valid token. Fine-grained authorization (e.g.,
  "only allow RebootNode calls from namespace X") can be added as a future
  enhancement by inspecting `TokenReview.Status.User` in the interceptor.

## Other Options

### Mutual TLS (mTLS) via cert-manager

Both the client and server receive certificates from cert-manager. The server
validates the client's certificate CN/SAN to establish identity.

**Trade-offs**: Doubles the number of cert-manager Certificate resources (one per
side), doubles the rotation surface, and requires maintaining a mapping of allowed
client certificate identities. Provides stronger cryptographic identity than token
auth, but at the cost of additional operational complexity. mTLS is well-suited when
the client is outside the K8s cluster; for intra-cluster communication, SA tokens
achieve the same goal with less overhead.

### Manual/Static Certificates

Pre-generated TLS certificates stored as Kubernetes Secrets, manually created and
rotated by operators.

**Trade-offs**: Simplest to understand, but introduces operational burden for
certificate rotation and creates a risk of expired certificates causing outages.
Does not align with NVSentinel's existing pattern of using cert-manager for all TLS
— every other component uses cert-manager, so a manual exception creates
inconsistency.

### Service Mesh Sidecar

Deploy a service mesh (e.g., Linkerd, Istio) that injects sidecar proxies to handle
mTLS transparently between pods.

**Trade-offs**: Transparent to application code — no TLS or auth changes needed in
the janitor or provider. However, introduces a significant infrastructure dependency
for a single gRPC connection. NVSentinel should not require a service mesh as a
prerequisite. Deployments that already have a mesh can layer it on independently
without changes to NVSentinel.

### Shared Secret / API Key

A static secret (API key) shared between the client and server, sent as a header on
every request.

**Trade-offs**: Simple to implement, but no automatic rotation and a weak identity
model (anyone with the secret can authenticate). Projected SA tokens provide stronger security properties with less
operational effort.

### OIDC / External Identity Provider

Use an external OIDC provider (e.g., Dex, Keycloak) to issue tokens for
service-to-service authentication.

**Trade-offs**: Strongest identity model with rich claims and policy support.
However, significant complexity for an intra-cluster gRPC connection between two
pods — introduces an external dependency, additional infrastructure to operate, and
latency for token issuance. Kubernetes already provides a built-in identity system
via ServiceAccount tokens that covers this use case.

## Security Properties

### What This Design Provides

| Property | Mechanism |
|----------|-----------|
| Encryption in transit | TLS 1.2+ with cert-manager-issued server certificate |
| Server identity | Client verifies server cert against CA bundle |
| Client authentication | Projected SA token validated via K8s TokenReview API |
| Token scoping | Audience-bound tokens (`nvsentinel-csp-provider`) |
| Automatic rotation | cert-manager rotates server cert; kubelet rotates SA token |
| Replay protection | Tokens are short-lived (1h default) and audience-scoped |

### What This Design Does NOT Provide

- **Fine-grained authorization** — any authenticated client can call any RPC method.
  All methods (SendRebootSignal, IsNodeReady, SendTerminateSignal) are equally
  accessible to any valid token.
- **Payload integrity beyond TLS** — no application-level message signing. TLS
  provides integrity for the channel, but there is no end-to-end signature on
  individual messages.
- **Audit logging** — authenticated identity is logged at `slog.Info` level but not
  persisted to an audit trail. This can be added by extending the server interceptor.

## Notes

- The `CSPProviderInsecure` flag is intended for local development only. Production
  deployments should always enable TLS + auth.
- The default audience (`nvsentinel-csp-provider`) is configurable via Helm values
  to support environments with custom audience requirements.
- cert-manager `Certificate` resources use the janitor's existing self-signed Issuer
  (`janitor-selfsigned-issuer`) by default, but can be pointed at a cluster-wide
  Issuer if one exists.

## References

- [ADR-019: Janitor GPU Reset](019-janitor-gpu-reset.md) — prior art for security
  model (privileged Jobs, RBAC scoping)
- [ADR-028: Generic Bare-Metal Reboot Provider](028-generic-baremetal-reboot-provider.md)
  — RBAC changes for provider write access
- [Kubernetes TokenReview API](https://kubernetes.io/docs/reference/kubernetes-api/authentication-resources/token-review-v1/)
- [Kubernetes Projected Service Account Tokens](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/#serviceaccount-token-volume-projection)
- [cert-manager Certificate Resources](https://cert-manager.io/docs/usage/certificate/)
- [gRPC Authentication Guide](https://grpc.io/docs/guides/auth/)
