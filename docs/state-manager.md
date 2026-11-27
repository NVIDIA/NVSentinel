# State Manager

## Overview

The State Manager is a Go library that the NVSentinel modules share. It manages the lifecycle of the `dgxc.nvidia.com/nvsentinel-state` node label across these modules. It implements a state machine with transition validation and observability. The state machine coordinates the operations of fault-quarantine, node-drainer, and fault-remediation.

## Purpose

The State Manager coordinates the node lifecycle state across three modules that operate on the same node:

- **fault-quarantine**: Detects faults and applies the `quarantined` state
- **node-drainer**: Evacuates workloads and applies `draining` → `drain-succeeded` or `drain-failed`
- **fault-remediation**: Runs the recovery and applies `remediating` → `remediation-succeeded` or `remediation-failed`

The State Manager provides:

- One authoritative record of the remediation status of each node
- State transition validation with metrics for unexpected flows
- Terminal state detection (drain-failed, remediation-failed, remediation-succeeded)
- Cancellation support (label removal from any state without validation)

## State Machine

```text
                ┌──────────────────┐
                │   [NO LABEL]     │  Healthy node
                └────────┬─────────┘
                         │
                         │ Fault detected
                         ▼
                ┌──────────────────┐
                │   quarantined    ├──────────────────────┐
                └────────┬─────────┘                      │
                         │                                │
                         │ Start drain                    │ No pods to drain
                         ▼                                │
                ┌──────────────────┐                      │
   Healthy ◄────┤     draining     │                      │
   event        └────────┬─────────┘                      │
   (cancel)              │                                │
      │                  │ Drain completed                │
      │         ┌────────┴────────┐                       │
      │         │                 │                       │
      │         ▼                 ▼                       │
      │  ┌───────────────┐  ┌────────────────┐            │
      │  │drain-failed   │  │drain-succeeded │◄───────────┘
      │  └───────────────┘  └──────┬─────────┘
      │    [TERMINAL]              │
      │                            │ Start remediation
      │                            ▼
      │                     ┌──────────────┐  Note: fault-remediation
      │                     │ remediating  │  only consumes drain-succeeded.
      │                     └──────┬───────┘  drain-failed is a terminal state.
      │                            │
      │                   ┌────────┴────────┐
      │                   │                 │
      │                   ▼                 ▼
      │            ┌─────────────┐  ┌──────────────┐
      │            │ remediation-│  │ remediation- │ [TERMINAL]
      │            │ succeeded   │  │   failed     │
      │            └─────┬───────┘  └──────────────┘
      │                  │
      │                  │ Healthy event
      ▼                  ▼
┌──────────────────────────────┐
│        [NO LABEL]            │
└──────────────────────────────┘
```

## State Label Values

| State                      | Applied By           | Description                            | Terminal |
|:---------------------------|:---------------------|:---------------------------------------|:---------|
| (no label)                 | Any                  | Healthy node, no active fault handling | No       |
| `quarantined`              | fault-quarantine     | Fault detected, node cordoned or tainted | No       |
| `draining`                 | node-drainer         | Workload evacuation in progress        | No       |
| `drain-succeeded`          | node-drainer         | All workloads evacuated successfully, or drain skipped (`drainOverrides.skip`) | No       |
| `drain-failed`             | node-drainer         | Workload evacuation failed             | Yes      |
| `remediating`              | fault-remediation    | Remediation action in progress         | No       |
| `remediation-succeeded`    | fault-remediation    | Remediation completed successfully     | Yes*     |
| `remediation-failed`       | fault-remediation    | Remediation action failed              | Yes*     |

*Terminal until a healthy event triggers the removal of the label. These states are terminal for one failure only. In the same quarantine, fault-remediation can still change the label after a partial recovery, or when it starts a new remediation. See the State Transitions table.

## State Transitions

This table lists the expected transitions. The State Machine diagram shows only the main flow.

| From State            | To State                   | Trigger                     |
|:----------------------|:---------------------------|:----------------------------|
| (no label)            | `quarantined`              | Fault detected              |
| `quarantined`         | `draining`                 | Drain started               |
| `quarantined`         | `drain-succeeded`          | No pods to drain, or drain skipped (`drainOverrides.skip`) |
| `draining`            | `drain-succeeded`          | All pods evacuated, or a custom drain CR completed |
| `draining`            | `drain-failed`             | Evacuation timeout or failure |
| `drain-succeeded`     | `remediating`              | Remediation started         |
| `drain-succeeded`     | `remediation-failed`       | Unsupported remediation action, or the maximum number of remediation attempts is reached |
| `remediating`         | `remediation-succeeded`    | Remediation completed       |
| `remediating`         | `remediation-failed`       | Remediation error           |
| `remediation-succeeded` | `remediation-failed`     | Partial recovery: a remaining active failure is unsupported or its remediation failed |
| `remediation-failed`  | `remediation-succeeded`    | Partial recovery: the recovered failure was the only failed one, and the remaining failures are remediated |
| `remediation-succeeded` | `remediating`            | New remediation-ready health event in the same quarantine, for example a fault after a reboot |
| `remediation-failed`  | `remediating`              | Retry of a failed maintenance CR with a new CR |
| (any state)           | (no label)                 | Healthy event (cancellation)|

### Example Flows

**Successful remediation:**

```text
none → quarantined → draining → drain-succeeded → remediating → remediation-succeeded → none
```

**No pods to drain:**

```text
none → quarantined → drain-succeeded → remediating → remediation-succeeded → none
```

**Failed drain:**

```text
none → quarantined → draining → drain-failed [TERMINAL]
```

**Cancelled drain (healthy event):**

```text
none → quarantined → draining → none
```

**Failed remediation:**

```text
none → quarantined → draining → drain-succeeded → remediating → remediation-failed [TERMINAL]
```
