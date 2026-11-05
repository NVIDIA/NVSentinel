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

To keep a wrong count from reaching a partition again, every expected label also records the partition it was learned in and counts only there, and a class can declare when a node is not settled, so that a node in the middle of a reconfiguration teaches nothing. [Prevention](#prevention) describes both.

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
4. Leave the ID in place. Removing it makes every old label count again, including any that a node without a count still carries. Use a new ID for the next relearn.

### Compatibility

A labeler without this change ignores the annotation and trusts the labels, which hold relearned values once the relearn is complete. No new RBAC is needed, because the labeler already patches Nodes, and there is no new informer, CRD or watched object. The labels keep their ADR-043 contract, so Kubernetes Object Monitor policies do not change.

### Concurrency work in #1846

The evidence rule is a pure function of a node and the class configuration, used inside `ReconcileCache`. A parallel startup sweep that prepares partition maxima before dispatch, as #1846 proposes, calls the same function. Both changes edit `expectedDeviceCountForPartition`, and whichever lands second adapts.

### Validation

- Unit tests on the `devicecounts` manager with the shipped GPU class and the reported fleet of 287 nodes: the regrouped fleet, a label removed from one node, a clean MIG disable, a relearn that stops halfway and restarts, event reconciles against old labels, a node missing a GPU during a relearn, a uniform loss after a relearn, a foreign write, a node without a count source, overrides, and configuration validation.
- An envtest test with the running labeler that plays the reported scenario, sets an ID, restarts with the same ID, and checks that a lost GPU is not absorbed after the relearn.
- A Helm unit test for the rendered field.

## Prevention

The relearn repairs a partition after a wrong count has reached it. Two further rules keep a wrong count from reaching a partition. #1914 shows two paths by which a count learned on one node ends up in a partition where it does not belong:

1. A label carried across partitions. A node's expected label moves with the node when its grouping labels change, and the maximum of its new partition takes it. A node that leaves MIG brings the 8 it learned as a MIG node into the plain partition. A node relabeled with another instance type does the same.
2. A node in the middle of a reconfiguration. While the GPU Operator MIG manager changes the MIG layout of a node, the counts and the grouping labels of the node can disagree, and a count observed then raises the partition that the node still belongs to.

The issue attributes the 8 to the second path: the two nodes reported 8 GPUs while they still carried the plain product. The order in which the labels changed on that fleet is not known. The first rule below closes the first path whatever the order was. The second rule keeps a node that is marked as being reconfigured from teaching others, but under the GPU Operator it does not cover the transient the issue describes, as explained below.

### Evidence stays in the partition it was learned in

With every expected label it writes, the labeler writes the partition key of the node into the annotation `nvsentinel.dgxc.nvidia.com/<class>.count.partition`, in the same patch, for example `source=gfd|node.kubernetes.io/instance-type=gb200|nvidia.com/gpu.product=NVIDIA-GB200`. An expected label counts as evidence, for its node and for the other nodes of the partition, only while the node is in the partition that the annotation names. A node whose partition changed learns its expected count again from its own current count and from the other nodes of its new partition. The labeler logs that with the label and both partition keys. Each time it computes the maximum of a partition, it counts the labels it leaves out of that maximum in `labeler_device_count_skipped_updates_total` with the reason `partition_changed`. A node's own label is counted that way too, through the copy of the node in the node cache, and not a second time when the node itself is reconciled.

This closes the first path. Every enabled class writes the annotation, with or without a relearn ID. Under an ID the relearn rule applies on top, so a label must also be certified.

- A change between two sources of a class is not a change of partition. The partition key starts with the source name, so a GPU Operator mode change changes it, and #2108 keeps the expected count across that change on purpose (`TestDeviceCountSources_ModeChange_KeepsExpectedCount`). A label recorded by another source of the class still counts.
- A node without a partition, because its grouping expression fails, learns only from itself as before. Its label still counts for it, and its annotation stays as it is.
- A label without the annotation, written before this change, still counts. The labeler adds the annotation at the next reconcile of the node.
- The node cache keeps the annotation of every class. The labeler checks at startup that every enabled class name forms a valid annotation key and that no two classes share one.
- A change to the annotation alone does not trigger a reconcile. The partition of a node changes only with its labels or its ResourceSlices, which already trigger one, and the labeler's own writes must not. A label whose annotation someone changed by hand stops counting until the next reconcile of the node relearns it.

### A node that is not settled teaches nothing

A class can set `settledExpression`, a CEL expression that returns a bool. It sees `node` and `resourceSlices` like `currentExpression`, with every ResourceSlice of the node, also in a class with sources. While it returns false for a node, the node is not settled. Neither its current count nor its expected label raises the expected count of another node. For the node itself the labeler writes the current label and leaves the expected label and both annotations as they are, so an override or a new relearn ID reaches the node once it is settled again. The labeler logs each such reconcile and counts it, and each node it leaves out of a partition, with the reason `unsettled`. A failed evaluation leaves the labels of the node as they are, as a failed `currentExpression` does, and the node teaches nobody, as when it is not settled. An expression that does not compile, or that CEL can tell returns something other than a bool, stops the labeler at startup.

The chart leaves it empty, so by default every node is settled and a node whose MIG change failed is treated as before. The chart values document this expression for the GPU class as an opt-in and recommend it under the GPU Operator:

```text
!('nvidia.com/mig.config.state' in node.metadata.labels) ||
node.metadata.labels['nvidia.com/mig.config.state'] == 'success'
```

The MIG manager of the GPU Operator (`pkg/mig/reconfigure/reconfigure.go` in mig-parted) sets the state to `pending`, stops the device plugin and GFD through their `nvidia.com/gpu.deploy.*` labels and waits up to five minutes for their pods to be gone, sets `rebooting` when it has to reboot the node, applies the layout, starts the device plugin and GFD again, and then sets `success`. When the change does not work it starts them again and then sets `failed`. A node in `failed` stays unsettled, with its expected count unchanged, until the MIG manager succeeds.

What this expression covers follows from that order. It covers any report that lands while the node is `pending` or `rebooting`, on either side of the change: a report from the device plugin or GFD in the moment between `pending` and their stop, and one from them after they start again but before `success` is written. In between they are not running, and GFD in its file mode removes its features file when it stops, so its labels go with it. The MIG manager writes `success` right after it starts the device plugin and GFD again, so the labels and allocatable resources of the new layout usually arrive after `success`, and the expression is not a reliable guard against a transient like the one #1914 describes. It matters for a node in `failed`, whose device plugin and GFD run again before that state is written, and for any count source that the MIG manager does not stop. The rule itself is general: an operator points `settledExpression` at whatever marks their nodes as being in the middle of a change.

After `success`, a count and a grouping label that come from different writers can still disagree for a while. A `currentExpression` that counts from allocatable resources while the grouping label comes from GFD is the case to know about, because the device plugin can register the new layout before GFD relabels the node. With the gfd source of the chart the count and the product both come from GFD, which writes them in one features file that NFD applies in one node update. Which mechanism produced the reported state is not known, and the relearn remains the way back from it.

### Why the relearn is still needed

Prevention starts with the upgrade. A fleet that already carries a wrong count, such as the 285 nodes at 8 in #1914, keeps it, because a label written before the annotation existed still counts, and only a relearn lowers it. A cause that neither rule foresees still raises a partition: a `currentExpression` that counts from allocatable resources while the grouping label comes from GFD, after the MIG manager reports `success`, a device plugin or driver that briefly reports more devices, a label written by hand. For those the explicit relearn stays the way down that ADR-043 names.

### Upgrade

The first reconcile of each node after the upgrade writes its partition annotations, which is one extra patch per node at the client rate limit of the labeler. It changes no label, because labels without the annotation still count. A node that has no count, or is not settled, at that reconcile keeps its label without the annotation, and that label counts in any partition. If the node changes partition before the labeler counts it again, for example because the upgrade fell in the middle of a MIG change, its old count reaches the new partition and spreads there. The docs therefore advise upgrading outside MIG changes and checking afterwards that every node with an expected label carries the annotation, with `kubectl get nodes -l nvsentinel.dgxc.nvidia.com/gpu.count.expected -o custom-columns='NAME:.metadata.name,PARTITION:.metadata.annotations.nvsentinel\.dgxc\.nvidia\.com/gpu\.count\.partition'`. A labeler without this change ignores the annotations.

### Consequences of prevention

Positive:

- A node that leaves MIG or changes instance type no longer raises its new partition, and a MIG change no longer needs a new relearn ID.
- With the MIG expression set, a count observed while the MIG manager reports a node as `pending`, `rebooting` or `failed` raises no other node. Under the GPU Operator that is a count from a source the MIG manager does not stop, or the first reports after a failed change.
- No learned count falls because devices vanished. A node that changes partition still gets the higher count of the other nodes of its new partition.

Negative:

- A node whose partition changes is relearned from current counts. Alone in its new partition and missing a device at that moment, it accepts the loss.
- A change of the partition key format, or of the grouping itself, such as new grouping labels or their order, a renamed source, or a product name that a new GFD or driver release reports differently, changes the key of every node once. Every node then relearns from current counts once, as in a relearn. The current counts of the other nodes of a partition still carry the higher value, unless every node of the partition lost devices at that same moment, which is the case ADR-043 protects against, for example a driver release that renames the product and hides a GPU at once.
- A node that changes source and partition in one step still carries its count, because a change of source is not a change of partition. A MIG node at 8 whose cluster moves to GPUCluster mode keeps 8 in its DRA partition.
- With the MIG expression set, a node whose MIG change failed keeps its expected count, overrides included, until the MIG manager succeeds. This is why the chart leaves it unset by default.
- A node without a count, or not settled, at its first reconcile after the upgrade keeps a label without the annotation that counts in any partition until the labeler counts the node again.
- Every enabled class adds an annotation to every node, and a class name that cannot form the annotation key stops the labeler at startup.

Mitigations:

- The labeler logs every node it relearns after a partition change, with both partition keys, and every expected count it lowers.
- An override pins a partition across a planned change of the grouping, and a relearn ID remains for any other cause.

### Alternatives considered for prevention

#### Raise a learned count only after it persisted for some time

**Rejected** because a transient longer than the window still raises the partition, a window long enough to cover a MIG reboot delays every real rise as long, and the labeler would need durable state for every pending value. It does nothing for a label carried across partitions.

#### Compare the GFD count label with the allocatable resources

**Rejected** as a built-in rule. The device plugin advertises MIG devices under different resource names for each MIG strategy, either side can lag the other after a change, and a disagreement does not tell which side is right. A deployment that wants the check can write it as a `settledExpression`.

#### Count MIG devices differently

**Deferred.** Under the single MIG strategy GFD sets `nvidia.com/gpu.count` to the number of MIG devices, so the GPU class changes its unit with the MIG mode. Counting physical GPUs on MIG nodes would give the class one unit in every mode. It changes what the GPU class reports, needs a source of physical GPU counts on MIG nodes in ClusterPolicy mode, and is a separate decision. Both rules here still apply under it, since partitions change and nodes are reconfigured without MIG too.

### Validation of prevention

- Unit tests on the `devicecounts` manager: a node leaving MIG, a node moved to another instance type in a class without sources, a label without the annotation, the label and both annotations changing in one update, a mode change between sources, a node without a partition, the states of the MIG manager, a pending node whose count source still reports 8 GPUs under the plain product, a node that is not settled under a relearn ID, a failed `settledExpression` whose node teaches nothing, the inputs it makes the labeler watch, and configuration validation for both rules.
- The ratchet story of #1914 goes on with the two nodes leaving MIG again under the same relearn ID, with and without the MIG manager keeping them pending.
- Envtest tests with the running labeler and 40 nodes: a MIG toggle with no relearn ID, and the same toggle while the MIG manager reports `pending` and a count source that it does not stop reports 8 GPUs under the plain product, with a restart while the nodes are pending. No node that stays plain is written in either, and a restart afterwards writes nothing.
- A test that the expected label and both annotations reach the API server in one patch, and that the node cache keeps the partition annotation.
- Helm unit tests for the rendered `settledExpression` and for the ResourceSlice access it can need.

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
- It repairs a baseline and does not prevent the next one on its own. [Prevention](#prevention) closes the two paths seen in #1914, and a cause outside them still needs a new ID.

### Mitigations

- The procedure shows every decrease before it happens, an override can hold any partition, and the labeler logs every node it lowers.
- Keeping the same ID never relearns again; only a new ID does.
- The annotation on each node shows which nodes the relearn has reached.
- [Prevention](#prevention) keeps every expected count in the partition it was learned in and learns nothing from a node that is not settled.

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

**Adopted in another form.** It stops a node from carrying its expected count into another partition, for example out of MIG. #2108 deliberately keeps the expected count across a change between the DRA and GFD sources (`TestDeviceCountSources_ModeChange_KeepsExpectedCount`), and that change also changes the partition key. [Prevention](#prevention) therefore records the partition in a separate annotation that every class writes, with or without a relearn ID, and does not treat a change of source as a change of partition.

## Notes

- Non-goals: a relearn of part of a class, a progress metric, and Kubernetes Events, which need new RBAC. A relearn limited to one partition can be added later with `matchLabels` restricted to grouping labels of every source of the class.
- In the reported fleet the MIG nodes report 8 GPUs on 4-GPU hardware. The GFD count there follows MIG devices rather than physical GPUs, so its unit changes with the MIG mode. [Prevention](#prevention) keeps that count out of the plain partition, and counting physical GPUs on MIG nodes is listed there as a separate decision.

## References

- [ADR-043: Labeler - Expected Device Count Labels](043-expected-device-count-labels.md)
- Issue [#1914](https://github.com/NVIDIA/NVSentinel/issues/1914)
- Issue [#1846](https://github.com/NVIDIA/NVSentinel/issues/1846): parallel startup sweep of the labeler
- PR [#2108](https://github.com/NVIDIA/NVSentinel/pull/2108): GPU count in both GPU Operator modes
- [ADR-059: Operator Recovery of Derived Conditions](059-derived-condition-recovery.md)
- [NVIDIA mig-parted](https://github.com/NVIDIA/mig-parted): the MIG manager of the GPU Operator and its `nvidia.com/mig.config.state` label
- [Labeler configuration](../configuration/labeler.md)
