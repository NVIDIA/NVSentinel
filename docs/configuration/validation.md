# Validation Configuration

## Overview

The purpose of the validation-controller is to reconcile ValidationRequests. Clients can create a ValidationRequest to specify that a given set of validation tests should be executed against a given set of nodes. The controller orchestrates running that request across different nodes and tests through a set of supported test providers.

Within NVSentinel, there are 2 clients that can be opted-in to create ValidationRequests.

- **New node validation**: the node-validation-controller can be configured to batch together a group of nodes matching newNodeValidation.criteria over batchPeriodSeconds, then creates a single ValidationRequest targeting every node in the batch with newNodeValidation.newNodeTests.
- **Post-remediation validation**: the fault-quarantine module can be configured to automatically create ValidationRequests when a node has an unquarantine event. The ValidationRequest includes tests from any unhealthy event in the node's quarantine session that required validation, derived from validation.ruleSets.

This document covers all Helm configuration options for system administrators, with a [Configuration Reference](#configuration-reference) provided at the end. See [Validation](../validation.md) for the feature overview.

## Enabling Validation

The validation-controller is enabled by default, but the lifecycle-manager component that hosts it is not. After enabling this component, if the default configuration is not sufficient, overrides can be provided within the lifecycle-manager Helm chart:

```yaml
global:
  lifecycleManager:
    enabled: true

lifecycle-manager:
  config:
    defaultTests:
      - dcgm-diag-test
    readinessCriteria:
      - name: gpu-allocatable
        expression: >-
          (has(node.status.allocatable) && "nvidia.com/gpu" in node.status.allocatable &&
          quantity(node.status.allocatable["nvidia.com/gpu"]) > 0) ||
          resourceSlices.exists(s, s.spec.driver == "gpu.nvidia.com" && has(s.spec.devices) &&
          size(s.spec.devices) > 0)
      - name: not-under-quarantine
        expression: >-
          !(has(node.metadata.annotations) &&
          "quarantineHealthEvent" in node.metadata.annotations)
    maxConcurrentGroups: 3
    templateMountPath: /etc/nvsentinel/templates
    providers:
      k8s-job-provider:
        apiGroup: "batch"
        version: "v1"
        kind: "Job"
        resource: "jobs"
        templateFile: k8s-job-template.yaml
        supportsTestBatching: false
        retries: 5
        timeoutSeconds: 1800
        successfulCondition:
          type: Complete
          status: "True"
        failedCondition:
          type: Failed
          status: "True"
    tests:
      dcgm-diag-test:
        provider: k8s-job-provider
        image: nvcr.io/nvidia/cloud-native/dcgm:4.7.0-ubuntu24.04
        command:
          - sh
          - -c
          - |
            # One DCGM Service exists per cluster: nvidia-dcgm-dra in GPU Operator GPUCluster (DRA) mode,
            # nvidia-dcgm otherwise. Use the first name that resolves.
            for h in nvidia-dcgm-dra.gpu-operator.svc nvidia-dcgm.gpu-operator.svc; do
              getent hosts "$h" >/dev/null 2>&1 && exec dcgmi diag --host "$h:5555" --run 2 --json
            done
            echo "no DCGM hostengine Service resolved" >&2
            exit 1
        supportsBatchingNodes: false
        minimumNodesPerBatch: 1
        batchFailurePolicy: fail
```

This configuration allows a client to create a ValidationRequest that runs dcgm-diag-test as a Kubernetes Job via the k8s-job-provider. Before the test group starts, the targeted node must report allocatable GPU capacity and not be under quarantine, as enforced by readinessCriteria. The gpu-allocatable criterion accepts either source of GPU capacity: the device plugin, which sets `nvidia.com/gpu` in the node allocatable resources, or the `gpu.nvidia.com` DRA driver, which publishes the GPUs in ResourceSlices in GPU Operator GPUCluster mode. If the ValidationRequest does not specify spec.tests, defaultTests provides which tests to run in the request.

## Enabling New Node Validation

The node-validation-controller is also part of the lifecycle-manager. This controller batches together a group of nodes matching particular criteria into a single ValidationRequest. Enabling this functionality requires enabling the validation-controller and specifying newNodeValidation as part of the ValidationConfiguration. This is on by default:

```yaml
lifecycle-manager:
  config:
    newNodeValidation:
      condition: NewNodeValidationRequested
      batchPeriodSeconds: 600
      criteria:
        - name: recently-joined
          expression: >-
            has(node.metadata.creationTimestamp) &&
            now() - timestamp(node.metadata.creationTimestamp) < duration("30m")
        - name: gpu-present
          expression: >-
            has(node.metadata.labels) && "nvidia.com/gpu.present" in node.metadata.labels &&
            node.metadata.labels["nvidia.com/gpu.present"] == "true"
        - name: cordoned
          expression: >-
            has(node.spec.unschedulable) && node.spec.unschedulable
      newNodeTests:
        - dcgm-diag-test
```

This configuration batches together nodes over a 10-minute period where all nodes were created within the past 30 minutes, have the nvidia.com/gpu.present=true label present, and are cordoned. Nodes which already have the NewNodeValidationRequested condition are not considered eligible, to prevent the same node from being included in multiple batches. This results in a ValidationRequest with newNodeTests which targets all nodes in the batch.

## Enabling Post-Remediation Validation

An operator can enable fault-quarantine to automatically create ValidationRequests when a given node has an unquarantine event. This ValidationRequest includes tests from any event in the node's quarantine session which required validation. Tests are derived from validation.ruleSets, which map a HealthEvent to a list of tests included in the ValidationRequest. To enable post-remediation validation, set fault-quarantine.validation.enabled to true.

```yaml
fault-quarantine:
  validation:
    enabled: true
    ruleSets:
      - enabled: true
        version: "1"
        name: "Post-reboot validation"
        match:
          all:
            - kind: "HealthEvent"
              expression: >-
                event.componentClass == 'GPU' && event.isFatal == true &&
                event.recommendedAction in [15, 24]
        tests:
          - dcgm-diag-test
```

The default ruleset creates a ValidationRequest which runs dcgm-diag-test against any unquarantined node that had an unhealthy event requiring a VM or BM restart remediation during its quarantine session.

A ValidationRequest is only created if at least one event from the quarantine session completed a full drain, so validation tests do not run while workloads that node-drainer would evict are still on the node. `fault-quarantine.validation.partialDrainEnabled` tells fault-quarantine whether node-drainer drains COMPONENT_RESET events partially (only the pods using the impacted GPU). It defaults to `false`, the same as `node-drainer.partialDrainEnabled`, so a completed drain of a COMPONENT_RESET event counts as a full drain and can trigger validation; a validation ruleset must also match the event, and the default ruleset does not match COMPONENT_RESET. If partial drain is enabled in node-drainer, set `fault-quarantine.validation.partialDrainEnabled: true` as well (or set `global.partialDrainEnabled: true`, which overrides both), so a partial drain never counts as a full drain. Change this together with node-drainer's setting, after open quarantine sessions finish: fault-quarantine judges each event by the current value, so an event drained partially before the change may count as a full drain after it. See [Partial Drain](node-drainer.md#partial-drain).

## Enabling NVCRE Validation

[NVCRE](https://github.com/NVIDIA/cluster-readiness-engine) support adds new node or post-remediation validation tests from an NVCRE test provider. The default configuration only supports a k8s-job-provider test provider that can run a dcgm-diag-test. The nvcre-provider is an additional test provider which currently supports nccl-loopback-nvswitch and nccl-all-reduce tests. An operator must opt-in to running NVCRE after enabling lifecycle-manager with its existing defaults.

To use the NVCRE test provider:

1. Install NVCRE: <https://github.com/NVIDIA/cluster-readiness-engine>.
2. Enable the lifecycle-manager component. The chart's defaults already enable k8s-job-provider with dcgm-diag-test, as well as new node validation.
3. Enable post-remediation validation in fault-quarantine. The default ruleset already triggers dcgm-diag-test for any remediation that required a reboot.
4. Apply the overrides in values-nvcre.yaml, which overrides a subset of variables from lifecycle-manager/values.yaml and fault-quarantine/values.yaml. It defines the nvcre-provider with the nccl-loopback-nvswitch and nccl-all-reduce tests, overrides post-remediation validation to run dcgm-diag-test and nccl-loopback-nvswitch, and overrides new node validation to run dcgm-diag-test, nccl-loopback-nvswitch, and nccl-all-reduce:

```yaml
lifecycle-manager:
  config:
    providers:
      nvcre-provider:
        apiGroup: "nvcre.nvidia.com"
        version: "v1alpha1"
        kind: "Certification"
        resource: "certifications"
        templateFile: nvcre-certification-template.yaml
        supportsTestBatching: true
        retries: 2
        timeoutSeconds: 5400
        successfulCondition:
          type: Succeeded
          status: "True"
        failedCondition:
          type: Failed
          status: "True"
    tests:
      nccl-loopback-nvswitch:
        provider: nvcre-provider
        supportsBatchingNodes: true
        minimumNodesPerBatch: 1
        batchFailurePolicy: fail
        bandwidthGBps: "580"
      nccl-all-reduce:
        provider: nvcre-provider
        supportsBatchingNodes: true
        minimumNodesPerBatch: 2
        batchFailurePolicy: ignore
        bandwidthGBps: "700"
    newNodeValidation:
      newNodeTests:
        - dcgm-diag-test
        - nccl-loopback-nvswitch
        - nccl-all-reduce

fault-quarantine:
  validation:
    ruleSets:
      - enabled: true
        version: "1"
        name: "Post-reboot validation"
        match:
          all:
            - kind: "HealthEvent"
              expression: >-
                event.componentClass == 'GPU' && event.isFatal == true &&
                event.recommendedAction in [15, 24]
        tests:
          - dcgm-diag-test
          - nccl-loopback-nvswitch
```

The example values-nvcre.yaml is tailored for a GB200 GCP environment. To support a custom CSP or GPU type or different tests, an operator should:

- Confirm NVCRE supports your CSP and GPU matrix.
- Define additional NVCRE communication tests to run, such as nccl-loopback, nccl-all-gather, or nccl-all-to-all. NVCRE also supports Nemotron training tests, which could be leveraged by the nvcre-provider (requiring goodputRatio to be set rather than bandwidthGBps).
- Override schedulingGate.taints, which lists permanent node taints that NVCRE test pods tolerate. Set remove to false on any taint that is not itself a validation gate, so the controller never removes it from the node after the current validation-session completes.
- Override nccl-loopback-nvswitch.bandwidthGBps and nccl-all-reduce.bandwidthGBps. These are the bandwidth thresholds in GB/s, below which NVCRE fails the test. These settings are optional. If not provided, NVCRE reports a successful NCCL test regardless of the observed bandwidth.

### Example NVCRE ValidationRequests

These examples were captured from a live GB200 GCP staging cluster with values-nvcre.yaml applied.

After injecting an XID 95 into DCGM, which triggered a RESTART_VM remediation, fault-quarantine created the following ValidationRequest to run dcgm-diag-test and nccl-loopback-nvswitch against the recovered node:

```yaml
apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: ValidationRequest
metadata:
  name: node-a-validation-9e957162e15e
spec:
  nodes:
    - name: node-a
  tests:
    - dcgm-diag-test
    - nccl-loopback-nvswitch
status:
  phase: Succeeded
  startTime: "2026-09-28T23:32:09Z"
  completionTime: "2026-09-28T23:40:20Z"
  testGroups:
    - name: dcgm-diag-test-group-1
      provider: k8s-job-provider
      phase: Succeeded
      nodes:
        - node-a
      tests:
        - dcgm-diag-test
    - name: nccl-loopback-nvswitch-group-1
      provider: nvcre-provider
      phase: Succeeded
      nodes:
        - node-a
      tests:
        - nccl-loopback-nvswitch
```

After 2 new nodes joined the cluster, the node-validation-controller batched them into a single ValidationRequest that ran dcgm-diag-test, nccl-loopback-nvswitch, and nccl-all-reduce:

```yaml
apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: ValidationRequest
metadata:
  name: validation-1790643391763878321
spec:
  nodes:
    - name: node-b
    - name: node-c
  tests:
    - dcgm-diag-test
    - nccl-loopback-nvswitch
    - nccl-all-reduce
status:
  phase: Succeeded
  startTime: "2026-09-29T00:56:51Z"
  completionTime: "2026-09-29T01:29:39Z"
  testGroups:
    - name: dcgm-diag-test-group-1
      provider: k8s-job-provider
      phase: Succeeded
      nodes:
        - node-b
      tests:
        - dcgm-diag-test
    - name: dcgm-diag-test-group-2
      provider: k8s-job-provider
      phase: Succeeded
      nodes:
        - node-c
      tests:
        - dcgm-diag-test
    - name: nccl-all-reduce-nccl-loopback-nvswitch-group-1
      provider: nvcre-provider
      phase: Succeeded
      nodes:
        - node-b
        - node-c
      tests:
        - nccl-loopback-nvswitch
        - nccl-all-reduce
```

## Configuration Reference

### lifecycle-manager.config

| Key | Type | Purpose |
|---|---|---|
| defaultTests | []string | The default set of tests run against ValidationRequests which do not include any tests |
| readinessCriteria | []CriteriaSpec | A set of CEL expressions which must all evaluate to true before a validation test can be started on a given node. Each entry is evaluated against an environment containing the node being validated (`node`) and the ResourceSlices whose `spec.nodeName` is that node (`resourceSlices`). When a criterion reads `resourceSlices`, the lifecycle-manager watches ResourceSlices so a slice published after the last node update still unblocks a pending request. It derives the drivers to watch from the expressions: slice events are filtered to the `spec.driver` values compared with string literals, as `s.spec.driver == "gpu.nvidia.com"` or `s.spec.driver in [...]`. A criterion that reads `spec.driver` any other way, or reads slices without testing the driver, turns the filter off and every driver's slice events are processed (logged at startup). Filtering only affects which events wake the controller; the `resourceSlices` variable always holds all of the node's slices. If an operator is externally applying a node cordon or taint and would like to block validation until these are applied, they can add these properties to readinessCriteria. If not met, this blocks a node from starting validation and fails validation if the criteria were initially met and then reverted |
| maxConcurrentGroups | int | The maximum number of test groups that may run concurrently. Groups are additionally constrained by node overlap. Two groups that share a node never run at the same time regardless of this setting |
| templateMountPath | string | The directory from which templateFile paths are resolved |
| providers | map[string]ProviderConfig | Test provider settings that apply to all tests using this provider, keyed by the name tests[].provider references (see below) |
| tests | map[string]TestConfig | The set of supported tests that can be requested by clients in ValidationRequests, keyed by test name (see below) |
| newNodeValidation | NewNodeValidationConfig | Groups the configuration for detecting and testing new nodes (see below) |
| schedulingGate | SchedulingGateConfig | Groups the scheduling gate controls the validation-controller manages during the lifecycle of a ValidationRequest (see below) |

### lifecycle-manager.config.providers

| Key | Type | Purpose |
|---|---|---|
| apiGroup, version, kind | string | The GroupVersionKind of the provider CRD. The controller uses this to register a watch for each provider type so that test provider resource events trigger ValidationRequest reconciliation |
| resource | string | The plural resource name of the provider CRD. Only used to generate RBAC rules for the validation-controller via Helm templating |
| templateFile | string | The filename of a Go text/template, resolved relative to templateMountPath, rendered by the controller to construct the test provider CRD for each test group |
| supportsTestBatching | bool | Whether this test provider supports batching multiple tests into a single test provider request |
| retries | int | The number of retries allowed for all tests referencing this test provider |
| timeoutSeconds | int64 | The maximum number of seconds allowed for a single test group attempt using this provider before it is marked as failed |
| successfulCondition | {type, status} | The condition the validation-controller polls to determine whether an attempt was successful |
| failedCondition | {type, status} | The condition the validation-controller polls to determine whether an attempt has failed |

### lifecycle-manager.config.tests

| Key | Type | Purpose |
|---|---|---|
| provider | string | The test provider that runs this test |
| image | string | The container image to use for a given test. Only meaningful for providers that render it, such as k8s-job-provider |
| command | []string | The command to run in the container for a given test. Only meaningful for providers that render it, such as k8s-job-provider |
| env | []EnvVar | A list of environment variables with names and values to set in the provider resource template for a given test |
| supportsBatchingNodes | bool | Whether this test supports testing multiple nodes in a single test |
| minimumNodesPerBatch | int | If supportsBatchingNodes is true, the minimum number of nodes that must be provided to the test. Must be 1 when supportsBatchingNodes is false |
| batchFailurePolicy | fail \| ignore | What to do when a test cannot start because it doesn't meet minimumNodesPerBatch. Setting this to fail marks the overall ValidationRequest as failed. Setting this to ignore skips this individual test and implicitly considers it successful |
| bandwidthGBps | string | An optional pass threshold for bandwidth-oriented tests such as NCCL tests. If set, the test is considered failed if the measured bandwidth falls below this threshold. This value is passed to the test provider, which is responsible for evaluating it as part of test execution prior to reporting success. The validation-controller is not responsible for enforcing this threshold |
| goodputRatio | string | An optional pass threshold for training-oriented tests such as Nemotron. If set, the test is considered failed if the measured goodput ratio falls below this threshold. Passed to the test provider the same way as bandwidthGBps |

### lifecycle-manager.config.newNodeValidation

| Key | Type | Purpose |
|---|---|---|
| condition | string | The name of the node condition the controller uses to track whether a node has already been validated. For a node to be targeted, this condition must be absent or false and every criteria expression must evaluate to true. Once a ValidationRequest is created, the controller sets this condition to True on the node so that subsequent evaluations no longer match |
| criteria | []CriteriaSpec | A set of CEL expressions evaluated against each node to determine whether it requires new node validation. All expressions must evaluate to true, along with the condition check above. The CEL environment exposes the node being validated (`node`) and its node-local ResourceSlices (`resourceSlices`), the same as readinessCriteria |
| newNodeTests | []string | The list of tests to run for new nodes. These take precedence over defaultTests when a ValidationRequest is created for a new node |
| batchPeriodSeconds | int64 | The window during which the controller collects eligible new nodes before creating ValidationRequests for them as a batch. Only applies to new node validation |

### lifecycle-manager.config.schedulingGate

| Key | Type | Purpose |
|---|---|---|
| cordon.remove | bool | Whether nodes should be uncordoned after completing validation. A node is only uncordoned once there are no pending, in-progress, or failed ValidationRequests, since a node may be targeted by multiple ValidationRequests |
| cordon.labelPrefix | string | Prefix for the cordon-by/cordon-reason/cordon-timestamp and matching uncordon-* labels the controller writes when cordon.remove is true |
| taints[].key, .value, .effect | string | Identifies a taint that test pods must tolerate during validation. The controller does not apply these taints. They are expected to be applied externally prior to the ValidationRequest being created. Every test pod automatically tolerates node.kubernetes.io/unschedulable and every taint listed here, regardless of whether the taint is present on the node |
| taints[].remove | bool | Whether the controller lifts this specific taint when validation completes. Set false for a taint that exists for reasons other than gating validation |

### fault-quarantine.validation

| Key | Type | Purpose |
|---|---|---|
| enabled | bool | Whether fault-quarantine requests validation after a node recovers from a fault |
| apiGroup, version, kind | string | The GroupVersionKind of the ValidationRequest resource fault-quarantine creates |
| templateFileName | string | The filename of the Go text/template, resolved from templates, rendered to build the ValidationRequest |
| templates | map[string]string | Inline template content, keyed by filename |
| ruleSets | []RuleSet | Maps HealthEvents from a quarantine session to the tests they require |
| partialDrainEnabled | bool | Must match node-drainer partialDrainEnabled. When false, a completed drain of a COMPONENT_RESET event counts as a full drain. Defaults to false. Set both with global.partialDrainEnabled |

### fault-quarantine.validation.ruleSets

| Key | Type | Purpose |
|---|---|---|
| enabled | bool | Whether this ruleset entry is evaluated |
| version | string | Ruleset schema version |
| name | string | Human-readable identifier |
| match.all[].kind, .expression | string | CEL conditions a HealthEvent from the quarantine session must satisfy for this ruleset to apply |
| tests | []string | Tests added to the ValidationRequest when this ruleset matches |
