# ADR-009: Behavior — Fault Remediation Triggering

## Context

After detecting, quarantining, and draining a faulty node, the system must trigger external remediation actions through a break/fix service (janitor). The remediation actions include creating Kubernetes Custom Resources like `RebootNode` or `TerminateNode` that janitor controllers watch and execute.

The Fault Remediation Module needs to:
- Watch MongoDB for health events that have been drained
- Create appropriate maintenance CRs based on the recommended action
- Track CR creation and status to prevent duplicates
- Support different remediation workflows (reboot, terminate, reset, etc.)

## Decision

Use a Go template-based system where the Fault Remediation Module watches MongoDB for drained health events, renders a maintenance CR from a template based on the recommended action, and creates it in the cluster. The template approach allows flexibility in CR schema without code changes.

## Implementation

The Fault Remediation Module watches MongoDB Change Streams and:

1. **Filters events** that are ready for remediation:
   - Health event has `isFatal == true`
   - Node has been drained (user pods evicted)
   - Remediation not already triggered for this event

2. **Checks for existing CRs**:
   - Queries for existing maintenance CRs for the same node
   - Checks status of existing CRs
   - Prevents duplicate remediation attempts

3. **Renders CR from template**:
   - Loads YAML template from ConfigMap mount
   - Executes Go template with event data
   - Template variables include: NodeName, RecommendedAction, HealthEventID

4. **Creates CR dynamically**:
   - Parses rendered YAML to unstructured object
   - Resolves GVK to GVR using RESTMapper
   - Creates resource via dynamic client
   - Adds owner reference to link CR to health event

Template example:

```yaml
{{ if eq .RecommendedAction "NODE_REBOOT" }}
apiVersion: janitor.dgxc.nvidia.com/v1alpha1
kind: RebootNode
metadata:
  generateName: {{ .NodeName }}-
  namespace: {{ .Namespace }}
spec:
  nodeName: {{ .NodeName }}
  force: false
{{ else if eq .RecommendedAction "RESTART_VM" }}
apiVersion: janitor.dgxc.nvidia.com/v1alpha1
kind: TerminateNode
metadata:
  generateName: {{ .NodeName }}-
  namespace: {{ .Namespace }}
spec:
  nodeName: {{ .NodeName }}
  force: false
{{ end }}
```

Template variables:
```go
type TemplateData struct {
    NodeName          string
    Namespace         string
    Version           string
    ApiGroup          string
    TemplateMountPath string
    TemplateFileName  string
    HealthEventID     string
    RecommendedAction protos.RecommendedAction
}
```

CR Status Checking:
- Factory pattern for different CR status checkers
- Each recommended action maps to a status checker
- Checks if CR is complete/failed/in-progress
- Uses informers to watch CRs without constant API polling

Configuration:
```toml
[fault-remediation-module]
namespace = "nvsentinel-system"
apiGroup = "janitor.dgxc.nvidia.com"
version = "v1alpha1"
templateMountPath = "/etc/templates"
templateFileName = "maintenance-template.yaml"
dryRun = false
updateMaxRetries = 3
updateRetryDelaySeconds = 5
enableLogCollector = false
```

## Rationale

- **Flexible**: Template system allows CR schema changes without recompilation
- **Type-safe**: Go templates with struct-based context
- **Dynamic**: Uses Kubernetes dynamic client to create any CR type
- **Prevents duplicates**: Checks existing CRs before creation
- **Observable**: Node labels show remediation state
- **Retryable**: Built-in retry logic with backoff

## Consequences

### Positive
- Template changes don't require code redeployment
- Works with any maintenance CR schema
- Easy to add new remediation types
- Status tracking prevents duplicate operations
- Node labels provide clear state visibility
- Dynamic client handles any GVK

### Negative
- Template syntax requires learning Go templates
- Runtime template errors (not caught at compile time)
- Requires dynamic client instead of typed client
- Template must be mounted via ConfigMap
- RESTMapper lookups add slight latency

### Mitigations
- Validate templates at startup and fail fast
- Log template rendering for debugging
- Provide tested default templates
- Include dry-run mode for testing templates
- Use factory pattern to cache RESTMapper results

## Alternatives Considered

### Hardcoded CR Types
**Rejected** because: Would require code changes for each new CR type. Doesn't support different janitor implementations across clusters. Makes the module tightly coupled to specific CR schemas.

### Webhook Mode
Considered but not primary approach: Could POST to an HTTP endpoint instead of creating CRs. Rejected as default because Kubernetes-native CRs provide better observability, auditability, and integration with kubectl/RBAC.

### Multiple Typed Clients
**Rejected** because: Would need separate typed client for each janitor implementation. Dynamic client provides flexibility without code changes. Typed clients would require code generation and compilation.

### Direct CSP API Calls
**Rejected** because: Would require credentials in fault remediation module. Breaks separation of concerns (remediation module just triggers, janitor executes). Makes multi-cloud support harder.

## Notes

- Template is loaded once at startup
- Dynamic client uses server-side apply for idempotency
- Owner references link CRs to health events
- Dry-run mode skips CR creation for testing
- RESTMapper cache reduces API server load
- Factory pattern allows easy addition of new status checkers

## References

- Go Templates: https://pkg.go.dev/text/template
- Kubernetes Dynamic Client: https://pkg.go.dev/k8s.io/client-go/dynamic
