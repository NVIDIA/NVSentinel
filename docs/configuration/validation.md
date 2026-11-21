# Validation Configuration

## Overview

The validation-controller reconciles ValidationRequests. A client can create a ValidationRequest to specify a set of validation tests to run against a set of nodes. The controller coordinates the run of that request across the nodes and tests, through a set of supported test providers.

NVSentinel has 2 clients that you can enable to create ValidationRequests:

- **New node validation**: you can configure the node-validation-controller to batch the nodes that match newNodeValidation.criteria over batchPeriodSeconds. It then creates one ValidationRequest with newNodeValidation.newNodeTests for all nodes in the batch.
- **Post-remediation validation**: you can configure fault-quarantine to create ValidationRequests automatically when a node has an unquarantine event. The ValidationRequest includes the tests from each unhealthy health event of the node's quarantine session that required validation. These tests come from validation.ruleSets.

This document covers all Helm configuration options for system administrators. The [Configuration Reference](#configuration-reference) is at the end. For the feature overview, see [Validation](../validation.md).

## Enabling Validation

The validation-controller is enabled by default, but the lifecycle-manager component that hosts it is not. Enable lifecycle-manager first. Then, if the default configuration is not sufficient, override it in the lifecycle-manager Helm chart:

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

With this configuration, a client can create a ValidationRequest that runs dcgm-diag-test as a Kubernetes Job through the k8s-job-provider. Before the test group starts, the targeted node must report allocatable GPU capacity, and it must not be under quarantine. The readinessCriteria enforce these requirements. The gpu-allocatable criterion accepts either source of GPU capacity:

- the device plugin, which sets `nvidia.com/gpu` in the node allocatable resources
- the `gpu.nvidia.com` DRA driver, which publishes the GPUs in ResourceSlices in GPU Operator GPUCluster mode

If the ValidationRequest does not specify spec.tests, defaultTests gives the tests to run in the request.

The default k8s-job-template.yaml sets nodeName on the Job, so NoSchedule taints do not block the test pod. The template also tolerates every NoExecute taint. As a result, a NoExecute taint that an operator or a bring-up process applies, before or during validation, does not stop the test. If fault-quarantine quarantines the node during validation, the not-under-quarantine readiness criterion still fails the test group. For this reason, keep this criterion. If you replace k8s-job-template.yaml with your own template, keep the NoExecute toleration in it.

## Enabling New Node Validation

The node-validation-controller is also part of lifecycle-manager. This controller batches the nodes that match specific criteria into one ValidationRequest. This function requires the validation-controller and a newNodeValidation section in the ValidationConfiguration. This function is on by default:

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

This configuration batches nodes over a 10-minute period. Each node in the batch must meet all of these criteria:

- Its creation time is within the past 30 minutes.
- It has the nvidia.com/gpu.present=true label.
- It is cordoned.

A node that already has the NewNodeValidationRequested condition is not eligible. This rule prevents the inclusion of the same node in multiple batches. The result is a ValidationRequest with newNodeTests that targets all nodes in the batch.

## Enabling Post-Remediation Validation

You can enable fault-quarantine to create ValidationRequests automatically when a node has an unquarantine event. This ValidationRequest includes the tests from each health event of the node's quarantine session that required validation. The tests come from validation.ruleSets, which map a HealthEvent to a list of tests for the ValidationRequest. To enable post-remediation validation, set fault-quarantine.validation.enabled to true.

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

The default ruleset creates a ValidationRequest that runs dcgm-diag-test against an unquarantined node. It does this if the node had an unhealthy health event that required a VM or BM restart remediation during its quarantine session.

fault-quarantine creates a ValidationRequest only if at least one health event of the quarantine session completed a full drain. As a result, validation tests do not run while the node still has workloads that node-drainer evicts.

`fault-quarantine.validation.partialDrainEnabled` tells fault-quarantine whether node-drainer drains COMPONENT_RESET events partially (only the pods that use the impacted GPU). It defaults to `false`, the same as `node-drainer.partialDrainEnabled`. As a result, a completed drain of a COMPONENT_RESET event counts as a full drain and can trigger validation. A validation ruleset must also match the event, and the default ruleset does not match COMPONENT_RESET.

If partial drain is enabled in node-drainer, also set `fault-quarantine.validation.partialDrainEnabled: true`. As an alternative, set `global.partialDrainEnabled: true`, which overrides both. Then a partial drain never counts as a full drain.

After the open quarantine sessions finish, change this value together with the node-drainer setting. The reason is that fault-quarantine judges each event by the current value. As a result, an event that node-drainer drained partially before the change can count as a full drain after the change. See [Partial Drain](node-drainer.md#partial-drain).

## Enabling NVCRE Validation

[NVCRE](https://github.com/NVIDIA/cluster-readiness-engine) support adds validation tests from an NVCRE test provider, for new nodes or after remediation. The default configuration supports only a k8s-job-provider test provider that can run a dcgm-diag-test. The nvcre-provider is an additional test provider that supports the nccl-loopback-nvswitch and nccl-all-reduce tests. You must explicitly enable NVCRE after you enable lifecycle-manager with its existing defaults.

To use the NVCRE test provider:

1. Install NVCRE: <https://github.com/NVIDIA/cluster-readiness-engine>.
2. Enable the lifecycle-manager component. The chart defaults already enable k8s-job-provider with dcgm-diag-test. They also enable new node validation.
3. Enable post-remediation validation in fault-quarantine. The default ruleset already triggers dcgm-diag-test for any remediation that required a reboot.
4. Apply the overrides in values-nvcre.yaml. This file overrides a subset of the values from lifecycle-manager/values.yaml and fault-quarantine/values.yaml. It defines the nvcre-provider with the nccl-loopback-nvswitch and nccl-all-reduce tests. It overrides post-remediation validation to run dcgm-diag-test and nccl-loopback-nvswitch. It also overrides new node validation to run dcgm-diag-test, nccl-loopback-nvswitch, and nccl-all-reduce:

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

The example values-nvcre.yaml is for a GB200 GCP environment. For a different CSP, a different GPU type or different tests, do these steps:

- Confirm that NVCRE supports your combination of CSP and GPU.
- Define the additional NVCRE communication tests to run, such as nccl-loopback, nccl-all-gather, or nccl-all-to-all. NVCRE also supports Nemotron training tests, which the nvcre-provider can use. These tests require goodputRatio, not bandwidthGBps.
- Override schedulingGate.taints, which lists the permanent node taints that NVCRE test pods tolerate. If a taint is not a validation gate itself, set remove to false on it. Then the controller never removes it from the node after the current validation-session completes.
- Override nccl-loopback-nvswitch.bandwidthGBps and nccl-all-reduce.bandwidthGBps. These are the bandwidth thresholds in GB/s. If the bandwidth is below a threshold, NVCRE fails the test. These settings are optional. If you do not set them, NVCRE reports a successful NCCL test, whatever the observed bandwidth.

### Example NVCRE ValidationRequests

These examples come from a GB200 GCP cluster with values-nvcre.yaml applied.

After an XID 95 fault and its RESTART_VM remediation, fault-quarantine creates this ValidationRequest to run dcgm-diag-test and nccl-loopback-nvswitch against the recovered node:

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

When 2 new nodes join the cluster, the node-validation-controller batches them into a single ValidationRequest that runs dcgm-diag-test, nccl-loopback-nvswitch, and nccl-all-reduce:

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
| defaultTests | []string | The default set of tests for a ValidationRequest that does not include tests |
| readinessCriteria | []CriteriaSpec | A set of CEL expressions. All of them must evaluate to true before a validation test can start on a node. The controller evaluates each entry against an environment that contains the node under validation (`node`) and the ResourceSlices whose `spec.nodeName` is that node (`resourceSlices`). When a criterion reads `resourceSlices`, lifecycle-manager watches ResourceSlices, so a slice published after the last node update still unblocks a pending request. The expressions decide which drivers lifecycle-manager watches. It filters the slice events to the `spec.driver` values that the expressions compare with string literals, as in `s.spec.driver == "gpu.nvidia.com"` or `s.spec.driver in [...]`. A criterion can read `spec.driver` in a different way, or read slices without a test of the driver. Such a criterion turns the filter off, and lifecycle-manager processes the slice events of every driver (it logs this at startup). The filter affects only the events that wake the controller. The `resourceSlices` variable always holds all slices of the node. If you apply a node cordon or taint externally, you can block validation until it is present. To do this, add these properties to readinessCriteria. If the criteria are not met, the node cannot start validation. If the criteria were met first and then reverted, validation fails |
| maxConcurrentGroups | int | The maximum number of test groups that can run at the same time. Node overlap also limits the groups. Two groups that share a node never run at the same time, whatever this setting is |
| templateMountPath | string | The directory from which the controller resolves the templateFile paths |
| providers | map[string]ProviderConfig | Test provider settings that apply to all tests of this provider. The key is the name that tests[].provider references (see below) |
| tests | map[string]TestConfig | The set of supported tests that clients can request in ValidationRequests. The key is the test name (see below) |
| newNodeValidation | NewNodeValidationConfig | Contains the configuration to detect and test new nodes (see below) |
| schedulingGate | SchedulingGateConfig | Contains the scheduling gate controls that the validation-controller manages during the lifecycle of a ValidationRequest (see below) |

### lifecycle-manager.config.providers

| Key | Type | Purpose |
|---|---|---|
| apiGroup, version, kind | string | The GroupVersionKind of the provider CRD. The controller uses it to register a watch for each provider type, so events of the test provider resources trigger the reconciliation of ValidationRequests |
| resource | string | The plural resource name of the provider CRD. The Helm templates use it only to generate RBAC rules for the validation-controller |
| templateFile | string | The filename of a Go text/template, relative to templateMountPath. The controller renders it to construct the test provider CRD for each test group |
| supportsTestBatching | bool | Whether this test provider supports the batching of multiple tests into one test provider request |
| retries | int | The number of retries allowed for all tests that reference this test provider |
| timeoutSeconds | int64 | The maximum number of seconds for one attempt of a test group with this provider. After this time, the controller marks the attempt as failed |
| successfulCondition | {type, status} | The condition that the validation-controller polls to find whether an attempt was successful |
| failedCondition | {type, status} | The condition that the validation-controller polls to find whether an attempt failed |

### lifecycle-manager.config.tests

| Key | Type | Purpose |
|---|---|---|
| provider | string | The test provider that runs this test |
| image | string | The container image for the test. It applies only to providers that render it, such as k8s-job-provider |
| command | []string | The command to run in the container for the test. It applies only to providers that render it, such as k8s-job-provider |
| env | []EnvVar | A list of environment variables (names and values) for the provider resource template of the test |
| supportsBatchingNodes | bool | Whether this test can test multiple nodes in one test |
| minimumNodesPerBatch | int | If supportsBatchingNodes is true, the minimum number of nodes that the test must get. Must be 1 when supportsBatchingNodes is false |
| batchFailurePolicy | fail \| ignore | The action when a test cannot start because it does not meet minimumNodesPerBatch. With fail, the controller marks the full ValidationRequest as failed. With ignore, the controller skips this test and implicitly considers it successful |
| bandwidthGBps | string | An optional pass threshold for bandwidth tests, such as NCCL tests. If you set it, the test fails if the measured bandwidth is below this threshold. The controller passes this value to the test provider. The test provider evaluates it during the test, before it reports success. The validation-controller does not enforce this threshold |
| goodputRatio | string | An optional pass threshold for training tests, such as Nemotron. If you set it, the test fails if the measured goodput ratio is below this threshold. The controller passes it to the test provider in the same way as bandwidthGBps |

### lifecycle-manager.config.newNodeValidation

| Key | Type | Purpose |
|---|---|---|
| condition | string | The name of the node condition that the controller uses to track whether it already validated a node. The controller targets a node only if this condition is absent or false and every criteria expression evaluates to true. After the controller creates a ValidationRequest, it sets this condition to True on the node. Then later evaluations do not match the node |
| criteria | []CriteriaSpec | A set of CEL expressions that the controller evaluates against each node to find whether it needs new node validation. All expressions must evaluate to true, and the condition check above must also pass. The CEL environment contains the node under validation (`node`) and its node-local ResourceSlices (`resourceSlices`), the same as readinessCriteria |
| newNodeTests | []string | The list of tests to run for new nodes. When the controller creates a ValidationRequest for a new node, these tests take precedence over defaultTests |
| batchPeriodSeconds | int64 | The period in which the controller collects eligible new nodes, before it creates ValidationRequests for them as a batch. Applies only to new node validation |

### lifecycle-manager.config.schedulingGate

| Key | Type | Purpose |
|---|---|---|
| cordon.remove | bool | Whether the controller uncordons the nodes after they complete validation. The controller uncordons a node only when the node has no pending, in-progress, or failed ValidationRequests, because multiple ValidationRequests can target a node |
| cordon.labelPrefix | string | The prefix for the cordon-by, cordon-reason and cordon-timestamp labels, and the matching uncordon-* labels, that the controller writes when cordon.remove is true |
| taints[].key, .value, .effect | string | Identifies a taint that test pods must tolerate during validation. The controller does not apply these taints. It expects another process to apply them before the creation of the ValidationRequest. Every test pod automatically tolerates node.kubernetes.io/unschedulable and every taint in this list, whether or not the taint is present on the node. The default k8s-job-template.yaml also tolerates every NoExecute taint |
| taints[].remove | bool | Whether the controller removes this specific taint when validation completes. For a taint that exists for reasons other than a validation gate, set false |

### fault-quarantine.validation

| Key | Type | Purpose |
|---|---|---|
| enabled | bool | Whether fault-quarantine requests validation after a node recovers from a fault |
| apiGroup, version, kind | string | The GroupVersionKind of the ValidationRequest resource that fault-quarantine creates |
| templateFileName | string | The filename of the Go text/template in templates. fault-quarantine renders this template to build the ValidationRequest |
| templates | map[string]string | Inline template content, keyed by filename |
| ruleSets | []RuleSet | Maps HealthEvents from a quarantine session to the tests they require |
| partialDrainEnabled | bool | Must match node-drainer partialDrainEnabled. When false, a completed drain of a COMPONENT_RESET event counts as a full drain. Defaults to false. Set both with global.partialDrainEnabled |

When fault-quarantine creates a ValidationRequest, it keeps the cordon and every taint that it applied during the quarantine session. After that, fault-quarantine does not track them. When validation passes, lifecycle-manager releases them.

For this reason, list every taint that a fault-quarantine rule-set applies in lifecycle-manager schedulingGate.taints. If a taint is not in the list, lifecycle-manager never removes it. Test pods also do not tolerate such a taint, unless the provider template tolerates it. For example, the default k8s-job-template.yaml tolerates NoExecute taints.

Keep the not-under-quarantine readiness criterion. With this criterion, a new quarantine fails the pending validation, and lifecycle-manager does not release the node.

Set remove to true only for taints that fault-quarantine alone applies. The reason is that lifecycle-manager matches on key, value, and effect. As a result, it also removes a matching taint that was on the node before the quarantine. If fault-quarantine creates no ValidationRequest, it removes its taints itself.

If another process also applies the same taint, set remove to false. The taint then stays on the node after validation passes. The other process or an operator must remove it.

### fault-quarantine.validation.ruleSets

| Key | Type | Purpose |
|---|---|---|
| enabled | bool | Whether fault-quarantine evaluates this ruleset entry |
| version | string | The ruleset schema version |
| name | string | A human-readable identifier |
| match.all[].kind, .expression | string | The CEL conditions that a HealthEvent from the quarantine session must satisfy for this ruleset to apply |
| tests | []string | The tests that fault-quarantine adds to the ValidationRequest when this ruleset matches |
