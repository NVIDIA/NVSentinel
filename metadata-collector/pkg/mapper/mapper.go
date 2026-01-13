// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mapper

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
)

const (
	jsonPatchPath  = "/metadata/annotations/dgxc.nvidia.com~1devices"
	mergePatchPath = `{"metadata":{"annotations":{%q:%q}}}`
)

type PodDeviceMapper interface {
	UpdatePodDevicesAnnotations() (int, error)
}

type podDeviceMapper struct {
	ctx context.Context

	kubeletHTTPSClient KubeletHTTPSClient
	kubeletGRPCClient  KubeletGRPClient
	kubernetesClient   kubernetes.Interface
}

func NewPodDeviceMapper(ctx context.Context) (PodDeviceMapper, error) {
	httpsClient, err := NewKubeletHTTPSClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("got an error creating Kubelet HTTPS client: %w", err)
	}

	grpcClient, err := NewKubeletGRPClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("got an error creating Kubelet gRPC client: %w", err)
	}

	clusterConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("got an error creating in-cluster config: %w", err)
	}

	k8sClient, err := kubernetes.NewForConfig(clusterConfig)
	if err != nil {
		return nil, fmt.Errorf("got an error creating in-cluster client: %w", err)
	}

	return &podDeviceMapper{
		ctx:                ctx,
		kubeletHTTPSClient: httpsClient,
		kubeletGRPCClient:  grpcClient,
		kubernetesClient:   k8sClient,
	}, nil
}

/*
This function will add a devices annotation to all pods running on the given node which have been allocated a GPU
device. The annotation is named dgxc.nvidia.com/devices with keys as the device resource name, either nvidia.com/gpu or
nvidia.com/pgpu, and values a list of GPU UUIDs for the corresponding devices.

Example annotation:

	"annotations": {
	  "dgxc.nvidia.com/devices": "{\"devices\":{\"nvidia.com/gpu\":[\"GPU-455d8f70-2051-db6c-0430-ffc457bff834\"]}},
	  ...
	}

It will follow these steps:

1. List all pods from the Kubelet's HTTPS /pods endpoint. This allows us to determine which pods need the devices
annotation added, updated, or removed. Note that this endpoint is bound to localhost and eth0 on the given node. The
/pods Kubelet endpoint is equivalent to the following K8s API request:

- /api/v1/pods?fieldSelector=spec.nodeName=<node-name>

Using the local Kubelet endpoint will significantly reduce load on the control plane from all metadata-collector
daemonset pods with the tradeoff that the endpoint has to be periodically scraped rather than watched. Since we have to
periodically scrape the PodResourcesLister endpoint in the next step, we will be consistent in how we're discovering
both the pod list for this node and the device to pod allocation by listing from a local endpoint on the node.

2. Discover the pod to device mapping using the Kubelet's gRPC PodResourcesLister endpoint. This local gRPC service is
exposed on a local Unix socket. This endpoint exposes devices allocated to pods through either resource requests through
device plugins or resource claims through DRA. Starting in v1.35, we can rely on the ResourceHealthStatus feature to
expose this same device mapping through container statuses and at that point we'll be able to remove this component
from the metadata-collector.

It is possible for the PodResourcesLister to include device allocations to pods with a terminal status. As a result,
completed or failed pods may continue to expose devices as allocated which are actually freed until 1.34:
https://github.com/kubernetes/kubernetes/pull/132028. This isn't an issue because the node-drainer-module ignores pods
in this state. If a new pod is allocated a the devices that were previously assigned to a completed pod, the devices
will be removed from the completed pod and added to the new pod.

3. Compare each pod object to the current set of allocated devices and determine if any pods need a device annotation
updated, added, or removed. We will issue a patch request which updates the dgxc.nvidia.com/devices annotation to the
desired value or remove it. To prevent unnecessary writes, we will only update this annotation if it is modified.
This comes with the tradeoff that consumers of this annotation (node-drainer-module) will not be able to detect stale
annotations outside of the annotation not existing for a pod that is requesting GPUs.
*/
func (mapper *podDeviceMapper) UpdatePodDevicesAnnotations() (int, error) {
	pods, err := mapper.kubeletHTTPSClient.ListPods()
	if err != nil {
		return 0, fmt.Errorf("error listing pods from Kubelet pods endpoint: %w", err)
	}

	devicesPerPod, err := mapper.kubeletGRPCClient.ListPodResources()
	if err != nil {
		return 0, fmt.Errorf("error listing pod resources from Kubelet ListPodResources service: %w", err)
	}

	numPodDevicesAnnotationsModified := 0

	for _, pod := range pods {
		patchType, patchBytes, err := mapper.getPatchRequestForPod(pod, devicesPerPod)
		if err != nil {
			return numPodDevicesAnnotationsModified, fmt.Errorf("error getting patch for pod: %w", err)
		}

		if len(patchBytes) > 0 {
			err = mapper.patchPodWithRetry(pod.GetName(), pod.GetNamespace(), patchType, patchBytes)
			if err != nil {
				return numPodDevicesAnnotationsModified, fmt.Errorf("error patching device annotation: %w", err)
			}

			numPodDevicesAnnotationsModified++
		}
	}

	return numPodDevicesAnnotationsModified, nil
}

func (mapper *podDeviceMapper) getPatchRequestForPod(pod corev1.Pod,
	devicesPerPod map[string]*model.DeviceAnnotation) (types.PatchType, []byte, error) {
	podKey := pod.GetNamespace() + "/" + pod.GetName()
	deviceAnnotation, podHasGPUDevices := devicesPerPod[podKey]
	existingDeviceAnnotation, podHasDeviceAnnotation := pod.Annotations[model.PodDeviceAnnotationName]

	var patchType types.PatchType

	var patchBytes []byte

	var err error

	if podHasGPUDevices {
		var deviceAnnotationJSON []byte

		deviceAnnotationJSON, err := json.Marshal(deviceAnnotation)
		if err != nil {
			return "", nil, fmt.Errorf("error marshalling device annotation: %w", err)
		}

		deviceAnnotationJSONStr := string(deviceAnnotationJSON)
		annotationUpdateRequired := podHasDeviceAnnotation && deviceAnnotationJSONStr != existingDeviceAnnotation
		annotationAddRequired := !podHasDeviceAnnotation

		if annotationUpdateRequired || annotationAddRequired {
			slog.Info("Updating devices annotation for pod", "podKey", podKey, "annotation", deviceAnnotationJSONStr)

			// We can't use a JSONPatchType for the following patch because the apiserver will not recursively create
			// nested objects for a JSON patch input. In other words, if a pod doesn't have an annotations object,
			// we can't do a patch for "/metadata/annotations/dgxc.nvidia.com~1devices" without first adding the
			// object for "/metadata/annotations". As a result, we will use a MergePatchType to add/update the
			// annotation but a JSONPatchType to remove the annotation.
			patchType = types.MergePatchType
			patchBytes = []byte(fmt.Sprintf(mergePatchPath, model.PodDeviceAnnotationName, string(deviceAnnotationJSON)))
		}
	} else if podHasDeviceAnnotation { // !podHasGPUDevices
		slog.Info("Removing devices annotation for pod since it no longer has devices allocated", "podKey", podKey)

		patchType = types.JSONPatchType
		patch := []map[string]string{
			{
				"op":   "remove",
				"path": jsonPatchPath,
			},
		}

		patchBytes, err = json.Marshal(patch)
		if err != nil {
			return "", nil, fmt.Errorf("error marshalling patch: %w", err)
		}
	}

	return patchType, patchBytes, nil
}

func (mapper *podDeviceMapper) patchPodWithRetry(podName string, namespace string, patchType types.PatchType,
	patch []byte) error {
	return retry.OnError(retry.DefaultRetry, isRetryableError, func() error {
		_, err := mapper.kubernetesClient.CoreV1().Pods(namespace).Patch(mapper.ctx, podName, patchType, patch,
			metav1.PatchOptions{})
		if err != nil && isRetryableError(err) {
			slog.Warn("Retryable error patching pod annotation. Retrying...",
				"pod", podName,
				"error", err)
		}

		if err != nil {
			return fmt.Errorf("failed to patch pod %s: %w", podName, err)
		}

		return nil
	})
}

func isRetryableError(err error) bool {
	// Conflict errors (resource version conflicts)
	if errors.IsConflict(err) {
		return true
	}

	// Server timeout or rate limiting
	if errors.IsServerTimeout(err) || errors.IsTooManyRequests(err) {
		return true
	}

	// Temporary network errors
	if errors.IsTimeout(err) || errors.IsServiceUnavailable(err) {
		return true
	}

	return false
}
