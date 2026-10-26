# Validation

## Overview

If a cluster has deployed GPU Operator and NVSentinel, there is no mechanism to ensure that new GPU nodes or existing nodes which underwent remediation are validated prior to being marked as schedulable. New nodes are able to accept GPU workloads after the nvidia-device-plugin advertises GPU capacity on a given node. Additionally, existing nodes which were remediated by NVSentinel are able to accept GPU workloads after the fault-quarantine module unquarantines them. In either case, there is no ability for an operator to declare that a set of GPU performance tests must succeed prior to a node being returned to service.

Note that NVSentinel remediation actions do not include any verification that the remediation action successfully resolved the underlying GPU fault (outside the same fault re-occurring and being re-emitted by the corresponding health monitor). Similarly, new nodes joining a cluster could represent either new hardware being deployed or existing hardware being re-provisioned. In either case, this hardware should be validated, especially if the underlying hardware previously belonged to a node that was terminated and returned for repair to the given provider.

### Why Do You Need This?

Verification tests for either new nodes or existing nodes returning from remediation ensure that a given node meets its minimum performance requirements and reduce the probability that a customer workload will encounter a fatal error. A scheduling gate on completing a verification test gives an opportunity for NVSentinel to increase the scope of its health checking by performing:

- **GPU performance tests**: such as NCCL or Nemotron tests, which require exclusive GPU access.
- **Disruptive health checks**: such as DCGM diagnostics.
- **Inducing faults**: a performance or active health check may induce a fault that would otherwise only surface when a real customer workload runs.

## How It Works

Clients can create a ValidationRequest to specify that a given set of validation tests should be executed against a given set of nodes. The validation-controller reconciles ValidationRequests by:

1. Adding the ValidationRequest to each targeted node's validation-session. A validation-session tracks all in-progress ValidationRequests against the given node.
2. Grouping the nodes and tests included in the ValidationRequest into test groups which correspond to a single test provider resource. Test providers can optionally support batching multiple nodes or tests into a single resource.
3. Enforcing that nodes remain healthy before a test group resource is created.
4. Creating the resource for each test group, ensuring that no groups which overlap in node membership are executed concurrently.
5. Monitoring that test resources run to completion and ensuring nodes remain healthy during the execution of the test.
6. Running all test groups to completion, retrying a failed group before marking it terminal. The ValidationRequest is marked as succeeded only if all test groups complete successfully.
7. Removing the ValidationRequest from each targeted node's validation-session. If this is the last ValidationRequest tracked in the session, the configured scheduling gate is removed from the node.

Within NVSentinel, there are 2 clients that can be opted-in to create ValidationRequests.

- **Post-remediation validation**: the fault-quarantine module can be configured to automatically create ValidationRequests when a node has an unquarantine event. The ValidationRequest includes tests from any unhealthy event in the node's quarantine session that required validation, derived from validation.ruleSets.
- **New node validation**: the node-validation-controller can be configured to batch together a group of nodes matching newNodeValidation.criteria over batchPeriodSeconds, then creates a single ValidationRequest targeting every node in the batch with newNodeValidation.newNodeTests.

An operator can also create a ValidationRequest manually or implement their own clients.

## Configuration

Enable validation through Helm values on the lifecycle-manager and fault-quarantine charts:

```yaml
global:
  lifecycleManager:
    enabled: true

fault-quarantine:
  validation:
    enabled: true
```

The lifecycleManager component hosts both the validation-controller and node-validation-controller, so turning it on activates new node validation by default. Set fault-quarantine.validation.enabled separately to opt in to post-remediation validation.

### Configuration Options

- **Tests and providers**: define which tests can be referenced in ValidationRequests. A test provider can support multiple tests and each test provider corresponds to a single test resource. Multiple tests or multiple nodes can optionally be batched into a single test resource if supportsTestBatching or supportsBatchingNodes are true. See [Enabling Validation](configuration/validation.md#enabling-validation).
- **Post-remediation validation**: configure fault-quarantine to automatically create a ValidationRequest when a node generates an unquarantine event. The set of tests to execute from the node is derived from evaluating all unhealthy events from the quarantine session against validation.ruleSets. The default configuration creates a K8s job that executes a dcgm-diag-test for any unhealthy event which required a RESTART_VM remediation and completed a full drain. See [Enabling Post-Remediation Validation](configuration/validation.md#enabling-post-remediation-validation).
- **New node validation**: configure a ValidationRequest to be automatically created for new nodes matching newNodeValidation.criteria over batchPeriodSeconds. The default configuration specifies that dcgm-diag-test should be executed from newNodeValidation.newNodeTests as nodes join the cluster. See [Enabling New Node Validation](configuration/validation.md#enabling-new-node-validation).
- **NVCRE test provider**: an additional nvcre-provider can be configured to run NCCL communication or Nemotron training tests through the [Cluster Readiness Engine](https://github.com/NVIDIA/cluster-readiness-engine). These tests can run during either post-remediation or new node validation. See [Enabling NVCRE Validation](configuration/validation.md#enabling-nvcre-validation).