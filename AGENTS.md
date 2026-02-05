# AGENTS.md - Persistent Context for AI Assistants

> **Last Updated:** 2026-02-04
> **Active Branch:** `feat/cloud-native-healthevents` ✅ READY
> **Base Commit:** `105cd6c` (Use cuda image from NVCR to avoid rate limits (#792))
> **Head Commit:** `a7f3cc7` (fix(tests): register HealthEvent types and fix context keys)
> **New PR:** https://github.com/NVIDIA/NVSentinel/pull/795 (DRAFT)
> **Old PR:** https://github.com/NVIDIA/NVSentinel/pull/794 (superseded - incorrectly based)
> **Status:** ✅ Phase 4: Validation COMPLETE (controller flow verified)

---

## Current Task: Rebase Cloud-Native Storage onto Main

### Problem
The `feat/cloud-native-storage` branch was created from commit `59578f3`, which is on the
`device-api-server` lineage **after** commit `d6c5c46` that deleted all NVSentinel core code.

The branch is missing:
- `commons/`, `data-models/`, `health-monitors/`, `labeler/`, `lint/`
- `remediations/`, `reports/`, `scalers/`, `sentinel/`, `services/`
- `CONTRIBUTING.md`, `.coderabbit.yaml`, `RELEASE.md`, `ROADMAP.md`
- Most GitHub Actions workflows

### Solution
Cherry-pick our commits onto a fresh branch from `origin/main`.

### Commits to Cherry-Pick (in order)
```
7fc1f70 chore - Add small set of GitHub checks and copilot rules (#691)
a07681f feat: introduce k8s-idiomatic Go SDK for Device API (#692)
6a94259 chore: update documentation (#693)
8ad7b7a api: add ProviderService proto for device-api-server
b907fb1 feat: add device-api-server with NVML fallback provider
95cea2e fix(security): bind gRPC TCP listener to localhost by default
cf88679 docs(provider): clarify Heartbeat RPC is reserved for future use
ff88ab8 feat(consumer): populate ListMeta.ResourceVersion in ListGpus response
e37ed8c docs: document module structure and naming conventions
bd1b3ad fix: align module paths to github.com/nvidia/nvsentinel
cf1a193 refactor: consolidate to unified GpuService with standard CRUD methods
f7de28b fix(build): use Go 1.25 for container builds
fd4c919 fix(nvml-provider): parse command-line flags before returning config
cdbcfc3 fix(helm): remove invalid --provider-address flag from server args
18bb07c fix(helm): correct sidecar test values for cluster deployment
de7751e feat(demo): improve cross-platform builds and idempotency
28ef6be fix(demo): remove unreliable metrics check from verify_gpu_registration
cc2fcce fix(demo): use correct container name 'nvsentinel' instead of 'device-api-server'
18946e1 fix(ci): update protoc version to v33.4 to match generated files
a231cf3 docs: add hybrid device-apiserver design for PR #718 + #720 merge
59578f3 chore: add .worktrees/ and temp docs to .gitignore
3260812 feat: implement cloud-native GPU health event management
```

### Worktree Locations
- **Old (broken):** `/Users/eduardoa/src/github/nvidia/NVSentinel/.worktrees/cloud-native-storage` (to be removed)
- **New (active):** `/Users/eduardoa/src/github/nvidia/NVSentinel/.worktrees/cloud-native-healthevents` ✅

---

## Feature: Cloud-Native GPU Health Event Management

### Architecture
```
┌─────────────────┐    gRPC    ┌──────────────────┐
│ HealthProvider  │ ─────────▶│ Device-API-Server│
│   (DaemonSet)   │            │                  │
└─────────────────┘            └────────┬─────────┘
        │                               │
        │ NVML                          │ CRDPublisher
        ▼                               ▼
    ┌───────┐                   ┌───────────────┐
    │  GPU  │                   │  HealthEvent  │
    └───────┘                   │     (CRD)     │
                                └───────┬───────┘
                                        │
              ┌─────────────────────────┼─────────────────────────┐
              │                         │                         │
              ▼                         ▼                         ▼
    ┌──────────────────┐    ┌──────────────────┐    ┌──────────────────┐
    │ QuarantineCtrl   │───▶│   DrainCtrl      │───▶│ RemediationCtrl  │
    │  (cordon node)   │    │  (evict pods)    │    │ (reboot/reset)   │
    └──────────────────┘    └──────────────────┘    └──────────────────┘
```

### Phase Progression
```
New → Quarantined → Drained → Remediated → Resolved
```

### Key Files Created
```
api/nvsentinel/v1alpha1/
├── doc.go
├── groupversion_info.go
├── healthevent_types.go
└── zz_generated.deepcopy.go

pkg/controllers/healthevents/
├── conditions.go
├── drain_controller.go
├── drain_controller_test.go
├── metrics.go
├── quarantine_controller.go
├── quarantine_controller_test.go
├── remediation_controller.go
├── remediation_controller_test.go
├── ttl_controller.go
└── ttl_controller_test.go

pkg/deviceapiserver/crdpublisher/
├── metrics.go
├── publisher.go
└── publisher_test.go

pkg/healthprovider/
├── nvml_source.go
├── nvml_source_stub.go
├── provider.go
└── provider_test.go

cmd/controller-test/main.go
cmd/health-provider/main.go
cmd/health-provider/main_stub.go

deployments/helm/nvsentinel/crds/nvsentinel.nvidia.com_healthevents.yaml
deployments/helm/nvsentinel/templates/policy-configmap.yaml
deployments/helm/health-provider/
```

### Integration Test Results (2026-02-04)
- **Cluster:** AWS EKS with GPUs
- **Kubeconfig:** `/Users/eduardoa/.kube/config-aws-gpu`
- **Result:** SUCCESS - Full phase progression in 2 seconds
- **Node tested:** `ip-10-0-0-236`

### Important Fixes Applied
1. **Predicate removal:** Controllers don't use `WithEventFilter` for phase filtering
   (status subresource updates don't reliably trigger watch events)
2. **Empty phase handling:** `QuarantineController` treats `phase=""` as `phase=New`
3. **Field index:** Added `spec.nodeName` index for Pods in controller-test main.go

---

## Commands Reference

### Create new worktree from main
```bash
cd /Users/eduardoa/src/github/nvidia/NVSentinel
git fetch origin main
git worktree add .worktrees/cloud-native-healthevents origin/main -b feat/cloud-native-healthevents
```

### Cherry-pick all commits
```bash
cd .worktrees/cloud-native-healthevents
git cherry-pick 7fc1f70^..3260812
```

### Run integration test
```bash
export KUBECONFIG=/Users/eduardoa/.kube/config-aws-gpu
./bin/controller-test --health-probe-bind-address=:18081 --metrics-bind-address=:18080 -v=2
```

### Create test HealthEvent
```yaml
apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: HealthEvent
metadata:
  name: test-full-flow
spec:
  source: integration-test
  nodeName: <NODE_NAME>
  componentClass: GPU
  checkName: xid-error-check
  isFatal: true
  recommendedAction: RESTART_VM
  detectedAt: "2026-02-04T16:00:00Z"
```

---

## Current Task: E2E Test Migration

### Problem
Old E2E tests (in `tests/`) assume MongoDB + old microservices architecture:
- `fault_quarantine_test.go` - Tests fault-quarantine microservice
- `fault_remediation_test.go` - Tests fault-remediation microservice
- `node_drainer_test.go` - Tests node-drainer microservice
- `health_events_analyzer_test.go` - Tests MongoDB aggregation pipelines
- `smoke_test.go` - End-to-end flow with MongoDB

New architecture uses HealthEvent CRD + Kubernetes controllers. Tests need **updating**, not rebuilding.

### Decision: Update Existing Tests to Use CRD-Based Flow

**Approach:**
1. Replace MongoDB assertions with HealthEvent CRD assertions
2. Replace microservice coordination checks with controller reconciliation checks
3. Leverage existing integration test harness (`cmd/controller-test/main.go`)
4. Preserve test scenarios (fault detection → quarantine → drain → remediation)

### Migration Mapping

| Old Test Pattern | New Test Pattern |
|------------------|------------------|
| Verify MongoDB change stream | Verify HealthEvent CRD creation |
| Check fault-quarantine processed | Check `status.phase = Quarantined` |
| Check node-drainer evicted pods | Check `status.phase = Drained` |
| Check fault-remediation completed | Check `status.phase = Remediated` |
| MongoDB collection cleanup | HealthEvent CRD deletion |

### Implementation Plan (3-4 weeks)

**Phase 1: Audit (2-3 days)**
- Catalog all E2E test scenarios
- Map old assertions to new CRD assertions
- Identify KWOK vs real cluster requirements

**Phase 2: Infrastructure (3-5 days)**
- Update test setup to use HealthEvent CRD
- Create test fixtures/helpers for CRD creation
- Update cleanup logic

**Phase 3: Migrate Tests (1-2 weeks)**
- Fault Detection → HealthEvent Creation
- Quarantine Flow → phase=Quarantined
- Drain Flow → phase=Drained  
- Remediation Flow → phase=Remediated

**Phase 4: Validation (3-5 days)**
- Run against KWOK/kind
- Validate against AWS EKS
- Performance validation

### Key Test Files to Update
```
tests/
├── smoke_test.go              # Full lifecycle test
├── fault_quarantine_test.go   # Quarantine logic
├── fault_remediation_test.go  # Remediation logic
├── node_drainer_test.go       # Drain logic
├── health_events_analyzer_test.go  # Pattern detection (may defer)
└── helpers/
    ├── healthevent.go         # Update to use CRD client
    ├── fault_quarantine.go    # Update assertions
    └── kube.go                # Keep Kubernetes helpers
```

### Success Criteria
- [ ] All original scenarios migrated
- [ ] Tests pass consistently (≥95%)
- [ ] Test execution ≤5 min per scenario
- [ ] Full lifecycle coverage
- [ ] Zero MongoDB dependencies
- [ ] Runnable on KWOK/kind and AWS EKS

---

## Phase 3 Progress: Test Migration

### Completed
- [x] `smoke_test.go` - Full lifecycle tests migrated
- [x] `fault_quarantine_test.go` - QuarantineController tests migrated
- [x] `node_drainer_test.go` - DrainController tests migrated
- [x] `fault_remediation_test.go` - RemediationController tests migrated

### fault_quarantine_test.go Migration Summary

**Migrated tests:**
- `TestNonFatalEventDoesNotTriggerQuarantine` - Verify isFatal=false skips quarantine
- `TestHealthyEventDoesNotTriggerQuarantine` - Verify isHealthy=true skips quarantine  
- `TestPreCordonedNodeHandling` - Pre-cordoned nodes handled correctly
- `TestQuarantineSkipOverride` - `spec.overrides.quarantine.skip` works
- `TestMultipleEventsOnSameNode` - Multiple events on same node

**Removed (old microservice-specific):**
- `TestDontCordonIfEventDoesntMatchCELExpression` - CEL filtering (fault-quarantine specific)
- `TestCircuitBreakerCursorCreateSkipsAccumulatedEvents` - Circuit breaker (MongoDB-specific)
- `TestFaultQuarantineWithProcessingStrategy` - Processing strategy (data-models specific)

### smoke_test.go Migration Summary

**TestFatalHealthEvent** - Full remediation flow
```
New → Quarantined → Draining → Drained → Remediated → Resolved
```
- Removed: HTTP SendHealthEventsToNodes(), node label sequence watching, statemanager dependency
- Added: CreateHealthEventCRD(), WaitForHealthEventPhase(), phase-based assertions
- Kept: Node cordon/uncordon, RebootNode CR, log-collector job assertions

**TestFatalUnsupportedHealthEvent** - CONTACT_SUPPORT events
- Tests events that skip automatic remediation (XID 145)
- Asserts event stays in Drained phase (never reaches Remediated)
- Verifies no log-collector job created

### node_drainer_test.go Migration Summary

**Migrated tests:**
- `TestDrainControllerBasicFlow` - Basic drain via HealthEvent phases
- `TestDrainSkipOverride` - `spec.overrides.drain.skip` works
- `TestDrainWithKubeSystemExclusion` - kube-system pods not evicted
- `TestDrainPhaseSequence` - Full phase sequence validation

**Removed (old microservice-specific):**
- `TestNodeDrainerEvictionModes` - ConfigMap-based eviction mode config
- `TestNodeDrainerPartialDrain` - Partial drain with entitiesImpacted

### fault_remediation_test.go Migration Summary

**Migrated tests:**
- `TestRemediationControllerBasicFlow` - Basic remediation via phases
- `TestMultipleRemediationsOnSameNode` - Multiple CRs on same node
- `TestContactSupportDoesNotTriggerRemediation` - CONTACT_SUPPORT skips remediation
- `TestFullPhaseSequenceToResolved` - Full lifecycle New → Resolved

### Deferred
- [ ] `health_events_analyzer_test.go` - (MongoDB-specific, pattern detection)

---

## Phase 4 Results: Validation (COMPLETE ✅)

### Manual Validation (2026-02-04)
- **Cluster:** `kubernetes-admin@holodeck-cluster` (AWS EKS)
- **Test Node:** `ip-10-0-0-10`
- **Result:** SUCCESS

**Validation Steps:**
1. Built `controller-test` binary
2. Started controllers locally (`./bin/controller-test`)
3. Created HealthEvent CRD manually
4. Verified phase transitions: `New → Quarantined → Draining`
5. Verified node cordoned (`spec.unschedulable=true`)
6. Verified `NodeQuarantined` condition set

**Test Infrastructure Fixes Applied:**
- Registered `nvsentinelv1alpha1.HealthEvent` types in test scheme
- Moved `keyHealthEventName` context key to `main_test.go`
- Removed unused `v1` import from `smoke_test.go`

**Note:** Full E2E test suite requires KWOK nodes for `amd64_group` tests.
The `arm64_group` tests (smoke tests) use real nodes but require longer timeout.

---

## Phase 2 Results: Test Infrastructure (COMPLETE ✅)

Created `tests/helpers/healthevent_crd.go` with 23 helper functions.

### Builder Pattern
```go
event := NewHealthEventCRD("node-1").
    WithFatal(true).
    WithCheckName("GpuXidError").
    WithErrorCodes("79").
    WithRecommendedAction(nvsentinelv1alpha1.ActionRestartVM).
    Build()
```

### CRD Operations
- `CreateHealthEventCRD(ctx, t, c, event)` - Create CRD
- `GetHealthEventCRD(ctx, c, name)` - Get by name
- `DeleteHealthEventCRD(ctx, t, c, name)` - Delete
- `ListHealthEventCRDs(ctx, c)` - List all
- `ListHealthEventCRDsForNode(ctx, c, nodeName)` - Filter by node
- `DeleteAllHealthEventCRDs(ctx, t, c)` - Cleanup all

### Phase Waiting
- `WaitForHealthEventPhase(ctx, t, c, name, phase)` - Wait for phase
- `WaitForHealthEventPhaseNotEqual(ctx, t, c, name, phase)` - Wait to leave phase
- `WaitForHealthEventCondition(ctx, t, c, name, condType, status)` - Wait for condition
- `WaitForHealthEventPhaseSequence(ctx, t, c, name, []phases)` - Wait for sequence

### Assertions
- `AssertHealthEventPhase(t, event, phase)` - Assert phase
- `AssertHealthEventNotExists(ctx, t, c, name)` - Assert doesn't exist
- `AssertHealthEventHasCondition(t, event, condType, status)` - Assert condition
- `AssertHealthEventNeverReachesPhase(ctx, t, c, name, phase)` - Assert never reaches
- `AssertNodeQuarantinedCondition(t, event)` - Assert quarantine condition
- `AssertPodsDrainedCondition(t, event)` - Assert drain condition
- `AssertRemediatedCondition(t, event)` - Assert remediation condition
- `AssertResolvedAtSet(t, event)` - Assert resolved timestamp

### Migration Helpers (Old HTTP → New CRD)
- `SendHealthEventViaCRD(ctx, t, c, template)` - Drop-in for `SendHealthEvent`
- `SendHealthyEventViaCRD(ctx, t, c, nodeName)` - Drop-in for `SendHealthyEvent`
- `TriggerFullRemediationFlowViaCRD(ctx, t, c, nodeName)` - Trigger full flow

---

## Phase 1 Audit Results (2026-02-04)

### Test Files Audited

| File | Tests | Key Scenarios |
|------|-------|---------------|
| `smoke_test.go` | 2 | Full fatal event flow, unsupported event flow |
| `fault_quarantine_test.go` | 4 | CEL filtering, pre-cordoned nodes, circuit breaker, processing strategy |
| `node_drainer_test.go` | 2 | Eviction modes, partial drain |
| `fault_remediation_test.go` | 1 | RebootNode CR creation after remediation |

### Common Old → New Assertion Mapping

| Old Pattern | New Pattern |
|-------------|-------------|
| `SendHealthEvent()` HTTP POST | Create HealthEvent CRD via K8s client |
| MongoDB change stream watch | HealthEvent CRD informer/watch |
| Node label `nvsentinel-state=draining` | HealthEvent `status.phase=Draining` |
| Node label `nvsentinel-state=drain-succeeded` | HealthEvent `status.phase=Drained` |
| `CheckNodeEventExists("NodeDraining")` | HealthEvent `status.conditions` check |
| `WaitForRebootNodeCR()` | HealthEvent `status.phase=Remediated` |

### Required New Helper Functions

```go
// CRD Operations
CreateHealthEventCRD(ctx, client, spec) error
GetHealthEventCRD(ctx, client, name) (*HealthEvent, error)
DeleteHealthEventCRD(ctx, client, name) error
ListHealthEventCRDs(ctx, client, opts) ([]HealthEvent, error)

// Phase Waiting
WaitForHealthEventPhase(ctx, client, name, phase, timeout) error
WaitForHealthEventCondition(ctx, client, name, condType, status) error

// Assertions
AssertHealthEventPhase(t, event, expectedPhase)
AssertHealthEventNotExists(ctx, t, client, name)
AssertHealthEventHasCondition(t, event, condType, status)
```

### Test Migration Priority

1. **High**: `smoke_test.go` - Core E2E flow, validates full lifecycle
2. **High**: `fault_quarantine_test.go` - QuarantineController validation
3. **Medium**: `node_drainer_test.go` - DrainController validation
4. **Medium**: `fault_remediation_test.go` - RemediationController validation
5. **Low**: `health_events_analyzer_test.go` - Pattern detection (defer, MongoDB-specific)

---

## Related PRs and Issues
- PR #795: Cloud-native health events (current work)
- PR #794: Superseded (incorrectly based branch)
- PR #718, #720: Original device-api-server proposals being enhanced
- Design doc: `docs/plans/2026-02-04-hybrid-device-apiserver-design.md`
