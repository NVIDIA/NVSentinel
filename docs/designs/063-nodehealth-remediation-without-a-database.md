# ADR-063: Kubernetes-Native Remediation Without a Database

## Summary

- **Remediation without a database, as a new datastore option.** In NodeHealth mode, which `global.datastore.provider: kubernetes` selects, fault-quarantine, node-drainer and fault-remediation read one small Kubernetes object per node with faults, a `NodeHealth` object, instead of MongoDB or PostgreSQL. No database runs in this mode. The default does not change, so existing installs are not affected.
- **A NodeHealth object keeps only the faults that are open now, not a history of events.** A cleared fault is removed, and a repeated event adds nothing, so the object never keeps past events. Each of the three consumers records its progress in its own section.
- **Only changes are written to the NodeHealth objects.** Monitors keep sending every event, as today. The platform connector compares each event with the node's open faults, and a repeat causes no write. So a GPU that logs an XID (a GPU driver error) 600 times a second causes as few writes as a GPU that logs it once. Each node also has a fixed limit on how much it can write.
- **It works with either platform connector.** Everything that matters is stored in the NodeHealth object, and the connector keeps nothing in memory that it needs after a restart. As a result, the DaemonSet connector and the deployment platform connector of [ADR-052](052-deployment-platform-connector.md) run the same code, and customers can turn the DaemonSet off.
- **A node is released only on evidence.** Before fault-quarantine removes a node's cordon and taints, a newer write must show that the fault cleared. Each clear must also hold for a short delay, so a fault that keeps coming back keeps the node quarantined. Missing data never releases a node. The existing circuit breaker still limits how many nodes are quarantined.
- **What we give up.** Quarantine rules can no longer match a few rarely used event details that a NodeHealth object does not keep, such as the event message ([list](#changes-for-fault-quarantine-rules)). The health-events-analyzer is not supported, and the event exporter and csp-health-monitor need changes before they can run in this mode. In the worst case, one 16 GB etcd that also holds the Node objects supports more than 21,000 nodes (estimate). If the circuit breaker caps quarantined nodes at 10% of GPU nodes, it supports more than 36,000 (estimate).

Numbers marked "(estimate)" are calculated from the design's fixed write limits, not measured. A scale test will measure them.

## Context

### How remediation works today

Health monitors send their events to the platform connector. It runs as a DaemonSet pod on each node, or as the deployment platform connector of ADR-052, and it stores each event in MongoDB or PostgreSQL. Three consumers react to each new event in the database: fault-quarantine cordons and taints faulty nodes, node-drainer evicts workloads, and fault-remediation creates the maintenance request that janitor carries out, such as a reboot or a GPU reset. So remediation needs a database.

### Motivation

[Issue #1950](https://github.com/NVIDIA/NVSentinel/issues/1950) names three problems:

1. **Integration cost.** Many new clouds have no managed database, so operators export health events and build their own remediation.
2. **Deployment friction.** A plain Helm install cannot remediate, because the user must also run a second stateful system.
3. **Lost events.** The consumers act on each event once, so a lost event is a fault that is never remediated. Consumers lost events after a requeue and a restart ([#798](https://github.com/NVIDIA/NVSentinel/issues/798)).

NVSentinel's own telemetry shows that floods of XIDs also lose events. On four flooding nodes of one production cluster, the connectors restarted 125 times in one day, and fault-quarantine received only 53% of the events that the connectors received.

### Lessons from other projects

Other projects that kept a lot of state in custom resources hit etcd and API server limits. This design avoids each of their mistakes:

| Project | What went wrong | What this design does instead |
|---|---|---|
| Kyverno | It created report objects at the rate of events. 1.6 million objects (9.4 GB) made etcd read-only ([kyverno#8974](https://github.com/kyverno/kyverno/issues/8974)) | The number of NodeHealth objects can never exceed the number of Nodes, and the connector has a write limit for each node |
| Crossplane | Hundreds of CRDs overloaded API servers ([crossplane#2649](https://github.com/crossplane/crossplane/issues/2649)). Two controllers that fought over one field raised API server load fourfold ([crossplane#5458](https://github.com/crossplane/crossplane/issues/5458)) | Two new CRDs, and one writer for each section of a NodeHealth object |
| Dex | It created more than 500,000 objects in under 24 hours, and its cleanup, which listed them all, timed out ([dex#1292](https://github.com/dexidp/dex/issues/1292)) | Each idle object is deleted by name, without listing |
| Rancher Fleet | A status list with an entry for every resource on about 70 clusters went over etcd's request size limit ([fleet#3482](https://github.com/rancher/fleet/issues/3482)). An empty cache read as "not found" and uninstalled workloads ([fleet#5406](https://github.com/rancher/fleet/issues/5406), fixed in [fleet#5585](https://github.com/rancher/fleet/pull/5585)) | No list of nodes inside one shared object. An empty or unsynced cache never releases a node |

The Kubernetes documentation describes the best fit for custom resources as a relatively small number of relatively small objects that change relatively infrequently ([Declarative APIs](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/#declarative-apis)). So this design stores one small object per node with faults, writes it only when something changes, and never stores one object per health event.

## Decision

We add **NodeHealth mode** as a third datastore option, next to MongoDB and PostgreSQL. It is selected with `global.datastore.provider: kubernetes`, and the default does not change. In this mode:

1. Each node that has faults gets one cluster-scoped `NodeHealth` object.
2. Monitors keep sending every event, as today. The platform connector, in either role, creates and updates the NodeHealth objects. It compares each event with the open faults in the node's NodeHealth object, and it writes new faults, changes and clears before it replies to the monitor. A repeat of an open fault causes no write, except its first report after a reboot, a finished remediation attempt or an uncordon.
3. The three consumers, fault-quarantine, node-drainer and fault-remediation, read the NodeHealth objects instead of the database. They build today's events from the open faults, so their decision code does not change. Before every cordon, eviction, reboot or reset, they read the current state again. A node is released only when a newer write shows that its faults cleared and their clear delays passed.
4. No database runs. The health-events-analyzer, which needs past events, is not supported. The event exporter is changed to read the NodeHealth objects instead of the database, and csp-health-monitor keeps its maintenance events in a new custom resource.

```mermaid
flowchart LR
  mon["Health monitors"] -->|"every event, as today"| pc["Platform connector: DaemonSet, deployment, or both"]
  pc -->|"changes only"| nh[("NodeHealth object, one per node with faults:<br/>faults in spec, consumer progress in status")]
  pc -->|"conditions and Events, as today"| node[("Node")]
  nh <--> fq["fault-quarantine"] -->|"cordon, taint, annotation"| node
  nh <--> nd["node-drainer"] -->|"evict"| pods["Pods"]
  nh <--> fr["fault-remediation"] -->|"maintenance request"| jan["janitor: reboot or reset"]
  nh -.->|"optional"| exp["event exporter"]
```

The modules still do not call each other. They meet only in the NodeHealth object, the Node and the maintenance requests.

## Implementation

### The NodeHealth object

A NodeHealth object is named after its Node and owned by it, so there is at most one per live Node, and deleting the Node deletes it. An object is idle when its node has had no faults for 24 hours. Then fault-quarantine deletes the object, because it already watches every object and times the clear delays. The write limit refills and the flap history expires within an hour, so a delete after 24 hours resets neither. Each object has two kinds of sections:

- **Fault sections in `spec`, written by the connector.** `spec.node` holds faults from monitors that run on the node, such as syslog-health-monitor. `spec.central` holds faults from cross-node monitors, such as csp-health-monitor or lifecycle-manager ([ADR-051](051-maintenance-request.md)). The two kinds follow different rules: for example, the boot ID applies only to `spec.node`. The split also lets admission policies limit which sections each DaemonSet connector pod may change. It also gives each section its own write limit, so a node's own faults cannot use up the limit that its central faults need.
- **Consumer sections in `status`.** Each of the three consumers writes only its own section, to record what it did.

An object with one fault is about 2 KB. The connectors limit the metadata and the fault sections to 12 KiB, and each device to 8 entities. Faults that do not fit merge into one overflow entry, which still keeps the node quarantined. A healthy event for a whole check removes that check from the entry. In the node section, a reboot also clears the entry, because monitors report their open faults again after a reboot. With the consumer sections, a quarantined object at that limit is about 19 KB.

The object stores nothing that can be derived from other data. There is no summary of the faults, no history of remediations, no count of repeats and no separate change counter. Most fields explain themselves or have a comment in the example. These need more explanation:

| Field | Meaning |
|---|---|
| `metadata.generation` and each device's `generation` | The API server raises the object's generation by 1 on every change to `spec`. The connector writes the generation of each device's latest change into the device, and fault-quarantine records the last generation it handled in `observedGeneration` when it writes for a decision. So the consumers can order changes without comparing clocks |
| `bootID`, `bootGeneration` | Node section only. `bootID` is the node's boot ID at the connector's last write, and `bootGeneration` is the generation of the first change in that boot. With them the connector finds the first report of an open fault after a reboot, which it writes once ([how](#handling-repeated-errors)) |
| `allowance` | How much of each part of the [write limit](#the-write-limit) is left. The central section has only `urgentBytes`, for all its writes |
| `entities` | The entities of the device's first event, at most 8. Only identifying entities, such as `GPU_UUID` or `PCI`, name the device. So a report that differs only in other entities, such as XID 74's register values, is a repeat |
| `flaps`, `recentlyClosed` | `flaps` counts how often the fault came back after a clear. `recentlyClosed` keeps each cleared device's clear time and flap count for 60 minutes |
| `sessionID` | Names the current quarantine session ([ADR-062](062-fault-quarantine-release-at-session-end.md)). A new one starts when fault-quarantine quarantines the node. The other two consumers copy it, so their records from an earlier session no longer count |
| `faultRemediation.remediations` | One entry per remediation target, with the attempt number, the generation at which it started, and whether it finished. The name of the maintenance request is derived from the session, the target and the attempt, so fault-remediation can find the request again after a restart |

**Example.** Node `gpu-node-17` has two open faults:

1. GPU 3 logged XID 48, a fatal memory error that calls for a GPU reset. The node is quarantined, the pods that use GPU 3 are evicted, and the GPU reset is running.
2. The cross-node monitor lifecycle-manager reported planned host maintenance for the node, which fault-quarantine has not handled yet.

The example leaves out fields that exist only when an event sets them, such as override flags.

```yaml
apiVersion: healthevents.dgxc.nvidia.com/v1alpha1
kind: NodeHealth
metadata:
  name: gpu-node-17
  generation: 13
  ownerReferences:
  - {apiVersion: v1, kind: Node, name: gpu-node-17, uid: 78ff5c0a-e565-4211-8378-6ddb47ee2a6b}
spec:
  node:                                         # faults from monitors that run on gpu-node-17
    bootID: 4f2c9a10-3e71-4a55-9c3e-8d1b2f7a6e04
    bootGeneration: 9
    allowance:
      urgentBytes: 28312
      normalBytes: 13312
      updatedAt: "2026-09-16T12:39:26Z"
    recentlyClosed: []
    faults:
    - agent: syslog-health-monitor
      componentClass: GPU
      check: SysLogsXIDError
      version: 1
      errorCode: "48"
      isFatal: true                             # stays true while the fault is open
      recommendedAction: COMPONENT_RESET        # the most severe action seen while open
      devices:
      - entities:
        - {type: GPU_UUID, value: GPU-67fec4a7-a3e1-385c-0c98-4361b4a372ca}
        - {type: PCI, value: "0002:00:01"}
        generation: 12
        flaps: 0
        since: "2026-09-16T12:39:26Z"            # when the fault opened on this device
  central:                                      # faults from cross-node monitors
    allowance: {urgentBytes: 30208, updatedAt: "2026-09-16T13:10:00Z"}
    recentlyClosed: []
    faults:
    - agent: lifecycle-manager
      componentClass: Node
      check: csp-scheduled-maintenance
      version: 1
      errorCode: ""
      isFatal: true
      recommendedAction: RESTART_VM
      attributes: {maintenanceRequestName: mr-gpu-node-17-7f3a}   # allow-listed metadata only
      devices:
      - {generation: 13, flaps: 0, since: "2026-09-16T13:10:00Z"}   # no entities: the whole node
status:
  faultQuarantine:
    state: Quarantined                          # Quarantined, WouldQuarantine (dry run) or Released
    sessionID: 7c0e8f52-3b1d-4c47-9a35-0d6e1f2b8a94
    observedGeneration: 12                      # generation 13 is not handled yet
  nodeDrainer:
    sessionID: 7c0e8f52-3b1d-4c47-9a35-0d6e1f2b8a94
    drains:                                     # one entry per drain scope
    - {scope: GPU_UUID=GPU-67fec4a7-a3e1-385c-0c98-4361b4a372ca, state: Succeeded, startedAt: "2026-09-16T12:40:01Z"}
  faultRemediation:
    sessionID: 7c0e8f52-3b1d-4c47-9a35-0d6e1f2b8a94
    remediations:
    - {target: GPU_UUID=GPU-67fec4a7-a3e1-385c-0c98-4361b4a372ca, attempt: 1, startedGeneration: 12, state: Running}
```

### Who writes each section

- The connector writes the fault sections, and each consumer writes only its own consumer section. Role-based access control (RBAC) and admission policies enforce this split.
- ADR-052's node binding ties each monitor's token to its node, so a monitor that runs on a node can report only about that node. One exception: when cross-node monitors publish through the DaemonSet connector, its pods may also write other nodes' central faults, so a compromised node could write them too. Today every connector pod already holds a database credential that can write events for any node.
- Every write, and fault-quarantine's delete, is conditional: it fails if the object changed after the writer read it. This is the standard Kubernetes check on `metadata.resourceVersion`, so no writer overwrites or deletes a change that it has not seen. Each writer patches only its own section, so a writer from an older release never changes another writer's section.

### How the consumers get today's events

The consumers keep the code that decides what to do with an event: fault-quarantine's rules, node-drainer's drain choices, and fault-remediation's actions and request templates. A shared package builds today's `HealthEvent` from each open fault on each device, and the consumers pass it to that code:

| `HealthEvent` field | Taken from |
|---|---|
| `nodeName` | The object's name |
| `agent`, `componentClass`, `checkName`, `version`, `isFatal`, `recommendedAction`, `customRecommendedAction`, `quarantineOverrides`, `drainOverrides` | The fault |
| `errorCode` | The fault's error code |
| `message` | Empty, because the object keeps no message (see below) |
| `entitiesImpacted` | The device's entities |
| `metadata` | The fault's allow-listed attributes |
| `generatedTimestamp` | `since`, when the fault opened on the device |
| `isHealthy` | Always false, because the object keeps only open faults |
| `processingStrategy` | Always `EXECUTE_REMEDIATION`, because [other events never change the object](#handling-repeated-errors) |
| `id` | Built from the object's UID, the fault, the device and `since`, so it stays the same after a restart |

**No event message.** The object keeps no event message, because a stored message would go stale. Repeats are not written, and one fault can cover several devices, so a stored message could even name the wrong device. The `HealthEvent` that the consumers get therefore has an empty `message`. The `cordon-reason` Node label of a forced quarantine gets the check name instead. Templates that read `.HealthEvent.Message` get an empty string, so a custom drain, a maintenance request or an ExternalRemediationRequest ([ADR-040](040-external-remediation-request.md)) made from one has no message. The `quarantineHealthEvent` Node annotation and the changes that the event exporter sends have no message either. Operators still see the messages in Node conditions and Events, as today.

The code around those decisions changes. The consumers watch the NodeHealth objects with an informer instead of a database change stream. Each cache keeps only the fields that its consumer uses. Each consumer acts on the faults in `spec` and on live state: the Node, its pods and the maintenance requests. It records what it did in its own section of `status`, instead of in status fields on each event. The next consumer reads that section to pick up the work, as the Deployment controller reads ReplicaSet status. As today, fault-quarantine also lists the faults that hold the node in its `quarantineHealthEvent` Node annotation, which it writes with the cordon, and the other two consumers read them there. After a restart the consumers read the current objects instead of replaying events.

### Changes in each component

| Component | Change for NodeHealth mode |
|---|---|
| Health monitors | Their events do not change. They keep sending every event in today's format |
| csp-health-monitor | Keeps its maintenance events in a new `CSPMaintenanceEvent` custom resource instead of the database |
| Platform connector, both roles | A new writer replaces the database writer. It writes only changes, and in both roles it replies to the monitor only after the write ([Handling repeated errors](#handling-repeated-errors)). Node conditions and Events stay as today, because other tools read them |
| fault-quarantine | Reads the NodeHealth objects instead of the database and builds today's events from them for its existing rules. It records each quarantine in its own section, times the clear delays, and deletes idle objects |
| node-drainer | Drains for the devices in fault-quarantine's Node annotation, using the action of each matching fault, and records each drain in its own section. Its work is keyed by node instead of the event priority queue of [ADR-041](041-node-drainer-priority-queue.md) |
| fault-remediation | Starts a remediation only after the drain finished, and records each maintenance request, and when it finished, in its own section |
| janitor | No change. It still carries out the maintenance requests |
| Event exporter | Watches the NodeHealth objects and sends each change, instead of reading the database |
| health-events-analyzer | Not supported in this mode |
| Helm chart | Adds `kubernetes` as a value of `global.datastore.provider`, the `global.nodeHealth.recordInformational` key, the NodeHealth and CSPMaintenanceEvent CRDs, RBAC and admission policies. In NodeHealth mode it runs no database and does not install the analyzer |

### Example: one GPU fault from detection to recovery

GPU 3 on node `gpu-node-17` logs XID 48, a memory error that calls for a GPU reset.

```mermaid
sequenceDiagram
  participant Mon as syslog-health-monitor
  participant PC as Platform connector
  participant NH as NodeHealth object
  participant FQ as fault-quarantine
  participant ND as node-drainer
  participant FR as fault-remediation
  participant Jan as janitor
  Mon->>PC: New fault, XID 48 on GPU 3
  PC->>NH: Create the object with the fault
  PC-->>Mon: Reply after the write
  Mon->>PC: The same XID again, many times
  Note over PC: A repeat causes no write
  NH-->>FQ: New fault
  FQ->>NH: A rule matches, so record the quarantine
  FQ->>FQ: Cordon, taint and annotate the Node
  NH-->>ND: Node quarantined
  ND->>ND: Evict the pods that use GPU 3
  ND->>NH: Record the finished drain
  NH-->>FR: Drain finished
  FR->>NH: Record the remediation
  FR->>Jan: GPUReset maintenance request
  Jan->>Jan: Reset GPU 3
  Mon->>PC: Healthy event, the reset worked
  PC->>NH: Remove the fault
  NH-->>FQ: No open faults
  FQ->>FQ: Wait the clear delay, then uncordon the Node
```

### Handling repeated errors

For each event, the connector looks for a matching open fault in its copy of the node's NodeHealth object. It uses the matching code of fault-quarantine, which moves to a shared package, so the connector and fault-quarantine agree on what counts as the same fault. Then:

- **A new or worse fault**, one that is newly fatal or needs a more severe action, is written to the object before the connector replies to the monitor.
- **A repeat of an open fault** causes no write. The one exception is a report that a consumer waits for: the first report of an open fault after a reboot, after a remediation attempt for it finished, or after someone uncordoned the node. The connector writes that report once, as a new `generation` on the device, so fault-remediation can try again or escalate, and fault-quarantine can quarantine the node again, as today. The connector knows of a reboot from the node's boot ID, as described below, and of the other two from the consumer sections.

For example, a node that floods XID 137 and 145 at 120 to 600 events per second costs at most 2 writes when the flood starts and none after that. That is at most about 4.3 KB of etcd history in total (estimate).

The connector's own writes keep its copy current. It reads the object again when a monitor connects, because the monitor's earlier events may have gone through another connector. It also reads it when a repeat finds the copy more than a minute old, so it sees the consumers' records without one read per event. These reads do not come from a cache, because a cache can miss a clear that another connector wrote. They are reads by name: the connector never lists or watches NodeHealth objects or Nodes.

**How the connector detects a reboot.** Database mode needs no boot ID, because it stores every event. Here a repeat causes no write, so the connector must know which boot the open faults were last written in, also after a restart or on another replica. The object stores it, and the connector never watches Nodes:

1. Each write to `spec.node` stores the boot ID that the connector last read from the Node (`status.nodeInfo.bootID`). The first write in a new boot also sets `bootGeneration`.
2. With each batch of events that repeats an open fatal fault, the connector reads the Node's boot ID by name, without a cache. The Node-condition path already makes this read for such batches today. The DaemonSet connector can read its own node's boot ID locally instead.
3. If that boot ID differs from `bootID`, the repeat is the first event of the fault after a reboot. The connector writes it once, together with the new boot ID.
4. The kubelet publishes the new boot ID when it starts, normally before the monitors run their first checks.

A finished attempt in fault-remediation's section is not enough on its own: monitors can send the event again before fault-remediation records the finished attempt, and a reboot by someone else leaves no record at all.

The writer handles each event after the connector's metadata and override stages and before the optional deduplication stage of [ADR-039](039-health-event-deduplication.md), which is off by default. That stage never drops an event. It only marks a repeated syslog XID or SXID event `STORE_AND_ANALYSE`, so that the Node conditions ignore it. Because the writer sees each event before any duplicate mark, a stale mark cannot hide a fault that came back, for example when a replica of the deployment platform connector missed the clear. Like fault-quarantine today, the writer uses only `EXECUTE_REMEDIATION` events, healthy or unhealthy. `STORE_ONLY` and `STORE_AND_ANALYSE` events never change a NodeHealth object. Both strategies mean an event must not change the cluster, so it cannot open or clear a fault, or use the node's write limit. Unless `global.nodeHealth.recordInformational` is `true`, the writer also skips informational faults from monitors on the node. An informational fault is not fatal and recommends `NONE`.

### The write limit

Each node may write only a fixed amount per time period, and the object's `allowance` fields store how much is left. So every connector replica sees the same limit, also after a restart. The urgent part, for new and worse faults and the reports that a consumer waits for, allows 32 KiB at once and refills 32 KiB every 10 minutes. The normal part, for clears and other changes, allows 13 KiB at once and refills 32 KiB every hour. The limits count bytes, because etcd's cost grows with the bytes written. A write bigger than a part's full amount goes through when the part is full, and the part then refills from below zero. A node with one fault can make about 18 urgent writes at once, then one about every 32 seconds (estimate). If a node has used up its write limit, the connector waits until it can write before it replies, so the monitor waits too. If the monitor stops waiting, the connector drops the write. All central sections in the cluster also share one limit of 64 MiB at once, refilled every 10 minutes, split between the connectors that receive cross-node events. Each keeps its share in memory and starts with an empty share after a restart, so restarts never add writes.

### Clearing faults and handling flapping devices

A healthy event clears the matching faults. Each clear then waits a short **clear delay** before fault-quarantine acts on it. The delay starts at 30 seconds and doubles each time the fault comes back, up to 30 minutes, so a device that keeps flapping keeps its node quarantined.

The connector removes a cleared device from the fault list at once and records it in `recentlyClosed`. Then fault-quarantine keeps the node quarantined for the clear delay, which it times with its own clock from when it sees the removal. So a difference between the clocks of two hosts cannot shorten the delay. A fault that comes back within 60 minutes of its clear neither waits for the write limit nor uses it. So a return during the clear delay reaches fault-quarantine before the delay ends, and the node stays quarantined. A later return quarantines the node again. In both cases the fault opens with one more flap, so its next clear delay is longer, and a device that flaps slowly gets longer and longer delays. Monitors need no flap logic of their own.

```mermaid
flowchart LR
  report(["A monitor reports the fault"]) --> open["Open<br/>The fault is in the list<br/>and keeps the node quarantined"]
  open -->|"A healthy event arrives"| delay["Removed, clear delay runs<br/>fault-quarantine keeps the node quarantined:<br/>30 s at first, doubling on each return, up to 30 min"]
  delay -->|"The fault comes back before the delay ends"| open
  delay -->|"The delay ends with no return"| released["Done<br/>The fault no longer keeps the node quarantined"]
  released -.->|"The fault comes back within 60 minutes:<br/>it keeps the longer delay"| open
```

The connector applies each monitor's events in the order they arrive, not by their timestamps. After a reboot, syslog-health-monitor first sends a clear and then reads the new boot's log from the start, so a new XID can carry an earlier time than the clear. Because the XID arrives after the clear, it reopens the fault.

### Safety rules

- **Release only on evidence.** Before fault-quarantine releases a node, a newer write must no longer contain its faults, and their clear delays must have passed with no return. A missing, unsynced or re-created object releases nothing. A lost `status`, for example after a restore, releases nothing either, because the faults in `spec` still hold the node.
- **Re-check before acting.** Each consumer reads the object and the Node again from the API server, not from its cache, before it acts. A remediation starts only after the drain finished, only for a fault that is still open, and never while another maintenance request for the node still runs. A remediation is never retried on a timer, only when the fault is reported again after the last attempt.
- **Circuit breaker.** The circuit breaker of [ADR-022](022-circuit-breaker-reset-mechanism.md) and its settings decide how many nodes fault-quarantine quarantines, as today. NodeHealth mode adds no limits of its own on quarantines, drains or remediations. The breaker's standing limit, `fault-quarantine.circuitBreaker.maxCordonedPercentage`, is off by default. Large clusters should set it, as [Scale and etcd usage](#scale-and-etcd-usage) shows. A breaker reset cannot skip open faults: fault-quarantine evaluates every open fault, whichever cursor (`RESUME` or `CREATE`) the reset names.
- **Uncordons by someone else.** If an operator or another controller uncordons a node while its fault is open, fault-quarantine records this in its section. As today, it detects the uncordon from its own cordon annotation on the Node, so a crash before its cordon is not taken for an uncordon. It quarantines the node again when the fault is reported again. Until then, that fault does not count in a later quarantine.
- **Safe restarts.** Every step is safe to repeat, and each consumer records what it will do before it does it. A crash test restarts each component after each write and checks the result.
- **A full etcd.** When etcd is full, no new cordon, drain or maintenance request starts.

### Release at session end and post-remediation validation

Two fault-quarantine features decide when a node's cordon and taints come off. Both keep working in NodeHealth mode:

- **Release at session end ([ADR-062](062-fault-quarantine-release-at-session-end.md)).** When a fault opens, fault-quarantine applies the taints and the cordon that its rules require, also when the node is already quarantined. It removes them only when the session ends: every fault in the session has cleared and its clear delay has passed.
- **Post-remediation validation ([ADR-049](049-node-validation.md)).** A NodeHealth object drops a fault once it clears, so fault-quarantine keeps recording which faults need validation in its Node annotation, as today. When the session ends, it checks the drain in `status.nodeDrainer` instead of the database, creates the `ValidationRequest`, and leaves the cordon and taints for lifecycle-manager, which removes them after validation passes, as today.

### Changes for fault-quarantine rules

The event that fault-quarantine gives its rules has the same 18 fields as today ([How the consumers get today's events](#how-the-consumers-get-todays-events)), and all 8 shipped rulesets keep matching. But rules can no longer select on:

- the event message, because the object keeps none,
- detail entities of later events, because the object keeps those of each device's first event,
- event metadata outside an allow-list,
- informational faults from monitors on the node, unless `global.nodeHealth.recordInformational` is set to `true`,
- more than 8 entities per device, which affects XID 74 events,
- faults beyond the size limit, which merge into one overflow entry,
- two error codes of one event together, because each code becomes its own fault. No shipped monitor or ruleset does this.

### Other components that use the database

Three components use the database today for more than remediation.

- **health-events-analyzer: not supported.** It finds patterns over time, such as an XID that repeats many times in a day, and 15 of its 22 default rules need past events. A NodeHealth object keeps only the faults that are open now, so the analyzer cannot run in NodeHealth mode, and the chart does not install it. Installs that need the analyzer stay in database mode.
- **Event exporter: needs a change.** It reads new events from the database and sends them to an HTTP endpoint ([ADR-012](012-health-events-exporter.md)). To run in NodeHealth mode, it must be changed to watch the NodeHealth objects instead, and to send each change, such as a new fault, a worse fault or a clear. It then sends changes, not every repeated event, and no `STORE_ONLY` or `STORE_AND_ANALYSE` events.
- **csp-health-monitor: moves its store to a custom resource.** It polls the cloud provider for planned maintenance, and it keeps each maintenance event and its status in the database. In NodeHealth mode it keeps them in a new `CSPMaintenanceEvent` custom resource instead: one object per maintenance event, with the same fields as the database record. Its existing store interface gets an implementation on these objects, so neither its logic nor its maintenance-notifier sidecar changes. A ConfigMap keeps how far it has read each cloud provider's events, and each object is deleted 7 days after its maintenance finished. So there are only about as many objects as nodes with planned or recent maintenance.

### Scale and etcd usage

The design must have a known worst-case cost from a 3-node cluster to 100,000 nodes, at 10 to 100 times today's peak event rate. The etcd cost grows with the number of nodes and with how often faults change, not with how many events the monitors see. The worst case counts the live objects and every old version that etcd still keeps. It assumes that every node uses its full write limit, every clear comes back, and every consumer works at full speed. The worst case also depends on the circuit breaker, because every quarantined node also carries the consumers' records. With the breaker's defaults, which have no standing limit, every GPU node can end up quarantined, and the worst case at 100,000 nodes is about 32 to 34 GB (estimate). With the standing limit set to 10% of GPU nodes (`fault-quarantine.circuitBreaker.maxCordonedPercentage: 10`), it is about 17 to 19 GB. A 16 GB etcd has less than either left after it stores the Node objects ([#1705](https://github.com/NVIDIA/NVSentinel/issues/1705)). One fault on each of 100,000 nodes costs only about 0.4 GB (estimate).

Each row of the table gives the largest cluster whose worst case fits in half of the etcd space that the Node objects leave. The other half stays free for the rest of the cluster. The ranges cover 10 and 15 minutes of etcd history ([why](#a-counter-that-changes-on-every-repeat-the-issues-proposal)).

| etcd size | Largest cluster with the breaker's defaults (estimate) | Largest cluster with a 10% standing limit (estimate) |
|---|---|---|
| 2 GiB (self-managed) | About 2,500 to 2,700 nodes | About 4,200 to 4,900 nodes |
| 8 GB (EKS standard, AKS) | About 10,400 to 11,200 nodes | About 17,800 to 20,000 nodes |
| 16 GB (EKS Provisioned) | About 21,300 to 22,800 nodes | About 36,500 to 40,800 nodes |
| 20 GB (EKS ultra-scale, if Node objects are in another etcd partition) | About 28,900 to 31,000 nodes | About 52,500 to 59,700 nodes |

Larger clusters need a provider that splits etcd storage or replaces etcd.

### Switching an existing install

A new install sets `global.datastore.provider` to `kubernetes`. NodeHealth mode has no data migration. An existing install that switches starts with no NodeHealth objects. So it switches only after fault-quarantine has handled every open fault, for example while the circuit breaker is not tripped:

- An operator applies the chart's CRDs before the upgrade, because `helm upgrade` does not install them.
- Nodes that are quarantined at the switch stay quarantined, because a missing object never releases a node. An operator removes their cordon and taints after checking them.
- A fault from before the switch counts only when its monitor sends it again. After the switch, csp-health-monitor sees only maintenance notices from about the time of the switch.
- A switch back to a database needs an empty database, because the consumers would act on events left from before the switch.

## Rationale

- **Each node's open faults are stored, not every event.** The number of NodeHealth objects grows with the number of nodes that have faults, and the number of writes grows with how often faults change. This avoids the failures in [Lessons from other projects](#lessons-from-other-projects).
- **Faults go in `spec`, and consumer progress goes in `status`.** Creating an object with its faults in `spec` takes one write, and `status` would need a second. Consumer writes to `status` do not change `metadata.generation`, so a consumer can tell a fault change from another consumer's write. RBAC can then separate the connector from the consumers, which the Kubernetes proposal [KEP-2527](https://github.com/kubernetes/enhancements/tree/master/keps/sig-architecture/2527-clarify-status-observations-vs-rbac) names as the reason for the split. A restore that drops `status`, as [Velero](https://velero.io/docs/main/restore-reference/) does by default, cannot drop open faults. Lease, CSINode and ResourceSlice also keep what node agents report in `spec`.
- **Monitors keep their contract, and the connector drops repeats.** The connector already receives every event in both roles, so it is the right place to compare an event with the open faults and drop a repeat. Everything else lives in the NodeHealth object, so any connector replica can handle any event. Kubernetes treats repeated Events in a similar way: it keeps one Event instead of one object per repeat, and it limits how often a source writes ([kubernetes#47367](https://github.com/kubernetes/kubernetes/pull/47367)).
- **Releasing a node is the risky action, so it needs evidence.** A wrong release puts a faulty GPU back into service. Stale state was the largest class of the controller bugs that [Sieve](https://www.usenix.org/system/files/osdi22-sun.pdf) found (19 of 46). Releasing only on evidence, conditional writes and fresh reads before each action close those paths.
- **The existing decision code stays**, so the rules and their tests carry over.

## Consequences

### Positive

- Remediation works without MongoDB or PostgreSQL. A plain Helm install can remediate, and a database outage can no longer stop remediation.
- During XID floods, the connector writes only changes, instead of one database write per event.
- The events that monitors send, including custom monitors, do not change.
- The same design works with the DaemonSet connector, the deployment platform connector or both, so customers can turn the DaemonSet off.
- A connector crash can no longer lose a fault or a clear, because both roles write them before they reply. Today the DaemonSet connector replies before it writes.
- The consumers act on the faults that are open now. After a restart they read the current NodeHealth objects instead of replaying old events, so a missed event no longer means a missed fault.
- Operators can list the nodes that had faults in the last 24 hours with `kubectl get nh` and see their open faults with `kubectl describe nh`.

### Negative

- Quarantine rules can no longer match a few event details, such as the event message or metadata outside an allow-list. Templates and the event exporter also get no message. Some custom rulesets and templates may need changes.
- Every clear waits its clear delay, also after a reboot or a GPU reset that worked. So a repaired node returns to service 30 seconds later, or up to 30 minutes later if its fault flapped in the last hour.
- There is no event history. A NodeHealth object keeps only open faults, and fault-quarantine deletes it 24 hours after the last fault clears. Investigations must rely on the exported changes or on logs.
- The health-events-analyzer cannot run, so patterns over time are not detected, such as an XID that keeps coming back or a node that needs remediation again and again. Installs that need it stay in database mode.
- The event exporter and csp-health-monitor need code changes before they can run in this mode.
- A node that has many faults and has used its write limit must wait before it writes again. In the worst case a clear waits up to about 35 minutes, so the node stays quarantined longer. A new fatal fault waits up to about 6 minutes before fault-quarantine sees it (estimate). Meanwhile the monitor waits for its reply. Monitors retry for up to 5 minutes, so in that worst case a change can be dropped until the monitor reports it again. A monitor sends one batch at a time, so a cross-node monitor that waits for one node also delays its reports about every other node.
- One etcd cannot hold the worst case for 100,000 nodes, and large clusters need the circuit breaker's standing limit to stay within the support table. The memory use of the API server and the consumers is unknown until the scale test.
- There is no data migration from or to a database ([Switching an existing install](#switching-an-existing-install)).
- The NodeHealth and CSPMaintenanceEvent types become public interfaces that must stay backwards compatible.
- The consumers must support both the database and the NodeHealth objects, because database mode stays.
- When the DaemonSet is off, every write goes through the deployment platform connector, which ADR-052 still marks experimental.

## Alternatives Considered

### A counter that changes on every repeat (the issue's proposal)

**Rejected** because the counter does not scale. A patch that changes one small field is small to send. But the API server still writes the whole object to etcd as a new version, and it sends the whole object to every watcher. A counter in `status` costs the same, because the API server stores `spec` and `status` as one object. Each old version stays in etcd for up to 10 minutes, or 15 during an API server failover. So a node that repeats an error 600 times a second would cause 600 full writes a second. The XID flood in [Handling repeated errors](#handling-repeated-errors) would add up to 1.23 GB of etcd history every 10 minutes (estimate). No decision uses how often a fault repeated. This design keeps the issue's core idea: one object per node that holds current state.

### One object per health event (ADR-027)

**Rejected** because the number of objects follows the event rate, which is how Kyverno filled etcd. Even one object per open fault would reach 1.6 million objects at 100,000 nodes with 16 faults each (estimate).

### NodeHealth objects behind the store-client health event interface

**Rejected** for the three consumers. NodeHealth mode is still selected like a database, and csp-health-monitor's maintenance store gets a Kubernetes implementation. But the store-client interface for health events assumes one stored document per event. The consumers update the status of each event, scan events by status and time when they start, look up later events of the same check, and resume change streams where they stopped. A NodeHealth object keeps only open faults, so an implementation would have to store every event, and each consumer's status for it, in the object again. The objects would then grow with the event rate, and the consumers would still act on each event once, which is how events get lost today.

### A separate datastore (SQLite, kine or an aggregated API)

**Rejected** for the first release, because each one adds another stateful system that can fail. The kine project stores Kubernetes objects in a SQL database instead of etcd. SQLite allows only one writer, so one pod becomes a single point of failure. A separate store stays the fallback if the scale test fails.

### An OpenTelemetry backend as the datastore

**Rejected** because these backends have no watch and no in-place update, and an outage of the observability stack would stop remediation. Exporting events to such a backend for analysis stays possible.

## References

- NVSentinel: [issue #1950](https://github.com/NVIDIA/NVSentinel/issues/1950); [ADR-027](027-kubernetes-data-store.md) (proposed to be superseded by this record) and its series [#640](https://github.com/NVIDIA/NVSentinel/pull/640), [#787](https://github.com/NVIDIA/NVSentinel/pull/787), [#882](https://github.com/NVIDIA/NVSentinel/pull/882), [#970](https://github.com/NVIDIA/NVSentinel/pull/970); issues [#798](https://github.com/NVIDIA/NVSentinel/issues/798), [#1705](https://github.com/NVIDIA/NVSentinel/issues/1705); the `HealthEvent` prototype [#795](https://github.com/NVIDIA/NVSentinel/pull/795)
- Related records: [ADR-012](012-health-events-exporter.md), [ADR-022](022-circuit-breaker-reset-mechanism.md), [ADR-039](039-health-event-deduplication.md), [ADR-040](040-external-remediation-request.md), [ADR-041](041-node-drainer-priority-queue.md), [ADR-049](049-node-validation.md), [ADR-051](051-maintenance-request.md), [ADR-052](052-deployment-platform-connector.md), [ADR-062](062-fault-quarantine-release-at-session-end.md)
- Kubernetes: [custom resources](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/); [KEP-2527](https://github.com/kubernetes/enhancements/tree/master/keps/sig-architecture/2527-clarify-status-observations-vs-rbac); [event spam filter #47367](https://github.com/kubernetes/kubernetes/pull/47367); stalled node-health proposals [KEP-4212](https://github.com/kubernetes/enhancements/issues/4212), [KEP-5233](https://github.com/kubernetes/enhancements/issues/5233), [KEP-5283](https://github.com/kubernetes/enhancements/issues/5283), [KEP-5770](https://github.com/kubernetes/enhancements/issues/5770)
- Lessons from other projects: [Kyverno #8974](https://github.com/kyverno/kyverno/issues/8974) and [reports-server](https://github.com/kyverno/reports-server); [Crossplane #2649](https://github.com/crossplane/crossplane/issues/2649) and [#5458](https://github.com/crossplane/crossplane/issues/5458); [Dex #1292](https://github.com/dexidp/dex/issues/1292); [Rancher Fleet #3482](https://github.com/rancher/fleet/issues/3482), [#5406](https://github.com/rancher/fleet/issues/5406) and [#5585](https://github.com/rancher/fleet/pull/5585); [Sieve](https://www.usenix.org/system/files/osdi22-sun.pdf); [Velero restore reference](https://velero.io/docs/main/restore-reference/)
- Storage and scale: [EKS Provisioned Control Plane](https://docs.aws.amazon.com/eks/latest/userguide/eks-provisioned-control-plane.html); [EKS ultra-scale](https://aws.amazon.com/blogs/containers/under-the-hood-amazon-eks-ultra-scale-clusters/); [AKS etcd limit](https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/create-upgrade-delete/troubleshoot-apiserver-etcd)
