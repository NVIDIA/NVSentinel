# Recovering an analyzer condition

Annotation recovery is opt-in for configured aggregation rules, and it requires MongoDB. It does not verify hardware health for you. After you repair and check the affected hardware, record that verification on the Kubernetes Node. An approved automation can also record it.

The analyzer publishes the healthy derived event. Then platform-connectors updates the node condition. If fault-quarantine quarantined the node for this fault, fault-quarantine releases that quarantine.

## Enable recovery

Enable Node watches and namespaced Event reporting in your Helm values:

```yaml
health-events-analyzer:
  nodeRecovery:
    enabled: true
```

Add a recovery block to each rule that you want to recover. Use a different annotation key for each rule. This example is for a rule that detects repeated failures on one GPU:

```toml
[rules.recovery]
annotation_key = "recovery.nvsentinel.nvidia.com/repeated-xid"
scope = "entity"
entity_types = ["GPU_UUID"]
```

Keep the existing stages and other settings of the rule. Set `entity_types` to the identity that the rule uses. For example, a GPC/TPC rule can require `GPU_UUID`, `GPC`, and `TPC`. A NIC rule needs the entity type that its monitor supplies. A node-wide or cross-GPU rule uses `scope = "node"` and does not set `entity_types`. Health events that do not have the required identity fields stay on the existing manual recovery path.

The default fault-quarantine configuration has an enabled ruleset that cordons the node for each fatal derived event. A derived event is fatal if its rule recommends an action other than `NONE`. All default rules emit fatal derived events, except `RepeatedXID31OnDifferentGPU`, `RepeatedXID13OnDifferentGPCAndTPC`, `XIDErrorSoloNoBurst`, and `XID74Reg4ECCError`. For a non-fatal derived event, or for a different quarantine, configure your own fault-quarantine rule. The rule must match the `health-events-analyzer` agent and the name of the analyzer rule (`checkName`). Recovery releases the quarantine through the normal healthy-event handling of fault-quarantine.

Rules without a recovery block do not get automatic recovery behavior. There is no healthy-source trigger. Specifically, if XID error logs stop, this is not an explicit healthy transition. The built-in XID burst detector is not part of these configurable mappings.

## Request recovery

Do these steps in sequence:

1. Finish your hardware verification.
2. Record the completion time in UTC. This time must be after the derived fault that you want to clear.
3. Use that recorded time in the request, even if you submit the annotation later.

The analyzer rejects future timestamps and timestamps before the creation of the Node.

To recover all active identities of the configured rule on a verified node:

```sh
verified_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
kubectl annotate node worker-1 recovery.nvsentinel.nvidia.com/repeated-xid="$verified_at" --overwrite
```

To recover one GPU, supply JSON with the actual GPU identity and your verification time:

```sh
kubectl annotate node worker-1 recovery.nvsentinel.nvidia.com/repeated-xid='{"recoveredAt":"2026-09-27T14:30:00Z","entities":[{"entityType":"GPU_UUID","entityValue":"GPU-123"}]}' --overwrite
```

Replace the example date with your recorded verification time. The JSON selector must contain exactly one value for each configured entity type. A timestamp-only request clears every eligible active identity for this rule. If your verification did not cover all of them, do not use a timestamp-only request.

A request with no eligible persisted fault does not create a healthy derived event or a history boundary. The rule then still counts the health events from before your verification time. The request stays on the Node, and the analyzer checks it again. Thus, if an older fault reaches storage later, the analyzer can still recover it without a change to the annotation.

## Check the result

The analyzer writes Node Events in the `default` namespace, because a cluster-scoped object reference requires this namespace:

```sh
kubectl get events -n default --field-selector involvedObject.kind=Node,involvedObject.name=worker-1 --sort-by=.lastTimestamp
kubectl describe node worker-1
```

| Event reason | Meaning |
|---|---|
| `RecoveryCompleted` | The datastore stored the matching healthy derived events. This also applies to a replay of an already completed request. |
| `RecoverySkipped` | No persisted fault was eligible on this attempt. The analyzer checks the retained request again. |
| `RecoveryInvalid` | Fix the annotation format, timestamp, or entity selector. |
| `RecoveryFailed` | Read the analyzer logs for the store or publication error. The request stays visible. |

After the storage confirmation, normal downstream reconciliation follows. Check that the Node condition of the rule becomes `False`. Then check that fault-quarantine releases the quarantine. Another active fault can keep the node cordoned, and this is correct behavior. Rules that use `STORE_ONLY` or `STORE_AND_ANALYSE` do not request downstream remediation or condition updates.

The Prometheus counter `recovery_events_published_total{rule_name,node_name}` counts the healthy derived events that platform-connectors accepted, for each rule and node. A series starts at zero when the analyzer first tries to publish for that pair. The counter includes successful republishes while the analyzer waits for storage. It does not include rejected or failed sends, or retained requests that need no new event.

If the analyzer publishes through the node-local socket, acceptance means only that the event is in a queue. To confirm storage, use `RecoveryCompleted`. To confirm that the node is uncordoned, use the Node state.

The analyzer keeps the annotation after processing. It checks retained requests again one time each minute. This also covers additional eligible identities whose faults arrive after an earlier recovery completes. Each retry adds one store query for each configured recovery rule on an annotated node. Queueing and transient failures can delay an attempt.

For a later repair, verify the hardware again. Then replace the annotation value with the new time. An old retained timestamp cannot clear a newer derived fault. If you remove the annotation, these retries stop. Thus, before you remove it, wait until the outstanding fault events are in storage and the recovery is complete. To remove your request manually:

```sh
kubectl annotate node worker-1 recovery.nvsentinel.nvidia.com/repeated-xid-
```

If you remove the annotation, the recovery metadata that is already stored with the healthy derived event stays. That metadata prevents pre-verification health events from immediately creating the derived condition again. The metadata also stays after analyzer restarts. Keep recovery records at least as long as the queryable history of the rule. Kubernetes Events expire normally. If you need a durable operator audit, export the Kubernetes Events separately.

The analyzer retries transient failures with backoff. A permanent publication rejection stops the retries for the current request until its value changes or the analyzer restarts. Correct the underlying problem before you submit another verification. Malformed stored records cause the recovery request to fail. The analyzer continues to process new health events.
