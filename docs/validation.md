# Validation

## Overview

Validation runs a set of GPU tests on a node before the node returns to service. It covers new GPU nodes and nodes that NVSentinel remediated. Without validation, a new node accepts GPU workloads when the nvidia-device-plugin advertises GPU capacity on the node. A remediated node accepts GPU workloads when fault-quarantine unquarantines it. Validation lets an operator declare a set of GPU tests that must succeed before a node returns to service.

NVSentinel remediation actions do not verify that the remediation action corrected the GPU fault. The only exception is when the same fault occurs again, and the related health monitor reports it again. Also, a new node that joins a cluster can be new hardware, or re-provisioned existing hardware. In both cases, validate this hardware. This is especially important if the hardware was part of a terminated node that returned to the provider for repair.

### Why Do You Need This?

Verification tests run on new nodes and on existing nodes that return from remediation. They make sure that a node meets its minimum performance requirements. They also decrease the probability of a fatal error in a customer workload. A scheduling gate that waits for a verification test to complete lets NVSentinel increase the scope of its health checks. NVSentinel can then do these actions:

- **GPU performance tests**: such as NCCL or Nemotron tests, which require exclusive GPU access.
- **Disruptive health checks**: such as DCGM diagnostics.
- **Fault induction**: a performance or active health check can cause a fault. Without the check, this fault appears only when a real customer workload runs.

## How It Works

A client can create a ValidationRequest. The request specifies a set of validation tests to run against a set of nodes. The validation-controller reconciles each ValidationRequest in these steps:

1. It adds the ValidationRequest to the validation-session of each targeted node. A validation-session tracks all in-progress ValidationRequests against its node.
2. It puts the nodes and tests of the ValidationRequest into test groups. Each test group corresponds to one test provider resource. As an option, a test provider can support the batching of multiple nodes or tests into one resource.
3. It makes sure that the nodes stay healthy before it creates a test group resource.
4. It creates the resource for each test group. It does not run two groups at the same time if they have a node in common.
5. It monitors each test resource until the test completes. It also makes sure that the nodes stay healthy while the test runs.
6. It runs all test groups to completion. It retries a failed group before it marks the group as terminal. It marks the ValidationRequest as succeeded only if all test groups complete successfully.
7. It removes the ValidationRequest from the validation-session of each targeted node. If the session tracks no other ValidationRequest and the node still satisfies the readiness criteria, the controller removes the configured scheduling gate.

NVSentinel has 2 clients that you can enable to create ValidationRequests:

- **Post-remediation validation**: you can configure fault-quarantine to create ValidationRequests automatically when a node has an unquarantine event. The ValidationRequest includes the tests from each unhealthy health event of the node's quarantine session that required validation. These tests come from validation.ruleSets.
- **New node validation**: you can configure the node-validation-controller to batch the nodes that match newNodeValidation.criteria over batchPeriodSeconds. It then creates one ValidationRequest with newNodeValidation.newNodeTests for all nodes in the batch.

An operator can also create a ValidationRequest manually, or write a custom client.

### Readiness Criteria at Release

Before the validation-controller releases a node, it evaluates the configured readiness criteria against the live Node. These criteria are the same criteria that gate a pending or running test group. As a result, with the default `not-under-quarantine` criterion, a new `quarantineHealthEvent` annotation blocks the release.

The controller evaluates every node of the request before it releases any of them. It also evaluates each node again inside the release patch. The patch carries the resourceVersion of the Node. A fault that is recorded between the read and the write makes the patch conflict. The controller then retries: it reads the node and evaluates it again.

If a node fails a criterion at release, the ValidationRequest completes as `Failed`, even if every test group succeeded. The controller logs a warning and sets the `ReleaseBlocked` condition with reason `NodeReadinessViolation`. The message of this condition names the failing nodes and criteria. The nodes keep their validation-session entry, marked as failed, and their scheduling gate. Release is not atomic across the nodes of a request. If a later node fails its check, a node that the controller already released stays released.

A failed request holds its nodes. The controller releases such a node in one of these cases:

- A new ValidationRequest with the same tests succeeds. This clears the failed session entry.
- An operator deletes the failed request after the node satisfies the readiness criteria again.

With post-remediation validation, fault-quarantine creates this new ValidationRequest when the new fault matches `validation.ruleSets`.

The deletion of a ValidationRequest removes its session entry. If it was the last entry, one of these conditions can stop the release of the node:

- The node fails a readiness criterion.
- The controller cannot evaluate a criterion against the node.

In these cases, the controller keeps the scheduling gate and logs a warning. It does not release the node. A later ValidationRequest that succeeds on the node releases the gate. Otherwise, an operator removes the gate.

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

The lifecycle-manager component hosts both the validation-controller and the node-validation-controller. As a result, if you enable lifecycle-manager, new node validation is also active by default. To enable post-remediation validation, set fault-quarantine.validation.enabled separately.

### Configuration Options

- **Tests and providers**: define the tests that ValidationRequests can reference. A test provider can support multiple tests, and each test provider corresponds to one test resource. If supportsTestBatching is true, the controller can batch multiple tests into one test resource. If supportsBatchingNodes is true, it can batch multiple nodes. See [Enabling Validation](configuration/validation.md#enabling-validation).
- **Post-remediation validation**: configure fault-quarantine to create a ValidationRequest automatically when a node generates an unquarantine event. To find the tests to run on the node, fault-quarantine evaluates all unhealthy health events of the quarantine session against validation.ruleSets. The default configuration creates a Kubernetes Job that runs dcgm-diag-test. It does this if an unhealthy health event required a RESTART_VM or RESTART_BM remediation and completed a full drain. See [Enabling Post-Remediation Validation](configuration/validation.md#enabling-post-remediation-validation).
- **New node validation**: configure the automatic creation of a ValidationRequest for new nodes that match newNodeValidation.criteria over batchPeriodSeconds. In the default configuration, newNodeValidation.newNodeTests contains dcgm-diag-test, which runs when nodes join the cluster. See [Enabling New Node Validation](configuration/validation.md#enabling-new-node-validation).
- **NVCRE test provider**: you can configure an additional nvcre-provider to run NCCL communication or Nemotron training tests through the [Cluster Readiness Engine](https://github.com/NVIDIA/cluster-readiness-engine). These tests can run during post-remediation validation or new node validation. See [Enabling NVCRE Validation](configuration/validation.md#enabling-nvcre-validation).
