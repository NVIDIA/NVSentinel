# ADR-003: Behavior — Rule-Based Node Quarantine

## Context

When hardware failures are detected, the system needs to prevent new workloads from being scheduled on affected nodes. Different failure types require different responses - some require immediate isolation (fatal GPU errors), others are warnings (thermal throttling).

The challenge is making the quarantine logic configurable without code changes. Operators should be able to define policies like:
- "Cordon and taint nodes with XID 64 errors"
- "Only taint (don't cordon) for thermal warnings"
- "Apply NoSchedule taint for memory errors, PreferNoSchedule for degraded performance"

Options considered:
1. Hardcoded decision logic in code
2. Webhook-based external policy engine
3. Configuration-driven rule evaluation using CEL expressions

## Decision

Implement a configuration-driven Fault Quarantine Module that evaluates CEL (Common Expression Language) rules against health events to determine node tainting and cordoning actions. Rules are organized into rule sets that define matching conditions and resulting actions.

## Implementation

- Configuration is defined in TOML format and loaded from Kubernetes ConfigMaps
- Rules use CEL expressions for flexible matching logic
- Rule sets contain:
  - Match conditions (any/all semantics)
  - Taint specifications (key, value, effect)
  - Cordon behavior (true/false)
  - Priority (for handling multiple matching rule sets)

Architecture:
```
Health Event (from storage)
    |
    v
Reconciler (watches for events)
    |
    v
Rule Set Evaluator
    |
    +-- Any Evaluator (OR logic)
    |       |
    |       +-- Rule 1 (CEL expression)
    |       +-- Rule 2 (CEL expression)
    |
    +-- All Evaluator (AND logic)
            |
            +-- Rule 3 (CEL expression)
    |
    v
Action: Apply taint + cordon (if matched)
```

Example configuration:
```toml
[[rule-sets]]
  name = "Fatal GPU Memory Error"
  priority = 100
  
  [[rule-sets.match.any]]
    kind = "HealthEvent"
    expression = 'componentClass == "GPU" AND errorCode == "64" AND isFatal == true'
  
  [rule-sets.taint]
    key = "gpu.health/memory-error"
    value = "true"
    effect = "NoSchedule"
  
  [rule-sets.cordon]
    shouldCordon = true

[[rule-sets]]
  name = "GPU Thermal Warning"
  priority = 50
  
  [[rule-sets.match.all]]
    kind = "HealthEvent"
    expression = 'componentClass == "GPU" AND checkName == "ThermalWatch"'
  
  [rule-sets.taint]
    key = "gpu.health/thermal-warning"
    value = "true"
    effect = "PreferNoSchedule"
  
  [rule-sets.cordon]
    shouldCordon = false
```

The Reconciler:
- Watches MongoDB Change Streams for new health events
- Evaluates all rule sets against the event
- Applies taints/cordons to affected nodes via Kubernetes API
- Removes taints/cordons when healthy events arrive

## Rationale

- **Flexibility**: Operators can define custom policies without code changes
- **Expressive**: CEL provides rich expression capabilities (field comparisons, boolean logic, string matching)
- **Safe**: CEL expressions are sandboxed and cannot execute arbitrary code
- **Maintainable**: Rules are declarative and easy to understand
- **Versionable**: Configuration can be tracked in git and reviewed before applying

## Consequences

### Positive
- New fault types can be handled by updating configuration, not code
- Different clusters can have different policies (dev vs production)
- Policy changes don't require redeploying the controller
- CEL is a Kubernetes standard (used in ValidatingAdmissionPolicy)
- Rules can be tested independently by evaluating CEL expressions
- Priority system handles overlapping rule sets

### Negative
- Operators must learn CEL syntax
- Complex rules can be hard to debug
- Rule evaluation adds latency vs hardcoded logic
- Misconfigured rules could fail to quarantine faulty nodes
- No UI for rule management (configuration is text-based)

### Mitigations
- Provide rule templates for common failure scenarios
- Include a validation webhook to check rule syntax on ConfigMap updates
- Log all rule evaluations with results for debugging
- Implement dry-run mode to test rules without applying actions
- Provide Grafana dashboards showing which rules are triggering
- Default to safe behavior (cordon + taint) if rule evaluation fails

## Alternatives Considered

### Hardcoded Decision Logic
**Rejected** because: Every new failure type would require code changes, testing, and redeployment. This creates operational burden and slows response to new hardware issues. Different customers may have different risk tolerances that can't be accommodated with fixed logic.

### Webhook-Based External Policy Engine
**Rejected** because: Adds complexity with an external service dependency. Introduces network latency for every health event evaluation. Requires additional operational overhead (deploying policy engine, managing its availability). Harder to debug when policy decisions don't match expectations.

### Scripted Plugins (Lua/Python)
**Rejected** because: Scripts are harder to sandbox safely than CEL. Scripting languages have performance overhead. Operators would need to learn a full programming language vs simple expressions. Error handling and testing are more complex for full scripts.

## Notes

- The system should include default rules that cover common failure scenarios
- Rules should be immutable once applied - use new ConfigMap versions for changes
- Consider rate limiting taint operations to prevent API server overload
- Un-tainting should be cautious - verify the component is truly healthy
- Multiple rule sets can match - highest priority wins for conflicting actions

## References

- CEL Specification: https://github.com/google/cel-spec
