// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package reconciler

import (
	"context"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
	"k8s.io/client-go/kubernetes"
)

// K8sClientInterface defines the methods used by Reconciler from k8sClient
type K8sClientInterface interface {
	GetNodeAnnotations(ctx context.Context, nodeName string) (map[string]string, error)
	GetNodesWithAnnotation(ctx context.Context, annotationKey string) ([]string, error)
	TaintAndCordonNodeAndSetAnnotations(ctx context.Context, nodeName string,
		taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error
	UnTaintAndUnCordonNodeAndRemoveAnnotations(ctx context.Context, nodeName string,
		taints []config.Taint, isUncordon bool, annotationKeys []string, labelsToRemove []string,
		labelMap map[string]string) error
	UpdateNodeAnnotations(ctx context.Context, nodeName string, annotations map[string]string) error
	GetK8sClient() kubernetes.Interface
	EnsureCircuitBreakerConfigMap(ctx context.Context, name, namespace string, initialStatus string) error
	ReadCircuitBreakerState(ctx context.Context, name, namespace string) (string, error)
	WriteCircuitBreakerState(ctx context.Context, name, namespace, status string) error
	GetTotalGpuNodes(ctx context.Context) (int, error)
}
