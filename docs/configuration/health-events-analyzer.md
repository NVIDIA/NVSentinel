# Health Events Analyzer Configuration

## Overview

The Health Events Analyzer evaluates MongoDB aggregation pipeline rules on incoming health events. When a rule matches, the analyzer emits a derived event with a recommended action. For example, repeated XID 48 errors on the same GPU in 24 hours cause a `CONTACT_SUPPORT` recommendation. This document describes the main Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

This setting enables or disables the deployment of the Health Events Analyzer in the cluster.

```yaml
global:
  healthEventsAnalyzer:
    enabled: false
```

### Replica Count

The number of Health Events Analyzer pod replicas to run.

```yaml
health-events-analyzer:
  replicaCount: 1
```

### Resources

Defines the CPU and memory resource requests and limits for the Health Events Analyzer pod.

```yaml
health-events-analyzer:
  resources:
    limits:
      cpu: "1"
      memory: "1Gi"
    requests:
      cpu: "1"
      memory: "1Gi"
```

### Logging

Sets the verbosity level of the Health Events Analyzer logs.

```yaml
health-events-analyzer:
  logLevel: info  # Options: debug, info, warn, error
```

### Processing Strategy

Controls whether derived events start downstream remediation actions, or NVSentinel only records them.

```yaml
health-events-analyzer:
  processingStrategy: EXECUTE_REMEDIATION  # Options: EXECUTE_REMEDIATION, STORE_AND_ANALYSE, STORE_ONLY
```

**Options:**

#### EXECUTE_REMEDIATION

The normal operating mode. When a rule matches, downstream modules can update the cluster state. They can apply node conditions, quarantine nodes, drain workloads or start remediations.

#### STORE_AND_ANALYSE

NVSentinel persists the derived events, but they do not start quarantine or remediation and do not set node conditions. The analyzer does not evaluate its own derived events. Thus, on the analyzer, this mode has the same effect as `STORE_ONLY`.

On another health monitor, this strategy has a different effect. The analyzer evaluates the raw health events of that monitor, and a rule match can emit `EXECUTE_REMEDIATION` derived events. For example, a burst of deduplicated `STORE_AND_ANALYSE` events can together satisfy a repeated-failure rule. Thus, on that monitor, the strategy prevents immediate action on the raw events, but the analyzer still correlates them.

#### STORE_ONLY

The observability-only mode. NVSentinel persists and exports derived events, but they do not change any cluster resources. Use this mode to shadow-test new or customised rules in production before you enable full remediation.

### Operator Recovery

Recovery is disabled by default. To use it with MongoDB, enable Node watches and namespaced Event reporting. Then add a `[rules.recovery]` mapping to each configured aggregation rule that needs operator recovery:

```yaml
health-events-analyzer:
  nodeRecovery:
    enabled: true
```

```toml
[rules.recovery]
annotation_key = "recovery.nvsentinel.nvidia.com/repeated-xid"
scope = "entity"
entity_types = ["GPU_UUID"]
```

For node-wide rules, use `scope = "node"` without `entity_types`. Each annotation key must be unique. In the annotation, write the time when the hardware verification completed. The analyzer keeps the annotation and reports the result as a Kubernetes Event. NVSentinel does not support healthy-source triggers or recovery with PostgreSQL. For commands, entity selection, timestamps, and result handling, see the [operator guide](../health-events-analyzer-recovery.md).

### Concurrent Event Processing

The Health Events Analyzer divides incoming health events by node name across a pool of concurrent workers. It evaluates health events for different nodes at the same time. It processes health events for the same node in strict chronological order. The analyzer moves its saved stream position past a health event only after that health event and all earlier health events are complete. Thus, after a restart, the analyzer processes each health event at least once. This method does not cause head-of-line blocking.

```yaml
health-events-analyzer:
  workers: 1       # Number of concurrent workers (default: 1)
  maxInFlight: 1000 # Maximum uncheckpointed in-flight events before backpressure (default: 1000)
  ruleConcurrency: 1 # Maximum rule queries that run at the same time for one event (default: 1)
```

#### Scaling Workers by Datastore Event Rate

For each health event, the analyzer runs rule aggregation queries on the datastore. The I/O round trips for one health event take approximately 17.5 ms on average. One worker can process a maximum of approximately 57 events/second. Throughput increases linearly with the number of workers ($\approx \text{Workers} \times 57\text{ events/s}$).

Use this reference table to configure `workers` and `maxInFlight` for the cluster size and the expected event rate:

| Offered Event Rate in DB | Recommended `workers` | Recommended `maxInFlight` | Estimated Throughput Capacity | Recommended Cluster Scale |
|---|---|---|---|---|
| $< 50$ events/s | `1` (default) | `1000` | ~57 events/s | Up to ~500 nodes |
| $50 - 200$ events/s | `4` | `1000` | ~220 events/s | 500 – 2,000 nodes |
| $200 - 400$ events/s | `8` | `2000` | ~440 events/s | 2,000 – 4,000 nodes |
| $400 - 800$ events/s | `16` | `2000` | ~860 events/s | 4,000 – 8,000 nodes |
| $800 - 1,500$ events/s | `32` | `4000` | ~1,680 events/s | 8,000 – 15,000 nodes |
| $> 1,500$ events/s | `64` | `8000` | ~3,500 events/s | 15,000+ nodes |

`maxInFlight` sets the maximum number of uncheckpointed in-flight events in memory. When the in-flight events reach this limit, stream ingestion pauses until the workers complete earlier events. For a larger number of workers, increase `maxInFlight` proportionally. The analyzer can then absorb bursts of event traffic without a stall of ingestion.

#### Concurrent Rule Evaluation

The analyzer runs one datastore query for each enabled rule when it evaluates an event. By default, it runs these queries one at a time. `ruleConcurrency` sets the maximum number of rule queries that run at the same time for one event.

`workers` and `ruleConcurrency` work at different levels:

- `workers` evaluates events from different nodes at the same time.
- `ruleConcurrency` evaluates the rules for one event at the same time.

The analyzer keeps the events from one node in order. Thus, more workers cannot process a burst of events from one node faster. A higher `ruleConcurrency` can.

The rule results do not change. The analyzer publishes the derived events in rule order, as it does when it runs the queries one at a time.

The analyzer runs the same queries, and each query examines the same documents. But on MongoDB, the datastore uses more CPU for each event when the queries run at the same time.

Use these guidelines:

- If the datastore CPU is near its limit, keep the default of `1`. More CPU for each event can decrease throughput.
- If the datastore has spare CPU, set `ruleConcurrency` to `4` or `8`.

The analyzer can run up to `workers` × `ruleConcurrency` rule queries at the same time.

### Matched-entity metric

`rule_matched_total` has only the labels `{rule_name, node_name}`. Thus, when a rule that selects on a GPU, GPC, TPC, or NIC matches, the metric does not show which unit the rule selected. The optional counter below adds that identity. It is disabled by default, because the extra labels increase the cardinality.

```yaml
health-events-analyzer:
  ruleMatchedEntityMetricEnabled: false
```

If you enable it, a match on an entity-keyed rule increments this counter:

```text
rule_matched_entity_total{rule_name, node_name, entity_type, entity_value}
```

`entity_type` and `entity_value` come from the impacted entities of the triggering health event. The counter exports only PCI, GPU, GPC, TPC, SM, NVLINK, NIC, NICPort, and NVSwitch, with the spelling of the producer. It never exports a GPU UUID, and it uses the PCI or GPU index instead. Node-scoped rules (for example, `MultipleRemediations`) do not emit this series. `RepeatedXIDErrorOnSameGPU` does. With PostgreSQL, the built-in XID burst detector also emits this series, with the rule name `RepeatedXidError`.

### Client Certificate Mount Path

The path in the container where Kubernetes mounts the TLS client certificates for authenticated MongoDB connections. Usually, cert-manager provisions the certificates, and Kubernetes mounts them through a secret volume.

```yaml
health-events-analyzer:
  clientCertMountPath: /etc/ssl/client-certs
```

### Rule Enable/Disable Flags

You can enable or disable each built-in rule independently. All rules are enabled by default. To disable a rule without removing it from the configuration, set its flag to `false`.

```yaml
health-events-analyzer:
  enableMultipleRemediationsRule: true
  enableRepeatedXIDErrorOnSameGPURule: true
  enableRepeatedXID31OnSameGPURule: true
  enableRepeatedXID31OnDifferentGPURule: true
  enableRepeatedXID13OnSameGPCAndTPCRule: true
  enableRepeatedXID13OnDifferentGPCAndTPCRule: true
  enableXIDErrorSoloNoBurstRule: true
  enableXID74Reg0SoloNVLinkErrorRule: true
  enableXID74Reg0ECCParityErrorRule: true
  enableRepeatedXID74Reg0HardwareIssueRule: true
  enableXID74Reg0SignalIntegrityErrorRule: true
  enableXID74Reg0Bit27Or29SetRule: true
  enableRepeatedXID74Reg2HardwareIssueRule: true
  enableXID74Reg2Bit13SetRule: true
  enableRepeatedXID74Reg2Bit16Or19SetRule: true
  enableRepeatedXID74Reg2Bit17Or18SetRule: true
  enableXID74Reg3UnexpectedErrorRule: true
  enableXID74Reg3Bit18SetRule: true
  enableXID74Reg4HardwareIssueRule: true
  enableXID74Reg4ECCErrorRule: true
  enableRepeatedNICDriverErrorRule: true
  enableRepeatedNICDegradationRule: true
```

This table summarises each flag, the XID or event type that it covers, and the recommended action that the rule emits when it matches.

| Flag | Rule | Recommended Action | Description |
|------|------|--------------------|-------------|
| `enableMultipleRemediationsRule` | MultipleRemediations | `CONTACT_SUPPORT` | 5 or more remediations on the same node within 7 days |
| `enableRepeatedXIDErrorOnSameGPURule` | RepeatedXIDErrorOnSameGPU | `CONTACT_SUPPORT` | Any XID whose immediate action in the Xid catalogue is not `IGNORE` or `RESTART_APP` (and not 45), plus the retirement failures 157 and 161. Five or more times within 24 hours on the same GPU (burst window 3 min, sticky XID window 3 h) |
| `enableRepeatedXID31OnSameGPURule` | RepeatedXID31OnSameGPU | `RUN_DCGMEUD` | XID 31 two or more times on the same GPU within 24 hours |
| `enableRepeatedXID31OnDifferentGPURule` | RepeatedXID31OnDifferentGPU | `NONE` | XID 31 on two or more different GPUs within 24 hours |
| `enableRepeatedXID13OnSameGPCAndTPCRule` | RepeatedXID13OnSameGPCAndTPC | `RUN_DCGMEUD` | XID 13 two or more times on the same GPC and TPC within 24 hours |
| `enableRepeatedXID13OnDifferentGPCAndTPCRule` | RepeatedXID13OnDifferentGPCAndTPC | `NONE` | XID 13 on two or more different GPC/TPC combinations within 24 hours |
| `enableXIDErrorSoloNoBurstRule` | XIDErrorSoloNoBurst | `NONE` | XID 13 or 31 appeared exactly once in the most recent burst within 24 hours |
| `enableXID74Reg0SoloNVLinkErrorRule` | XID74Reg0SoloNVLinkError | `CONTACT_SUPPORT` | XID 74 with REG0 bit 1 or 20 set, no other active errors on the same GPU |
| `enableXID74Reg0ECCParityErrorRule` | XID74Reg0ECCParityError | `CONTACT_SUPPORT` | XID 74 with REG0 bit 4 or 5 set, two or more times on the same NVLink and GPU within 24 hours |
| `enableRepeatedXID74Reg0HardwareIssueRule` | RepeatedXID74Reg0HardwareIssue | `CONTACT_SUPPORT` | XID 74 with REG0 bits 8, 9, 12, 16, 17, 24, or 28 set, two or more times on same GPU within 24 hours |
| `enableXID74Reg0SignalIntegrityErrorRule` | XID74Reg0SignalIntegrityError | `CONTACT_SUPPORT` | XID 74 with REG0 bit 21 or 22 set, no other active errors on the same GPU |
| `enableXID74Reg0Bit27Or29SetRule` | XID74Reg0Bit27Or29Set | `CONTACT_SUPPORT` | XID 74 with REG0 bit 27 or 29 set, two or more times on same GPU within 24 hours |
| `enableRepeatedXID74Reg2HardwareIssueRule` | RepeatedXID74Reg2HardwareIssue | `CONTACT_SUPPORT` | XID 74 with REG2 bits 0, 1, 2, or 6 set, two or more times on same NVLink and GPU within 24 hours |
| `enableXID74Reg2Bit13SetRule` | XID74Reg2Bit13Set | `CONTACT_SUPPORT` | XID 74 with REG2 bit 13 set |
| `enableRepeatedXID74Reg2Bit16Or19SetRule` | RepeatedXID74Reg2Bit16Or19Set | `CONTACT_SUPPORT` | XID 74 with REG2 bit 16 or 19 set, two or more times within 24 hours |
| `enableRepeatedXID74Reg2Bit17Or18SetRule` | RepeatedXID74Reg2Bit17Or18Set | `CONTACT_SUPPORT` | XID 74 with REG2 bit 17 or 18 set, two or more times within 24 hours |
| `enableXID74Reg3UnexpectedErrorRule` | XID74Reg3UnexpectedError | `CONTACT_SUPPORT` | XID 74 with an unexpected REG3 value |
| `enableXID74Reg3Bit18SetRule` | XID74Reg3Bit18Set | `CONTACT_SUPPORT` | XID 74 with REG3 bit 18 set |
| `enableXID74Reg4HardwareIssueRule` | XID74Reg4HardwareIssue | `CONTACT_SUPPORT` | XID 74 with a hardware-indicating REG4 value |
| `enableXID74Reg4ECCErrorRule` | XID74Reg4ECCError | `NONE` | XID 74 with an ECC-indicating REG4 value |
| `enableRepeatedNICDriverErrorRule` | RepeatedNICDriverError | `CONTACT_SUPPORT` | Non-fatal NIC driver kernel-log pattern (for example, TX/RX timeout or NAPI lockup) three or more times on the same node within 1 hour |
| `enableRepeatedNICDegradationRule` | RepeatedNICDegradation | `CONTACT_SUPPORT` | Non-fatal NIC counter degradation three or more times on the same NIC port within 1 hour |

## Rule Configuration

### Rule Structure

The `config:` block of `values.yaml` defines the rules as TOML. Each rule entry has these fields:

```toml
[[rules]]
name        = "RuleName"
description = "Human-readable description"
recommended_action = "CONTACT_SUPPORT"   # or RUN_DCGMEUD, NONE, etc.
evaluate_rule = true                     # Helm template expression; maps to the enable flag
when = "size(event.errorCode) > 0 && event.errorCode[0] == '74'"   # Optional CEL; see below
stage = [
  '{ "$match": { ... } }',              # MongoDB aggregation pipeline stages as JSON strings
  '{ "$count": "count" }',
  '{ "$match": { "count": { "$gte": N } } }'
]
```

The full default ruleset, with all the definitions of the aggregation pipeline stages, is in the `values.yaml` file of the chart at `distros/kubernetes/nvsentinel/charts/health-events-analyzer/values.yaml`. When you write or review custom rules, refer to that file.

### Skip Rules That Cannot Match

Each enabled rule sends one query to the datastore for each event. Most rules apply to only one XID or one check. For all other events the query cannot match, but the datastore still reads the events of the node to answer it.

The optional `when` field prevents this. It is a CEL expression over the incoming event. The analyzer runs the stages of the rule only when `when` is true. When it is false, the analyzer sends no query and the rule does not match.

```toml
[[rules]]
name = "XID74Reg0ECCParityError"
evaluate_rule = true
when = "size(event.errorCode) > 0 && event.errorCode[0] == '74'"
stage = [ ... ]
```

The expression can read these fields of the incoming event: `agent`, `checkName`, `componentClass`, `errorCode`, `isFatal`, `isHealthy`, `recommendedAction`, `customRecommendedAction`, `nodeName`, `metadata`, and `message`. `errorCode` is a list and `metadata` is a map. The platform connector overrides and the event exporter filter use the same fields.

**The expression must be true for every event that the stages can match.** If it is false for such an event, the analyzer misses that match. To write it safely, copy the stage of the rule that compares `this.` fields to constants. For example, the stage `{"$match": {"$expr": {"$eq": ["this.healthevent.errorcode.0", "74"]}}}` becomes the `when` in the example above. If the event has no error code, the analyzer resolves `this.healthevent.errorcode.0` to null. Thus, check `size(event.errorCode) > 0` before you read `event.errorCode[0]`.

The analyzer handles `when` as follows:

- If `when` is empty or not set, the analyzer runs the rule for every event.
- If `when` does not compile or does not return a boolean, the analyzer does not start. The error names the rule.
- If `when` fails for an event, the analyzer runs the query of the rule and logs a warning. A failure costs a query, but it does not hide a match.


### MultipleRemediations Rule

The `MultipleRemediations` rule matches when NVSentinel performed five or more remediations on the same node in the last 7 days. Unlike other rules, **it applies a node condition that NVSentinel does not automatically clear**, because the rule does not emit healthy derived events.

After you correct the underlying hardware issue, remove the condition manually:

```bash
kubectl get node {NODE_NAME} -o json \
  | jq '.status.conditions |= map(select(.type != "MultipleRemediations"))' \
  | kubectl replace -f - --subresource=status
```

If a custom rule applied a different condition, replace `MultipleRemediations` with the actual name of the condition type.

### XID 74 Register Rules

XID 74 (NVLink error) has register fields (`REG0`–`REG6`) that encode the specific failure mode. The XID 74 rules examine the bit patterns of these registers to distinguish three categories of failure:

**Signal integrity issues** — REG0 bits 21 or 22 set (`XID74Reg0SignalIntegrityErrorRule`). This pattern can indicate marginal signal levels. Check the mechanical connections of the link. If the issue continues, run field diagnostics.

**ECC/parity errors** — REG0 bits 4 or 5 set (`XID74Reg0ECCParityErrorRule`), or REG2 bits 0, 1, 2, or 6 set (`RepeatedXID74Reg2HardwareIssueRule`). Repeated occurrences on the same NVLink and GPU indicate a hardware fault.

**Unexpected or unexplained errors** — REG0 bits 1 or 20 set, or a REG3 anomaly: the rule sets `CONTACT_SUPPORT` immediately. REG0 bits 27 or 29 set: the rule sets `CONTACT_SUPPORT` after two or more occurrences within 24 hours (`XID74Reg0Bit27Or29SetRule`).

The REG4 rules match after two or more occurrences on the same NVLink and GPU within 24 hours. A hardware-indicating REG4 value gets `CONTACT_SUPPORT` (`XID74Reg4HardwareIssueRule`). An ECC-indicating REG4 value gets `NONE` (`XID74Reg4ECCErrorRule`).

Rules that check REG0 bits 1, 20, 21, or 22 also verify that no other XID errors are active on the same GPU. They do this check before they match. This check prevents false positives during multi-error storms.

### XID 31 and XID 13 Rules

Both XID 31 (GPU memory error) and XID 13 (graphics engine fault) have two rule variants:

**Same GPU / same GPC+TPC** — repeated occurrences on the same hardware unit can indicate a hardware defect. The recommended action is `RUN_DCGMEUD` (run DCGM End-User Diagnostics). If the tests pass, run field diagnostics next.

**Different GPU / different GPC+TPC combinations** — the same error on different hardware units in a short window can indicate a software or workload problem. The recommended action is `NONE`. Investigate the application process that the PID in the error identifies.

Both variants use a burst-window algorithm: a 3-minute window for XID 31 and a 15-second window for XID 13. Thus, the rules do not count rapid re-occurrences of the same event as separate bursts.

### NIC Rules

Two NIC rules complement the NIC Health Monitor module:

`enableRepeatedNICDriverErrorRule` — correlates `SysLogsNICDriverError` events from the Syslog Health Monitor. It matches when the same kernel-log error pattern (for example, `mlx5_tx_timeout_detected` or `netdev_watchdog`) occurs three or more times on the same node in one hour. This shows that automatic driver recovery does not work.

`enableRepeatedNICDegradationRule` — correlates `InfiniBandDegradationCheck` and `EthernetDegradationCheck` events from the NIC Health Monitor. It matches when a non-fatal counter degradation occurs three or more times on the same NIC port in one hour. This can indicate a persistent physical-layer problem.

For the related check and detection configuration, see the [NIC Health Monitor configuration](nic-health-monitor.md).
