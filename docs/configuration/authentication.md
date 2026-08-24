# Authentication Configuration

Operator reference for how health-event publishers authenticate to
platform-connector: who may submit health events, and about which node.

> The older `docs/platform-connectors.md` predates these changes and describes
> the previous behaviour. Where it disagrees with this document, this document
> is correct.

---

## Health-event publishers (platform-connector)

```yaml
global:
  platformConnectorAuth:
    enabled: true
    audience: "platform-connector.nvsentinel.nvidia.com"
    tokenExpirationSeconds: 3600
    tokenMountPath: "/var/run/secrets/nvsentinel/platform-connector"
    crossNodeServiceAccounts: []
```

### `enabled`

Must be a **real YAML boolean** and must be set explicitly — there is no
default. A quoted `"false"` is refused rather than read as `true`, and `null`
or `0` is refused rather than silently disabling enforcement. The connector
also refuses to start if the ConfigMap omits the key, so an old chart driving
new images fails loudly instead of guessing whether authentication is required.

Disabling this lets any caller that can reach the connector's socket submit an
event naming any node. It is not a supported production configuration.

### `audience`

The single audience every publisher's token is minted for, and that
platform-connector requires TokenReview to echo back in `status.audiences`.
A token issued for a different service is not accepted.

### `tokenExpirationSeconds`

Token lifetime. Kubernetes rejects a projected token lifetime below **600**
seconds or above **2^32**, and rejects fractional values, so the chart enforces
`600 <= x <= 4294967296` and whole numbers at render time rather than letting
the API server reject the pod at creation.

### `tokenMountPath`

Directory each publisher mounts the token at; the file is `<mountPath>/token`.

### `crossNodeServiceAccounts`

**Additional** canonical usernames permitted to report health events about nodes
other than the one they run on. Every entry grants the ability to have any node
in the cluster cordoned, drained and rebooted — add sparingly.

The bundled cluster-scoped monitors — `csp-health-monitor`,
`kubernetes-object-monitor`, `slurm-drain-monitor` and `health-events-analyzer`
— are **derived from the release namespace** for whichever of them are enabled.
They do not belong in this list, and installing into a namespace other than
`nvsentinel` needs no edit.

List only publishers this chart does not ship. Entries are matched **verbatim**
against the username TokenReview reports:

- form `system:serviceaccount:<namespace>:<name>`
- namespace: DNS-1123 label, ≤63 characters
- name: DNS-1123 subdomain, ≤253 characters
- no leading, trailing or internal whitespace; no uppercase

Nothing is trimmed or normalised for you. An entry that does not match exactly
can never equal a real username, so the chart and the connector both reject it
rather than accepting a rule that silently matches nothing.

Omitting the key means "no additional callers". Writing `null` is refused,
because null and "none" are not the same statement.

### The per-caller rule

A caller presenting a token is asked two independent questions.

**1. Is the token being presented where it was issued?** If it carries a node
claim, that claim must name this connector's own node — for every caller,
allowlisted or not. Rejected as `node_claim_mismatch`.

**2. May this identity name other nodes?** Only if it is on the allowlist.
Otherwise it is pinned to the connector's node, and an event naming another node
is rejected as `node_mismatch`.

An allowlisted caller must also present a token bound to a **running pod on a
scheduled node**. `kubectl create token <sa> --audience=…` without
`--bound-object-ref` produces a token with neither claim; it is refused for
own-node events too, not merely downgraded.

Callers presenting **no** token are accepted and pinned to the connector's node.
Reaching the socket already granted that much.

### Where to run cross-node monitors

Their pods hold a credential platform-connector accepts for any node name. Run
them on a system or control-plane node pool rather than GPU nodes serving
tenants, using whichever of `nodeSelector`, `affinity` or tolerations your
cluster already uses.

### Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `platform_connector_auth_decisions_total` | `decision` = `node_local` \| `cross_node` | Scope granted to an accepted batch. |
| `platform_connector_auth_violations_total` | `reason` (below) | Batches rejected by the interceptor. |
| `platform_connector_auth_node_claim_total` | `result` = `verified` \| `absent` | Whether the token carried a node claim. |

`reason` values:

| Reason | Meaning |
|---|---|
| `node_mismatch` | A node-local caller named a different node. |
| `node_claim_mismatch` | The token was issued on another node. |
| `unbound_cross_node_token` | An allowlisted caller presented a token bound to no pod. |
| `cross_node_claim_absent` | Pod-bound, but the pod was never scheduled. |
| `missing_node_name` | An event carried no node name and none could be stamped. |
| `token_invalid` | TokenReview rejected the token. |
| `malformed_credentials` | The authorization header was duplicated, or did not use the Bearer scheme. A *completely absent* header is not a violation — that caller is accepted and pinned to the connector's node. |
| `validator_unavailable` / `validator_timeout` / `validator_error` | The API server could not be reached, or returned no identity. |

A healthy cluster reports zero violations. A sustained non-zero
`validator_unavailable` is an API-server problem, not a caller problem.

---

## Migration

| Old | New |
|---|---|
| `crossNodeServiceAccounts` listing the bundled monitors | Remove them; they are derived from the release namespace. |

Upgrade the chart and the images together. New images against an old chart do
not start: the connector requires `enableNodeBindingAuth` to be present, and an
old chart does not write it.
