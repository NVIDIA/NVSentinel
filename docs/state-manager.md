# State Manager

## Overview

The State Manager is a Go library (`commons/pkg/statemanager`) that manages the `dgxc.nvidia.com/nvsentinel-state` node label lifecycle across NVSentinel modules. It provides a state machine implementation with transition validation and observability for coordinating Fault Quarantine, Node Drainer, and Fault Remediation operations.

## Purpose

Coordinates node lifecycle state across three modules operating on the same node:
- **fault-quarantine**: Detects faults, applies `quarantined` state
- **node-drainer**: Evacuates workloads, applies `draining` → `drain-succeeded` or `drain-failed`
- **fault-remediation**: Executes recovery, applies `remediating` → `remediation-succeeded` or `remediation-failed`

Provides:
- Single source of truth for node remediation status
- State transition validation with metrics for unexpected flows
- Terminal state detection (drain-failed, remediation-failed, remediation-succeeded)
- Cancellation support (label removal from any state without validation)

## State Machine

```
                ┌──────────────────┐
                │   [NO LABEL]     │  Healthy node
                └────────┬─────────┘
                         │ Fault detected
                         ▼
                ┌──────────────────┐
                │   quarantined    │
                └────────┬─────────┘
                         │ Start drain
                         ▼
                ┌──────────────────┐
   Healthy ◄────┤     draining     │
   event        └────────┬─────────┘
   (cancel)              │ Drain completed
      │         ┌────────┴────────┐
      │         │                 │
      │         ▼                 ▼
      │  ┌───────────────┐  ┌─────────────┐
      │  │drain-succeeded│  │drain-failed │ [TERMINAL]
      │  └──────┬────────┘  └─────────────┘
      │         │ Start remediation
      │         ▼
      │  ┌──────────────┐
      │  │ remediating  │
      │  └──────┬───────┘
      │    ┌────┴────────────────┐
      │    │                     │
      │    ▼                     ▼
      │  ┌─────────────┐  ┌──────────────┐
      │  │ remediation-│  │ remediation- │ [TERMINAL]
      │  │ succeeded   │  │   failed     │
      │  └─────┬───────┘  └──────────────┘
      │        │ Healthy event
      ▼        ▼
┌──────────────────────────────┐
│        [NO LABEL]            │
└──────────────────────────────┘
```

## State Label Values

| State                      | Applied By           | Description                            | Terminal |
|:---------------------------|:---------------------|:---------------------------------------|:---------|
| (no label)                 | Any                  | Healthy node, no active fault handling | No       |
| `quarantined`              | fault-quarantine     | Fault detected, node cordoned/tainted  | No       |
| `draining`                 | node-drainer         | Workload evacuation in progress        | No       |
| `drain-succeeded`          | node-drainer         | All workloads evacuated successfully   | No       |
| `drain-failed`             | node-drainer         | Workload evacuation failed             | Yes      |
| `remediating`              | fault-remediation    | Remediation action in progress         | No       |
| `remediation-succeeded`    | fault-remediation    | Remediation completed successfully     | Yes*     |
| `remediation-failed`       | fault-remediation    | Remediation action failed              | Yes      |

*Terminal until healthy event triggers label removal

## State Transitions

| From State            | To State                   | Trigger                     |
|:----------------------|:---------------------------|:----------------------------|
| (no label)            | `quarantined`              | Fault detected              |
| `quarantined`         | `draining`                 | Drain initiated             |
| `draining`            | `drain-succeeded`          | All pods evacuated          |
| `draining`            | `drain-failed`             | Evacuation timeout/failure  |
| `drain-succeeded`     | `remediating`              | Remediation initiated       |
| `remediating`         | `remediation-succeeded`    | Remediation completed       |
| `remediating`         | `remediation-failed`       | Remediation error           |
| (any state)           | (no label)                 | Healthy event (cancellation)|

### Example Flows

**Successful remediation:**
```
none → quarantined → draining → drain-succeeded → remediating → remediation-succeeded → none
```

**Failed drain:**
```
none → quarantined → draining → drain-failed [TERMINAL]
```

**Canceled drain (healthy event):**
```
none → quarantined → draining → none
```

**Failed remediation:**
```
none → quarantined → draining → drain-succeeded → remediating → remediation-failed [TERMINAL]
```
