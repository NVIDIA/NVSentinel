# ADR-070: Labeler - Explicit Relearn of Expected Device Counts

Status: Proposed (tracking issue [#1914](https://github.com/NVIDIA/NVSentinel/issues/1914))

## Context

[ADR-043](043-expected-device-count-labels.md) has the labeler learn an expected device count for each class and hardware partition. A learned count may rise when a higher count is observed, and it must not fall automatically, so that a node or a whole partition that loses devices keeps being reported. ADR-043 names the ways down: an admin removing the expected label from a node or a hardware class, an explicit labeler command or configuration flag that relearns a class, and a future configuration object that declares expected counts. Only the first exists, and it does not work for a partition, as described below. ADR-043 also leaves a mitigation open: document admin relearn procedures before enabling fatal remediation from these labels by default.

Issue #1914 reports a GB200 fleet of 287 nodes where 285 nodes carry `gpu.count.current=4` and `gpu.count.expected=8`. Two nodes were switched to MIG. While they still carried the plain `NVIDIA-GB200` product label, they reported 8 GPUs, which raised the expected count of the plain partition to 8. They then moved to their MIG partition. No node in the plain partition reports 8 anymore, and every node in it still reads 8.

Three code paths in `labeler/pkg/devicecounts` keep the higher count:

- A node's own expected label is a floor for its next value.
- The expected labels of the other nodes in the partition raise the partition's value, together with their current counts.
- A change to a label the labeler manages does not trigger a reconcile. An expected label removed from one node stays removed until the node's next input event, and that reconcile restores the old value from the other nodes. On a partition of 285 nodes the labels cannot all be removed before one of the nodes is reconciled.

A transient observation is not even needed. A MIG node that is cleanly returned to the plain product carries its MIG-era expected count into the plain partition, and the next reconcile spreads it to every node there.

The only recovery that works today is `expectedCountOverrides`. An override pins a count and stops learning for the nodes it matches. It needs per-deployment knowledge: on the reported fleet one blanket count would fix one partition and break the other, so both had to be pinned, and a pinned count no longer follows the hardware.

In the issue thread the reporter asked to keep the ADR-043 rule even when every node of a partition reports a lower count, because a driver or firmware change that hides a GPU on every node looks the same, and to add an explicit one-shot relearn for a class, through a flag or a configuration field, that recomputes the expected count from the current counts of the partition's nodes and ignores the existing labels.

A relearn has to stay correct while the labeler keeps reconciling. Rewriting each node from current counts is not enough on its own:

- An event reconcile of a node already rewritten, or of a node not reached yet, still reads the old value on the other nodes and restores it.
- A node that the relearn skips, because it has no count source, its expression fails or its patch fails, keeps the old value and raises the partition again once the relearn is over.
- A flag that stays set relearns again at every restart, so the first restart after a real fleet-wide loss would lower the count automatically.

The relearn therefore needs state that survives restarts and tells a value written by the relearn from a value written before it. The options considered for the request and for that state:

1. A field in the expected device count configuration of each class.
2. A Node or namespace annotation, or a separately watched ConfigMap.
3. A CLI command or a one-shot Job.
4. An override with an expiry.
5. A documented procedure that deletes the expected labels.

## Decision

Add an optional `relearnID` to each device count class. A new value asks the labeler to relearn that class once from current counts. Under an ID the labeler records every expected value it writes in a node annotation, `nvsentinel.dgxc.nvidia.com/<class>.count.relearn: "<relearnID>/<value>"`, in the same patch as the label, and an expected label counts as evidence for its node and for the other nodes of its partition only when that annotation records the configured ID and the label's value. Without an ID nothing changes.

## Implementation

### Configuration

`ClassConfig` gains `relearnID`: TOML `relearnID`, Helm `expectedDeviceCounts.classes[].relearnID`, empty by default. The labeler does not interpret the value. A date and a reason work well, for example `2026-11-05-mig`. It must be a valid label value, at most 63 characters of letters, digits, `-`, `_` and `.`, so it cannot contain the `/` that separates it from the count in the annotation. The class name must form a valid annotation key, and two classes cannot use the same annotation. The labeler does not start with an invalid value, as for other invalid class settings.

The chart helper `labeler.expectedDeviceCountsConfig` renders the field. The labeler Deployment already carries a checksum of that configuration, so a new ID restarts the labeler, and the labeler reads the configuration only at startup. No other trigger is needed.

### Evidence rule

In `expectedDeviceCount` and `expectedDeviceCountForPartition` (`labeler/pkg/devicecounts/device_counts.go`) every read of an expected label goes through one function. Without an ID it returns the label, as today. With an ID it returns the label only when the node's relearn annotation equals `<relearnID>/<label value>`. Current counts are used as today, and overrides still take precedence.

A node's expected count is the highest of its current count, the current counts of the other nodes in its partition, and the certified labels in the partition. In the first reconcile under a new ID no label is certified, so each partition is relearned from current counts. From then on every label the labeler writes is certified, and learning follows the ADR-043 rule again: counts rise from observations and never fall on their own.

Under an ID the labeler writes the annotation for every expected value it writes, override values included, so removing an override later keeps today's behaviour. The labeler logs every node whose expected count goes down, with the class, the previous and the new value, and the ID.

### One patch per node

`kubeclient.NodeMergePatch` builds one merge patch from the labels and annotations of a node, so a node never carries a new label without its annotation or the reverse. The node cache keeps the relearn annotation of each class with an ID, as it keeps `nvsentinel.dgxc.nvidia.com/dcgm-bootstrap-completed` today. A restart with the same ID therefore finds every certified label and writes nothing.

### Reconcile trigger

`nodeRequiresReconciliation` also reconciles a node when an update leaves a label that its annotation certified uncertified: the label was changed or removed, or the annotation was removed. That catches a hand edit, or a write by an older labeler during a rollout. The check compares the label with the value in the annotation, so the labeler's own writes never trigger it, and an annotation that changes only its ID does not either. Two labelers with different IDs during a rollout therefore do not reconcile each other's writes.

### When the relearn runs

The startup reconcile of every node uses one shared `ReconcileCache` and relearns every node of the class that has a count. Event reconciles apply the same rule, so a node reconciled while other nodes still carry old labels does not read them. A node without a count, because its source is missing or its expression fails, keeps its labels as today. Its old label is not certified, so it raises nothing, and the node is relearned when its count returns. A node whose patch failed is relearned at its next reconcile, or at the next restart with the same ID.

### Operator procedure

1. Before setting an ID, compare the current and expected labels of each partition, for example with `kubectl get nodes -L` and the class labels and grouping labels. A relearn sets each partition to the highest current count among its nodes, so a partition where every node reports less than its expected count goes down. Pin with an override any partition that has really lost devices.
2. Set a new `relearnID` on the class and upgrade the release.
3. Check that every node of the class carries the annotation with the new ID. A node without it has no count or failed to patch.
4. Leave the ID in place. Removing it makes every old label count again, including any that a node without a count still carries. Use a new ID for the next relearn, for example after the next MIG change.

### Compatibility

A labeler without this change ignores the annotation and trusts the labels, which hold relearned values once the relearn is complete. No new RBAC is needed, because the labeler already patches Nodes, and there is no new informer, CRD or watched object. The labels keep their ADR-043 contract, so Kubernetes Object Monitor policies do not change.

### Concurrency work in #1846

The evidence rule is a pure function of a node and the class configuration, used inside `ReconcileCache`. A parallel startup sweep that prepares partition maxima before dispatch, as #1846 proposes, calls the same function. Both changes edit `expectedDeviceCountForPartition`, and whichever lands second adapts.

### Validation

- Unit tests on the `devicecounts` manager with the shipped GPU class and the reported fleet of 287 nodes: the regrouped fleet, a label removed from one node, a clean MIG disable, a relearn that stops halfway and restarts, event reconciles against old labels, a node missing a GPU during a relearn, a uniform loss after a relearn, a foreign write, a node without a count source, overrides, and configuration validation.
- An envtest test with the running labeler that plays the reported scenario, sets an ID, restarts with the same ID, and checks that a lost GPU is not absorbed after the relearn.
- A Helm unit test for the rendered field.

## Rationale

- It is the explicit path that ADR-043 already allows, and it keeps the rule that counts never fall on their own.
- The request lives where classes and overrides are declared, so it is versioned with the rest of the deployment and needs no new API.
- An annotation on each node is the smallest durable state that keeps the relearn correct across restarts, partial progress and old labels on other nodes. It follows the existing DCGM bootstrap annotation and is written in the same patch as the label.
- Binding the annotation to the value as well as the ID means that a value written by anyone else is never taken as relearned.

## Consequences

### Positive

- A reconfigured fleet recovers without a permanent pin and without knowing the right count for each partition.
- A node missing devices in a partition with healthy nodes is still flagged during and after a relearn.
- A relearn that stops halfway resumes safely, and a restart with the same ID changes nothing.

### Negative

- A relearn accepts any shortfall that every node of a partition reports at that moment, including a single-node partition with a missing device. That is the operator's explicit decision.
- A node that has no count when the labeler reaches it, because its device plugin or ResourceSlice is down or it is opted out, keeps its uncertified label and is relearned from whatever it reports once its count returns. A device lost in between is accepted without a new ID. The procedure therefore avoids a relearn during a driver or device plugin upgrade, checks afterwards that every node carries the annotation, and pins a node still without one before its count returns.
- Every node of the class is patched once to carry the annotation, even where the value does not change.
- The ID stays in the configuration, and each class with an ID adds one annotation to each node.
- It repairs a baseline and does not prevent the next one. A node that leaves MIG carries its MIG-era expected count into the plain partition, and the next reconcile spreads it, so after such a change the operator sets a new ID.

### Mitigations

- The procedure shows every decrease before it happens, an override can hold any partition, and the labeler logs every node it lowers.
- Keeping the same ID never relearns again; only a new ID does.
- The annotation on each node shows which nodes the relearn has reached.
- Prevention is left to a separate decision (see Notes).

## Alternatives Considered

### Lower the expected count after a stable lower count on every node of a partition

**Rejected** because a fleet-wide device loss produces the same observations. ADR-043 forbids it, and the reporter asked to keep that rule.

### Node or namespace annotation, or a watched ConfigMap

**Rejected** because the request is about a device class, not a node. An annotation the operator writes on every node as the request is many writes for the operator, and a partition where only some nodes carry it is split between two rules. An annotation on one node disappears with that node while the old labels on other nodes may still need to be ignored, and two requests in one partition need arbitration. A namespace annotation or a separate ConfigMap needs new RBAC and an informer for a rare operation. [ADR-059](059-derived-condition-recovery.md) uses Node annotations because its recovery is verified for each node and entity, which is a different shape.

### CLI command or one-shot Job

**Rejected** because it runs beside the labeler, which keeps restoring old values between its writes. A partial failure leaves old labels behind, it duplicates CEL evaluation outside the labeler, and it needs its own RBAC. A plain command-line flag on the labeler is not one-shot, because it relearns again at every restart.

### Override with an expiry

**Rejected** because the operator must know the right count for every partition, nothing reconciles when the expiry passes, an expiry before every node is rewritten brings the old value back, and nothing signals completion.

### Delete the expected labels

**Rejected** as a procedure. It works only if every old label in the partition is removed before any node of the partition is reconciled. One reconcile in between restores the old value from another node.

### Tie the annotation to the partition

**Deferred.** It would stop a node from carrying its expected count into another partition, for example out of MIG. But #2108 deliberately keeps the expected count across a change between the DRA and GFD sources (`TestDeviceCountSources_ModeChange_KeepsExpectedCount`), and that change also changes the partition key.

## Notes

- Non-goals: automatic prevention of MIG-related changes to a partition's count, a relearn of part of a class, a progress metric, and Kubernetes Events, which need new RBAC. A relearn limited to one partition can be added later with `matchLabels` restricted to grouping labels of every source of the class.
- In the reported fleet the MIG nodes report 8 GPUs on 4-GPU hardware. The GFD count there follows MIG devices rather than physical GPUs, so its unit changes with the MIG mode. Both ways the higher count reaches the plain partition start from that, so it belongs with the prevention question.

## References

- [ADR-043: Labeler - Expected Device Count Labels](043-expected-device-count-labels.md)
- Issue [#1914](https://github.com/NVIDIA/NVSentinel/issues/1914)
- Issue [#1846](https://github.com/NVIDIA/NVSentinel/issues/1846): parallel startup sweep of the labeler
- PR [#2108](https://github.com/NVIDIA/NVSentinel/pull/2108): GPU count in both GPU Operator modes
- [ADR-059: Operator Recovery of Derived Conditions](059-derived-condition-recovery.md)
- [Labeler configuration](../configuration/labeler.md)
