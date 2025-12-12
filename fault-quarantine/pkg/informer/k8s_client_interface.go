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

package informer

import (
	"context"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	v1 "k8s.io/api/core/v1"
)

// K8sClientInterface defines the methods used by Reconciler from k8sClient
type K8sClientInterface interface {
	QuarantineNodeAndSetAnnotations(ctx context.Context, nodeName string,
		taints []config.Taint, isCordon bool, annotations map[string]string, labelMap map[string]string) error
	UnQuarantineNodeAndRemoveAnnotations(ctx context.Context, nodeName string,
		taints []config.Taint, annotationKeys []string, labelsToRemove []string,
		labelMap map[string]string) error
	HandleManualUncordonCleanup(ctx context.Context, nodeName string, taintsToRemove []config.Taint,
		annotationsToRemove []string, annotationsToAdd map[string]string, labelsToRemove []string) error
	UpdateNode(ctx context.Context, nodeName string, updateFn func(*v1.Node) error) error
	EnsureCircuitBreakerConfigMap(ctx context.Context, name, namespace string, initialStatus breaker.State) error
	ReadCircuitBreakerState(ctx context.Context, name, namespace string) (breaker.State, error)
	WriteCircuitBreakerState(ctx context.Context, name, namespace string, state breaker.State) error
	ReadCursorMode(ctx context.Context, name, namespace string) (breaker.CursorMode, error)
	WriteCursorMode(ctx context.Context, name, namespace string, mode breaker.CursorMode) error
	GetTotalNodes(ctx context.Context) (int, error)
}
