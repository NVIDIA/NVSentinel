// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package webhook

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// draLookupTimeout bounds all ResourceClaim and ResourceClaimTemplate reads
// for one pod. It stays well under the webhook timeout (10 s in the chart), so
// a slow API server skips the checks for the pod instead of rejecting it.
const draLookupTimeout = 3 * time.Second

// requestsDRAGPU reports whether one of the pod's resource claims requests a
// device from the GPU DeviceClass. It fails open: a claim or template
// that cannot be read counts as non-GPU, so the pod is admitted without checks
// rather than rejected, or given checks that cannot see its GPUs.
func (i *Injector) requestsDRAGPU(ctx context.Context, pod *corev1.Pod) bool {
	if i.draReader == nil || len(pod.Spec.ResourceClaims) == 0 {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, draLookupTimeout)
	defer cancel()

	for _, podClaim := range pod.Spec.ResourceClaims {
		requests, err := i.claimRequests(ctx, pod.Namespace, podClaim)
		if err != nil {
			logClaimLookupError(ctx, pod, podClaim, err)

			continue
		}

		if hasGPURequest(requests) {
			slog.Info("Pod requests GPUs through DRA",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"claim", podClaim.Name)

			return true
		}
	}

	return false
}

// claimRequests reads the device requests behind one pod resource claim, from
// its ResourceClaimTemplate or from the ResourceClaim it names.
func (i *Injector) claimRequests(
	ctx context.Context,
	namespace string,
	podClaim corev1.PodResourceClaim,
) ([]resourcev1.DeviceRequest, error) {
	switch {
	case podClaim.ResourceClaimTemplateName != nil:
		key := client.ObjectKey{Namespace: namespace, Name: *podClaim.ResourceClaimTemplateName}

		var tmpl resourcev1.ResourceClaimTemplate
		if err := i.draReader.Get(ctx, key, &tmpl); err != nil {
			return nil, fmt.Errorf("get ResourceClaimTemplate %s: %w", key, err)
		}

		return tmpl.Spec.Spec.Devices.Requests, nil
	case podClaim.ResourceClaimName != nil:
		key := client.ObjectKey{Namespace: namespace, Name: *podClaim.ResourceClaimName}

		var claim resourcev1.ResourceClaim
		if err := i.draReader.Get(ctx, key, &claim); err != nil {
			return nil, fmt.Errorf("get ResourceClaim %s: %w", key, err)
		}

		return claim.Spec.Devices.Requests, nil
	default:
		return nil, nil
	}
}

// logClaimLookupError logs a failed claim read. A missing object is a user
// error and logs at warn; anything else (RBAC, timeout, API not served) is an
// operator problem that silently removes the checks, so it logs at error.
func logClaimLookupError(ctx context.Context, pod *corev1.Pod, podClaim corev1.PodResourceClaim, err error) {
	level := slog.LevelError
	if apierrors.IsNotFound(err) {
		level = slog.LevelWarn
	}

	slog.Log(ctx, level, "Failed to read DRA claim, treating it as non-GPU",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"claim", podClaim.Name,
		"error", err)
}

// hasGPURequest reports whether any request asks for the GPU device class.
func hasGPURequest(requests []resourcev1.DeviceRequest) bool {
	return slices.ContainsFunc(requests, isGPURequest)
}

// isGPURequest reports whether the request always allocates a GPU. The GPU
// class is the DeviceClass for full GPUs, which the NVIDIA DRA driver names
// after itself (model.GPUDRADriverName). MIG devices (mig.nvidia.com) are not
// detected. An adminAccess request does not count: it gives monitoring access
// to devices that can already be in use by other pods. A firstAvailable request
// counts only when every alternative is the GPU class, because the scheduler
// can pick any of them.
func isGPURequest(req resourcev1.DeviceRequest) bool {
	if req.Exactly != nil {
		adminAccess := req.Exactly.AdminAccess != nil && *req.Exactly.AdminAccess

		return !adminAccess && req.Exactly.DeviceClassName == model.GPUDRADriverName
	}

	if len(req.FirstAvailable) == 0 {
		return false
	}

	for _, sub := range req.FirstAvailable {
		if sub.DeviceClassName != model.GPUDRADriverName {
			return false
		}
	}

	return true
}
