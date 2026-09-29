# Recovering an analyzer condition

Annotation recovery is opt-in for configured aggregation rules and currently requires MongoDB. It does not verify hardware health for you. After repairing and checking the affected hardware, an operator or approved automation records that verification on the Kubernetes Node. The analyzer publishes the healthy derived event; platform-connectors and fault-quarantine handle condition updates and uncordoning.

## Enable recovery

Enable Node watches and namespaced Event reporting in your Helm values:

```yaml
health-events-analyzer:
  nodeRecovery:
    enabled: true
```

Add a recovery block to each rule you want to recover. Choose a different annotation key for each rule. For a rule that detects repeated failures on one GPU:

```toml
[rules.recovery]
annotation_key = "recovery.nvsentinel.nvidia.com/repeated-xid"
scope = "entity"
entity_types = ["GPU_UUID"]
```

Keep the rule's existing stages and other settings. Match `entity_types` to the identity used by that rule. A GPC/TPC rule may need `GPU_UUID`, `GPC`, and `TPC`; a NIC rule needs the entity type its monitor actually supplies. A node-wide or cross-GPU rule uses `scope = "node"` and omits `entity_types`. Events missing required identity fields remain on the existing manual recovery path.

Rules without a recovery block do not acquire automatic recovery behavior. There is no healthy-source trigger in this version. In particular, stopping XID error logs is not an explicit healthy transition. The built-in Go XID burst detector is outside these configurable mappings.

## Request recovery

First finish your hardware verification. Record its completion time in UTC, after the derived fault you want to clear. Use that recorded time in the request, even if you submit the annotation later. Future timestamps and timestamps before Node creation are rejected.

For all active identities of the configured rule on a verified node:

```sh
verified_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
kubectl annotate node worker-1 recovery.nvsentinel.nvidia.com/repeated-xid="$verified_at" --overwrite
```

For one GPU, supply JSON with the actual GPU identity and your verification time:

```sh
kubectl annotate node worker-1 recovery.nvsentinel.nvidia.com/repeated-xid='{"recoveredAt":"2026-09-27T14:30:00Z","entities":[{"entityType":"GPU_UUID","entityValue":"GPU-123"}]}' --overwrite
```

Replace the example date with your recorded verification time. The JSON selector must contain exactly one value for each configured entity type. A timestamp-only request clears every eligible active identity for this rule; use it only when your verification covered all of them. A request with no eligible persisted fault does not create a healthy event or a history boundary. It stays on the Node and is checked again, so an older fault that reaches storage later can still be recovered without editing the annotation.

## Check the result

Node Events are written in the `default` namespace, as required for a cluster-scoped object reference:

```sh
kubectl get events -n default --field-selector involvedObject.kind=Node,involvedObject.name=worker-1 --sort-by=.lastTimestamp
kubectl describe node worker-1
```

| Event reason | Meaning |
|---|---|
| `RecoveryCompleted` | Matching healthy derived events were stored, including a replay of an already completed request. |
| `RecoverySkipped` | No persisted fault was eligible on this attempt; the retained request will be checked again. |
| `RecoveryInvalid` | Fix the annotation format, timestamp, or entity selector. |
| `RecoveryFailed` | Read analyzer logs for the store or publication error; the request remains visible. |

Storage confirmation is followed by normal downstream reconciliation. Check that the rule's Node condition becomes `False` and that quarantine is released. Another active fault can legitimately keep the node cordoned. Rules using `STORE_ONLY` or `STORE_AND_ANALYSE` do not request downstream remediation or condition updates.

The analyzer retains the annotation after processing and rechecks retained requests from its Node cache once a minute. This also covers additional eligible identities whose faults arrive after an earlier recovery completes. Retries add a store query per configured recovery rule on an annotated node; queueing and transient failures can delay an attempt. For a later repair, verify again and replace it with the new time. An old retained timestamp cannot clear a newer derived fault. Removing the annotation stops these retries, so wait until outstanding fault events have reached storage and recovery is complete. To remove the operator's request manually:

```sh
kubectl annotate node worker-1 recovery.nvsentinel.nvidia.com/repeated-xid-
```

Removing the annotation does not remove the recovery metadata already stored with the healthy event. That metadata prevents pre-verification events from immediately recreating the derived condition and survives analyzer restarts. Keep recovery records at least as long as the rule's queryable history. Kubernetes Events follow normal expiration and should be exported separately if you need a durable operator audit.

Transient failures retry with backoff. A permanent publication rejection stops retries for the current request until its value changes or the analyzer restarts. Correct the underlying problem before submitting another verification. Malformed stored records fail the recovery request without changing the source processor's existing checkpoint policy.

See [ADR-059](designs/059-derived-condition-recovery.md) for the design and failure handling.
