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

// evaluatePodPolicyActions lists each namespace once to group eligible pods by mode,
// then checks the entire selected scope again before reporting success.
func (e *NodeDrainEvaluator) evaluatePodPolicyActions(ctx context.Context,
	healthEvent model.HealthEventWithStatus, partialDrainEntity *protos.Entity) (*DrainActionResult, error) {
	nodeName := healthEvent.HealthEvent.NodeName

	allNamespaces, err := e.informers.GetNamespacesMatchingPattern(ctx, "*", e.config.SystemNamespaces, nodeName)
	if err != nil {
		return nil, fmt.Errorf("find namespaces for pod drain policies: %w", err)
	}

	force := healthEvent.HealthEvent.GetDrainOverrides().GetForce()

	podsByMode, err := e.listPodsByMode(allNamespaces, nodeName, partialDrainEntity, force)
	if err != nil {
		return nil, err
	}

	for _, candidate := range []struct {
		mode    config.EvictMode
		action  DrainAction
		timeout time.Duration
	}{
		{config.ModeImmediateEvict, ActionEvictImmediate, e.config.EvictionTimeoutInSeconds.Duration},
		{config.ModeDeleteAfterTimeout, ActionEvictWithTimeout,
			time.Duration(e.config.DeleteAfterTimeoutMinutes) * time.Minute},
		{config.ModeAllowCompletion, ActionCheckCompletion, 0},
	} {
		pods := podsByMode[candidate.mode]
		if len(pods) == 0 {
			continue
		}

		filter := e.podModeFilter(candidate.mode, force)
		if candidate.mode == config.ModeImmediateEvict &&
			e.informers.CheckIfObservedPodsAreEvictedInImmediateMode(ctx, allNamespaces, nodeName,
				candidate.timeout, partialDrainEntity, pods, filter) {
			continue
		}

		return &DrainActionResult{
			Action: candidate.action, Namespaces: allNamespaces, Timeout: candidate.timeout,
			PartialDrainEntity: partialDrainEntity, PodFilter: filter,
		}, nil
	}

	// A cache update may change the selected scope after the initial observation.
	// Refresh it before completing the drain, including after force deletion.
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

	return &DrainActionResult{Action: ActionUpdateStatus, Status: model.StatusSucceeded}, nil
}

// listPodsByMode groups one observation of each namespace by its first matching policy.
func (e *NodeDrainEvaluator) listPodsByMode(namespaces []string, nodeName string,
	partialDrainEntity *protos.Entity, force bool) (map[config.EvictMode][]*v1.Pod, error) {
	podsByMode := make(map[config.EvictMode][]*v1.Pod)

	for _, namespace := range namespaces {
		pods, err := e.informers.FindEvictablePodsInNamespaceAndNode(namespace, nodeName,
			partialDrainEntity, e.podModeFilter("", force))
		if err != nil {
			return nil, fmt.Errorf("list pods for drain policies in namespace %q: %w", namespace, err)
		}

		for _, pod := range pods {
			mode, matches := e.podPolicies.Match(pod)
			if !matches {
				continue
			}

			if force {
				mode = config.ModeImmediateEvict
			}

			podsByMode[mode] = append(podsByMode[mode], pod)
		}
	}

	return podsByMode, nil
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
