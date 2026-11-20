# Authentication Configuration

Operator reference for how health-event publishers authenticate to platform-connector: who can submit health events, and about which node.

---

## Health-event publishers (platform-connector)

```yaml
global:
  platformConnectorAuth:
    enabled: true
    mode: enforce
    failOpenOnUnavailable: false
    audience: "platform-connector.nvsentinel.nvidia.com"
    tokenExpirationSeconds: 3600
    tokenMountPath: "/var/run/secrets/nvsentinel/platform-connector"
    crossNodeServiceAccounts: []
```

### `enabled`

Must be a **real YAML boolean**. The chart default is `true`. A `null` or non-boolean value fails the render. As a result, the chart does not read a quoted `"false"` as `true`, and `null` or `0` does not silently disable enforcement. If the ConfigMap omits the key, platform-connector does not start. It does not guess whether authentication is required.

If you disable it, any caller that can reach the socket of platform-connector can submit a health event that names any node. NVSentinel does not support this configuration in production.

### `mode`

`enforce` (default) rejects a request that violates the node-binding rule. `audit` validates every request and increments the same `platform_connector_auth_violations_total` counters by reason, but lets the request through instead of rejecting it.

Use `audit` to roll out node binding against real traffic:

1. Run `audit` for a period.
2. Confirm that the violation counters stay at zero.
3. Switch to `enforce` with evidence.

This way, you do not learn in production what `enforce` breaks. `audit` still requires `enabled: true`. In `audit` mode, platform-connector still validates every request. It still records a validator failure (including `validator_unavailable` and `validator_timeout`), but the failure does not cause a rejection. `audit` changes what happens after platform-connector detects a violation, not whether platform-connector checks the requests.

`mode` does not affect one rejection. In both `audit` and `enforce`, platform-connector always rejects a health event with no node name from a caller with **verified** cross-node scope (`missing_node_name`). No downstream component can handle an empty node name. As a result, a forwarded event of this type is not a useful preview of what `enforce` does. It is only an event in the datastore that `enforce` can never produce.

This rule does not cover a caller with a **degraded** node-local scope under `failOpenOnUnavailable`. No check verified the identity of that caller as cross-node. As a result, platform-connector stamps a blank name from that caller like the blank name of any other node-local caller. See [`failOpenOnUnavailable`](#failopenonunavailable).

### `failOpenOnUnavailable`

Distinguishes a validator that never reached a verdict from a validator that reached a verdict and rejected the credential. `validator_unavailable` and `validator_timeout` (the API server was unreachable, or the call timed out) say nothing about the caller. They are not evidence of a forged token. They are only evidence about the availability of the validator. `token_invalid` (a rejected or malformed credential) does say something about the caller. Under `mode: enforce`, platform-connector always rejects it, whatever the value of this setting.

The default is `false`: an unreachable validator rejects the request. To use a **degraded** node-local scope instead, set it to `true`. This scope is a guess, not a verified identity. With it, a short control-plane problem limits publishers to their own node, and does not block their health events outright:

- In either `mode`, platform-connector accepts and stamps a health event with a blank or matching node name, exactly as for a verified node-local caller. This is the common case for this setting. Most publishers present a token but leave the node name blank, so that platform-connector writes it.
- Under `mode: enforce`, platform-connector refuses a health event that names a *different* node. It uses the retryable code `Unavailable`, the same code that the validator returned. It does not refuse it as `node_mismatch` with `PermissionDenied`. The caller can be an allowlisted cross-node publisher that platform-connector cannot verify because of the outage, so the event is not necessarily a real mismatch. For this reason, platform-connector does not count it as `node_mismatch`, which is one of the reasons that you alert on as suspected credential abuse. It also does not reject it in a way that publishers treat as non-retryable. The publisher retries it after the validator recovers, and does not drop it permanently.
- Under `mode: audit`, platform-connector records that same `Unavailable` for a health event that names a *different* node. Then it lets the event through, like any other audited violation. As a result, it forwards the event with the node name that the event carried.

### `audience`

The single audience for which the token of every publisher is minted. TokenReview must return this audience in `status.audiences`, as platform-connector requires. As a result, platform-connector does not accept a token issued for a different service.

### `tokenExpirationSeconds`

Token lifetime. Kubernetes rejects a projected token lifetime below **600** seconds or above **2^32**. It also rejects fractional values. For this reason, the chart enforces `600 <= x <= 4294967296` and whole numbers at render time. This prevents a rejection of the pod by the API server at creation.

### `tokenMountPath`

Directory where each publisher mounts the token. The file is `<mountPath>/token`.

### `crossNodeServiceAccounts`

**Additional** canonical usernames that can report health events about nodes other than the node that they run on. Every entry gives the ability to have any node in the cluster cordoned, drained and rebooted. Add as few entries as possible.

The chart derives the usernames of the bundled cluster-scoped monitors **from the release namespace**, for each enabled monitor. These monitors are `csp-health-monitor`, `kubernetes-object-monitor`, `nvcre-certification-monitor`, `slurm-drain-monitor` and `health-events-analyzer`. The chart adds the `lifecycle-manager` ServiceAccount in the same way when `global.lifecycleManager.enabled` and `lifecycle-manager.controllers.maintenanceRequest.enabled` are both `true`. Do not put them in this list. An installation into a namespace other than `nvsentinel` needs no edit.

List only publishers that this chart does not ship. Each entry must match **verbatim** the username that TokenReview reports:

- form `system:serviceaccount:<namespace>:<name>`
- namespace: DNS-1123 label, ≤63 characters
- name: DNS-1123 subdomain, ≤253 characters
- no leading, trailing or internal whitespace; no uppercase

NVSentinel does not trim or normalise the entries for you. An entry that does not match exactly can never equal a real username. For this reason, the chart and platform-connector both reject such an entry. They do not accept a rule that silently matches nothing.

An omitted key means "no additional callers". The chart refuses `null`, because null and "none" are not the same statement.

### The per-caller rule

For a caller that presents a token, platform-connector asks two independent questions.

**1. Does the caller present the token on the node for which the API server issued it?** If the token carries a node claim, that claim must name the node where platform-connector runs. If the claim names a different node, platform-connector rejects the request as `node_claim_mismatch`. This rule applies to every caller, allowlisted or not.

**2. Can this identity name other nodes?** Only if it is on the allowlist. Otherwise, platform-connector pins it to the node where platform-connector runs. For such a caller, platform-connector rejects a health event that names another node as `node_mismatch`.

An allowlisted caller must also present a token bound to a **running pod on a scheduled node**. `kubectl create token <sa> --audience=…` without `--bound-object-ref` produces a token with neither claim. platform-connector refuses such a token, even for own-node events. It does not downgrade the scope.

If a caller presents **no** token, platform-connector accepts it and pins it to the node where platform-connector runs. A caller that can reach the socket already has this scope.

### Where to run cross-node monitors

Their pods hold a credential that platform-connector accepts for any node name. Run them on a system or control-plane node pool, not on GPU nodes that serve tenants. Use `nodeSelector`, `affinity` or tolerations, whichever your cluster already uses.

### Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `platform_connector_auth_decisions_total` | `decision` = `node_local` \| `cross_node` | Scope granted to a batch. |
| `platform_connector_auth_violations_total` | `reason` (below) | Batches that violated the node-binding rule. Under `mode: enforce`, platform-connector rejects them. Under `mode: audit`, it records them but lets them through. |
| `platform_connector_auth_node_claim_total` | `result` = `verified` \| `absent` | Whether the token carried a node claim. |

`reason` values:

| Reason | Meaning |
|---|---|
| `node_mismatch` | A node-local caller named a different node. |
| `node_claim_mismatch` | The node claim of the token names another node. |
| `unbound_cross_node_token` | An allowlisted caller presented a token bound to no pod. |
| `cross_node_claim_absent` | Pod-bound, but the pod was never scheduled. |
| `token_missing` | Deployment platform connector only: the caller presented no token. There is no local node to pin a tokenless caller to. |
| `unbound_token` | Deployment platform connector only: the token is bound to no pod. |
| `node_claim_absent` | Deployment platform connector only: the token is bound to a pod that never scheduled, so there is no node to pin its health events to. |
| `missing_node_name` | A health event carried no node name, and platform-connector had no node name to stamp. |
| `token_invalid` | TokenReview rejected the token. |
| `malformed_credentials` | The caller duplicated the authorization header, or did not use the Bearer scheme. On the node-local socket, a *completely absent* header is not a violation. In this case, platform-connector accepts the caller and pins it to the node where platform-connector runs. On the deployment platform connector, an absent header counts as `token_missing`. |
| `validator_unavailable` / `validator_timeout` / `validator_error` | The API server was unreachable, or it returned no identity. `validator_error` also counts a permanent TokenReview failure, such as a missing `create` permission on `tokenreviews`. Under `mode: enforce`, `validator_error` always causes a rejection of the request. With `failOpenOnUnavailable: true`, `validator_unavailable` and `validator_timeout` still increment this counter. But the caller then gets a degraded node-local scope, and platform-connector does not reject the request. For how that scope treats a blank node name and a different node name, see [`failOpenOnUnavailable`](#failopenonunavailable). |

A healthy cluster reports zero violations. A sustained non-zero `validator_unavailable` is an API-server problem, not a caller problem.

---

## Deployment platform connector

The deployment platform connector reads the same node-binding settings as the DaemonSet, from the same `config.json`. `enableNodeBindingAuth`, `AuthAudience` and `AuthCrossNodeServiceAccounts` mean exactly what they mean above. You must enable node binding for the deployment platform connector. Otherwise, it does not start.

The deployment platform connector has no local node. For this reason, one thing is different: **every caller must present a token**, bound to a running pod on a scheduled node. The node named in that token is the node that the caller can report on. Only an allowlisted caller can name other nodes.

`AuthMode: audit` and `AuthFailOpenOnUnavailable` are socket settings. The deployment platform connector has no node to use as a fallback. As a result, it always enforces, and it logs a warning if you set either of them.

The chart refuses to render the deployment platform connector while node binding is disabled.

A publisher on the deployment platform connector (`publishTo: deployment`) uses the same projected token as on the socket path. The chart mounts it at `tokenMountPath` and gives the file, `<tokenMountPath>/token`, to the publisher as `HEALTH_PUBLISH_TOKEN_PATH`. The token has the same `audience` and the same lifetime, `tokenExpirationSeconds`. Thus the token of a monitor stays valid when the monitor moves from the socket to the deployment platform connector.

`crossNodeServiceAccounts` applies to both roles. The network policy of the deployment platform connector also admits publisher pods from the namespace of each entry. Such a pod must carry the label `nvsentinel.nvidia.com/health-publisher: "true"`.
