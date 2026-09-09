// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

package evaluator

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/informers"
)

// evaluatePodPolicyActions applies pod policies and checks
// the entire selected scope again before reporting success after per-mode checks.
func (e *NodeDrainEvaluator) evaluatePodPolicyActions(ctx context.Context,
	healthEvent model.HealthEventWithStatus, partialDrainEntity *protos.Entity) (*DrainActionResult, error) {
	nodeName := healthEvent.HealthEvent.NodeName

	allNamespaces, err := e.informers.GetNamespacesMatchingPattern(ctx, "*", e.config.SystemNamespaces, nodeName)
	if err != nil {
		return nil, fmt.Errorf("find namespaces for pod drain policies: %w", err)
	}

	force := healthEvent.HealthEvent.GetDrainOverrides().GetForce()

	filters := make(map[config.EvictMode]informers.PodFilter)
	for _, mode := range []config.EvictMode{
		config.ModeImmediateEvict, config.ModeDeleteAfterTimeout, config.ModeAllowCompletion,
	} {
		filters[mode] = e.podModeFilter(mode, force)
	}

	action := e.getAction(ctx, namespaces{
		immediateEvictionNamespaces:  allNamespaces,
		deleteAfterTimeoutNamespaces: allNamespaces,
		allowCompletionNamespaces:    allNamespaces,
		podFilters:                   filters,
	}, nodeName, partialDrainEntity)
	if action.Action != ActionUpdateStatus || action.Status != model.StatusSucceeded {
		return action, nil
	}

	// A label update can move a pod into a mode that was already checked. Before
	// completing the drain, check the selected scope without separating modes.
	selected := e.podModeFilter("", force)
	for _, namespace := range allNamespaces {
		pods, err := e.informers.FindEvictablePodsInNamespaceAndNode(namespace, nodeName, partialDrainEntity, selected)
		if err != nil {
			return nil, fmt.Errorf("check remaining selected pods: %w", err)
		}

		if len(pods) > 0 {
			return &DrainActionResult{Action: ActionWait, WaitDelay: time.Second}, nil
		}
	}

	return action, nil
}

// podModeFilter selects pods assigned to mode; an empty mode selects the whole drain scope.
// Force changes matched pods to Immediate without including otherwise unmatched pods.
func (e *NodeDrainEvaluator) podModeFilter(mode config.EvictMode, force bool) informers.PodFilter {
	return func(pod *v1.Pod) bool {
		selected, matches := e.podPolicies.Match(pod)
		if force && matches {
			selected = config.ModeImmediateEvict
		}

		return matches && (mode == "" || selected == mode)
	}
}
