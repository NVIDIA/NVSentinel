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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/informers"
)

type policyInformers struct {
	InformersInterface
	namespaces     []string
	pods           map[string][]*v1.Pod
	listCalls      map[string]int
	beforeList     func(string, int) error
	checkImmediate func([]*v1.Pod) bool
	immediatePods  []*v1.Pod
}

// GetNamespacesMatchingPattern returns the namespaces in the policy test's cache.
func (i *policyInformers) GetNamespacesMatchingPattern(context.Context, string, string, string) ([]string, error) {
	return i.namespaces, nil
}

// CheckIfObservedPodsAreEvictedInImmediateMode records the selected immediate-mode snapshot.
func (i *policyInformers) CheckIfObservedPodsAreEvictedInImmediateMode(_ context.Context, _ []string,
	_ string, _ time.Duration, _ *protos.Entity, pods []*v1.Pod, _ ...informers.PodFilter) bool {
	i.immediatePods = pods
	if i.checkImmediate != nil {
		return i.checkImmediate(pods)
	}
	return false
}

// FindEvictablePodsInNamespaceAndNode counts cache reads and can inject updates between observations.
func (i *policyInformers) FindEvictablePodsInNamespaceAndNode(namespace, _ string,
	_ *protos.Entity, filters ...informers.PodFilter) ([]*v1.Pod, error) {
	i.listCalls[namespace]++
	if i.beforeList != nil {
		if err := i.beforeList(namespace, i.listCalls[namespace]); err != nil {
			return nil, err
		}
	}
	var selected []*v1.Pod
	for _, pod := range i.pods[namespace] {
		matches := pod.Status.Phase != v1.PodSucceeded
		for _, filter := range filters {
			if filter != nil && !filter(pod) {
				matches = false
			}
		}
		if matches {
			selected = append(selected, pod)
		}
	}
	return selected, nil
}

// newPolicyEvaluator compiles the test policies and initializes cache-read counters.
func newPolicyEvaluator(t *testing.T, observations *policyInformers) *NodeDrainEvaluator {
	t.Helper()
	cfg := config.TomlConfig{
		EvictionTimeoutInSeconds:  config.Duration{Duration: 7 * time.Second},
		DeleteAfterTimeoutMinutes: 5,
		PodDrainPolicies: []config.PodDrainPolicy{
			{Name: "finish", PodSelector: "mode in (completion,overlap)", Mode: config.ModeAllowCompletion},
			{Name: "replace", PodSelector: "mode in (immediate,overlap)", Mode: config.ModeImmediateEvict},
			{Name: "bounded", PodSelector: "mode=timeout", Mode: config.ModeDeleteAfterTimeout},
		},
	}
	policies, err := config.CompilePodDrainPolicies(cfg.PodDrainPolicies)
	require.NoError(t, err)
	observations.listCalls = make(map[string]int)
	return &NodeDrainEvaluator{config: cfg, informers: observations, podPolicies: policies}
}

// policyPod creates a running workload with a mode-selection label.
func policyPod(namespace, mode string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: mode, Namespace: namespace, Labels: map[string]string{"mode": mode}},
		Status:     v1.PodStatus{Phase: v1.PodRunning},
	}
}

// TestEvaluatePodPolicyActions_MixedModes_ListsEachNamespaceOnce
// checks priority, first-match semantics, force scope and cache-read counts.
func TestEvaluatePodPolicyActions_MixedModes_ListsEachNamespaceOnce(t *testing.T) {
	tests := []struct {
		name     string
		modes    []string
		force    bool
		action   DrainAction
		timeout  time.Duration
		selected []string
	}{
		{name: "immediate first", modes: []string{"completion", "timeout", "immediate", "unmatched"},
			action: ActionEvictImmediate, timeout: 7 * time.Second, selected: []string{"immediate"}},
		{name: "timeout before completion", modes: []string{"completion", "timeout", "unmatched"},
			action: ActionEvictWithTimeout, timeout: 5 * time.Minute, selected: []string{"timeout"}},
		{name: "completion", modes: []string{"completion", "unmatched"},
			action: ActionCheckCompletion, selected: []string{"completion"}},
		{name: "first policy wins", modes: []string{"overlap", "unmatched"},
			action: ActionCheckCompletion, selected: []string{"overlap"}},
		{name: "force keeps selected scope", modes: []string{"completion", "timeout", "unmatched"}, force: true,
			action: ActionEvictImmediate, timeout: 7 * time.Second, selected: []string{"completion", "timeout"}},
		{name: "unmatched does not block completion", modes: []string{"unmatched"}, action: ActionUpdateStatus},
		{name: "empty completes", action: ActionUpdateStatus},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observations := &policyInformers{namespaces: []string{"workloads", "training"}, pods: make(map[string][]*v1.Pod)}
			for _, namespace := range observations.namespaces {
				for _, mode := range tt.modes {
					observations.pods[namespace] = append(observations.pods[namespace], policyPod(namespace, mode))
				}
			}
			evaluator := newPolicyEvaluator(t, observations)
			event := model.HealthEventWithStatus{HealthEvent: &protos.HealthEvent{
				NodeName: "node-a", DrainOverrides: &protos.BehaviourOverrides{Force: tt.force},
			}}
			result, err := evaluator.evaluatePodPolicyActions(t.Context(), event, nil)
			require.NoError(t, err)
			require.Equal(t, tt.action, result.Action)
			require.Equal(t, tt.timeout, result.Timeout)
			expectedReads := 1
			if tt.action == ActionUpdateStatus {
				expectedReads = 2 // One fresh scope check before success.
				require.Equal(t, model.StatusSucceeded, result.Status)
			} else {
				require.Equal(t, observations.namespaces, result.Namespaces)
				require.NotNil(t, result.PodFilter)
				var selected []string
				for _, mode := range tt.modes {
					if result.PodFilter(policyPod("workloads", mode)) {
						selected = append(selected, mode)
					}
				}
				require.Equal(t, tt.selected, selected)
			}
			for _, namespace := range observations.namespaces {
				require.Equal(t, expectedReads, observations.listCalls[namespace])
			}
			if tt.action == ActionEvictImmediate {
				require.Len(t, observations.immediatePods, 2*len(tt.selected))
				for _, pod := range observations.immediatePods {
					require.Contains(t, tt.selected, pod.Name)
				}
			} else {
				require.Empty(t, observations.immediatePods)
			}
		})
	}
}

// TestEvaluatePodPolicyActions_CacheChangesBeforeCompletion_WaitsThenDrains
// guards the final scope refresh after an empty snapshot or an immediate-mode force deletion.
func TestEvaluatePodPolicyActions_CacheChangesBeforeCompletion_WaitsThenDrains(t *testing.T) {
	for _, afterDeletion := range []bool{false, true} {
		name := "new match after snapshot"
		if afterDeletion {
			name = "mode changes during immediate deletion"
		}
		t.Run(name, func(t *testing.T) {
			observations := &policyInformers{namespaces: []string{"workloads"}, pods: make(map[string][]*v1.Pod)}
			nextMode, nextAction := "immediate", ActionEvictImmediate
			observations.pods["workloads"] = []*v1.Pod{policyPod("workloads", "unmatched")}
			if afterDeletion {
				observations.pods["workloads"] = []*v1.Pod{policyPod("workloads", "immediate")}
				nextMode, nextAction = "completion", ActionCheckCompletion
				observations.checkImmediate = func([]*v1.Pod) bool { return true }
			}
			observations.beforeList = func(namespace string, call int) error {
				if call == 2 {
					observations.pods[namespace] = []*v1.Pod{policyPod(namespace, nextMode)}
				}
				return nil
			}
			evaluator := newPolicyEvaluator(t, observations)
			event := model.HealthEventWithStatus{HealthEvent: &protos.HealthEvent{NodeName: "node-a"}}
			result, err := evaluator.evaluatePodPolicyActions(t.Context(), event, nil)
			require.NoError(t, err)
			require.Equal(t, ActionWait, result.Action)
			result, err = evaluator.evaluatePodPolicyActions(t.Context(), event, nil)
			require.NoError(t, err)
			require.Equal(t, nextAction, result.Action)

			observations.pods["workloads"][0].Status.Phase = v1.PodSucceeded
			result, err = evaluator.evaluatePodPolicyActions(t.Context(), event, nil)
			require.NoError(t, err)
			require.Equal(t, ActionUpdateStatus, result.Action)
			require.Equal(t, model.StatusSucceeded, result.Status)
		})
	}
}

// TestEvaluatePodPolicyActions_CacheReadFails_DoesNotReportSuccess
// checks error propagation from both the snapshot and the final scope refresh.
func TestEvaluatePodPolicyActions_CacheReadFails_DoesNotReportSuccess(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		observations := &policyInformers{namespaces: []string{"workloads"}}
		lookupErr := errors.New("cache lookup failed")
		observations.beforeList = func(_ string, call int) error {
			if call == failAt {
				return lookupErr
			}
			return nil
		}
		evaluator := newPolicyEvaluator(t, observations)
		event := model.HealthEventWithStatus{HealthEvent: &protos.HealthEvent{NodeName: "node-a"}}
		result, err := evaluator.evaluatePodPolicyActions(t.Context(), event, nil)
		require.ErrorIs(t, err, lookupErr)
		require.Nil(t, result)
	}
}
