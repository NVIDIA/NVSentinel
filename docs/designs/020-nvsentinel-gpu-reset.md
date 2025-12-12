# NVSentinel Support for GPU Reset

Author: Nathan Herz

## Overview

This document outlines how to add GPU reset support in NVSentinel. Currently, if NVSentinel detects that a single GPU needs to be remediated via a reset, it will drain all workload pods on the given node and execute a reboot. In addition to having to drain all workload pods, the GPU node will not be returned to service until the reboot is completed and the driver is re-installed. Supporting GPU resets comes with the benefit that pods leveraging healthy GPUs on the same node do not need to be drained and taken offline during the time the reboot is executed and the driver is re-installed. Additionally, unhealthy GPUs which do need to be taken offline for a reset will have a lower downtime compared to reboots.

Supporting GPU reset can be broken into 2 parts:

1. Add an API to Janitor to support resetting a set of GPUs on a given node
2. Add support to NVSentinel to invoke this API when a GPU reset is required

This document will summarize the changes made to Janitor to support GPU reset before primarily focusing on the changes required in NVSentinel.

## Part 1: Janitor support for GPU reset

The design for adding Janitor support for GPU resets was covered in [Janitor Support for GPU Reset](019-janitor-gpu-reset.md
). The scope of Janitor GPU reset support is to implement a custom resource which can reset a subset of GPUs on a given node without disrupting existing workloads leveraging other healthy GPUs on the same node.

The implementation introduces a GPU Reset Controller, a GPUReset CRD, and a GPU Reset Job. An administrator or an automated monitoring system, such as NVSentinel, can create an instance of this CRD to initiate the reset of a specific GPU on a target node.

The workflow is managed by two components:

1. **GPU Reset Controller:** a cluster-level operator that watches for GPUReset custom resources. Its primary role is to prepare the node for the reset and manage the lifecycle of the GPU Reset Job.
2. **GPU Reset Job:** A temporary, privileged pod created by the controller. It is scheduled to a specific node to execute the reset command directly against the target device.

When a GPUReset CR is created, the controller initiates the following sequence:

1. **Tear Down Node-Level GPU Operator Daemons:** the controller signals the GPU Operator to remove its user-space components from the node to release any handles on the target GPU device. The primary components targeted from removal are the NVIDIA Device Plugin, NVIDIA DCGM, the DCGM Exporter, and GPU Feature Discovery.
2. **Create the GPU Reset Job:** the controller creates a Kubernetes Job configured with a nodeName to ensure it runs on the correct node. The Job’s container is passed the target GPU  UUID.
3. **Execute GPU Reset:** the pod created by the Job performs the following actions in order:
    1. Disables persistence mode for the target GPU (if enabled).
    2. Executes a reset of the target GPU via nvidia-smi.
    3. Runs a health check to verify the GPU’s status.
    4. Re-enables persistence mode (if it was originally enabled).
    5. Exits with a standard success code or a non-zero failure code based on the outcome.
4. **Report Outcome:** The controller monitors the Job for completion and updates the status field of the GPUReset CR with the final result.
5. **Redeploy Node-Level Services:** regardless of the GPU Reset Job result, the controller reverts the node labels to signal the GPU Operator to redeploy its components.

Refer to the dedicated design document for additional details about the Janitor implementation for GPU reset. Part 2 will cover how NVSentinel can leverage this existing GPUReset custom resource.

## Part 2: NVSentinel support for GPU reset

To add support for GPU reset in NVSentinel, we will need the following functionality:

* Detect when a GPU reset remediation is required
* Detect when a GPU reset occurred
* Add support to lookup the mapping between GPUs and pods
* Add support to execute a partial drain where we only drain pods leveraging unhealthy GPUs instead of all pods on the given node
* Add support to create a GPUReset custom resource

To implement the functionality above, we will cover which modifications are required for each of these NVSentinel components in the following sections:

* **Syslog-Health-Monitor:** responsible for detecting XID errors written to syslog and notifying dependent modules via HealthEvents with metadata to identify and remediate the impacted GPU. We rely on health-monitors to detect that a remediation action was taken and send a corresponding healthy event.
* **Fault-Quarantine-Module:** responsible for consuming HealthEvents from health-monitors and determining whether an impacted node needs to be cordoned for unhealthy events or uncordoned for healthy events after a successful remediation.
* **Node-Drainer-Module:** this module will initiate pod draining against nodes needing remediation in response to cordoned HealthEvents from the fault-quarantine-module.
* **Fault-Remediation-Module:** this module will ingest HealthEvents for nodes which have successfully completed draining and proceed with executing the remediation action suggested by the health-monitor which originally generated the unhealthy event.

## 1. Syslog-Health-Monitor

**1.1 How will this component handle events requiring a GPU reset?**

* **Populate the COMPONENT_RESET recommended action:** the syslog-health-monitor references the XID catalog to map XID errors to their recommended remediation action ([ref](https://docs.nvidia.com/deploy/xid-errors/analyzing-xid-catalog.html)). The monitor already includes functionality to map the RESET_GPU action to the internal COMPONENT_RESET action.
* **Populate the GPU UUID:** previously, we did not need to populate the GPU UUID as part of HealthEvents because this information was not required to initiate a drain or trigger a reboot. However, we now need to ensure that the entities impacted field includes the GPU UUID so that the node-drainer-module can only drain pods utilizing the given GPU and so that the fault-remediation-module can create a GPUReset custom resource which only targets the unhealthy GPU.
    * The syslog-health-monitor has already been updated to populate the impacted GPU UUID for XID errors so we can rely on this information being present in HealthEvents ([ref](https://github.com/NVIDIA/NVSentinel/pull/288)).
    * While the GPUReset custom resource supports resetting multiple GPUs as part of a single resource, we will maintain the invariant that each HealthEvent (and resulting GPUReset custom resource) only contains a single GPU.

Log event for an XID 48 which will be assigned a COMPONENT_RESET recommend action:

```
NVRM: GPU at PCI:0000:03:00: GPU-455d8f70-2051-db6c-0430-ffc457bff834
NVRM: GPU Board Serial Number: 1324023049334
NVRM: Xid (PCI:0000:03:00): 48, pid=91237, name=nv-hostengine, Ch 00000076, errorString CTX SWITCH TIMEOUT, Info 0x3c046
```

Corresponding unhealthy HealthEvent:

```
{
  _id: ObjectId('68fa139a72e48b1a7a1bbbc8'),
  createdAt: ISODate('2025-10-23T11:38:02.208Z'),
  healthevent: {
    version: Long('1'),
    agent: 'syslog-health-monitor',
    componentclass: 'GPU',
    checkname: 'SysLogsXIDError',
    isfatal: true,
    ishealthy: false,
    message: 'ROBUST_CHANNEL_CTXSW_TIMEOUT_ERROR',
    recommendedaction: 2,
    errorcode: [
      '48'
    ],
    entitiesimpacted: [
      {
        entitytype: 'PCI',
        entityvalue: '0000:03:00'
      },
      {
        entitytype: 'GPU_UUID',
        entityvalue: 'GPU-455d8f70-2051-db6c-0430-ffc457bff834'
      }
    ],
    metadata: null,
    generatedtimestamp: {
      seconds: Long('1761219482'),
      nanos: 207323127
    },
    nodename: 'node1',
    quarantineoverrides: null,
    drainoverrides: null
  },
  healtheventstatus: {
    nodequarantined: 'Quarantined',
    userpodsevictionstatus: {
      status: 'Succeeded'
    },
    faultremediated: null
  }
}
```

**1.2 How will this component handle events indicating a GPU reset occurred?**

* **Populate the identifying fields for the healthy HealthEvent:** the healthy event sent from the syslog-health-monitor needs to include the sufficient information to map it to the original unhealthy event persisted as a quarantineHealthEvent annotation on the node object.
    * This includes the health-monitor name, the component class, check name, node name, and entities impacted. Specifically, we need to ensure that the given GPU UUID and PCI are included in the healthy event (or include logic to allow unhealthy events with a GPU UUID and PCI to be cleared in quarantineHealthEvents if only the GPU UUID recovers).
* **Detect a reset occurred for the given GPU:** for reboot remediations, the syslog-health-monitor detects that the boot ID changed on component start-up and sends healthy events for all checks executed by this monitor. These events have no entities populated which the fault-quarantine-module will interpret as all entities recovering from the given event. Detecting GPU resets is not trivial since there does not exist a corresponding syslog entry nor a DCGM module which exposes GPU resets. As a result, we will need to notify the syslog-health-monitor that a GPU reset successfully completed.
    * **GPUReset job writes a syslog event:** after the privileged GPU reset pod created by Janitor successfully completes the GPU reset, this pod can write a log entry that will be consumed by the syslog-health-monitor and result in a healthy event for the given GPU. Note that the monitor will likely need to look up the corresponding PCI to include in the healthy event since the GPUReset custom resource does not reference the impacted PCI.

```
GPU reset occurred: GPU-455d8f70-2051-db6c-0430-ffc457bff834
```

* See [Appendix-1](#Appendix) for alternative GPU reset detection approaches considered.

Healthy HealthEvent:

```
{
  _id: ObjectId('68fa139a72e48b1a7a1bbbc8'),
  createdAt: ISODate('2025-10-23T11:38:02.208Z'),
  healthevent: {
    version: Long('1'),
    agent: 'syslog-health-monitor',
    componentclass: 'GPU',
    checkname: 'SysLogsXIDError',
    isfatal: true,
    ishealthy: true,
    entitiesimpacted: [
      {
        entitytype: 'PCI',
        entityvalue: '0000:03:00'
      },
      {
        entitytype: 'GPU_UUID',
        entityvalue: 'GPU-455d8f70-2051-db6c-0430-ffc457bff834'
      }
    ],
    metadata: null,
    generatedtimestamp: {
      seconds: Long('1761219482'),
      nanos: 207323127
    },
    nodename: 'node1'
}
```

## 2. Fault-Quarantine-Module

**2.1 How will this component handle events requiring a GPU reset?**

* **Do not support partial quarantines:** in the future, we could choose to keep nodes uncordoned if a subset of GPUs on the node are unhealthy to allow new pods to be scheduled to healthy GPUs. To prevent additional complexity, we will continue to cordon nodes that need a single GPU reset even if there’s additional GPU capacity.
    * The device plugin and DRA drivers do not have the same view of GPU health as NVSentinel. Specifically, device plugin health checking was recently disabled. As a result, it’s possible for the device plugin to advertise a GPU as healthy even if NVSentinel is currently remediating a GPU and needs to prevent pods from utilizing it.
    * The GPU Operator team is targeting Q1 2026 for onboarding all components to leveraging NVSentinel for GPU health checking. Once our remediation and device management systems have the same view of GPU health, we can explore partial cordons.

**2.2 How will this component handle events indicating a GPU reset occurred?**

* **Consume the identifying fields for the healthy HealthEvent:** section 1.2 covers how the syslog-health-monitor will need to send healthy events in response to GPU resets. These events will include all identifying fields which will allow the fault-quarantine-module to remove the corresponding unhealthy event from the quarantineHealthEvent annotation. This module already includes support for mapping healthy events to specific entities or to all entities for a given check.

## 3. Node-Drainer-Module

**3.1 How will this component handle events requiring a GPU reset?**

* **Determine whether a full or partial drain is required:** if the recommended action is COMPONENT_RESET, the node-drainer-module will only need to execute a partial drain against the given node. Previously, this component would drain all pods running on the node needing remediation. For partial drains, we will only drain the single pod which has exclusive access to the given GPU.
    * Once a drain is completed, we will derive whether a partial or full drain was completed as part of the remediation action rather than add a new state to the HealthEvent.
* **Determine if draining has already completed:** there’s existing logic in the node-drainer-module to prevent executing multiple drains against a node if the node was already quarantined and had a drain successfully complete.
    * If a full drain has successfully completed, we will skip subsequent full drains and partial drains. If a partial drain completed previously, we will require that the follow-up full or partial drain must complete. We could add complexity to allow partial drains for a given GPU UUID to be skipped, however, we will punt on this functionality.
* **Discover the mapping between GPUs and pods:** the Kubernetes API does not currently expose the mapping for devices allocated through pod requests. Since the existing node-drainer-module discovers all pods for a given node through the Kubernetes API, we will need to discover which pods have access to the GPU listed in the impacted entities of our HealthEvent.
    * **Long-term approach on K8s versions >= 1.35**: rely on the ResourceHealthStatus feature which exposes the health of devices allocated to pods as a container status. This container status includes the GPU UUID and this approach works for both [device plugins](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/#device-plugin-and-unhealthy-devices) and [DRA](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#device-health-monitoring). The ResourceHealthStatus feature gate was added as an alpha feature in 1.31 and targeted will be graduated to a beta release in 1.35 (requires enabling the feature gate on both the kube-apiserver and kubelet).
        * Example container status:

```
status:
  containerStatuses:
  - name: my-gpu-intensive-container
    # ... other container statuses
    allocatedResourcesStatus:
    - name: "claim:my-gpu-claim"
      resources:
      - resourceID: "example.com/gpu-a1b2-c3d4"
        health: "Unhealthy"
```

* **Short-term approach on K8s versions < 1.35:** to enable GPU resets on currently supported K8s versions, we will need to rely on the Kubelet PodResourcesLister gRPC service to expose this mapping ([ref](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/#monitoring-device-plugin-resources)). This gRPC service is only exposed on a local Unix socket and is not included as part of the Kubelet HTTPS server. According to K8s documentation, this is the recommended approach for looking up the mapping between pods and failed devices. Additionally, this approach is supported by both device plugins and DRA.
    * Since NVSentinel does not support GPU-sharing, each partial drain will only need to drain 0 or 1 pods. Note that a given pod could be requesting several GPUs and if any of its GPUs need reset, it will need to be drained.
    * This approach is currently leveraged by the dcgm-exporter for determining the mapping between devices and pods ([ref](https://github.com/NVIDIA/dcgm-exporter/blob/main/internal/pkg/transformation/kubernetes.go#L415)).
    * See [Appendix-2](#Appendix) for additional methods to discover the GPU to pod mapping.

Example PodResourcesLister output:

```
    {
      "name": "gpu-job-r9g6j",
      "namespace": "default",
      "containers": [
        {
          "name": "gpu-container",
          "devices": [
            {
              "resourceName": "nvidia.com/gpu",
              "deviceIds": [
                "GPU-07bf6b30-9192-8167-70ae-909c383d543a"
              ],
              "topology": {
                "nodes": [
                  {
                    "ID": "1"
                  }
                ]
              }
            }
          ]
        }
      ]
    }
```

* **Invoking PodResourcesLister** **from the node-drainer-module:** since our mapping is only available from the PodResourcesLister Unix socket on the given node, we will need to create a mechanism to invoke this local gRPC service. Rather than directly invoke this API on demand, we will expose an up-to-date mapping between GPUs and pods by writing a GPUDevices annotation to each pod object using the output from the PodResourcesLister API.
    * We will add a reconcile loop to the metadata collector, periodically invoke the local PodResourcesLister service, and update the GPUDevices annotation if there was a change from the latest observed state for the given pod. The node-drainer-module will not be able to derive if a given mapping is stale unless we periodically update a heart beat indicating that the mapping is up-to-date. However, we do not want to issue a large amount of updates to pod objects that are not required.
    * See [Appendix-3](#Appendix) for alternative approaches for invoking PodResourcesLister in the node-drainer-module.
    * Example devices annotation (note that we do not need to track this information per-container):

```
"devices": [
  {
    "resourceName": "nvidia.com/gpu",
    "deviceIds": [
      "GPU-07bf6b30-9192-8167-70ae-909c383d543a"
    ]
  }
]
```

* **Janitor handling of unrecognized processes:** if a pod exists with access to the given GPU we’re resetting, either a PID created by the pod will have a context on the GPU which would require Janitor to remove the process or a process in a pod could attempt to create a context on the GPU while it is being reset and run into an application-level error. We need to decide whether we force drain the pod or fail the reset.
    * Note that pods which bypass the device plugin to gain access to GPUs by leveraging the NVIDIA_VISIBLE_DEVICES environment variable or through HostPath volumes will not be tracked in PodResourcesLister output so we will need to handle this case on the Janitor side (even outside of node-drainer-module bugs).


**3.2 How will this component handle events indicating a GPU reset occurred?**

* **Canceling partial drains:** an unquarantined event will cause any in-progress full or partial drains to be cancelled. Note that we do not support partial unquarantines where we would only cancel a drain for a single event which recovered.
    * The fault-quarantine-module does not send events to the node-drainer-module nor fault-remediation-module for events which were removed from the quarantineHealthEvent annotation but did not result in uncordoning.

## 4 Fault-Remediation-Module

**4.1 How will this component handle events requiring a GPU reset?**

* **Map the COMPONENT_RESET action to the GPUReset custom resource:** currently, the fault-remediation-module supports a single equivalence group for called restart which maps COMPONENT_RESET, RESTART_VM, and RESTART_BM to the RebootNode custom resource. We will add a new equivalence group called reset which maps the COMPONENT_RESET action to the GPUReset custom resource.
* **Construct new equivalence groups for each GPU being reset:** an equivalence group represents remediation actions which result in the same action being taken against a node. For GPUResets, the action is only equivalent if it maps to exactly the same set of GPUs being reset. As a result, we will need to construct different equivalence groups depending on the specific GPUs being reset.
    * This can be accomplished by naming the equivalence group like reset-<GPU-UUID> which will ensure that the same GPU cannot be reset concurrently. Recall that each HealthEvent will only include a single GPU.
* **Update the latestFaultRemediationState annotation:** if a successful GPUReset occurs, this state will need to be tracked as part of the existing latestFaultRemediationState annotation. In addition to the per-GPU equivalence group, the maintenance custom resource type will need to be stored as part of the annotation or looked up dynamically from the equivalence group name since the underlying resource could be a RebootNode or GPUReset.

```
{
  "equivalenceGroups": {
    "restart": {
      "kind": "RebootNode",
      "maintenanceCR": "maintenance-10.54.108.38-6915f6bd0ecfb2f4a6c75575",
      "createdAt": "2025-11-13T17:18:32.163469826Z"
    },
    "reset-GPU-455d8f70-2051-db6c-0430-ffc457bff834": {
      "kind": "GPUReset",
      "maintenanceCR": "maintenance-10.54.108.38-6915f6bd0ecfb2f4a6c75575",
      "createdAt": "2025-11-13T17:19:32.163469826Z"
    },
    "reset-GPU-938m2ds2-2051-db6c-0430-ffc457bff834": {
      "kind": "GPUReset",
      "maintenanceCR": "maintenance-10.54.108.38-6915f6bd0ecfb2f4a6c75575",
      "createdAt": "2025-11-13T17:20:32.163469826Z"
    },
  }
}
```

**4.2 How will this component handle events indicating a GPU reset occurred?**

* **Remove the latestFaultRemediationState annotation:** we will rely on the existing logic to remove the given annotation from the node when it receives an unquarantined event. Recall that we do not support partial unquarantines where we would only need to remove an equivalence group for a specific GPUReset (see section 3.2 for more context).

**4.3 How will this component support concurrent GPU resets and reboots?**

* **Ensure that concurrent remediation actions cannot occur on the same node:** with the introduction of GPUResets, the fault-remediation-module and Janitor will need to coordinate to prevent concurrent maintenance requests on the same node. The only module which currently includes functionality to support long-running workflows is the node-drainer-module (with functionality to continually poll the drain status, reflect an in-progress state, and resume in-progress drains on start-up).
* **Implement serial node-level operations in Janitor:** there are 2 high-level approaches for assuring that multiple maintenance requests cannot execute on the same node concurrently. We can either add implement node-level locking between the existing controllers or add a single-threaded global controller.
    * See [Appendix-4](#Appendix) for alternative approaches for preventing concurrent maintenance operations on the same node.
* **Add a distributed lock:** to allow the existing Janitor controllers to run concurrently, we would need to add coordination between the controllers to not schedule maintenance on the same node. We can avoid coordination between controller Go routines by leveraging distributed locks in the K8s API. A controller can acquire a lock by creating a K8s object (or by writing a field to an existing object) which references the node under maintenance. After maintenance completes, the lock can be released by deleting the resource (or by writing again to the given field).
    * This approach does not need to re-build the lock state after a component crash.
    * We can add an OwnerReference pointing to the maintenance resource on our distributed lock so that the lock is deleted in case a user manually deletes this resource.
    * See [Appendix-5](#Appendix) for alternative approaches for implementing serial node-level operations in Janitor.
* **Case 1: RebootNode -> RebootNode:** this case already exists today where we will skip the second RebootNode request if the first is still in progress. The second reboot request will be executed if the first has reached a terminal state.
* **Case 2: GPUReset -> GPUReset:** if we have multiple GPUResets and the first GPUReset is still in-progress, we should skip the second GPUReset if it’s for the same equivalence group. If the second GPUReset is in a terminal status or if it’s for a different equivalence group, we should execute the remediation.
* **Case 3: GPUReset -> RebootNode:** if a GPUReset is in-progress or in a terminal status, we should always allow a follow-up RebootNode to execute.
    * If we go with option 1 above to implement node-level locking in Janitor, it’s possible that a GPUReset would occur after the RebootNode even if the GPUReset was created first (for example, if Janitor was not running and then started after both custom resources were already created).
* **Case 4: RebootNode -> GPUReset:** if a RebootNode is in-progress, we should skip any incoming remediations that need a GPUReset. Note that this is a unique situation where the GPUReset group will be counted as part of the RebootNode group. If the RebootNode has completed, we should allow the GPUReset.

## Appendix

**Appendix-1: Alternative GPU reset detection mechanisms**

* **Watch GPUReset objects:** the syslog-health-monitor can watch for GPUReset objects created by NVSentinel that have a successful terminal status and send a healthy event. The monitor will need to ensure it doesn’t emit multiple healthy events for the same GPUReset object, so this state will need to be persisted as a status condition.
* **Add an API to send healthy HealthEvents:** we could add a new API to allow remediation components to signal health-monitors that a given remediation action has occurred. This would be useful in any situation where the health-monitor does not have a reliable signal to detect that it should create a healthy HealthEvent for one of the checks that it owns.
    * This approach also comes with the benefit of not needing to re-implement the same remediation detection mechanisms across multiple health-monitors.

**Appendix-2: GPU to pod mapping discovery alternatives**

* **Use nvidia-smi to look up PIDs per GPU:** nvidia-smi and other tools provide PIDs for any processes with a context on a given GPU. It would be possible to lookup the corresponding pod for that PID by mapping that PID to a container using containerd. This isn’t sufficient since it’s possible that a pod might have a GPU mounted in its container but that process might not yet have created a context on that GPU (for example if the pod crashes and restarts or is initially starting when we call nvidia-smi).
* **Check the GPUs visible inside a given pod:** most pods do not explicitly set the NVIDIA_VISIBLE_DEVICES environment variable in its pod spec and rely on a request to receive a GPU from the device plugin. As a result, the pod object would not have sufficient information to determine which GPU it was eventually allocated (until the ResourceHealthStatus feature is enabled to display this information as a container status). To lookup the GPU device the pod was allocated, we would have to check one of the following:
    * Exec into the pod and check GPU device mounts visible in the pod (could run nvidia-smi inside the pod to view this directly).
    * Exec into the pod and check the value of the NVIDIA_VISIBLE_DEVICES environment variable.
    * Check check devices are allocated to a given container using a container runtime API (crictl inspect).
* **Don’t use the device plugin or DRA to allocate GPUs:** rather than use Kubernetes features for allocating devices to pods, an external controller can manage GPU health checking, updating GPU capacity for each node, and binding pods to given GPUs. The Dynamo team is exploring manually setting NVIDIAVISIBLE_DEVICES in all pods to control which GPUs a given pod is assigned. This offloads complexity normally handled by DRA or the device plugin, however, it prevents the need to fully cordon nodes when a given GPU is unhealthy.

**Appendix-3: Alternative approaches for invoking PodResourcesLister in the node-drainer-module**

* **Call PodResourcesLister from the syslog-health-monitor**: the benefit of this approach is that the given syslog-health-monitor could call the PodResourcesLister locally while it is generating the unhealthy event on the given node. The downside of this approach is that this mapping is looked up prior to cordoning so the GPU to pod assignment for a given node could be stale by the time the node-drainer-module consumes the HealthEvent.
    * To determine if our mapping is stale, we can check if the given pod is still running by calling the K8s API. If the pod is still running, we can safely drain it. If there was no pod using the GPU originally or if we detect a stale mapping, we can either proceed with a full drain or terminate the break-fix workflow.
* **Wrap the PodResourcesLister socket with a new pod-device-lister component:** we can add a new component called the pod-device-lister which exposes an HTTPS server that wraps the PodResourcesLister socket. Rather than make a K8s API call for the set of pods running on the given node, the node-drainer-module could directly call this new service running as a DaemonSet pod.
    * **Direct or proxy request:** this network call could be implemented by reaching out to the given PodIP directly or through a K8s API proxy call. For EKS, the default security group does not allow outbound proxy traffic from cross-account ENIs to non-Kubelet ports so we would have to confirm this works across all CSPs.
* **Ruled-out options:**
    * Delay looking up the GPU to pod mapping until Janitor executes the GPUReset job. This would require duplicating drain functionality into Janitor.
    * Call the dcgm-exporter metrics endpoint and derive the GPU to pod mapping exposed in Prometheus metrics.
    * Always drain all pods on the node. This would only be a feasible option if a majority of pods request all GPUs on a given node which is not the case for our customers.
    * Wait to support GPU reset until K8s version 1.35 when the ResourceHealthStatus feature is enabled to expose the devices used by each pod through container statuses.

**Appendix-4: Alternative approaches for preventing concurrent maintenance operations on the same node.**

* **Add an in-progress state to the fault-remediation-module**: this module could be modified to have a similar architecture to the node-drainer-module where we support long-polling through a work queue, reflect an in-progress status, and allow events to finish their processing if the module restarts.
    * The fault-remediation module is single-threaded so we would not need to coordinate between threads to ensure only 1 event is scheduled at a time, however, the module would be blocked from processing events from different nodes while it is waiting.

**Appendix-5: Alternative approaches for implementing serial node-level operations in Janitor**

* **Add an in-memory lock:** we can leverage a shared data structure between the controllers to implement node-level locking. In this case, a controller must acquire an in-memory lock before processing a maintenance request and then release the lock after the request reaches a terminal status. If the controller crashes when with an in-progress maintenance, it will need to re-build the in-memory lock from these custom resources.
    * We will need to detect manual deletion of maintenance resources to ensure the in-memory lock is released.
* **Implement a single-threaded global controller:** we can add a global controller scheduler to Janitor which watches all custom resource types (RebootNode, GPUReset, and TerminateNode) and determines whether a given node has an in-progress maintenance prior to processing a request. A single-threaded controller removes the need for implementing a lock on any node which needs maintenance.
    * If the controller crashes when with an in-progress maintenance, it will need to re-build its view of which nodes are currently under maintenance.
    * We could simplify this approach by condensing all maintenance types into a single custom resource which would be processed by this controller. If we ever want to support concurrent processing of custom resources (setting MaxConcurrentReconciles > 1), we will need to implement a node-level locking mechanism described in options 1 or 2.
* **Ordering guarantees:** if maintenance resources contain a creation timestamp, we will not be able to guarantee they are executed in order of creation timestamp with the Janitor locking approach.
    * In most cases, maintenance requests will be executed in the order created by NVSentinel, however, if Janitor is not running when these resources are created, they could be applied in any serial ordering. We will punt on this complexity because it is only relevant when a GPUReset is followed by a RebootNode.
    * Two solutions to this issue would be for Janitor to schedule maintenance in order of creation timestamp or for NVSentinel to wait on remediation actions to go in-progress prior to scheduling the next remediation.