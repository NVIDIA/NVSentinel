# ADR-036: Data Model — Custom Remediation Actions

## Context

NVSentinel's `RecommendedAction` is a protobuf enum with a fixed set of values (`COMPONENT_RESET`, `RESTART_VM`, `RESTART_BM`, `REPLACE_VM`, etc.). The fault-remediation module uses this enum's string representation as the key to look up which maintenance CR to create.

This works for built-in GPU-centric workflows, but breaks down when operators want to extend NVSentinel to custom component classes (disks, NICs, DPUs, custom accelerators) or define organization-specific remediation playbooks. The enum is a closed set—adding a new action today requires a proto change, code regeneration, and a new release.

The fault-remediation module's *output* side is already generic: it uses Go templates and the dynamic Kubernetes client to create arbitrary CRs, keyed by a string in Helm config. The bottleneck is the *input* side: the health event can only express actions that exist in the enum.

## Decision

Add a `CUSTOM` value to the `RecommendedAction` enum and a `string customRecommendedAction` field to the `HealthEvent` message. When `recommendedAction` is `CUSTOM`, consumers read the string field for the action name. Built-in actions continue using the enum directly.

## Implementation

### Proto Change

`data-models/protobufs/health_event.proto`:

```protobuf
enum RecommendedAction {
  NONE = 0;
  COMPONENT_RESET = 2;
  CONTACT_SUPPORT = 5;
  RUN_FIELDDIAG = 6;
  RESTART_VM = 15;
  RESTART_BM = 24;
  REPLACE_VM = 25;
  RUN_DCGMEUD = 26;
  CUSTOM = 27;
  UNKNOWN = 99;
}

message HealthEvent {
  // ... existing fields 1-17 ...
  string customRecommendedAction = 18;  // Only read when recommendedAction == CUSTOM
}
```

The `RecommendedAction` enum gains a single new value. The `customRecommendedAction` string field is added as a standalone field (not a oneof) to preserve compatibility with BSON/JSON serialization used by the MongoDB pipeline.

### Resolution Helper

Add a shared helper in `data-models/pkg/model/`:

```go
func GetEffectiveActionName(he *protos.HealthEvent) string {
    if he.RecommendedAction == protos.RecommendedAction_CUSTOM {
        return he.CustomRecommendedAction
    }
    return he.RecommendedAction.String()
}
```

Validation of `CUSTOM` + empty string is handled at the Platform Connector gRPC boundary (see Validation section), so this helper does not need to duplicate that check.

### Fault-Remediation Code Changes

Two call sites switch from `healthEvent.RecommendedAction.String()` to `GetEffectiveActionName(healthEvent)`:

1. **`fault-remediation/pkg/common/equivalence_groups.go`** — `GetGroupConfigForEvent` action lookup.
2. **`fault-remediation/pkg/remediation/remediation.go`** — `CreateMaintenanceResource` action routing.

`fault-remediation/pkg/reconciler/reconciler.go` — `shouldSkipEvent` checks `action == protos.RecommendedAction_NONE`, which remains correct since `CUSTOM` is a distinct enum value. No change needed here.

### Validation

#### `fault-remediation/pkg/config/config.go`

`validateResourceImpactedEntityScope` currently hardcodes `protos.RecommendedAction_COMPONENT_RESET.String()` as the only action allowed to have an `ImpactedEntityScope`:

```go
if actionName != protos.RecommendedAction_COMPONENT_RESET.String() {
    return fmt.Errorf("action '%s' cannot have an ImpactedEntityScope defined", actionName)
}
```

Update this to also allow custom actions (action names that don't match any built-in enum value) to declare an `ImpactedEntityScope`, provided the entity type supports partial draining (the existing `EntityTypeToResourceNames` check handles this):

```go
_, isBuiltinAction := protos.RecommendedAction_value[actionName]
if isBuiltinAction && actionName != protos.RecommendedAction_COMPONENT_RESET.String() {
    return fmt.Errorf(
        "built-in action '%s' cannot have an ImpactedEntityScope; "+
            "only COMPONENT_RESET and custom actions support this", actionName)
}
```

This lets custom actions like `"REPLACE_DISK"` use `ImpactedEntityScope` while keeping the restriction for built-in actions where it doesn't make sense (e.g., `RESTART_BM` always targets the whole node).

#### Platform Connector

In the gRPC handler (`platform-connectors/pkg/server/platform_connector_server.go`), reject `CUSTOM` events with an empty `customRecommendedAction` at the gRPC boundary. This enforces correctness at ingress — the health monitor gets an immediate error rather than the event silently becoming a no-op downstream:

```go
for _, event := range he.Events {
    if event.RecommendedAction == protos.RecommendedAction_CUSTOM &&
        event.CustomRecommendedAction == "" {
        return nil, status.Errorf(codes.InvalidArgument,
            "recommendedAction is CUSTOM but customRecommendedAction is empty (node=%s, agent=%s)",
            event.NodeName, event.Agent)
    }
}
```

This differs from the `ProcessingStrategy_UNSPECIFIED` normalization pattern (ADR-025) intentionally: an unspecified strategy has a safe default (`EXECUTE_REMEDIATION`), but a `CUSTOM` action with no name has no safe fallback — silently storing it would hide a configuration error in the health monitor.

### Operator Usage

A custom health monitor emitting a disk remediation event:

```go
event := &protos.HealthEvent{
    Agent:                   "disk-health-monitor",
    ComponentClass:          "Disk",
    RecommendedAction:       protos.RecommendedAction_CUSTOM,
    CustomRecommendedAction: "REPLACE_DISK",
    // ...
}
```

Operator adds to Helm values:

```yaml
maintenance:
  actions:
    REPLACE_DISK:
      apiGroup: "storage.example.com"
      version: "v1"
      kind: "ReplaceDisk"
      scope: "Cluster"
      completeConditionType: "DiskReplaced"
      templateFileName: "replace-disk.yaml"
      equivalenceGroup: "disk-replace"
```

Once implemented, adding new custom actions requires no further code changes—only Helm configuration.

### Event Exporter

`event-exporter/pkg/transformer/cloudevents.go` currently sets `"recommendedAction": event.RecommendedAction.String()` in exported CloudEvents. For `CUSTOM` events this would emit `"CUSTOM"` as the action name, which is not useful for external monitoring systems. Update `ToCloudEvent` to include `customRecommendedAction` when set:

```go
healthEventData["recommendedAction"] = event.RecommendedAction.String()
if event.CustomRecommendedAction != "" {
    healthEventData["customRecommendedAction"] = event.CustomRecommendedAction
}
```

### Health Event Overrides

The existing CEL-based override system (ADR-021) currently supports overriding `isFatal`, `isHealthy`, and `recommendedAction`. To enable rerouting built-in events to custom actions, the override `Override` struct in `platform-connectors/pkg/overrides/` must also accept a `customRecommendedAction` string field. When an override rule sets `recommendedAction: CUSTOM`, `customRecommendedAction` must be non-empty — validated at startup alongside CEL expression compilation. This is an additive change to ADR-021's implementation scope.

## Rationale

- **Backward compatible**: Existing events and consumers are unaffected; `CUSTOM` is purely additive
- **Minimal code change**: Two call-site swaps plus one helper function in fault-remediation
- **Serialization safe**: Both fields have standard `json`/`protobuf` tags, so BSON and JSON serialization through the MongoDB pipeline works without modification
- **Type-safe for built-in actions**: The enum retains compile-time safety for known actions
- **Open-ended for custom actions**: Operators can define arbitrary actions through config
- **Clear contract**: The enum value `CUSTOM` is an unambiguous signal to read the string field—no guessing which field to consult

## Consequences

### Positive
- Custom health monitors can specify any remediation action without proto changes
- NVSentinel becomes extensible to non-GPU component classes (disks, NICs, etc.)
- Leverages the existing template-based CR creation system with zero changes to the output path
- Operators can add new actions entirely through Helm configuration
- Works with existing MongoDB BSON serialization and CEL evaluation without infrastructure changes

### Negative
- Two fields encode the same concept (enum + string) for custom actions
- Consumers must use the resolution helper consistently; direct `.RecommendedAction.String()` calls will return `"CUSTOM"` instead of the actual action name

### Mitigations
- Platform Connector rejects invalid `CUSTOM` events (empty string) at the gRPC boundary with `InvalidArgument`, so health monitors get immediate feedback
- Provide `GetEffectiveActionName` in a shared package so all consumers have a single correct path
- Add a code review guideline to flag direct `.RecommendedAction.String()` usage in remediation paths

## Alternatives Considered

### Protobuf `oneof` wrapping `recommendedAction` and `customRecommendedAction`

```protobuf
oneof action {
  RecommendedAction recommendedAction = 8;
  string customRecommendedAction = 18;
}
```

Moving an existing field into a new `oneof` is [wire-safe in proto3](https://protobuf.dev/programming-guides/proto3/#updating), and provides cleaner type-system enforcement (exactly one variant is set). However, this approach was **rejected** for two reasons:

1. **MongoDB/BSON serialization incompatibility**: NVSentinel stores health events in MongoDB via BSON marshal/unmarshal of proto structs. The `oneof` interface field (`Action isHealthEvent_Action`) has no `json` or `bson` struct tag, so BSON serialization silently drops the field. This breaks the entire MongoDB pipeline — health events written to the store would lose their `recommendedAction`, and events read back would have a nil oneof. Fixing this would require custom BSON codecs or switching to `protojson` for all MongoDB operations — a large infrastructure change.

2. **Large blast radius**: The `oneof` changes the generated Go field access pattern. Every direct `he.RecommendedAction` access across ~65 files must change to `he.GetRecommendedAction()`, and every struct literal setting the field must use the oneof wrapper type (`Action: &protos.HealthEvent_RecommendedAction{RecommendedAction: ...}`). While the typed Go migration is mechanical (compiler catches missed usages), CEL override rules, dynamic Kubernetes client access, and the fault-quarantine CEL evaluation engine all use reflection/string-based field access that the compiler cannot catch — these require targeted integration test validation. The CEL evaluator's `structToInterface` function also needed patching to handle `protobuf_oneof` tags, which are skipped by the existing JSON-tag-based serialization.

### Replace enum with string field
**Rejected** because: Changing a proto field's type on the same field number is a wire-breaking change. Adding a new string field and deprecating the enum would create the same two-field duplication but without the clear discriminator signal. Built-in actions would lose compile-time type safety.

### CEL-based action routing in fault-remediation config
**Rejected** because: Requires operators to write and maintain CEL expressions that enumerate error conditions (e.g., XIDs). Doesn't scale when new error codes are constantly added—the health monitor already knows the right action, it just needs an open-ended way to express it.

### Extend the enum for every new action
**Rejected** because: Every new action requires a proto change, code regeneration, and release. Custom/organization-specific actions can never be added this way without forking the proto.

## Notes

- The `CUSTOM` enum value uses `27`, the next sequential value after `RUN_DCGMEUD = 26`
- RBAC for custom CRD kinds must be configured separately by the operator; the Helm chart's auto-generated RBAC only covers built-in kinds
- This change pairs well with ADR-021 (Health Event Property Overrides) for rerouting built-in events to custom actions

## References

- [GitHub Issue #1141](https://github.com/NVIDIA/NVSentinel/issues/1141) — Support for custom remediation actions
- [GitHub Issue #182](https://github.com/NVIDIA/NVSentinel/issues/182) — Support for arbitrary 3rd party CRD remediation actions
- [Proto3 Updating A Message Type](https://protobuf.dev/programming-guides/proto3/#updating) — Wire-safety of oneof changes (considered but rejected)
- [ADR-009: Fault Remediation Triggering](./009-fault-remediation-triggering.md)
- [ADR-021: Health Event Property Overrides](./021-health-event-property-overrides.md)
