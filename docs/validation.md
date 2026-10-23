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
7. Removing the ValidationRequest from each targeted node's validation-session. The last request releases the configured scheduling gate when the release checks pass.

Within NVSentinel, there are 2 clients that can be opted-in to create ValidationRequests.

- **Post-remediation validation**: the fault-quarantine module can be configured to automatically create ValidationRequests when a node has an unquarantine event. The ValidationRequest includes tests from any unhealthy event in the node's quarantine session that required validation, derived from validation.ruleSets.
- **New node validation**: the node-validation-controller can be configured to batch together a group of nodes matching newNodeValidation.criteria over batchPeriodSeconds, then creates a single ValidationRequest targeting every node in the batch with newNodeValidation.newNodeTests.

An operator can also create a ValidationRequest manually or implement their own clients.

### Quarantine During Validation

Before releasing a node, the validation-controller checks the live Node for `quarantineHealthEvent`, including when a release patch is retried after a conflict. A new quarantine invalidates the successful results of affected test groups. Their attempts are marked `NodeReadinessViolation`, with the original test end time preserved; a warning log records that the result was invalidated before release. When retries remain, a fresh test is required after quarantine clears. Pending tests also check the live Node before starting, so a stale readiness cache cannot start a retry on a node already known to be quarantined.

Release is not atomic across multiple nodes. A test group cannot retry if one of its members may already have returned to service or is no longer owned by this request. Independent groups are not blocked from retrying solely because another group's node was released or deleted.

Deleting a ValidationRequest during quarantine removes its session entry and preserves the scheduling gate. If it was the last entry, the controller logs the retained gate. After remediation, a new ValidationRequest can run the required tests. Its ability to release the cordon follows the configured ownership policy; under strict ownership, a manual request without an ownership grant retains the cordon for an explicit operator decision.

### Preserve Existing Cordons

Set `lifecycle-manager.config.schedulingGate.cordon.onlyOwned: true` to preserve cordons that existed before GPU repair. Fault quarantine passes its ownership decision in the `nvsentinel.nvidia.com/cordon-ownership` request annotation, with a Node owner reference.

Lifecycle manager records the Node UID, request UID, and ownership decision in every new session entry, with either release policy. Existing name-only entries retain their original representation. A preservation decision survives controller restarts and overlapping requests through the session and cordon claim. Strict release requires matching identities, successful tests, proven ownership, and no current quarantine or pending handoff. Missing or conflicting ownership retains the cordon, and lifecycle manager logs the reason.

Fault quarantine records a claim for one continuous cordon in `nvsentinel.nvidia.com/cordon-claim`. A new fault during validation inherits this claim when an active quarantine, handoff, or owned session proves continuity, including a fault whose rules only taint the node. Requests and session entries carry the claim ID. A detached claim grants no release rights. The last successful validation removes the claim with the scheduling gate.

Enable `fault-quarantine.validation.persistHandoff` with this policy. Fault quarantine retains the validation source until lifecycle manager registers the matching request. Registration and removal of the matching source use one version-protected Node update. A pending handoff blocks an older request from releasing the node.

Fault quarantine automatically revisits pending handoffs after quarantine clears, including after a restart. It verifies an existing request or recreates a missing request with the same name from the retained source. Checks repeat every 30 seconds while the source remains; API errors retry with backoff. A stopped lifecycle manager keeps the node isolated until it resumes. Recovery preserves the source and scheduling gate until the normal registration and validation checks succeed.

Upgrade lifecycle manager before the claim-producing fault quarantine controller. Enable strict ownership after old requests, sessions, quarantines, and pending handoffs finish. Complete active claims and handoffs before reverting either controller. The [configuration reference](configuration/validation.md#enabling-strict-cordon-ownership) lists the checks.

Ownership gates only the cordon. The configured validation taints are lifted when the session ends, exactly as with the default policy. Applied taint records remain while validation holds the taints and are pruned after removal. A taint outside the removal configuration keeps its record. Deleting a request clears its association and never releases the cordon. Other trusted owned requests retain permission to release after successful validation. Manual, unknown, or preserved associations keep their preservation decision in the remaining session and claim. Deleting the last request without a pending successor retires its claim and retains the cordon. New-node requests and older producers without ownership information also retain the cordon. Operators can review a logged retained cordon and uncordon the node explicitly.

The default value is false and preserves the original ownership policy. Fault quarantine retains cordon and validation taints while a claim has an associated validation session. This applies even with `onlyOwned: false` or `persistHandoff: false`. Lifecycle manager completes the release after validation. Deploy the updated lifecycle manager before expanding repair to previously cordoned nodes. Both lifecycle manager and fault quarantine must support the ownership handoff for automatic release.

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
