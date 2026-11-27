# ADR-067: Kubernetes-Native Remediation Without a Database

## Summary

- **Remediation without a database.** In Kubernetes mode, the three consumers of health events use one `NodeHealth` object for each node with faults, instead of a database. These consumers are fault-quarantine, node-drainer and fault-remediation. This mode is for small clusters, where a database is more work to operate than it is worth. Large clusters keep a database.
- **Only open faults, written only when they change.** A NodeHealth object keeps the faults that are open now, not a history of events. Each consumer records its progress in its own section of `status`. Monitors still send every event. The platform connector writes the object only when a fault opens, changes or clears, or when a consumer or monitor asks for a repeat.
- **A generic connector.** The connector reads NodeHealth objects and Nodes only by name. It never lists or watches them, so it has no informer cache, only a short-lived copy of each object that it read ([details](#how-the-connector-writes-events)). It needs nothing from this copy after a restart, so the DaemonSet connector and the deployment platform connector run the same write code. Two rules of the `HealthEvent` data model tell it which events report the same fault, and which faults a healthy event clears. Instead of rules for particular monitors, it reads optional metadata keys that a monitor sets on the few events that need other treatment ([keys](#keys-that-monitors-set)).
- **One reconcile loop for all providers.** Each consumer runs one reconcile loop on `NodeStore`, a new store-client interface that the MongoDB, PostgreSQL and new `kubernetes` providers implement. Database installs keep today's loop until the new loop is proven in Kubernetes mode ([delivery order](#changes-in-each-component)). In each pass, a consumer reads the current state of one node and does only the work that is left ([details](#how-the-providers-fit-store-client)). The fault-quarantine module releases a node only when a newer version of the same object shows that the faults that hold the node cleared. It does not release a node when the object or its data is not there, or when the object has a new UID.
- **What we give up.** Rules can no longer match a few rarely used event details, such as the event message ([list](#changes-for-rules)). The health-events-analyzer, the event exporter and csp-health-monitor do not run in this mode yet. Kubernetes mode supports up to about 1,900 nodes with a 16 GB etcd. This figure assumes the default standing limit of the circuit breaker in this mode. With it, fault-quarantine cordons at most 10% of GPU nodes at one time ([details](#scale-and-etcd-usage)).

## Context

This ADR proposes remediation without a database, because of two problems. First, someone must operate the MongoDB or PostgreSQL database that the chart can install. They must set its storage and size, upgrade it and monitor it. In a small cluster, this work can cost more than remediation gives.

Second, the consumers act on each event only once, so if an event is lost, no consumer remediates its fault. The DaemonSet connector replies to a monitor before it writes the event, so a crash loses the events in its queue. Bugs in the recovery after a restart can also skip an event.

## Decision

We add **Kubernetes** as a third datastore option for small clusters, next to MongoDB and PostgreSQL. The Helm value `global.datastore.provider: kubernetes` selects it. The default does not change. In this document, *Kubernetes mode* means this option, and *database mode* means MongoDB or PostgreSQL. This ADR proposes to supersede [ADR-027](027-kubernetes-data-store.md), which stores one object for each health event ([why](#one-object-per-health-event-adr-027)). The first diagram shows the parts of this mode, and the list below explains them.

```mermaid
flowchart LR
  mon["Health monitors"] -->|"every event, as today"| pc["Platform connector<br/>DaemonSet, Deployment or both"] -->|"opens, changes, clears<br/>and requested repeats"| nh[("NodeHealth object, one for each node with faults<br/>spec: open faults, written only by the connector<br/>status: one section for each consumer")]
  pc -->|"Node conditions and Events, as today"| k8snode["Node"]
  nh <-->|"NodeStore"| fq["fault-quarantine"] -->|"cordon, taint, annotation"| k8snode
  nh <-->|"NodeStore"| nd["node-drainer"] -->|"evict"| pods["Pods"]
  nh <-->|"NodeStore"| fr["fault-remediation"] -->|"remediation request"| jan["janitor: reboot or reset"]
  ttl["TTL controller"] -->|"deletes idle objects"| nh
```

1. The platform connector, as a DaemonSet on each node or as a Deployment, compares each event with the open faults of the node. It updates one cluster-scoped `NodeHealth` object for the node: as a Deployment before it replies, and as a DaemonSet after it replies, as today. It adds new faults, updates changed faults and removes cleared faults. A clear is a healthy event that removes a fault. A repeat is an event that reports an open fault again with no change. The connector writes a repeat only in two cases ([details](#how-the-connector-writes-events)):
   - A consumer asks for the next report of the fault with a report-again request (`reportAgainUpTo`). It asks when it must decide again, for example after a failed drain or a finished remediation.
   - The monitor sets the metadata key `forcePublish` on the event.
2. The three consumers, fault-quarantine, node-drainer and fault-remediation, read the NodeHealth objects instead of the database. Each consumer works like a Kubernetes controller. In each pass for a node, it compares the open faults with its own records and does the work that is left. It gives each open fault to today's decision code as a health event. The node-drainer and fault-remediation modules record a decision before they act, and the fault-quarantine module updates the Node first, as today ([details](#rules-for-every-write)). The fault-quarantine module releases a node only when a newer version of the same object shows that the faults that hold the node cleared.
3. Three components use the database for more than remediation, so they do not run in this mode yet. Support for them is follow-on work. Of the 22 default rules of the health-events-analyzer, 21 read past events, and so do their datastore-neutral versions, which work with MongoDB and PostgreSQL. The event exporter sends every stored event to an HTTP endpoint. The csp-health-monitor keeps each planned maintenance event and its status in the database. Only these three use the event methods of store-client, which return an error in the `kubernetes` provider ([details](#how-the-providers-fit-store-client)).
4. All NodeHealth code is in a new `kubernetes` provider of store-client. Each module uses it as a library, in its own process, so no new component stands between the modules and the API server ([why](#a-store-proxy-deployment)). In Kubernetes mode, the connector writes each batch of events through one method of this provider, which applies the batch to the objects. Each consumer runs one reconcile loop on `NodeStore`, a new store-client interface for the state of one node, which all three providers implement. Only the providers differ between the modes ([details](#how-the-providers-fit-store-client)).
5. The TTL controller, which deletes janitor's remediation requests today, moves out of janitor into its own Deployment. It deletes a NodeHealth object that has no open fault and no consumer record, after it stays unchanged for 60 minutes ([Idle objects](#idle-objects)).

**One fault from detection to recovery.** The second diagram follows one fault in the simplest case. A quarantine session is one quarantine of a node, from its start to its release or cancel. A cancel happens when someone else uncordons or untaints the node. GPU 3 on node `gpu-node-17` logs XID 48, a memory error that calls for a GPU reset. The example assumes that partial drains, GPU resets and the log collector are turned on.

```mermaid
%%{init: {"sequence": {"useMaxWidth": false, "wrap": true, "width": 180, "actorMargin": 50, "messageMargin": 45, "actorFontSize": 18, "messageFontSize": 18, "noteFontSize": 18}}}%%
sequenceDiagram
  autonumber
  participant Mon as syslog-health-monitor
  participant PC as Platform connector
  participant NH as NodeHealth object
  participant FQ as fault-quarantine
  participant N as Node
  participant ND as node-drainer
  participant P as Pods
  participant FR as fault-remediation
  participant J as janitor
  Mon->>PC: Unhealthy event: XID 48 on GPU 3
  PC->>NH: Create the object with the new fault
  PC-->>Mon: Reply after the write, or before it in the DaemonSet
  NH-->>FQ: The watch shows the new fault
  FQ->>N: A rule matches: cordon, taint and annotate the Node in one update
  FQ->>NH: Record a new quarantine session
  ND->>NH: Record a drain of GPU 3
  ND->>P: Evict the pods that use GPU 3
  ND->>NH: Record that the drain succeeded
  FR->>NH: Record the GPU reset as Pending
  Note over FR: Run the log collector, then read the object again
  FR->>J: Create the GPU reset request
  FR->>NH: Record that the request runs
  J-->>FR: GPU 3 is reset, and the request succeeded
  FR->>NH: Record the result, and ask for the next report
  Mon->>PC: Healthy event: XID 48 on GPU 3 is clear
  PC->>NH: Remove the fault
  FQ->>N: No open fault holds the node: uncordon, untaint and remove the annotation in one update
  FQ->>NH: Record the release
  Note over ND,FR: The session ended, so both modules remove their records
  Note over NH: After 60 idle minutes, the TTL controller deletes the object
```

## Implementation

### The NodeHealth object

The connector creates the NodeHealth object of a node when it writes the first fault of the node. The object has the name of the Node, and the Node is its owner. So each Node has at most one object, and Kubernetes deletes the object when it deletes the Node. The consumers and the TTL controller never create an object ([Idle objects](#idle-objects)).

A new Node with the same name can appear before Kubernetes deletes the object of the old Node. So the connector and the consumers compare the owner UID of the object with the UID of the Node. The consumers treat an object with another UID as not there. Until the garbage collector deletes the old object, the deployment platform connector rejects events for the node with gRPC `UNAVAILABLE`. The monitor sends these events again, and the next event after the delete creates a new object. The DaemonSet connector tries the write again from its queue instead, and drops the events if the old object stays longer than its retries.

Only the connector writes `spec`, which contains the open faults that health events report for the node. Each consumer writes its decisions and its progress in its own section of `status`. Role-based access control (RBAC) keeps the connector out of `status` and the consumers out of `spec`.

**Faults and devices.** A fault is one error code that one check reports for one device. An event gives one fault for each of its error codes, or one fault when it has no error code. This is similar to database mode, where the fault-quarantine module keeps one key in its Node annotation for each error code and entity. The object groups the faults of each check in one check entry, which stores `agent`, `check`, `componentClass` and `version` one time for all its faults. These four fields are part of the identity of each fault.

Identity entities are all entities of an event, except the entity types that its `detailEntities` key lists ([keys](#keys-that-monitors-set)). A device entry contains the faults of one check that have the same identity entities. So in this document, a device is one set of identity entities, not always one part of the hardware. One GPU can have more than one device entry. Each fault has its own `isFatal`, action and other fields, because one device can have errors that need different actions.

A device entry with no entities is for the whole node. Each entity keeps its type and its value in two fields, as in the `HealthEvent` proto. Each field accepts any string, so the connector rejects no event because of its entities.

**Size and cap.** An object with one fault is about 1.7 KB. An object can have at most 128 device entries and 256 faults, and a device entry can have at most 8 entities. All health monitors share this one cap. The cap has room for all faults of a full failure of any common GPU node, where each GPU, NVSwitch and NIC port fails ([sizes](#scale-and-etcd-usage)). It also has room for the faults that the shipped components send about other nodes, with their default settings.

A new fault needs room for one fault, and also for one device entry if its device is new. Repeats, changes and clears need no room, so a full object never rejects them. The connector keeps the first 8 identity entities of an event, in their order, and drops the others. When the last fault of a device entry clears, the connector removes the device entry too.

When a new fault has no room, the deployment platform connector rejects it with the gRPC code `RESOURCE_EXHAUSTED`, and a metric counts it. The direct client, which sends to it, keeps its current behavior. It tries again for up to 5 minutes, its retry window, and then returns an error. During that time, the later events of that monitor wait, also its events about other nodes. The DaemonSet connector has already replied, so it tries the write again from its queue, as for a database today. Then it drops the new fault, and a metric counts it.

With the deployment platform connector, the `Publish` contract tells the monitor to send such an event again, so the fault is late but not lost. In syslog-health-monitor, the journal position stays at the line that it could not deliver. In gpu-health-monitor, the state of a fault changes only after the connector accepts its event. So both monitors send the fault again at their next poll, by default after 15 seconds. The other shipped health monitors send it again at their next poll or reconcile. A preflight check sends its fault again only when the check runs again.

A new fault waits or is lost only when the object is full: 256 faults, or 128 device entries for a new device. Each stored fault is fatal or sets `quarantineOverrides.force`, so a fault in the object usually holds the node already. The node then stays quarantined while the new fault waits, or after the DaemonSet connector drops it.

**Decisions for each fault.** The fault-quarantine module records the quarantine session and its state in `status.faultQuarantine`. As today, its Node annotation lists the faults that hold the node. It records nothing for an open fault that no rule matches, and it evaluates such a fault again in each pass.

The node-drainer and fault-remediation modules keep the decisions of [ADR-064](064-node-and-device-selection.md) for each fault, as database mode keeps them on each event. They identify a fault by its write number, `seq`, which the connector gives the fault at each write ([write numbers](#rules-for-every-write)). When the connector writes a fault again, the fault gets a new number, so each module decides again, as for a new event today. In each write, a module removes the numbers of faults that are not open now, but only from the lists `faults`, `skipped` and `stopped`. A drain record or a remediation record stays until its session ends. The next table shows where the `kubernetes` provider keeps each decision that ADR-064 records.

| ADR-064 | Kubernetes mode |
|---|---|
| Quarantine session | `faultQuarantine.session` |
| `drainDecision.method` and `drainDecision.ruleSet` | `nodeDrainer.method` and `nodeDrainer.ruleSet`, one of each for each session |
| `drainDecision.scope` | The scope of the drain record |
| `drainDecision.reusedFrom` | Not needed, because the drain record lists each fault that it covers |
| Drain status `Stopped` | `nodeDrainer.stopped` |
| Drain status `Succeeded` or `AlreadyDrained` | A `Succeeded` drain record of the session lists the fault, or `nodeDrainer.skipped` lists it |
| `remediationDecision.action` and `remediationDecision.ruleSet` | The `action` and `ruleSet` of the remediation record |
| `remediationDecision.stopped` | `faultRemediation.stopped` |

**Example.** The example shows all fields of the object, and its comments explain them. The object stores no data that it can get elsewhere: no summary of the faults, no history of remediations and no count of repeats. Node `gpu-node-17` is an HGX node with partial drains and GPU resets turned on, which are off by default.

The fault-quarantine module started session 8 for XID 48 on GPU 3. The connector first wrote this fault with the write number 38, so this example calls it fault 38. Unlike the second diagram, an operator deleted the first reset request, so the reset failed.

The fault-remediation module then set its `reportAgainUpTo` to 40, the `spec.seq` that it read. Writes 39 and 40 were clears of other faults. When GPU 3 logged XID 48 again, the connector wrote it as fault 41, because 38 is at or below 40. In the same write, XID 64 on GPU 3, which also calls for a GPU reset, opened as fault 42. The reset for fault 41 also covered fault 42 and succeeded, and then syslog-health-monitor cleared both faults in write 43. Now the node waits for a reboot, because GPU 5 fell off the bus and a maintenance request also asks for a reboot.

```yaml
apiVersion: healthevents.dgxc.nvidia.com/v1alpha1
kind: NodeHealth
metadata:
  name: gpu-node-17                           # the name of the Node
  uid: 0d4b6c3e-2a91-4f7e-b8d5-6c1f0e9a2b47  # the first part of each event ID
  generation: 38                              # changes only when spec changes; a connector write fails if it changed
  ownerReferences: [{apiVersion: v1, kind: Node, name: gpu-node-17, uid: 78ff5c0a-e565-4211-8378-6ddb47ee2a6b}]
spec:                                         # only the connector writes spec
  seq: 53                                     # the last write number; a write with no fault, such as a clear, also takes one
  checks:
  - agent: syslog-health-monitor              # agent, check, componentClass and version make the check entry
    check: SysLogsXIDError
    componentClass: GPU
    version: 1
    devices:                                  # GPU 3 has no device entry in this check: faults 41 and 42 cleared after the reset for fault 41
    - entities: [{type: PCI, value: "0002:00:05"}, {type: GPU_UUID, value: GPU-9b1d2f30-7c44-11ee-b962-0242ac120002}]   # GPU 5, which fell off the bus; at most 8 identity entities
      faults:
      - errorCode: "79"                       # no errorCode when the event has none; optional: customRecommendedAction, quarantineOverrides
        isFatal: true                         # a fatal fault stays fatal while it is open
        recommendedAction: RESTART_BM
        seq: 45                               # event ID <uid>-45
        generatedTimestamp: "2026-09-16T12:46:12.104382Z"   # from the last written event; read by metrics, never by a rule
  - agent: gpu-health-monitor                 # another check entry, with a device entry for GPU 3
    check: GpuPcieWatch
    componentClass: GPU
    version: 1
    devices:
    - entities: [{type: GPU, value: "3"}, {type: PCI, value: "0002:00:03"}, {type: GPU_UUID, value: GPU-67fec4a7-a3e1-385c-0c98-4361b4a372ca}]
      faults:
      - {errorCode: DCGM_FR_PCI_REPLAY_RATE, isFatal: true, recommendedAction: CONTACT_SUPPORT, seq: 44, generatedTimestamp: "2026-09-16T12:45:30.518220Z"}
  - agent: nic-health-monitor
    check: InfiniBandStateCheck
    componentClass: NIC
    version: 1                                # an override rule of the operator set drainOverrides.skip on this port fault
    devices:
    - entities: [{type: NIC, value: mlx5_2}, {type: NICPort, value: "1"}]
      faults:
      - {isFatal: true, recommendedAction: CONTACT_SUPPORT, drainOverrides: {skip: true}, seq: 46, generatedTimestamp: "2026-09-16T12:46:40.007341Z"}
  - agent: lifecycle-manager                  # sends faults about other nodes; check and action come from the maintenance request
    check: PlannedMaintenance
    componentClass: NODE
    version: 1                                # no entities: the fault is for the whole node; in Kubernetes mode, each maintenance request must be fatal; attributes contains the keepMetadata keys
    devices:
    - entities: []
      faults:
      - {isFatal: true, recommendedAction: RESTART_BM, attributes: {maintenanceRequestName: mr-7f3c2a}, seq: 47, generatedTimestamp: "2026-09-16T12:47:02.660915Z"}
  - agent: slurm-drain-monitor                # a custom fatal pattern of the operator; no shipped rule set matches it
    check: SlurmNotResponding
    componentClass: NODE
    version: 1                                # not held: fault-quarantine evaluates it again in each pass, with no write
    devices:
    - entities: [{type: v1/Pod, value: slurm/slurmd-gpu-node-17}]
      faults:
      - {isFatal: true, recommendedAction: CONTACT_SUPPORT, seq: 52, generatedTimestamp: "2026-09-16T12:52:19.381774Z"}
status:                                       # each consumer writes only its own section
  faultQuarantine:
    session: 8                                # the quarantine session of ADR-064; plus one in the write that records the start of a quarantine
    state: Quarantined                        # or WouldQuarantine (dry run), Released or Cancelled; the Node annotation has the keys of faults 44 to 47
    reportAgainUpTo: 30                       # set when an operator cancelled session 7 with an uncordon
  nodeDrainer:
    session: 8                                # the session of all records below
    method: BuiltIn                           # drainDecision.method of ADR-064, chosen one time in each session
    ruleSet: ""                               # drainDecision.ruleSet; "" is the default
    reportAgainUpTo: 0                        # set when a drain fails
    drains:                                   # at most one Running or Succeeded record for each scope; the timeout counts from startedAt; a custom drain record also has request, the name of the custom drain
    - {scope: {type: GPU_UUID, value: GPU-67fec4a7-a3e1-385c-0c98-4361b4a372ca}, state: Succeeded, faults: [], startedAt: "2026-09-16T12:39:58.270113Z", finishedAt: "2026-09-16T12:40:41.902455Z"}   # faults 41 and 42 cleared; the record stays until the session ends
    - {state: Succeeded, faults: [44, 45, 47], startedAt: "2026-09-16T12:45:51.336071Z", finishedAt: "2026-09-16T12:47:30.118529Z"}   # no scope: the whole node, which these faults need
    skipped: [46]                             # drainOverrides.skip, so fault 46 counts as drained
    stopped: []                               # faults that a drain stop rule set of ADR-064 stopped
  faultRemediation:
    session: 8
    reportAgainUpTo: 42                       # 40 after the reset for fault 38 failed; 42 after the reset for fault 41 finished
    remediations:                             # Pending, Running, Succeeded or Failed; a deleted request counts as Failed; startedSeq is the spec.seq that the module read when it decided, raised when it records Running
    - {target: {type: GPU_UUID, value: GPU-67fec4a7-a3e1-385c-0c98-4361b4a372ca}, action: COMPONENT_RESET, ruleSet: "", fault: 38, startedSeq: 39, state: Failed, request: maintenance-gpu-node-17-0d4b6c3e-2a91-4f7e-b8d5-6c1f0e9a2b47-38}   # an operator deleted the request
    - {target: {type: GPU_UUID, value: GPU-67fec4a7-a3e1-385c-0c98-4361b4a372ca}, action: COMPONENT_RESET, ruleSet: "", fault: 41, startedSeq: 42, state: Succeeded, request: maintenance-gpu-node-17-0d4b6c3e-2a91-4f7e-b8d5-6c1f0e9a2b47-41}   # fault 38 written again; also covered fault 42
    - {action: RESTART_BM, ruleSet: "", fault: 45, startedSeq: 53, state: Pending, request: maintenance-gpu-node-17-0d4b6c3e-2a91-4f7e-b8d5-6c1f0e9a2b47-45}   # no target: the whole node; the log collector runs; also covers fault 47
    stopped: []                               # faults that a remediation stop rule set of ADR-064 stopped
```

**Compatibility.** The NodeHealth type becomes a public interface that must stay backwards compatible. The backwards compatibility review covers its Go types, and the CRD schema comes from them.

A `helm upgrade` does not update CRDs, so each component checks at start that the installed CRD has each field that it writes. A writer keeps the fields that it does not know. So during a rolling upgrade, an older connector does not remove the fields of a newer one.

The fault fields come from the `HealthEvent` proto of today. A unit test lists each proto field as stored, rebuilt or dropped. So a new proto field fails the test until someone decides where it goes. The consumer sections contain the information of the `HealthEventStatus` of today, except `lastRemediationTimestamp`, which no component reads. The consumers write them only through the typed writes of `NodeStore`. So a new status field needs a change in each provider ([How the providers fit store-client](#how-the-providers-fit-store-client)).

### Rules for every write

- **Conditional writes.** Every write, and the delete of an idle object, is conditional on the version that the writer read. So no writer overwrites or deletes a change that it has not seen. The consumers and the TTL controller use `metadata.resourceVersion`.
- **The write condition of the connector.** The connector makes its writes conditional on the UID and `metadata.generation`. The API server changes `metadata.generation` only when `spec` changes. So the writes of the consumers to `status` never make a write of the connector fail. In `status`, the connector reads only `reportAgainUpTo`, and this value only goes up. So an old copy of the object can only delay a repeat that a consumer asked for.
- **Own sections.** Each writer patches only its own section, so a writer from an older release never changes the section of another writer.
- **Failed writes.** After a failed write, the connector reads the object again and handles the event once more. If the deployment platform connector still cannot write, it rejects the event with gRPC `UNAVAILABLE`. It stops work on a batch when the gRPC call that sent the batch ends. So a slow replica cannot apply an old event after the monitor already sent that event again. The DaemonSet connector tries the batch again from its queue, as today, and then drops it.
- **Records and actions in today's order.** The node-drainer and fault-remediation modules record each decision before they act, as ADR-064 does. The fault-quarantine module first updates the Node, with its annotation, and then records the change in its section, as today. It records a cancel first and then removes its annotations, also as today. After a crash between the two steps, the next pass uses the annotation to finish the step ([The passes](#how-the-consumers-reconcile-a-node)).
- **Write numbers.** For each fault that it writes, the connector adds one to `spec.seq` and gives the new value to the fault. A write with no fault, such as a clear, also adds one to `spec.seq`. Each fault keeps the number of its last write in `seq`, so a rewritten fault gets a new number and a new event ID. A consumer that records a point in the history of the object, such as `reportAgainUpTo` or `startedSeq`, stores the `spec.seq` that it read. The `seq` of a fault is its [order key](#how-the-providers-fit-store-client): the value that the consumer code compares to put faults and records in order.
- **Numbers only go up.** A new object has a new UID, and its `spec.seq` starts again at 1. Event IDs stay unique, because each one starts with the UID. The CRD rejects a write that removes or lowers `spec.seq`, a `session` or a `reportAgainUpTo`. So a restore through the API server cannot write an older `spec` over a newer one. A restore of etcd does not go through the API server, so it can still bring back older objects ([Stale faults and objects](#stale-faults-and-objects)).
- **Times.** Each fault keeps the `generatedTimestamp` of its last written event. Each drain record keeps `startedAt` and `finishedAt`, which take the place of `quarantineFinishTimestamp` and `drainFinishTimestamp` of today's event status. The object does not store `lastRemediationTimestamp`, because no component reads it. The times use `metav1.MicroTime`, because some duration metrics measure less than one second. Only metrics compare times from different hosts.
- **The drain timeout.** In Kubernetes mode, the node-drainer module counts the drain timeout from the start of its drain record, on its own clock. Database mode keeps today's start, the creation of the event, which is earlier when a drain starts late. In Kubernetes mode, a pass can match a fault long after the fault opened ([why](#how-the-providers-fit-store-client)). A timeout that counts from the `generatedTimestamp` of the fault could then delete pods at once, with no grace period.

### How the connector writes events

The connector keeps a copy of the object of each node that it gets events for, only to save reads. When it has no copy, it reads the object by name. It also reads the Node by name, to set or check the owner reference of the object. If the Node does not exist, the connector accepts the event and writes nothing. It never lists or watches NodeHealth objects or Nodes, and the `kubernetes` provider starts no informer for it. So a connector on every node adds no list or watch load.

Another replica may have written earlier events, so the connector reads the object again at the first event for the node on each new connection. All events of one connection must reach the same replica, as they do with a Kubernetes Service and the default gRPC client. When a repeat arrives and the copy of the object is older than one minute, the connector reads the object again. So it sees a report-again request within about one minute. It drops the copy of a node when no event for that node arrives for `global.datastore.kubernetes.copyIdleTime`, 10 minutes by default.

**The same fault, and clears.** Two rules of the `HealthEvent` data model say which events report the same fault, and which faults a healthy event clears. One shared implementation follows the comments of the proto. It does not use the code or the rules of a consumer. Two events report the same fault when they have the same check entry, the same error code or none, and the same identity entities. A healthy event clears an open fault when all of these conditions are true:

- The event and the fault have the same check entry.
- The event has the error code of the fault, or no error code.
- The event has no entities, or it names every identity entity of the fault. For this condition, entities of the types that the event lists in `detailEntities` do not count. When the event has entities of any type, at least one entity of a type that `detailEntities` does not list must match.

Only a healthy event with no entities clears a fault with no entities. A healthy event that names only some entities of a fault leaves the fault open. In database mode, the node also stays quarantined in this case, because the annotation keys of the entities that the event does not name stay. Someone can replace a GPU with a new GPU in the same slot. Then the `detailEntities` key lets the healthy events of gpu-health-monitor clear the faults of the old GPU, as in database mode ([keys](#keys-that-monitors-set)).

For each event, the connector looks for the same fault in its copy. Then:

- **A new or changed fault.** The connector writes it with a new number. A fault changes when it becomes fatal, or when its action or override flags change. A fatal fault stays fatal while it is open. A change of only the attributes that `keepMetadata` keeps causes no write, but each write stores the attributes of its event.
- **A clear.** The connector removes the fault ([details](#how-faults-clear)). A healthy event for a fault that is not open does nothing, and a clear never creates an object.
- **A repeat of an open fault.** It causes no write, except in two cases, in which the connector writes the repeat with a new number:
  - **When a consumer asks.** A consumer that needs a new report of the open faults makes a report-again request. It sets `reportAgainUpTo` in its own section to the `spec.seq` that it read. The connector then writes the next repeat of each open fault whose `seq` is at or below the largest `reportAgainUpTo` in `status`. The repeat gets a new, larger number, so each request causes at most one write for each fault.
  - **When its monitor asks.** The event has the metadata key `forcePublish: "true"`, which a monitor sets when it sends a fault again on purpose. For example, while gpu-health-monitor runs, it sends each fault until the connector accepts it, and then not again. After each start, such as after a reboot, it sets the key on the first event of each fault. The connector writes such a repeat once for each fault on each monitor connection. It keeps the list of these faults in memory, one list for each connection.

The connector does not know why a consumer asks. A consumer asks so that it gets the next report of a fault, as it gets the next event in database mode. The fault-quarantine module asks after a cancel and after a `CREATE` reset of the circuit breaker ([Safety rules](#safety-rules)). The node-drainer module asks when a drain fails, so each fault of the drain gets a new drain decision at its next report. The fault-remediation module asks when a remediation attempt finishes, so the next report lets it retry or escalate. As in database mode, a repeat that the connector writes after the start of an attempt is a newer event of the fault.

**Which events the connector uses.** Like the fault-quarantine module today, the connector uses only `EXECUTE_REMEDIATION` events, healthy or unhealthy, because `STORE_ONLY` and `STORE_AND_ANALYSE` events must not change the cluster. Kubernetes mode keeps no record of them, so an install that needs them can forward them with the gRPC sink connector. The connector also skips unhealthy events that are not fatal, unless the event sets `quarantineOverrides.force`. The connector does not filter unhealthy events by the rules of fault-quarantine, because these rules read Node labels that can change.

Every shipped rule set acts only on fatal events, except the rule set for maintenance requests. So in Kubernetes mode, the webhook of lifecycle-manager rejects a MaintenanceRequest with `isFatal: false`, as it already rejects one with a healthy event. A maintenance request cordons and drains the node, so it is fatal as the `HealthEvent` data model defines it. For planned cloud maintenance, csp-health-monitor already sends fatal events. An install whose own rule sets act on events that are not fatal turns on the `OverrideTransformer` stage of the connector. An override rule there marks these events fatal.

The connector reads each event after the stages that add Node metadata and apply override rules, and before the deduplication stage. That stage is off by default and never drops an event. It only changes a repeated event to `STORE_AND_ANALYSE`, so that the fault-quarantine module and the Node conditions ignore it. But this stage can wrongly mark a new event as a repeat, for example when a connector replica missed the clear. Because the connector reads the event before this stage, such a wrong mark cannot hide a fault that came back. In database mode, the database writer still gets each batch after every stage, as today.

**Replies.** For each batch, the connector makes one write for each node, with every event for that node that it can write. The provider returns one error for the whole batch. The DaemonSet connector replied before the write, so it tries a failed batch again from its queue and then drops it, as today. The deployment platform connector sends the first of these replies that applies, or success when none applies:

- `UNAVAILABLE`: a read or a write failed, or the object still names the UID of an earlier Node with the same name ([The NodeHealth object](#the-nodehealth-object)).
- `RESOURCE_EXHAUSTED`: a new fault waits for room in a full object.

### Keys that monitors set

The connector has no rules for particular monitors, checks, entity types or error codes. Instead, a monitor marks an event that needs other treatment with an optional metadata key. The `HealthEvent` schema does not change. The connector does not store the keys in the object. In database mode, the keys go into the database with the other metadata and change nothing. A key that lists items separates them with commas.

| Key | What the connector does | Where it is set |
|---|---|---|
| `forcePublish: "true"` | Writes this repeat of an open fault, once for each fault on each monitor connection | The first event of each fault after gpu-health-monitor starts |
| `detailEntities` | Removes the listed entity types from the event before it compares the event with the open faults and before it stores a fault. A healthy event also clears faults whose entities of these types differ ([clear rule](#how-the-connector-writes-events)) | `NVLINK` on the SXID events of syslog-health-monitor. `REG0` to `REG6` on its XID 74 events, and `GPC`, `TPC` and `SM` on its XID 13 events. `GPU_UUID` on the healthy events of gpu-health-monitor |
| `keepMetadata` | Stores the listed metadata keys in the fault's `attributes`. A change of only these values causes no write | `maintenanceRequestName` on the events of lifecycle-manager, which the shipped rule set for maintenance requests reads. `creator_id` on the events of a tool that forces a quarantine, which the `cordon-by` Node label reads |

These keys keep today's behavior. The SXID check clears its faults only together, after a reboot, so the NVLink of an SXID event needs no fault of its own. Without the `detailEntities` key, the SXIDs of a complete failure of an HGX A100 node would need 96 device entries. Because XID 74 keeps its NVLink as an identity entity, the healthy event of a GPU reset, which names no NVLink, does not clear it. Monitors still send the entities that `detailEntities` lists, because database mode stores them and the health-events-analyzer reads some of them. A monitor without these keys, such as a custom monitor, gets safe defaults: every entity is an identity entity, so no fault clears early.

### Write rate

The connector writes the object of a node only when a fault opens, changes or clears, and for the two kinds of repeat ([details](#how-the-connector-writes-events)). It writes all changes of one batch for one node in one write. So the writes follow changes of state, not the rate of events. For example, a GPU that floods XID 137 and 145 at 120 to 600 events a second causes at most 2 writes at first. Like database mode, the connector has no write limit for each node. A limit in memory can come later, with no change to the object.

Of the shipped monitors, a NIC port that goes down and up is the fastest source, at about one write a second. In both modes, each cycle also changes the condition and the cordon of the Node, at a higher cost to etcd. The `portStateHoldDown` option of nic-health-monitor, off by default, stops both for a port that recovers within the hold-down time. As in database mode, other ways to handle such devices are out of scope. The connector keeps the `qps` and `burst` limits of its Kubernetes client, under the default API Priority and Fairness rules of the API server. A metric counts the writes for each node and reason.

### How the providers fit store-client

The `kubernetes` provider registers like the PostgreSQL provider. It is in the package `store-client/pkg/datastore/providers/kubernetes`, which `store-client/pkg/datastore/providers/providers.go` imports. It registers with `RegisterProvider` in `store-client/pkg/datastore/registry.go` as `ProviderKubernetes`. Each module selects it with `global.datastore.provider: kubernetes`. All NodeHealth code stays inside the provider.

**Why the provider does not implement `HealthEventStore`.** Today the consumers work on single events, in a chain. The fault-quarantine module reads a stream of new events, healthy events too, and writes its decision on each event by its ID. The node-drainer module acts on such a decision, and the fault-remediation module acts on a drain result. After a restart, each module reads its stream again from where it stopped. `HealthEventStore`, the change stream and the queries of `DatabaseClient` give this access.

The PostgreSQL provider implements these interfaces, because it stores the same data as MongoDB: one row for each event, with its status. Triggers also copy each change into a change log, which its change stream reads. So the PostgreSQL provider only translates the queries and the change stream. A NodeHealth object keeps only the faults that are open now. It has no row for each event, no healthy events, no status for each event and no history. A clear removes the fault.

To implement the event interfaces, the `kubernetes` provider must add these items to each NodeHealth object:

- A made-up healthy event for each clear, because the fault-quarantine module records a release on the healthy event.
- A status for each event and each consumer.
- Each cleared fault and each made-up event, until every consumer reads it.
- The read position of each consumer, because a watch of the API server can replay only a few minutes of changes.

The objects then grow with the events instead of with the open faults. The delete of old records can also lose a clear when a consumer stops for too long. The node then stays quarantined until an operator acts. This is the event store of ADR-027 in another form ([why not](#one-object-per-health-event-adr-027)).

So `DataStore` gets a second accessor, `NodeStore()`, next to `HealthEventStore()`. Each provider implements `NodeStore`, an interface for the state of one node. One shared implementation serves MongoDB and PostgreSQL, and it calls their event interfaces. So their stored data and their event interfaces do not change. The decision code of the consumers still gets one health event for each open fault ([details](#how-the-consumers-reconcile-a-node)). The next block shows the two interfaces:

```go
type DataStore interface {
    HealthEventStore() HealthEventStore // as today; removed in the last delivery step
    NodeStore() NodeStore               // new: the state of one node
    // Ping, Close and Provider as today
}

type NodeStore interface {
    // Watch sends the name of each node whose state may have changed.
    // It first sends every node with open faults or work that is not done.
    Watch(ctx context.Context) (<-chan string, error)
    // View returns the current state of one node for this consumer, whatever
    // started the pass: the faults that hold the node, the faults that the
    // consumer has not decided yet, the quarantine session and its records.
    // Each fault and record has an order key.
    View(ctx context.Context, node string) (*NodeView, error)
    // Typed writes. In Kubernetes mode, each write fails if the object
    // changed after the view.
    RecordQuarantine(ctx context.Context, v *NodeView, q Quarantine) error
    RecordRelease(ctx context.Context, v *NodeView, r Release) error
    RecordDrain(ctx context.Context, v *NodeView, d Drain) error
    RecordRemediation(ctx context.Context, v *NodeView, r Remediation) error
    RequestReportAgain(ctx context.Context, v *NodeView) error
}
```

`NodeStore` has a different shape from `HealthEventStore` on purpose. `HealthEventStore` finds events with a query and changes the status of one event by its ID. `NodeStore` reads the state of one node and records the decision of one consumer, so a provider without an event history can implement it.

One contract suite tests the `NodeStore` implementations of all three providers. The registry tests and the `DataStore` compliance tests also add the `kubernetes` provider. We write each consumer once, against `NodeStore`, with no change to its decision code. To find which fault or record is newer, the consumer code compares only their order keys. A new scenario suite runs the consumers on all three providers. Only the providers differ between the modes:

| Method | MongoDB and PostgreSQL | Kubernetes |
|---|---|---|
| `Watch` | The change stream, as node names. It first sends each node that has events with work that is not done, and it sends them again at each resync. This replaces the cold start, so a missed change only delays an event. It keeps the lag metrics of the change stream, with the same names and labels | An informer on the NodeHealth objects, with a resync ([triggers](#how-the-consumers-reconcile-a-node)) |
| `View` | Each pass runs one indexed query. It reads the events of the node that the consumer has not decided yet, the events that the quarantine annotation lists, and their statuses. The statuses of today mark what each consumer decided, so `View` does not depend on what started the pass. Like the cold start of today, it skips an undecided fault that a later undecided clear supersedes. The order key is `createdAt`, and `View` returns the quarantine session as ADR-064 defines it. This query may need a compound index on the node name and the status fields | The NodeHealth object: all open faults and the sections of `status`. The order key is `seq` ([write numbers](#rules-for-every-write)) |
| `RecordQuarantine`, `RecordRelease`, `RecordDrain`, `RecordRemediation`, `RequestReportAgain` | Write the same event statuses as today, at the same points. For example, they write the quarantine status after the cordon. So the stored data does not change, except after a crash ([details](#negative)). Old and new modules also work together during a rolling upgrade. `RequestReportAgain` does nothing, because every event arrives anyway, except after a `CREATE` reset of fault-quarantine. Then it makes `View` skip the events of the node that fault-quarantine has not decided yet, as the `CREATE` cursor does today | Write the section of the consumer in `status`. `RequestReportAgain` sets `reportAgainUpTo` there |
| Connector write | Today's store connector, with no change (`InsertManyIdempotent`) | One method of the provider, `ApplyHealthEvents`, with one write for each node and one error for the batch ([how](#how-the-connector-writes-events)) |
| Methods of `HealthEventStore`, the change stream watcher and the filter methods of `DatabaseClient` | As today | Return an error, because the provider keeps no event history and no change stream of events. No consumer calls them in this mode, because the consumers run the `NodeStore` loop. The health-events-analyzer, the event exporter and csp-health-monitor call only the change stream watcher and `DatabaseClient`, and these three do not run in this mode. The `DataStore` compliance tests expect this error |

**The difference that a consumer can see.** In Kubernetes mode, the view contains all open faults, because the fault-quarantine module records nothing for a fault that no rule matches. So a changed rule or Node label can apply to an open fault at the next pass for the node. In database mode, the view contains the faults that hold the node and the events that the consumer has not decided yet. So such a change applies only at the next event of the fault.

This ADR adds no mechanism for such changes, which are out of scope. The standing limit of the circuit breaker bounds the number of nodes that the fault-quarantine module can cordon after such a change ([Safety rules](#safety-rules)).

### How the consumers reconcile a node

Each consumer works like a Kubernetes controller. It puts the node names from `Watch` in its own queue, which contains each node at most once. For each node in the queue, it runs one pass. The pass reads the view from `NodeStore` and the live state, such as the Node, its pods, the custom drains and the remediation requests. It compares the open faults with its own records and does only the work that is not done yet. The next consumer reads these records, as the Deployment controller reads ReplicaSet status.

After a restart, the consumers read the current state. They do not replay events. A pass that finds no work to do writes nothing. As today, the consumer queues the node again with a backoff when a pass fails. It also queues the node again after a delay while a drain or a remediation request runs, so new nodes do not wait behind it.

**The queue.** The queue belongs to the consumer in every mode, so node-drainer keeps its priority queue, with node names instead of events. The Node and pod changes that a consumer watches go into the same queue. In Kubernetes mode, `Watch` uses an informer cache with only the fields that the consumer uses. The consumer reports the length of its queue and the wait of the oldest node.

A write of a consumer to its own section does not queue the node for that consumer. In Kubernetes mode, each resync queues every node, and a change of one of these inputs queues the node, even during a pass:

| Consumer | The node goes into the queue when this changes |
|---|---|
| fault-quarantine | `spec`, or the cordon, the taints or the annotations of fault-quarantine on the Node. So an uncordon or untaint by someone else starts a pass. Also after a reset of the circuit breaker |
| node-drainer | `spec`, the section of fault-quarantine, its annotation on the Node, which lists the devices to drain, or the pods and custom drains of the node |
| fault-remediation | `spec`, or the sections of fault-quarantine and node-drainer, but not a Node annotation |

**The input of the decision code.** The consumers keep their decision code: the rules of fault-quarantine, the drain choices of node-drainer, and the actions and request templates of fault-remediation. This code gets one `HealthEvent` for each open fault. In Kubernetes mode, `View` builds these events from the object, in the order of the object. The object keeps no event message. The connector does not write most repeats, so a stored message would describe an old event.

| `HealthEvent` field | Taken from |
|---|---|
| `nodeName`, `agent`, `checkName`, `componentClass`, `version` | The name of the object, and the check entry of the fault |
| `errorCode`, `isFatal`, `recommendedAction`, `customRecommendedAction`, `quarantineOverrides`, `drainOverrides`, `metadata` | The fault, and its attributes for `metadata`. A fault without an error code gives an event without one |
| `entitiesImpacted` | The entities of the device entry of the fault |
| `generatedTimestamp` | The `generatedTimestamp` of the fault |
| The creation time of the stored event | For node-drainer, the start of its drain record, so it times each drain on its own clock ([Times](#rules-for-every-write)) |
| `message`, `isHealthy`, `processingStrategy` | `message` is always empty, because the object keeps no message. `isHealthy` is always false, because the view contains only open faults. `processingStrategy` is always `EXECUTE_REMEDIATION`, because [other events never change the object](#how-the-connector-writes-events). So the templates that read `.HealthEvent.Message`, and the `quarantineHealthEvent` Node annotation, get no message. The `cordon-reason` Node label of a forced quarantine gets the check name. Operators still see the messages in Node conditions and Events |
| `id` | The UID of the object, a dash and the `seq` of the fault. It stays the same after a restart, and it changes when the connector writes the fault again. Its characters are valid in the object names that node-drainer and the shipped remediation templates build from it. The annotation of fault-quarantine keeps the first event that it recorded for a fault, until the recommended action changes. So the ID in the annotation can be older than the current ID of the fault. For this reason, the consumers match a fault in the annotation by its check entry, error code and entities, not by its ID |

**Quarantine sessions.** The fault-quarantine module adds one to `session` in the write that records the start of a quarantine. This number is the quarantine session of [ADR-064](064-node-and-device-selection.md). The node-drainer and fault-remediation modules store the session of their records in the `session` field of their own sections. A session is active while the state of fault-quarantine is `Quarantined`, or `WouldQuarantine` for a module in dry run.

Each of these two modules compares the session in its own section with `faultQuarantine.session`. So it finds a session that ended, also when the fault-quarantine module already started the next one. The fault-quarantine module cannot remove their records, because each section has one writer. The section of fault-quarantine keeps the state of its latest session only. So when the next session already started, the fault-remediation module cannot see if the old session ended with a release or a cancel. It then updates its Node annotation as after a release, which keeps the attempt counts only while a `ValidationRequest` holds the node.

**The passes.** Each consumer runs the same steps with every provider. The field names below are those of Kubernetes mode. The fault-quarantine module runs three steps in each pass. While the circuit breaker is tripped, steps 2 and 3 wait ([Safety rules](#safety-rules)). With a database, step 2 also evaluates each undecided repeat of a held fault and marks its event `AlreadyQuarantined` on a match, as today.

1. **Compare the Node with the records.** This step finds a quarantine that someone else ended, and it finishes a step that a crash interrupted. When its cached Node and its records do not agree, the module first reads the Node from the API server, as today. When the node has no object with the UID of the annotation, the module records nothing in this step. Then it applies the first of these rules that matches:
   - Its annotation is on the Node, but the quarantine ended. A quarantine from dry run ends when the module is no longer in dry run. A quarantine that is not from dry run ends when its cordon or one of its taints is gone. If it has not recorded `Cancelled` for this session yet, the module records `Cancelled` and a report-again request. Then it removes its annotations from the Node.
   - Its annotation is on the Node, but its section shows no active session. The module records the start of a session, with `WouldQuarantine` for an annotation from dry run.
   - Its state is `Quarantined` or `WouldQuarantine`, but its annotation is not on the Node. The module records `Released`.
2. **Evaluate.** The module gives the shared rule sets each open fault that is not in its annotation and whose `seq` is above `reportAgainUpTo`. On a match without an active session, it cordons, taints and annotates the node in one Node update. Then it records the start of a session. On a match during a session, one Node update adds the fault to its annotation and applies the actions of its rules. A fault that no rule matches costs no write.
3. **Release.** The module removes a key from its annotation when no open fault of the same object has that key. When it removes the last key, one Node update removes its annotations and the last key, and it releases the node as today. Then the module records `Released`.

**The node-drainer and fault-remediation pass.** Each pass of these two modules runs these steps:

1. **Session check.** When the quarantine session changes or ends, the module first stops the work of its old session. The node-drainer module deletes the custom drains that its records name. The fault-remediation module updates its Node annotation as today: `KeepAttemptsForValidation` after a release when `maxRemediationAttempts` is set, and `ClearRemediationState` in the other cases. Then, in one write, the module removes its records and decisions and sets the current session, but keeps `reportAgainUpTo`. The module writes nothing for a new session before this write, so a crash between the steps only repeats them.
2. **Drain decision.** The node-drainer module takes each open fault that is in the annotation of fault-quarantine but not yet in its own section. A fault with `drainOverrides.skip` goes to `skipped`. A fault goes to `stopped` when a stop rule set for drains, a rule set with `stop: true`, matches it. If a `Running` or `Succeeded` drain record has the scope of the fault or no scope, the module adds the fault to that record. Otherwise, the module writes a `Running` drain record for the fault.
3. **Drain result.** The node-drainer module evicts the pods or creates the custom drain that the drain record names. Then it records `Succeeded` or `Failed`. When a drain fails, it also sets `reportAgainUpTo`, so it decides again at the next report of the fault. It checks, waits for and deletes a custom drain only by the name in its record. Each of its writes also removes the numbers of faults that are no longer open.
4. **Remediation records.** A remediation record is `Pending` until its request exists, then `Running` until the request finishes, then `Succeeded` or `Failed`. For a `Pending` record, the fault-remediation module runs the log collector, reads the object again and creates the remediation request. Its name comes from the event ID of the fault, so after a crash, the next pass finds it and creates no second request. A `Running` record whose request is gone counts as `Failed`, for example when an operator takes a node back from an external repair. In dry run, the module creates no request, so it records `Succeeded` in place of `Running`, as today's code marks the event remediated.
5. **Remediation candidates.** A candidate is an open fault in the annotation of fault-quarantine that node-drainer drained or skipped in this session, and that is not in `stopped`. The fault-remediation module gives each candidate to the shared decision code: the rule sets of [ADR-064](064-node-and-device-selection.md), the `maintenance.actions` entry, the equivalence group and the target. A fault goes to `stopped` when a stop rule set matches it. A fault gets no record when this code selects no remediation, its equivalence group reached `maxRemediationAttempts` or its partial drain does not allow the remediation. Otherwise, the module writes a `Pending` record, unless a record covers the fault, or a `Pending` or `Running` record blocks the remediation ([Safety rules](#safety-rules)).

**Which faults a record covers.** A record always covers its own fault, whatever its result. A record that is not `Failed` also covers another fault when all of these conditions are true:

- The fault is on the target of the record, or the record has no target (the whole node).
- The fault is in the equivalence group of the record, or in a group that lists the group of the record in `supersedingEquivalenceGroups`.
- The `seq` of the fault is at or below `startedSeq`, the `spec.seq` that the module read when it decided on the remediation.

A covered fault gets a new attempt only when the connector writes it again. The module never retries a remediation on a timer. The write that records `Running` raises `startedSeq` to the `spec.seq` of the second read. So, as today, the third condition also holds for a fault that the connector writes before the request exists.

### How faults clear

A healthy event removes the faults that it clears from the object at once, also when the object is full ([clear rule](#how-the-connector-writes-events)). After a reboot, syslog-health-monitor first sends one healthy event with no entities for each of its checks, which clears every fault of the check. Then the monitor reads the log of the new boot from the start. The connector applies the events of each monitor in the order in which they arrive, not by their timestamps. So an XID that the GPU logs in the new boot before the clear arrives after the clear, and it opens the fault again. But a clear can still wait behind a new fault that the connector rejected ([Negative](#negative)).

**Release by the keys of the Node annotation.** As today, the fault-quarantine module lists the faults that hold the node in its `quarantineHealthEvent` Node annotation. The module removes a key only when a newer version of the same object has no open fault with that key. It releases the node only at the session end ([details](#safety-rules)). Each event ID in the annotation starts with the UID of the object. So when the object is gone or has a new UID, the module removes no key ([details](#safety-rules)).

As in database mode, clears have no delay. So when a device fails and recovers again and again, the module releases and quarantines its node at each cycle ([details](#write-rate)).

**Clears while a consumer is stopped.** The consumers act on the faults that are open now, so no consumer has to see a clear when it happens. As in database mode at a cold start, a fault that opens and clears while the fault-quarantine module is stopped does not quarantine the node. The next pass removes the key of a fault that cleared meanwhile, and the module releases the node at the session end. The node-drainer and fault-remediation modules act only on open faults, so no drain or remediation starts for a fault that cleared.

### Safety rules

- **Release only on evidence.** The fault-quarantine module removes its cordon and taints only at the session end, as today. The session ends when no key of its Node annotation is left, and its rules match no open fault whose `seq` is above its `reportAgainUpTo`. The module removes a key only when a newer version of the same object, with the same UID, shows that the fault cleared. So the module does not release a node when its object is not there, is not synced, or has a new UID ([how faults clear](#how-faults-clear)). If the object loses its `status`, the module also does not release the node, because the faults in `spec` still hold it.
- **Remediation only after a drain.** A remediation starts only for an open fault that a succeeded drain of the current quarantine session lists, or that the node-drainer module skipped. As in database mode, a partial drain allows only a remediation whose target is the scope of the drain record ([ADR-071](071-device-scope-from-configuration.md)). For another remediation, such as a reboot, the fault-remediation module writes no record, increments `fault_remediation_processing_errors_total` and queues the node again with a backoff. As today, a remediation never starts while a remediation record of its equivalence group, or of a group that supersedes it, is `Pending` or `Running`. A fault that a record covers gets a new attempt only when the connector writes it again ([How the consumers reconcile a node](#how-the-consumers-reconcile-a-node)).
- **Circuit breaker.** The circuit breaker and its [settings](../configuration/fault-quarantine.md#circuit-breaker) limit the quarantines as today. While it is tripped, the fault-quarantine module runs only step 1 of its pass, which records cancels and finishes interrupted steps. After a reset with the cursor `RESUME`, the module evaluates the faults that are open now. After a reset with the cursor `CREATE`, it sets `reportAgainUpTo` to the current `spec.seq` in the object of each node that it does not quarantine. So these faults count only from their next report, as in database mode, where `CREATE` skips the events that came while the breaker was tripped.
- **Standing limits.** The breaker also has two standing limits on the number of cordoned nodes, `maxCordonedNodes` and `maxCordonedPercentage`, which are off by default in database mode. In Kubernetes mode, the default of `fault-quarantine.circuitBreaker.maxCordonedPercentage` is 10. A value that the user sets overrides this default. The figure of about 1,900 nodes in [Scale and etcd usage](#scale-and-etcd-usage) assumes this default. Kubernetes mode changes only this default and adds no limit of its own.
- **Uncordons and untaints by someone else.** An operator or another controller can uncordon the node, or remove a taint of the fault-quarantine module, while a fault is open. The module then records `Cancelled` and sets `reportAgainUpTo` to the `spec.seq` that it read, in one write that is conditional on that version. Then it removes its annotations from the Node, but leaves its taints and the labels of its rules, as today. The new `reportAgainUpTo` is a report-again request, so a fault with a `seq` at or below it counts again only after its next repeat. If the node has no NodeHealth object, the module records nothing and only removes its annotations.
- **Records from an earlier quarantine session.** A drain record, a remediation record, or a `skipped` or `stopped` decision counts only when its section names the current quarantine session. That session must also be active ([Quarantine sessions](#how-the-consumers-reconcile-a-node)). So after an uncordon and a new quarantine, the node-drainer module drains the node again before the fault-remediation module acts. This drain also evicts the pods that started on the node in between.
- **Dry run.** In dry run, the fault-quarantine module records `WouldQuarantine` and writes its annotation without a cordon. A consumer that is not in dry run treats `WouldQuarantine` as no quarantine and does nothing for the node. The node-drainer and fault-remediation modules mark each record that they write in dry run as a dry-run record. Only consumers that are also in dry run count these records, as in database mode. When an operator turns off dry run, the fault-quarantine module cancels each session of dry run, so these records never count.
- **Safe restarts.** Every step is safe to repeat. The node-drainer and fault-remediation modules record what they will do before they do it, and they find what they created by its name. The fault-quarantine module uses its Node annotation to find and finish a step that a crash interrupted. A crash test restarts each component after each write and checks the result.
- **A full etcd and rollouts.** When etcd is full, every write fails, also the Node update, so no new cordon, drain or remediation request starts. During a rollout, two pods of one component can run at once. With conditional writes and request names from event IDs, the second pod fails its write or finds the request of the first pod.

### Idle objects

Because Kubernetes has no built-in expiry for custom resources, the TTL controller moves out of janitor into its own Deployment. The TTL controller still deletes janitor's remediation requests as today, with the same `ttl` values. It runs when janitor is installed or in Kubernetes mode, so Kubernetes mode does not depend on janitor. In database mode, it does not watch NodeHealth objects, so it does not need the NodeHealth CRD. For NodeHealth objects, it adds three rules that janitor's remediation requests do not use:

- **A check before the delete.** The TTL controller deletes an object only when no fault is open. The state in the section of fault-quarantine must not be `Quarantined` or `WouldQuarantine`. The sections of node-drainer and fault-remediation must contain no record in any state. The node-drainer module needs its records to delete the custom drains of a session that ended. The Node must have no quarantine annotation of fault-quarantine and no equivalence group in the remediation annotation of fault-remediation.
- **Idle time instead of a fixed expiry.** The TTL controller deletes an object only after it stays unchanged for `global.datastore.kubernetes.objectIdleTime`, 60 minutes by default. It keeps the idle time in memory, so the timer costs no write. A restart of the TTL controller only delays a delete. An idle object, which holds no node, stays after its idle time only while the TTL controller is down.
- **A conditional delete.** Before the delete, it reads the object and the Node by name, then checks the conditions again. The delete is conditional on the `resourceVersion` that it read, so it fails if a connector or a consumer wrote the object in between. A connector whose copy is older than the delete gets "not found" on its next write. It then drops its copy and handles the event as for a node that has no object.

### Stale faults and objects

- **A fault is stale** when it stays open because no monitor can send the healthy event that clears it. If a rule matched the fault, the node stays quarantined. The causes are a removed monitor, check or kubernetes-object-monitor policy, and an upgrade that renames a check or an error code. A Slurm drain that a custom fatal pattern reported can also end while slurm-drain-monitor restarts, so the monitor does not see the end. A restore of etcd also opens again each fault that cleared after the backup, because its monitor does not send the clear a second time. Database mode has the same problem for each cause, and for the last cause after a restore of the database.
- **Nothing removes a fault** because of its age ([why](#other-ways-to-remove-faults-and-objects)). Instead, the TTL controller exports two metrics for each monitor and check: the number of open faults and the age of the oldest one. An alert on this age shows stale faults to the operator. A fix for the gaps of kubernetes-object-monitor and slurm-drain-monitor is separate work that helps both modes.
- **When the operator uncordons a node** that a stale fault holds, the fault-quarantine module records a cancel and removes its annotations, as today. The stale fault stays open, but it counts again only after the connector writes it again ([Safety rules](#safety-rules)). To remove a stale fault, the operator deletes it from `spec` of the object. The fault-quarantine module treats this delete as a clear. Do not delete the whole object, because this deletes the records of the consumers too. The fault-quarantine module removes nothing when the object is not there or has a new UID.

### Scale and etcd usage

- **Kubernetes mode is for small clusters**, but larger clusters keep a database. With the caps of [The NodeHealth object](#the-nodehealth-object), it supports up to about 1,900 nodes with one 16 GB etcd. The figure assumes the default standing limit of the circuit breaker in this mode, `maxCordonedPercentage` at 10% of GPU nodes ([Safety rules](#safety-rules)). Even 10 to 100 times today's peak event rate does not change the etcd use, which grows with nodes and fault changes, not with events. All sizes and node counts in this section are estimates. The sizes are JSON bytes, where KB, MB and GB are powers of 1,000.
- **A complete failure of a common GPU node fits the caps.** The next table shows upper bounds: nodes on which every GPU, every NVSwitch path to a GPU and every NIC port fail at once. For each GPU, it counts 3 XIDs, a fall off the bus, and 2 errors in each of 3 gpu-health-monitor checks. It also counts 2 SXIDs on each NVSwitch path, a lost DCGM connection, and a device file error on each NIC. On each NIC port, it counts the port state and 3 error counters. As in database mode, the annotation of fault-quarantine needs about 0.5 KB for each fault, of the 256 KiB that Kubernetes allows for all annotations.

| Node with everything failed | Device entries | Faults | Object with all consumer records | Annotation of fault-quarantine |
|---|---|---|---|---|
| HGX A100: 8 GPUs, 6 NVSwitches, 10 NICs with 12 ports | 123 | 235 | 59 KB | 121 KB |
| HGX H100: 8 GPUs, 4 NVSwitches, 10 NICs with 12 ports | 107 | 203 | 51 KB | 104 KB |
| HGX B200: 8 GPUs, 2 NVLink switch chips, 10 NICs with 12 ports | 91 | 171 | 44 KB | 88 KB |
| GB200 NVL72 compute tray: 4 GPUs, 5 NICs with 6 ports | 38 | 70 | 21 KB | 36 KB |
| Any node at the caps, with 8 entities in each device entry | 128 | 256 | 97 KB | 136 KB |

**The worst case for etcd** counts the live object and every old version that etcd keeps. With the 5-minute default compaction of kube-apiserver, etcd keeps each old version for up to 10 minutes, or 15 during an API server failover ([more](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/storage/etcd3/compact.go)). The model has every consumer at full speed and every object at the caps, with values as long as those of the shipped monitors. In the model, every cleared fault opens again. The model assumes 16 connector writes for each node at once, then one every 4 minutes, all for changes of state ([Write rate](#write-rate)). Each connector write causes at most one consumer write, for example when node-drainer decides a fault again after the connector writes it again.

| Worst case for each node | No standing limit | 10% standing limit (default), average |
|---|---|---|
| Live object | 97 KB | 89 KB |
| Connector writes in 10 to 15 minutes, and the consumer writes that they cause | 3.6 to 3.8 MB | 3.3 to 3.5 MB |
| Consumer writes for one quarantine: 9 for one drain and remediation, plus 5 for each further GPU to drain and reset (24 for 4 GPUs) | 0.9 to 2.3 MB | 90 to 230 KB |
| Total | About 4.6 to 6.3 MB | About 3.5 to 3.8 MB |

Without a standing limit, every GPU node can be quarantined and have consumer records, but with the 10% standing limit only one in ten can. The NodeHealth objects get half of the etcd space that is left after the Node objects, which take about 55 KB each. The other half is for the rest of the cluster and for the old versions of the Node objects. Each row of the next table gives the largest cluster whose worst case fits in the half for NodeHealth objects. The ranges cover 10 to 15 minutes of etcd history and 9 to 24 consumer writes for each quarantine. Because etcd stores data in whole 4 KiB pages, it needs some more space than these sizes.

| etcd size | Largest cluster with no standing limit | Largest cluster with the 10% standing limit |
|---|---|---|
| 2 GiB (self-managed) | About 170 to 230 nodes | About 280 to 310 nodes |
| 8 GB (EKS standard, AKS) | About 640 to 870 nodes | About 1,000 to 1,100 nodes |
| 16 GB (EKS Provisioned) | About 1,300 to 1,700 nodes | About 2,100 to 2,300 nodes |

In a 16 GB etcd, 1,900 Node objects use about 0.1 GB, so the NodeHealth objects get half of the rest, about 7.9 GB. Their worst case is about 6.6 to 7.3 GB with the 10% standing limit, and 8.7 to 11.9 GB without it, which does not fit. A GPU reset takes only minutes. So within the 10 to 15 minutes of etcd history, some quarantines can end and new ones can start on other nodes. The 10% column does not count these extra quarantines. With them, the largest cluster with the 10% standing limit drops by about a tenth, to about 1,900 nodes for 16 GB.

A device that fails and recovers many times causes more writes than the model assumes ([Write rate](#write-rate)). A planned scale test measures the real etcd size, also with NIC ports that go down and up, and faults that change on every event. To know how close an install is to the limit, an operator compares its number of nodes with these tables. The tables give the worst case for each etcd size.

### Change an install to Kubernetes mode

A new install sets `global.datastore.provider` to `kubernetes` and `global.k8sdatastoreCrds.enabled` to `true`. Kubernetes mode has no data migration, so an install that switches starts with no NodeHealth objects. The consumers do not take over work that started before the switch. A fault from before the switch, or from an old connector pod during the upgrade, counts only when its monitor sends it again. A current install switches to Kubernetes mode in three steps:

1. Wait until the maintenance of each open MaintenanceRequest ends, and then delete the request. The lifecycle-manager component sends the unhealthy event of a request only one time, so Kubernetes mode never sees a request from before the switch. Create no new request until step 2 ends. Then wait until the fault-quarantine module handles every health event, with no drain or remediation request in progress. Make sure that the circuit breaker is not tripped. Make sure that fewer than 10% of GPU nodes are cordoned, or set `maxCordonedPercentage` for the switch.
2. Apply the CRDs of the chart, because `helm upgrade` does not install them. Then upgrade with `global.datastore.provider: kubernetes` and `global.k8sdatastoreCrds.enabled: true`. Set `global.mongodbStore.enabled` and `postgresql.enabled` to `false`. Turn off the health-events-analyzer, the event exporter and csp-health-monitor. After this step, create each new MaintenanceRequest with `isFatal: true`.
3. Make sure that each node that is still quarantined is healthy. Then uncordon or untaint it, so that the fault-quarantine module removes its annotations, as for any cancel. Also remove the `latestFaultRemediationState` annotation of the fault-remediation module from these nodes. When a node has no object, the fault-quarantine module does not release it. So these nodes stay quarantined until an operator uncordons them.

To switch back to a database, do the same three steps, but select the database in step 2. Start with an empty database, because the consumers act on any events that are left in it.

### Changes in each component

| Component | Change |
|---|---|
| data-models | Comments in the `HealthEvent` message define the identity of a fault and the rule for when a healthy event clears it. No field changes |
| Health monitors | They still send every event in today's format. Of the shipped components, only gpu-health-monitor, syslog-health-monitor and lifecycle-manager add the keys of [Keys that monitors set](#keys-that-monitors-set) to some events. A tool that forces a quarantine lists `creator_id` in `keepMetadata` |
| lifecycle-manager | It needs two changes for Kubernetes mode. First, the chart gives it the value of `global.datastore.provider`, so it knows the mode. Second, in Kubernetes mode, its webhook rejects a MaintenanceRequest with `isFatal: false`, as it already rejects one with a healthy event ([why](#how-the-connector-writes-events)). Its docs say that a request must set `isFatal: true` in this mode. Both changes come in the last step of the delivery order, and database mode does not change. It still sends its events in today's format |
| store-client | Adds the `kubernetes` provider, which registers as `ProviderKubernetes` and contains all NodeHealth code. Adds the `NodeStore` interface and the `NodeStore()` accessor of `DataStore`, which all three providers implement ([how](#how-the-providers-fit-store-client)). Removes the `HealthEventStore()` accessor of `DataStore` in the last step of the delivery order |
| Platform connector, both roles | In Kubernetes mode, a new store connector writes each batch through one method of the `kubernetes` provider ([how](#how-the-providers-fit-store-client)). The deployment platform connector writes each batch before it replies, as today. The DaemonSet connector keeps today's queue: it replies first and then writes from the queue, with today's retries. Database mode keeps today's store connector. Node conditions and Events stay as today, because other tools read them |
| fault-quarantine | Runs one reconcile loop on `NodeStore` in every mode, with today's decision code ([how](#how-the-consumers-reconcile-a-node)). In Kubernetes mode, it records each quarantine under a new `session` number. For the validation of [ADR-049](049-node-validation.md), it checks for a full drain in a `Succeeded` drain record of the session that has no scope. In database mode, this drain must come from a fault that asks for validation. |
| node-drainer | Runs the same `NodeStore` loop, with today's decision code. The `drainOverrides.force` override works as today |
| fault-remediation | Runs the same `NodeStore` loop, with today's decision code. It keeps the attempt counts and equivalence groups in its Node annotation, as today. It computes its remediation state label on the Node again in each pass |
| janitor and the TTL controller | The TTL controller moves out of janitor ([how](#idle-objects)). It still deletes janitor's remediation requests as today, 14 days by default. It also deletes idle NodeHealth objects. It exports the metrics of open faults ([why](#stale-faults-and-objects)). Janitor still carries out the remediation requests |
| Helm chart | Adds `kubernetes` as a value of `global.datastore.provider`, and these keys under `global.datastore.kubernetes`: `copyIdleTime` (10 minutes) and `objectIdleTime` (60 minutes). In Kubernetes mode, `fault-quarantine.circuitBreaker.maxCordonedPercentage` is 10 by default ([why](#scale-and-etcd-usage)). The NodeHealth CRD, short name `nh`, goes in the current `k8sdatastore-crds` chart. The chart adds the RBAC rules of [The NodeHealth object](#the-nodehealth-object). It refuses Kubernetes mode while `global.k8sdatastoreCrds.enabled` is `false`, or while `global.mongodbStore.enabled` or `postgresql.enabled` is `true`. It also refuses the mode while the health-events-analyzer, the event exporter or csp-health-monitor is enabled, because they do not run in this mode ([why](#decision)) |

**Delivery order.** Database installs keep today's loop until the new loop is proven, so a mistake in the new loop first reaches only Kubernetes mode. The table above shows the components after the last step. The work ships in these steps:

1. Each consumer gets the `NodeStore` loop next to today's loop. At start, it runs the new loop in Kubernetes mode and today's loop with a database. The `kubernetes` provider comes in the same step.
2. The chart allows Kubernetes mode. The current tests, the new scenario suite and end-to-end tests check this mode, while database installs still run today's loop.
3. The MongoDB and PostgreSQL providers get their `NodeStore` implementation. With a database, an environment variable of each consumer selects the loop, and today's loop stays the default. The end-to-end test matrix runs each database with both loops.
4. We validate the new loop until we are confident in it. After that, database installs move to the new loop, which becomes the default, first for PostgreSQL and then for MongoDB.
5. Then we delete today's loop and the environment variable entirely. `DataStore` no longer has the `HealthEventStore()` accessor, because no module outside store-client calls it then. The MongoDB and PostgreSQL providers use its methods only inside their `NodeStore` implementation. In the same step, lifecycle-manager gets the mode from the chart, and its webhook starts to reject a MaintenanceRequest with `isFatal: false` in Kubernetes mode.

### Changes for rules

The `kubernetes` provider gives the decision code one health event for each open fault, with today's fields ([how](#how-the-consumers-reconcile-a-node)). So the shipped rule sets of fault-quarantine still match. The rule sets of [ADR-064](064-node-and-device-selection.md) for node-drainer and fault-remediation also work in this mode. In Kubernetes mode, rules can no longer select on these items:

- The event message, the entities that the event lists in `detailEntities`, and the metadata that the event does not ask the connector to keep. Examples are the register values of XID 74, and the provider ID and labels of the Node, which the connector adds. The object keeps none of these. It also keeps at most 8 entities for each fault.
- Unhealthy events that are not fatal, unless the event sets `quarantineOverrides.force`. This rule is the same for every monitor, so the connector does not store the default events of slurm-drain-monitor. An install can mark such events fatal with an override rule ([how](#how-the-connector-writes-events)).
- Two error codes of one event together, because each code becomes its own fault. No shipped monitor or rule set does this.

## Rationale

- **The object stores each node's open faults, not every event, so etcd stores only bounded objects.** [ADR-002](002-storage-layer-selection.md) rejected etcd as the store for health events, because events can be large and because analysis needs aggregation queries. Even at its caps, with every consumer record, a NodeHealth object stays far below the 1.5 MiB that etcd accepts in one request ([sizes](#scale-and-etcd-usage)). Kubernetes mode runs no aggregation, because the health-events-analyzer does not run in this mode yet. Database mode keeps the store that ADR-002 chose. A follow-on design for the analyzer must answer both reasons again.
- **Custom resources fit this use.** The Kubernetes documentation names a relatively small number of relatively small objects that change relatively infrequently as the best fit for custom resources ([Declarative APIs](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/#declarative-apis)). This design stores one object for each node with faults, never one for each health event, and writes it only when something changes. A NodeHealth object with one fault is about 1.7 KB and grows past a few KB only with many open faults ([Scale and etcd usage](#scale-and-etcd-usage)). The same page lists more than thousands of objects and tens of requests a second as signs of a poor fit. This design reaches these signs only when thousands of nodes have faults.
- **Faults in `spec`, consumer progress in `status`.** One write creates an object with its faults in `spec`, but faults in `status` need a second write. The split lets RBAC separate the connector from the consumers, a use that [KEP-2527](https://github.com/kubernetes/enhancements/tree/master/keps/sig-architecture/2527-clarify-status-observations-vs-rbac) accepts. KEP-2527 also drops the rule that a controller must be able to rebuild `status` from what it observes, so consumers can keep their records there. Consumer writes to `status` do not change `metadata.generation`, so a consumer can tell a fault change from the write of another consumer.
- **Why faults go in `spec`.** A restore that drops `status`, as [Velero](https://velero.io/docs/main/restore-reference/) does by default, cannot drop open faults. Lease, CSINode and ResourceSlice also keep what node agents report in `spec`.
- **Monitors still send today's events. The connector drops repeats.** The connector already receives every event in both roles, so it is the right place to compare an event with the open faults. All other state lives in the NodeHealth object, so any connector replica can handle any event. The event recorder in client-go also limits repeats: it counts them on one Event, and its spam filter limits writes for each object. A consumer that needs the next report of a fault asks for it with a report-again request ([details](#how-the-connector-writes-events)).
- **One reconcile loop for all providers.** As Kubernetes controllers do, each consumer compares the open faults with its own records, so a restart or a missed update loses nothing. Kubernetes mode needs no ordered stream of events. Each consumer has one code path, on the `NodeStore` interface that all three providers implement ([details](#how-the-providers-fit-store-client)). So the decision code, the rules and their tests do not change. The database providers write the same event statuses as today, except after a crash, so old and new modules work together during an upgrade.
- **Write numbers give the order. A release needs evidence.** The `spec.seq` numbers follow the order of the conditional writes, so no rule compares times from different hosts, whose clocks can be wrong ([details](#rules-for-every-write)). The release of a node is the risky action, because a wrong release puts a faulty GPU back into service. Stale state was the largest class of the controller bugs that [Sieve](https://www.usenix.org/system/files/osdi22-sun.pdf) found (19 of 46). So the module releases a node only when a newer version of the same object shows that the faults that hold the node cleared.

## Consequences

### Positive

- **No database to operate.** Remediation works without MongoDB or PostgreSQL, so no one has to operate a database for it. A database outage can no longer stop remediation. The same design works with the DaemonSet connector, the deployment platform connector or both, so an install can turn the DaemonSet off. Operators can list the nodes that have faults, or had one in the last hour, with `kubectl get nh`. They can see the open faults of a node with `kubectl describe nh`.
- **No change to monitors.** Shipped and custom monitors still send today's events. The only additions are optional metadata keys, so the connector needs no rules for particular monitors or events. During XID floods, the connector writes only changes, instead of one database write per event.
- **Fewer lost faults.** With the deployment platform connector, a crash or restart no longer loses a fault, because it writes a fault or a clear before it replies. After a restart, the consumers read the current NodeHealth objects instead of old events, so a missed event no longer means a missed fault. In database mode too, a missed change only delays an event, because each start and resync finds the work that is not done. The DaemonSet connector keeps today's queue.

### Negative

- **Fewer event details.** Rules and templates get no event message, and only the metadata that an event asks the connector to keep. So a custom rule set or template that reads these details must change. An ExternalRemediationRequest made in this mode also carries less detail. Today, the consumers join one trace through the `trace_id` that the database writer adds to each event. The health events that the `kubernetes` provider returns have no `trace_id`, so each consumer starts its own trace.
- **No history and no migration.** An object keeps only open faults, so a fault that opens and clears before the fault-quarantine module handles it leaves no record. The connector does not store `STORE_ONLY` and `STORE_AND_ANALYSE` events, which database mode stores. These include the events of a node with the `nvsentinel.dgxc.nvidia.com/managed=false` label. Investigations must use logs, Node conditions and Events. Kubernetes mode has no data migration from or to a database ([how to switch](#change-an-install-to-kubernetes-mode)).
- **Components that do not run yet.** The health-events-analyzer, the event exporter and csp-health-monitor do not run in this mode yet. Until they do, nothing detects patterns over time, such as an XID that comes back many times. The analyzer rule `MultipleRemediations` also does not run, so no rule detects a node that fails again after each remediation. The `maxRemediationAttempts` option works as in database mode, because the fault-remediation module keeps its attempt counts in its Node annotation in both modes. To replace `MultipleRemediations`, a follow-on design can count the remediation requests, which janitor keeps for 14 days by default.
- **Late faults and later releases.** With the deployment platform connector, a full object makes a new fault late, and the later events of its monitor wait too ([details](#the-nodehealth-object)). A clear after a GPU reset can then wait behind the rejected fault, because syslog-health-monitor does not skip a line that it could not deliver. So the node stays quarantined until a reboot or an operator acts. Two healthy events that each name some of the entities of a fault do not clear it, but database mode releases the node after both.
- **Cluster size and control plane load.** Kubernetes mode supports up to about 1,900 nodes, but only with a 16 GB etcd and a 10% standing limit (estimate). This mode sets the limit by default, so the circuit breaker trips sooner than in database mode when many nodes get faults at once. The remediation load moves from the database to the API server and etcd, which an operator cannot always scale on a managed control plane. The connector reads the object and the Node by name, so this mode makes more API calls than database mode. With the default client limit of 5 requests a second in fault-quarantine and node-drainer, a fault on many nodes at once takes longer to handle.
- **Database mode changes too.** After the new loop is proven in Kubernetes mode, database installs also move to it ([details](#how-the-providers-fit-store-client)). The behavior of the consumers must not change, except after a crash (see the next item). A mistake in this move can change how database installs remediate, so each database moves only after end-to-end tests with both loops ([Delivery order](#changes-in-each-component)). In database mode, each pass also runs one indexed query, and the queue merges a burst of events for one node into one pass. The TTL controller of janitor also moves to its own Deployment in every install.
- **A crash fix in database mode.** The fault-quarantine module uses its Node annotation to finish a step that a crash interrupted, also in database mode ([The passes](#how-the-consumers-reconcile-a-node)). After a crash between its Node update and its status write, it now writes the status that is not there. This is `Quarantined` after a cordon, and `UnQuarantined` after an uncordon. Today's code writes `AlreadyQuarantined` after such a crash at a cordon, and no status after one at an uncordon. It also adds a fault to a session in one Node update, in place of two.
- **Two loops for a time.** Until the last step of the [delivery order](#changes-in-each-component), each consumer has two loops: today's loop and the `NodeStore` loop. Both loops call the same rules, drain choices, Node updates, pod evictions and request templates, about half of the code of each consumer. The other code exists two times. It reads the state, finds the work that is left, releases or cancels a quarantine, checks the circuit breaker and records the decisions. A fix in that code can be necessary in both loops.
- **Maintenance requests must be fatal in Kubernetes mode.** In this mode, a MaintenanceRequest must set `isFatal: true`, although today's docs and [ADR-051](051-maintenance-request.md) use `false` in their examples ([why](#how-the-connector-writes-events)). Until the last [delivery step](#changes-in-each-component), the connector skips the event of a request with `isFatal: false`, so the request has no effect. From that step, the webhook of lifecycle-manager rejects such a request. During maintenance, the platform connector sets a Node condition to True, in place of a Node Event, and metrics show the event as fatal. Database mode does not change.

## Alternatives Considered

### Do not add this mode, or run the database on a local volume

**Rejected** because someone must still operate a database on a local volume: set its storage and size, upgrade it and monitor it.

### A counter that changes on every repeat (the issue's proposal)

**Rejected** because each change of the counter, also in `status`, makes the API server store and send a full new version of the object. So 600 repeats a second on one node make 600 full writes a second, about 1 GB of etcd history in 10 minutes (estimate). No decision uses the count, so the connector writes only changes of state ([Write rate](#write-rate)).

### One object per health event (ADR-027)

**Rejected** because the number of objects follows the event rate, which is how Kyverno filled etcd ([Appendix](#appendix-lessons-from-other-projects)). Even with one object for each open fault, a node can have up to 256 objects instead of one.

### Other designs of the object, its writes and its reads

- **Separate objects for monitors that report about other nodes.** Rejected because it needs a second cap, a rule for which connector writes each object, and a join of two objects in every consumer. The fault-quarantine module releases a node only on evidence from one version of one object. So it cannot also prove that the faults in a second object cleared.
- **Node conditions as the store.** Rejected because a Node has one condition of each type, so it cannot store a fault for each device. Each write to a Node also goes to every watcher of Nodes. For this reason, Kubernetes moved node heartbeats to Lease objects ([KEP-589](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/589-efficient-node-heartbeats)).
- **A write limit stored in each object.** Rejected because the connector writes only changes of state, so a repeat causes no write ([Write rate](#write-rate)). A stored limit also compares times from different hosts. On a busy node, it delays new faults.
- **Times or `metadata.generation` as the order of the writes.** Rejected because times come from writers on different hosts with different clocks. If each write must store a later time than the last write, one fast clock moves every later time ahead. For example, a node that starts before it sets its clock can move every later time 4 hours ahead. The API server sets `metadata.generation` during the write, so the connector cannot store the new value on a fault in that write. A restore also creates the object again with generation 1, while the restored faults keep their old numbers.
- **A list of the faults to report again.** Rejected because each consumer must remove the served numbers in each write, and the list can grow to 256 entries. One number ends by itself, because each fault that the connector writes again moves above it. It causes more writes only at real repeats, at most one for each fault, and database mode stores each such repeat as a new event.
- **Implement the event interfaces over NodeHealth objects.** Rejected because the provider must then make up healthy events, store a status for each event and keep cleared faults ([details](#how-the-providers-fit-store-client)). These made-up events and marks form a second, hidden state machine, which the reconcile pass does not need.
- **A permanent loop only for Kubernetes mode in each consumer.** Rejected because the two code paths in each consumer drift apart. Each consumer must also know its mode. In the [delivery order](#changes-in-each-component), the new loop runs only in Kubernetes mode for a time, and then database installs move to it.

### Other ways to remove faults and objects

- **Delete a fault that fault-quarantine skipped, or an old open fault.** Rejected because the consumers see a removed fault as a clear, so they can release a node that still has the fault. A skipped fault that a monitor reports only once is then lost. To delete a skipped fault, the fault-quarantine module must also write `spec`, which only the connector writes. A monitor that repeats the fault brings it back after each delete, so each pass costs two writes. Instead, an operator removes a stale fault by hand ([Stale faults and objects](#stale-faults-and-objects)).
- **Keep cleared faults until the consumers acknowledge them.** Rejected because each clear then costs two more writes. The cleared faults also take room under the cap. The consumers do not need them, because each pass compares the open faults with the records of the consumer.
- **Delete the object when its last fault clears.** Rejected because the consumers still need their sections until the quarantine session, the drain records and the remediation records end ([details](#idle-objects)). The fault-quarantine module also releases a node only when it reads the same object, with the same UID, without the held faults.

### A store proxy Deployment

In this alternative, a new Deployment runs the `kubernetes` provider one time for all three consumers, like a proxy in front of a database. The consumers send their `NodeStore` calls to the proxy over gRPC. The proxy, not each consumer, keeps a cache of the NodeHealth objects.

**Rejected** for now, for these reasons:

- **No fewer writes.** The cost for etcd comes from writes. The proxy writes the same changes to the same objects as the consumers do without it.
- **No faster reads.** Each consumer already reads its own cache in memory, and the API server serves watches from memory. Each consumer must still write its records to the API server, and a proxy cannot make these writes faster. Each call to the proxy also adds a network hop.
- **A small gain.** Each proxy replica keeps one cache in place of the three caches of the consumers. With two replicas, the proxy saves only one cache and one watch.
- **The same hard part.** The code that builds the view of each consumer and writes its records is the same in a proxy and in a library. The proxy only moves this code into another process.
- **A new dependency for all remediation.** If the proxy is down, for example after a bad upgrade, the three consumers stop, so all remediation stops.
- **Replicas and reconnects.** The proxy needs two or more replicas, so that one failure does not stop remediation. When a replica fails, its consumers connect to another replica and read again each node with work that is not done.
- **One more API to keep compatible.** During a rolling upgrade, old consumers call a new proxy, or new consumers call an old proxy. So the gRPC API of the proxy must stay backwards compatible, as each other public interface does.
- **Every install needs it.** Each install in Kubernetes mode must run the proxy, also an install with only the DaemonSet connector. The deployment platform connector is optional, so the proxy cannot run inside it.

**When to add it.** A proxy can come later as one more provider of `NodeStore`, with no change to the consumers. We can add it if the scale test shows that the consumers use too much memory or too many watches.

### Other datastores

- **An in-memory store with a best-effort history.** Rejected because every install must then run a database again, which this mode exists to avoid. In this alternative, a Deployment keeps the state in memory, copies each change to the NodeHealth objects, and loads them again after a restart.
  - **No less load on the API server.** Each change that must survive still goes through the API server to etcd, and the consumers still cordon and evict through it.
  - **No faster reads.** The `kubernetes` provider already reads the objects from memory, through an informer.
  - **No storage savings.** The objects and their old versions stay in etcd, and the store adds its own copy in memory.
  - **Complex redundancy.** All remediation stops while the store is down, so the store needs replicas, replication and failover. A network split can leave two replicas that accept writes. A crash between the two writes of a change leaves two different copies.
  - **One more API to keep compatible.** Its API and its data must stay backwards compatible, because old and new modules run together during an upgrade.
  - **Little gain.** The only gain is a short, best-effort history of events.
  - **A better way to give the analyzer a history.** The gRPC sink connector and a watch on the NodeHealth objects add no service to the remediation path. The remediation requests of janitor stay for days, so this part of the history survives a restart of the analyzer.
- **An event database on each node (for example SQLite on the host).** Rejected because the consumers run centrally, so they must poll thousands of node databases that have no watch. It also needs the DaemonSet connector, which an operator can turn off. The circuit breaker limits quarantines across the cluster, so the fault-quarantine module cannot move to the nodes. The history also fails with its node, and a VM replacement or a re-image deletes it.
- **An OpenTelemetry backend as the datastore.** Rejected because these backends have no watch and no in-place update. An outage of the observability stack also stops remediation. An install can still export events to such a backend for analysis.

### The NVIDIA maintenance operator for cordon and drain

**Not part of this ADR**, because the [maintenance operator](https://github.com/Mellanox/maintenance-operator) stores no faults and so cannot replace the database or the NodeHealth objects. It can take over cordon and drain, but it sets no taints and cannot evict only the pods of one GPU. By default, it also gives a node back after `maxNodeMaintenanceTimeSeconds`, one hour, even when the node still has a fault. It has one drain mode for all pods, but the node-drainer module chooses a mode for each pod and supports custom drains.

## Notes

- **Follow-on work outside this ADR.** The health-events-analyzer, the event exporter and csp-health-monitor need their own designs for this mode. One coordinator for remediation and for driver and network maintenance, such as the NVIDIA maintenance operator, needs its own ADR.
- **Estimates.** Numbers marked "(estimate)", and the sizes and node counts in [Scale and etcd usage](#scale-and-etcd-usage), come from the caps and limits, not from measurements. A scale test will measure them. It will also measure the memory use of the API server and the consumers, which is unknown now. All limits and times in this design are fixed, unless the text calls them configurable.

## Appendix: Lessons from other projects

| Project | What went wrong | What this design does instead |
|---|---|---|
| Kyverno | It created one report object for each admission request, faster than it could merge and delete them. Its 1.6 million objects (9.4 GB) made etcd read-only | At most one NodeHealth object for each Node. The connector writes only changes of state |
| Crossplane | Hundreds of CRDs overloaded API servers. Providers that shared one ServiceAccount replaced each other's owner references on it again and again. API server requests rose fourfold | One new CRD, and one writer for each section of a NodeHealth object |
| Dex | It created more than 500,000 objects in under 24 hours. Its cleanup listed them all and timed out | The TTL controller deletes each idle object by name. No list call looks for idle objects |
| Rancher Fleet | A status list with an entry for every resource on about 70 clusters went over etcd's request size limit. An empty cache read as "not found" and uninstalled workloads | No list of nodes inside one shared object. An empty or unsynced cache never releases a node |

## References

- NVSentinel: [ADR-027](027-kubernetes-data-store.md), which this ADR proposes to supersede. Related ADRs: [ADR-002](002-storage-layer-selection.md), [ADR-064](064-node-and-device-selection.md), [ADR-071](071-device-scope-from-configuration.md).
- Kubernetes: [custom resources](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/); [KEP-2527](https://github.com/kubernetes/enhancements/tree/master/keps/sig-architecture/2527-clarify-status-observations-vs-rbac); [KEP-589](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/589-efficient-node-heartbeats) (node heartbeats); [etcd compactor](https://github.com/kubernetes/kubernetes/blob/master/staging/src/k8s.io/apiserver/pkg/storage/etcd3/compact.go). Storage and scale: [EKS Provisioned Control Plane](https://docs.aws.amazon.com/eks/latest/userguide/eks-provisioned-control-plane.html); [AKS etcd limit](https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/create-upgrade-delete/troubleshoot-apiserver-etcd). Lessons from other projects: [Sieve](https://www.usenix.org/system/files/osdi22-sun.pdf); [Velero restore reference](https://velero.io/docs/main/restore-reference/). Maintenance coordination: [NVIDIA maintenance operator](https://github.com/Mellanox/maintenance-operator).
