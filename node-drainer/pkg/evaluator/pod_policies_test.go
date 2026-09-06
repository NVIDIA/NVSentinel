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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/informers"
)

// Change labels between the evaluator's mode checks to reproduce a cache update
// that would otherwise let an active pod disappear from every mode's observation.
type relabellingInformers struct {
	InformersInterface
	pod     *v1.Pod
	relabel bool
}

func (i *relabellingInformers) GetNamespacesMatchingPattern(context.Context, string, string, string) ([]string, error) {
	return []string{"workloads"}, nil
}

func (i *relabellingInformers) CheckIfAllPodsAreEvictedInImmediateMode(_ context.Context, _ []string,
	_ string, _ time.Duration, _ *protos.Entity, filters ...informers.PodFilter) bool {
	pods, _ := i.FindEvictablePodsInNamespaceAndNode("workloads", "node-a", nil, filters...)
	if i.relabel {
		i.pod.Labels["mode"] = "immediate"
		i.relabel = false
	}
	return len(pods) == 0
}

func (i *relabellingInformers) FindEvictablePodsInNamespaceAndNode(_, _ string,
	_ *protos.Entity, filters ...informers.PodFilter) ([]*v1.Pod, error) {
	if i.pod.Status.Phase == v1.PodSucceeded {
		return nil, nil
	}
	for _, filter := range filters {
		if filter != nil && !filter(i.pod) {
			return nil, nil
		}
	}
	return []*v1.Pod{i.pod}, nil
}

func TestEvaluatePodPolicyActions_RelabelDuringModeChecks_WaitsThenEvicts(t *testing.T) {
	cfg := config.TomlConfig{PodDrainPolicies: []config.PodDrainPolicy{
		{Name: "finish", PodSelector: "mode=completion", Mode: config.ModeAllowCompletion},
		{Name: "replace", PodSelector: "mode=immediate", Mode: config.ModeImmediateEvict},
	}}
	policies, err := config.CompilePodDrainPolicies(cfg.PodDrainPolicies)
	require.NoError(t, err)
	observations := &relabellingInformers{
		pod: &v1.Pod{Namespace: "workloads", Labels: map[string]string{"mode": "completion"},
			Status: v1.PodStatus{Phase: v1.PodRunning}},
		relabel: true,
	}
	evaluator := &NodeDrainEvaluator{config: cfg, informers: observations, podPolicies: policies}
	event := model.HealthEventWithStatus{HealthEvent: &protos.HealthEvent{NodeName: "node-a"}}

	result, err := evaluator.evaluatePodPolicyActions(t.Context(), event, nil)
	require.NoError(t, err)
	require.Equal(t, ActionWait, result.Action, "a pod moved to a previously checked mode still needs draining")
	result, err = evaluator.evaluatePodPolicyActions(t.Context(), event, nil)
	require.NoError(t, err)
	require.Equal(t, ActionEvictImmediate, result.Action)

	observations.pod.Status.Phase = v1.PodSucceeded
	result, err = evaluator.evaluatePodPolicyActions(t.Context(), event, nil)
	require.NoError(t, err)
	require.Equal(t, ActionUpdateStatus, result.Action)
	require.Equal(t, model.StatusSucceeded, result.Status)
}
