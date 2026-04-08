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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var profileGVR = schema.GroupVersionResource{
	Group:    "nvsentinel.nvidia.com",
	Version:  "v1alpha1",
	Resource: "preflightprofiles",
}

// PreflightProfileSpec defines the desired preflight check configuration.
type PreflightProfileSpec struct {
	InitContainers []PreflightContainerOverride `json:"initContainers,omitempty"`
}

// PreflightContainerOverride defines per-pod overrides for a single preflight
// init container. Matches by name against the configured init containers.
type PreflightContainerOverride struct {
	Name    string          `json:"name"`
	Enabled *bool           `json:"enabled,omitempty"`
	Env     []corev1.EnvVar `json:"env,omitempty"`
}

// ProfileReader reads PreflightProfile CRDs from the Kubernetes API.
type ProfileReader interface {
	GetProfile(ctx context.Context, namespace, name string) (*PreflightProfileSpec, error)
}

// K8sProfileReader reads PreflightProfile CRDs using the dynamic client.
type K8sProfileReader struct {
	client dynamic.Interface
}

// NewK8sProfileReader creates a ProfileReader backed by the Kubernetes API.
func NewK8sProfileReader(client dynamic.Interface) ProfileReader {
	return &K8sProfileReader{client: client}
}

// GetProfile fetches a PreflightProfile CRD by namespace and name.
func (r *K8sProfileReader) GetProfile(ctx context.Context, namespace, name string) (*PreflightProfileSpec, error) {
	obj, err := r.client.Resource(profileGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get PreflightProfile %s/%s: %w", namespace, name, err)
	}

	specRaw, found := obj.Object["spec"]
	if !found {
		return &PreflightProfileSpec{}, nil
	}

	specBytes, err := json.Marshal(specRaw)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal profile spec: %w", err)
	}

	var spec PreflightProfileSpec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return nil, fmt.Errorf("failed to parse profile spec: %w", err)
	}

	return &spec, nil
}
