# ADR-065: Validation — Test Selection for Node Groups

Status: Proposed.

## Context

NVSentinel validates a node before the node runs workloads ([ADR-049](049-node-validation.md)). In a heterogeneous cluster, the nodes are not all the same. Some nodes have a different GPU model, and some nodes are in a different node pool. Thus, different node groups need different tests. This ADR lets validation select tests for each node group.

**Example:** A cluster has GB200 nodes and H100 PCIe nodes. The `nccl-loopback-nvswitch` test needs NVSwitch. Only the GB200 nodes have NVSwitch, so only the GB200 nodes must run this test.

Validation has two paths. Before this ADR, neither path could select a test for only the GB200 nodes:

- **New-node validation:** lifecycle-manager gives every new node the same list of tests, `newNodeTests`. If the list includes `nccl-loopback-nvswitch`, the H100 PCIe nodes also run it, but they do not have the hardware that it tests. If the list does not include it, the GB200 nodes never run it.
- **Post-remediation validation:** fault-quarantine selects the tests with the rule sets in `validation.ruleSets`. A rule set can check only the health event, for example the error code. A rule set cannot check the node, so it cannot know that the node is a GB200 node. Thus, after a repair, no rule set can select `nccl-loopback-nvswitch` for only the GB200 nodes.

In both paths, lifecycle-manager runs the tests, and uncordons the node only if all the tests pass.

[ADR-064](https://github.com/NVIDIA/NVSentinel/pull/2026) (proposed) adds the same type of selection by node to node-drainer and fault-remediation. It lists validation as a non-goal. This ADR adds the selection to validation.

## Decision

Validation selects tests for each node group with rule sets that can check the node labels. Both validation paths use the same rule set format:

1. **Post-remediation validation:** A fault-quarantine rule set can check the node labels, so it can select a test for a specific node group. fault-quarantine selects the tests when the unhealthy event arrives, as before this ADR. If fault-quarantine cannot select the tests, it does not skip them. It tries again, and the node stays out of service until the tests are selected and pass.
2. **New-node validation:** lifecycle-manager gets a new optional list of rule sets, `newNodeValidation.ruleSets`. Each rule set that matches a node adds tests to the tests in `newNodeTests` for that node. lifecycle-manager puts each test in a ValidationRequest with all the nodes that need that test. Tests that need the same nodes share one ValidationRequest.

The rule set format keeps the format and behavior of the current fault-quarantine `validation.ruleSets`. Each rule set that matches adds its tests, and no rule set wins over a different rule set. With no new rule sets, validation works as it did before this ADR.

## Implementation

### Rule set format

- **Parts of a rule set:** A rule set has a name, conditions in `match`, and tests. Each condition has a `kind`. A `HealthEvent` condition checks the health event. A `Node` condition checks the node, for example its labels.
- **One format for both paths:** The format is the format of `validation.ruleSets` in fault-quarantine. lifecycle-manager uses the same format in `newNodeValidation.ruleSets`. The code that checks the rule sets moves to `commons` in ADR-064 phase 1, and both components use it.
- **Conditions for each path:** A post-remediation rule set can have `HealthEvent` conditions and `Node` conditions. A new-node rule set can have only `Node` conditions, because a new node has no health event. lifecycle-manager does not start if a new-node rule set has a `HealthEvent` condition.
- **All matches add tests:** fault-quarantine and lifecycle-manager check all the enabled rule sets. Each rule set that matches adds its tests. The final list has no duplicate tests, and it is sorted.
- **Current fields keep their behavior:** The Helm chart sets `enabled` to true when it is not set. A rule set can have `all` and `any` together, and it matches when one of them matches. `priority` has no effect, and the component logs a warning when it is not zero. The configuration reference states that rule sets add tests and that `priority` has no effect.
- **Missing labels:** A `Node` condition tests for the label key first, for example `"example.com/group" in node.metadata.labels && node.metadata.labels["example.com/group"] == "gb200"`. If the node does not have the label, the condition does not match, and the rule set adds no tests. Thus, the documentation tells operators to put the tests that all nodes need in a rule set without a `Node` condition, so that a node with a missing label still gets them. As in ADR-064, the component logs a warning at startup for a condition that reads a label but does not test for its key.
- **Stable labels:** The documentation tells operators to select node groups with labels that do not change during a repair, such as the GPU product label or a dedicated label for each group. `nvidia.com/gpu.count` is not stable, because it changes when a GPU falls off the bus.

```yaml
validation:
  ruleSets:
    - name: gpu-reboot
      match:
        all:
          - kind: HealthEvent
            expression: 'event.componentClass == "GPU" && event.isFatal'
      tests: [dcgm-diag-test]
    - name: gb200-nvswitch
      match:
        all:
          - kind: HealthEvent
            expression: 'event.componentClass == "GPU" && event.isFatal'
          - kind: Node
            expression: >-
              "example.com/group" in node.metadata.labels &&
              node.metadata.labels["example.com/group"] == "gb200"
      tests: [nccl-loopback-nvswitch]
```

A GB200 node with a fatal GPU event gets `dcgm-diag-test` and `nccl-loopback-nvswitch`. An H100 PCIe node with the same event gets only `dcgm-diag-test`. A node without the `example.com/group` label also gets `dcgm-diag-test`, because the `gpu-reboot` rule set has no `Node` condition.

### Post-remediation test selection

- **Rule sets can check the node:** fault-quarantine keeps a copy of each node in a cache for its quarantine rule sets. This ADR lets the validation rule sets use the same cache. Before this ADR, the validation rule sets could not use the cache (`NewValidationClient` in `fault-quarantine/pkg/validation/validation.go`), so they could not have `Node` conditions. The cache already keeps the labels that the validation rule sets check (`fault-quarantine/pkg/nodecache/nodecache.go`).
- **Tests are selected when the event arrives:** fault-quarantine checks the rule sets when an unhealthy event arrives, as before this ADR. It saves the event and its tests in the `quarantineValidationHealthEvent` annotation of the node.
- **A failed selection does not stop the quarantine:** A `Node` condition can fail when a `HealthEvent` condition does not, for example when the node is not in the cache yet. If a rule set fails, fault-quarantine still quarantines the node. It does not skip the failed rule set, as it did before this ADR. It saves the event as a pending selection, logs an error, and increments a metric with the rule set name.
- **Try again:** fault-quarantine tries the selection again for each node, and waits longer after each failure. node-drainer tries again in the same way (`node-drainer/pkg/queue`). Thus, a failure on one node does not delay the events of other nodes. At startup, fault-quarantine finds the pending selections on the nodes and tries them again. How fault-quarantine saves a pending selection is an open question.
- **The retry also does the steps after the selection:** If the node is still quarantined when the selection succeeds, fault-quarantine saves the tests, and nothing else changes. If the last unhealthy event of the node already recovered, the retry also checks the drain and creates the ValidationRequest. fault-quarantine removes the pending selection only after the ValidationRequest exists. If the rule sets select no test, fault-quarantine uncordons the node, as it does when no tests are necessary.
- **The node stays cordoned while a selection is pending:** When the last unhealthy event of the node recovers and a selection is pending, fault-quarantine does not uncordon the node, and it does not remove its taints. Before this ADR, fault-quarantine does the same when it cannot create the ValidationRequest (`triggerValidationOnUnquarantine` in `fault-quarantine/pkg/reconciler/reconciler.go`).

### New-node test selection

- **New rule sets:** lifecycle-manager gets `newNodeValidation.ruleSets` in its validation configuration. The list is empty by default.
- **Tests for each node:** lifecycle-manager starts with the tests in `newNodeTests`. If `newNodeTests` is empty, it starts with `defaultTests`, as before this ADR. Then each rule set that matches the node adds its tests. A rule set can only add tests, and it cannot remove a test from `newNodeTests`. Thus, an operator puts a test in `newNodeTests` only if all new nodes need it, and puts a test that only some node groups need in a rule set.
- **When the rule sets are checked:** lifecycle-manager checks the rule sets one time, when the batch period ends. But a group label can appear some time after the node joins. If the label is not on the node yet, lifecycle-manager does not add the tests of that group. To prevent this, the operator adds a condition to `newNodeValidation.criteria` that requires the group label. Then lifecycle-manager waits for the label before it adds the node to a batch. lifecycle-manager already waits for the `nvidia.com/gpu.present` label in the same way. The default criteria accept only a node that joined less than 30 minutes ago. If the group label can appear later than that, the operator must increase this time.
- **One ValidationRequest for each set of nodes:** lifecycle-manager finds the nodes that need each test. Tests that need the same nodes go into one ValidationRequest. For example, nodes A and B are in one batch, and only node A is a GB200 node. lifecycle-manager creates one ValidationRequest for nodes A and B with `dcgm-diag-test`, and one ValidationRequest for node A with `nccl-loopback-nvswitch`.
- **A failed rule set:** If a rule set fails, lifecycle-manager creates no ValidationRequest for the batch. It logs an error, increments a metric with the rule set name, and tries again. The nodes stay out of service.
- **More than one ValidationRequest:** lifecycle-manager cannot create all the ValidationRequests of a batch in one step. Thus, it keeps each node out of service until it has created all the ValidationRequests of that node. How lifecycle-manager keeps this plan is an open question.
- **No new rule sets:** With an empty `newNodeValidation.ruleSets`, lifecycle-manager creates one ValidationRequest for the batch, as before this ADR. If an operator removes the rule sets while a batch is in progress, that batch finishes with the tests that lifecycle-manager already selected.

### Safety rules

Both validation paths must follow these rules.

1. **No uncordon before all the work is recorded:** A component does not uncordon a node while a part of its validation is not recorded on the node. In post-remediation validation, this part is a pending selection. In new-node validation, it is a ValidationRequest that lifecycle-manager planned but did not create yet.
2. **A failure keeps the node cordoned:** If a step fails, the component does not skip the step, and the node stays cordoned. A metric shows each failure, so that an operator can find the node.
3. **A retry checks what it finds:** A retry can find a ValidationRequest that already exists, for example after a restart. The retry uses that ValidationRequest only if the same component created it, and only if it has the expected nodes and tests. If not, the retry fails, and rule 2 applies.
4. **Validation continues after a retry or a restart:** When a node joins a new-node batch, lifecycle-manager records this on the node. After a retry or a restart, lifecycle-manager reads the record and continues the validation. It does not check `newNodeValidation.criteria` again, so it does not drop a node that is now older than 30 minutes. fault-quarantine does the same with the pending selections that it records.
5. **Saved data has a version:** Each JSON format that a component saves on a node has a version number. A new version of the component can read the data of the previous version. If a component finds a version or a type of entry that it does not know, it keeps the node cordoned.

## Rationale

- **Node labels identify the node group:** Nodes already have labels for their GPU model and node pool, and operators already use them. ADR-064 also selects by node labels. Thus, operators use one method to describe a node group in all components.
- **One rule set format:** Operators already know the format of `validation.ruleSets` in fault-quarantine. With the same format in lifecycle-manager, an operator writes a rule set one way for both paths, and the existing configuration does not change.
- **Rule sets only add tests:** In ADR-064, one rule set wins, because a node can get only one drain or one remediation. A node can run many tests, so all the tests of all the matching rule sets can run. Thus, the order of the rule sets does not matter, and a node in two groups gets the tests of both groups. A rule set cannot remove a test, so an error in a rule set cannot make validation weaker than it was before this ADR.
- **Tests are selected when the fault arrives:** fault-quarantine already selects the tests at this time, so the flow of the saved tests does not change. The stable labels that this ADR recommends do not change during a repair. Thus, the rule sets select the same tests when the fault arrives as they would select when the fault recovers.
- **A failed selection keeps the node out of service:** If fault-quarantine skips a failed rule set, the node can miss a test that it needs. Then a node with a fault can return to service. A node that returns to service late is safer than a node with a fault that returns to service.
- **Retry for each node:** A failed selection is often temporary, for example when the node is not in the cache yet. A retry for each node lets the other nodes continue, as in node-drainer.
- **One ValidationRequest for each test:** Some tests run across many nodes, for example an NCCL test. Such a test must have all its nodes in one ValidationRequest. If lifecycle-manager put each node with the nodes that have the same full list of tests, a node with one more test would go into a different ValidationRequest, and the test across nodes would be divided.
- **Wait for labels with the criteria:** `newNodeValidation.criteria` already makes lifecycle-manager wait for the `nvidia.com/gpu.present` label. The same method can wait for a group label, so this ADR adds no new method to wait.
- **Empty by default:** With no new rule sets, validation works as before this ADR. An operator adds rule sets only after the upgrade.

## Consequences

### Positive

- Each node group runs the tests that it needs. A node does not run a test for hardware that it does not have.
- Post-remediation validation does not skip a failed rule set. Thus, a node cannot return to service without a test that it needs.
- Validation uses the same `Node` conditions as node-drainer and fault-remediation in ADR-064. Thus, operators describe a node group in one way in all components.
- Existing configuration works without changes. With no new rule sets, validation works as before this ADR.

### Negative

- A failed selection keeps a node cordoned until a retry succeeds. If a rule set always fails, for example because its expression has an error, the node stays cordoned until an operator corrects the rule set.
- A node without its group label gets only the tests that all nodes get. It does not get the tests of its group.
- A new-node batch can create more than one ValidationRequest, and lifecycle-manager saves more data on each node while the batch is in progress.
- A rollback to a version before this ADR can leave a node cordoned. The old version cannot read the data that the new version saves for a batch in progress.

### Mitigations

- For each failure, the component logs an error and increments a metric with the rule set name. Thus, an operator can find the node and the rule set.
- At startup, the components check the rule sets. lifecycle-manager does not start if a new-node rule set has a `HealthEvent` condition. The components log a warning for a condition that reads a label but does not test for its key.
- The documentation tells operators to put the tests that all nodes need in a rule set without a `Node` condition. Then a node without its group label still gets these tests.
- The new rule sets are empty by default. Thus, the new data is saved only after an operator adds rule sets. The documentation tells operators to upgrade all components first, and then add rule sets.
- Before a rollback, the operator removes the new rule sets and waits until no batch is in progress. The documentation describes this procedure, and how to release a node that a rollback left cordoned.

## Alternatives Considered

### lifecycle-manager adds the post-remediation tests for each node group

fault-quarantine selects the tests for the fault, and lifecycle-manager adds the tests for the node group when it gets the ValidationRequest.

**Rejected because:** A ValidationRequest has only the node names and the test names. lifecycle-manager does not know the fault, so it cannot select a test for a fault on a specific node group, for example an NVLink fault on a GB200 node. To know the fault, it must read the event store or get the fault in the ValidationRequest. Both are larger changes, and they move the selection away from fault-quarantine, which has the fault.

### The health event includes the device model

The health monitors add the GPU model to each health event, and the rule sets check the event.

**Rejected because:** All the GPUs on a node are the same model, so the node label already gives the model. This change needs a new field in the health event and changes in all the health monitors. Also, new-node validation has no health event, so it cannot use the field.

### Each test checks the node and skips itself

A test checks for the hardware that it needs, and it passes without a check when the hardware is not there.

**Rejected because:** A test cannot know if the hardware is missing because of the node group or because of a fault. For example, a GB200 node with a failed NVSwitch looks the same as an H100 PCIe node, so the test passes and the node returns to service with a fault. Also, each test must have its own check for its node groups, so the node groups are not in one place in the configuration.

### Keep the skip of a failed rule set

fault-quarantine skips a rule set that fails, logs a warning, and creates the ValidationRequest with the other tests, as before this ADR.

**Rejected because:** A `Node` condition can fail, for example when the node is not in the cache yet. Then the node can return to service without a test that it needs (see Rationale).

### Select the post-remediation tests when the fault recovers

fault-quarantine checks the rule sets when the last unhealthy event recovers, not when each event arrives.

**Rejected because:** The stable labels that this ADR recommends do not change during a repair, so this gives the same tests. It also changes when fault-quarantine saves the tests, and other code reads the saved tests.

### One new-node configuration for each node group

lifecycle-manager gets a list of new-node blocks. Each block has its own criteria, batch period, and tests.

**Rejected because:** Each block copies the criteria that all nodes share. A node that matches two blocks needs a rule that selects one block. lifecycle-manager must also keep one batch for each block, not one batch. This alternative is better only if node groups need different batch settings, for example a GB200 NVL72 rack that must be in one ValidationRequest.

### Each test lists the nodes that it applies to

Each test in the validation configuration gets a `Node` condition, and a test runs only on the nodes that match it.

**Rejected because:** This is a second format to describe node groups, different from the rule sets of fault-quarantine and ADR-064. It also cannot check the health event, so post-remediation validation cannot use it.

## Notes

### Non-goals

- **Readiness checks for each node group:** The readiness checks in `readinessCriteria` can already read node labels, so they can already be different for each node group. This ADR does not change them.
- **Validation after a skipped drain:** ADR-064 lets a rule set skip the drain for a node group. Then fault-remediation does not repair the node, and fault-quarantine does not create a ValidationRequest, because ADR-049 validates a node only after a drain. This ADR does not change this behavior.
- **Externally managed nodes:** Validation does not read the `nvsentinel.dgxc.nvidia.com/managed=false` label ([ADR-040](040-external-remediation-request.md)). Thus, it can test and release a node that a different system manages. This problem also occurs in a cluster where all nodes are the same, so it is not a node-group problem. A separate ADR covers it.

### Open questions

- How does fault-quarantine save a pending selection? The preferred option is a separate Node annotation with a version, which keeps only the IDs of the pending events. At startup, fault-quarantine reads the IDs, gets the events from the event store, and adds them to the retry queue. The alternatives are to change the `quarantineValidationHealthEvent` annotation, or to keep the pending state in the event store.
- Does this ADR also fix the limit of ten entries in `quarantineValidationHealthEvent`? When a quarantined node has more than ten events with tests, fault-quarantine removes the oldest entry. The separate annotation for pending selections prevents the loss of a pending selection. It does not prevent the loss of an older selection that is complete.
- Where does lifecycle-manager keep the plan of a new-node batch that it divides into more than one ValidationRequest? The preferred option is a temporary entry in the `validation-session` annotation of each node in the batch. The alternatives are an internal ConfigMap, a new `ValidationBatch` CRD, and one ValidationRequest with tests for each node. A new CRD is the last choice, because it adds a permanent public API.
- What happens when a group label appears after the time limit in `newNodeValidation.criteria`? The node never joins a batch. It stays cordoned, and lifecycle-manager does not log an error. An operator can create a ValidationRequest for the node by hand, and lifecycle-manager uncordons the node if the tests pass. The preferred option is a new optional list, `newNodeValidation.waitFor`. lifecycle-manager checks it only after a node passes the criteria and joins a batch. It selects the tests of the node only when all the `waitFor` conditions are true. Thus, a late label does not drop the node. If the conditions are not true after a set time, lifecycle-manager logs an error and increments a metric, and the node stays cordoned. The alternative is to keep the criteria as they are, and to add only a metric for a node that stays cordoned without validation.

## References

- [ADR-003: Rule-Based Node Quarantine](003-rule-based-node-quarantine.md)
- [ADR-040: External Remediation Request](040-external-remediation-request.md)
- [ADR-049: Node Validation](049-node-validation.md)
- [ADR-062: Release Quarantine Actions Only at Session End](062-fault-quarantine-release-at-session-end.md)
- [PR #2026: ADR-064, node and device selection in each component (proposed)](https://github.com/NVIDIA/NVSentinel/pull/2026)
