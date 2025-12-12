# Janitor Support for GPU Reset

Author: Dan Huenecke

## Overview

This document outlines a proposal for selectively resetting individual GPU devices on a Kubernetes node. Currently, in DGX Cloud (DGXC), the standard response to an isolated and recoverable GPU failure is a full node reboot, a process that disrupts all workloads, including those on healthy GPUs. This proposed solution introduces a more granular (and automated), device-level reset capability to improve GPU availability and minimize the blast radius of common isolated and recoverable GPU errors.

## Context

Currently, the standard response to a single GPU failure is a full node reboot. This heavy-handed approach became the default due to historical difficulties with targeted resets and creates a significant operational problem. For instance, on an 8-GPU node, a recoverable error on one device forces the termination of all workloads and renders all eight GPUs unavailable for up to 20 minutes until the node reboots and is ready to accept new workloads.

This proposal solves the problem with a more surgical, device-level reset. The new approach reduces the impact from 20 minutes of downtime for all 8 GPUs to less than 2 minutes of downtime for only the single affected GPU.

## Goals and Non-Goals

### Goals

* **Implement an automated device-level GPU reset mechanism.** A GPU reset is successfully triggered by creating a Custom Resource (CR), and the process completes with the target device returning to a usable state.

* **Reduce workload disruption from isolated and recoverable GPU failures.** A reset of one GPU results in *zero disruptions* for workloads on other healthy GPUs on the same node.

### Non-Goals

* **Fixing persistent hardware failures.** If a GPU fails to recover after a reset, no further corrective action will be taken.

## Scope of Support

The initial scope for the GPU reset feature will focus only on configurations that can be managed with nvidia-smi without external dependencies.

### Supported Configurations

* NVIDIA Ampere architecture and newer GPUs (e.g., A100, H100, L40S) with NVLink connections.
* Hopper architecture (e.g., H100) and newer GPUs NVSwitch systems.

### Unsupported Configurations

* Pre-Ampere architectures (e.g., V100). Requires resetting an entire group of NVLink-connected GPUs at once
* Ampere-based (e.g., A100) NVSwitch systems (requires Fabric Manager)
* MIG-enabled vGPU Guests (not supported)

## Existing Solution

The current solution for handling a malfunctioning GPU is to reboot the entire node. The process involves identifying the faulty node, cordoning it to prevent new workloads from being scheduled, draining all existing user workloads, issuing a system reboot, and then waiting until it is ready to accept new workloads.

## Proposed Solution

The proposed solution addresses the needs of both cluster administrators and end users by providing a targeted and efficient recovery mechanism for isolated and recoverable GPU failures.

### User Stories

#### Administrator

As an administrator, when a single GPU on a multi-GPU node fails, I want to trigger a reset for only that device so that I can restore its capacity without disrupting the critical, long-running workloads on the other healthy GPUs.

#### End User

As an end user, when my workload is terminated due to a recoverable GPU failure, I want the GPU to be repaired and quickly made available for future workloads, without affecting any of my other running pods on the same node.

### Implementation Details

The proposed solution introduces a **GPU Reset Controller**, a **ResetGpu Custom Resource Definition** (CRD) (NVIDIA, n.d.-f), and a **GPU Reset Job**. An administrator or an automated monitoring system, such as NVSentinel, can create an instance of this CRD to initiate the reset of a specific GPU on a target node.

The workflow will be managed by two components:

1. **GPU Reset Controller:** A cluster-level operator that watches for a ResetGpu CR. Its primary role is to prepare the node for the reset and manage the lifecycle of the reset Job.

2. **GPU Reset Job:** A temporary, privileged pod created by the controller. It is scheduled to a specific node to execute the reset command directly against the target device.

#### Reset Workflow

When a ResetGpu CR is created, the controller initiates the following sequence:

1. **Tear Down Node-Level GPU Services:** The controller signals the NVIDIA GPU Operator to remove its user-space components from the node to release any handles on the target GPU device. The primary components targeted from removal are the **NVIDIA Device Plugin**, **NVIDIA DCGM**, the **DCGM Exporter**, and **GPU Feature Discovery**. Following the official guidance from the GPU Operator team (NVIDIA, n.d.-i), the controller performs this declaratively by updating the corresponding node labels to false (e.g., nvidia.com/gpu.deploy.device-plugin=false).

2. **Create the Reset Job:** The controller creates a Kubernetes Job configured with a nodeName to ensure it runs on the correct node. The Job’s container is passed the target GPU’s ID.

3. **Execute GPU Reset:** The pod created by the Job performs the following actions in order:
    1. Disables NVIDIA persistence mode for the target GPU (if enabled).
    2. Executes a reset of the target GPU (i.e., via nvidia-smi --gpu-reset --id <ID>; NVIDIA, n.d.-k).
    3. Runs a health check to verify the GPU’s status.
    4. Re-enables NVIDIA persistence mode (only if it was originally enabled).
    5. Exits with a standard success code (**0**) or a non-zero failure code (e.g., **1**) based on the outcome.

4. **Report Outcome:** The controller monitors the Job for completion and updates the status field of the ResetGpu CR with the final result.

5. **Redeploy Node-Level Services:** *Regardless of the outcome*, the controller reverts the node labels to signal the GPU Operator to redeploy its components.

## Alternate Solutions

Several alternative approaches were considered, but the proposed Controller and Job architecture was chosen for its security, efficiency, and alignment with Kubernetes-native principles.

### Controller-driven SSH

An alternative was for the controller to use **SSH** to connect to the target node and execute the reset remotely, instead of creating a Job. This approach was rejected due to significant security and operational concerns.

### GPU Reset Agent (DaemonSet)

An alternative was a privileged **DaemonSet** running a persistent agent on every GPU node. This approach was rejected because a long-running privileged process on every node presents a larger security attack surface than a short-lived, ephemeral Job. This agent would also constantly consume resources while idle and require more complex logic, making the Job-based approach more secure, resource-efficient, and simpler to manage.

## Testability

The solution is designed to be testable at multiple levels, from individual components to the complete end-to-end workflow. Testing will be divided into unit, integration, and end-to-end (e2e) tests to ensure correctness and reliability.

## Observability

Observability is critical to ensuring the GPU reset mechanism is reliable and to provide visibility into the health of the GPU fleet. Our strategy is based on three pillars: metrics for quantitative analysis, structured logs for debugging, and alerts for actionable events.

### Metrics

The following metrics will be exported and provided by the GPU Reset Controller:

| Metric                             | Type | Labels                         | Description                                                                                                        |
|:-----------------------------------| :---- |:-------------------------------|:-------------------------------------------------------------------------------------------------------------------|
| gpu_reset_requests_total           | Counter | node, gpu_type, gpu_id         | Total number of GPU reset requests initiated.                                                                      |
| gpu_reset_requests_completed_total | Counter | node, gpu_type, gpu_id, status | Total number of completed reset Jobs, labeled by their final status (success or failure).                          |
| gpu_reset_duration_seconds         | Histogram | node, gpu_type,status          | The end-to-end duration of the reset workflow, from CR creation to Job completion.                                 |
| gpu_reset_active_requests          | Gauge | node, gpu_type                 | The number of reset operations currently in progress on a given node.                                              |
| gpu_reset_failure_reasons_total    | Counter | node, gpu_type, reason         | Tracks the specific reason for a reset failure (e.g., teardown_failed, reset_comment_failed, health_check_failed). |

### Dashboard

The dedicated Breakfix/Automation Operations Grafana dashboards (NVIDIA, n.d.-m) will be updated to include the health and performance of the GPU reset system. It will provide at-a-glance answers to key questions:

* **Overall Health:** What is the success vs. failure rate of GPU resets over the last 24 hours?
* **Performance:** What is the p95 and p99 reset duration? Is it within the expected SLO (e.g., under 2 minutes)?
* **Hotspots:** Are resets concentrated on specific clusters, nodes, or GPU types?
* **Failure Analysis:** What are the most common reasons for reset failures?

### Logging

Both the GPU Reset Controller and the GPU Reset Job pods will emit structured logs to standard output. Key log events will include:

* CR received and validated
* Node-level GPU services teardown initiated and completed.
* Reset Job created.
* GPU reset command executed within the Job.
* Post-reset health check started, with its outcome.
* Final status (Success/Failure) recorded in the CR.

### Alerting

Alerts will be designed to be actionable and will be categorized into two priority levels.

#### High Priority

* **High Reset Failure Rate:** If the percentage of failed GPU resets across the cluster exceeds 20% over a 1-hour window. This indicates a potential systemic issue with the reset process or a widespread infrastructure problem.
* **Controller Unhealthy:** If the GPU Reset Controller deployment has zero pods ready for more than 5 minutes.
* **Consecutive Failures on the Same GPU:** If the same GPU (node + gpu_id) fails to reset 3 consecutive times within 24 hours. This signals a likely persistent hardware failure, and an automated process or SRE should cordon the node for manual inspection.

#### Low Priority

* **Single Reset Failure:** If any individual reset job exists with a non-zero status code. The notification will include the cluster, node, gpu_type, gpu_id, and failure reason.
* **Reset Duration SLO Breach:** If the p95 reset duration exceeds the SLO (e.g., under 2 minutes) over a 30-minute period.
* **Anomalous Reset Volumes:** If the number of resets in the last hour is more than three standard deviations above the weekly average, indicating a potential fleet-wide event.

## Cross-Team Impact

### End Users

This feature will be beneficial for all teams consuming GPUs in DGXC. The **primary impact is a reduction in disruption**. Instead of having long-running jobs on healthy GPUs terminated due to an issue with a single faulty GPU, their workloads will continue uninterrupted.

### Security

The most significant security consideration is that the GPU Reset **Job *must* run as a privileged pod** on the node to perform the low-level hardware reset.

* **Risk:** A privileged container has root access to the host node, which could be exploited if the container image is compromised or if the job is created with malicious intent.

* **Mitigation:** This feature will require a security review. Access to create ResetGpu Custom Resources will be strictly limited via RBAC (Role-Based Access Control) to a specific group of automation service accounts. The Job itself will run with a dedicated, minimal-permission ServiceAccount.
