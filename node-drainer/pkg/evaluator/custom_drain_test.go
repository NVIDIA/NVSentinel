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

type nodeSelectorInformers struct {
	InformersInterface
	nodes map[string]*v1.Node
}

// GetNode serves the node cache the drain path decision reads.
func (i *nodeSelectorInformers) GetNode(nodeName string) (*v1.Node, error) {
	node, exists := i.nodes[nodeName]
	if !exists {
		return nil, errors.New("node not found in cache")
	}

	return node, nil
}

// GetNamespacesMatchingPattern stands in for the namespace scan custom drain does before
// creating its CR.
func (i *nodeSelectorInformers) GetNamespacesMatchingPattern(context.Context, string, string,
	string) ([]string, error) {
	return []string{"workloads"}, nil
}

// CheckIfAllPodsAreEvictedInImmediateMode reports the native path's node as already empty.
func (i *nodeSelectorInformers) CheckIfAllPodsAreEvictedInImmediateMode(context.Context, []string, string,
	time.Duration, *protos.Entity, ...informers.PodFilter) bool {
	return true
}

// FindEvictablePodsInNamespaceAndNode reports the native path's node as already empty.
func (i *nodeSelectorInformers) FindEvictablePodsInNamespaceAndNode(string, string, *protos.Entity,
	...informers.PodFilter) ([]*v1.Pod, error) {
	return nil, nil
}

type countingCustomDrainClient struct {
	existsCalls int
}

// ExistsForNode reports no CR yet, which makes custom drain ask for one to be created.
func (c *countingCustomDrainClient) ExistsForNode(context.Context, string) (bool, bool, error) {
	c.existsCalls++
	return false, false, nil
}

func (c *countingCustomDrainClient) GetCRStatus(context.Context, string) (bool, bool, error) {
	return false, false, nil
}

// newNodeSelectorEvaluator builds an evaluator whose custom drain is scoped by nodeSelector.
func newNodeSelectorEvaluator(t *testing.T, nodeSelector string,
	nodes map[string]*v1.Node) (*NodeDrainEvaluator, *countingCustomDrainClient) {
	t.Helper()

	cfg := config.TomlConfig{
		SystemNamespaces: "kube-*",
		UserNamespaces:   []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
		CustomDrain: config.CustomDrainConfig{
			Enabled:               true,
			NodeSelector:          nodeSelector,
			TemplateMountPath:     "/etc/drain-template",
			TemplateFileName:      "drain-template.yaml",
			Namespace:             "nvsentinel",
			ApiGroup:              "nvsentinel.nvidia.com",
			Version:               "v1alpha1",
			Kind:                  "DrainRequest",
			StatusConditionType:   "DrainComplete",
			StatusConditionStatus: "True",
		},
	}

	client := &countingCustomDrainClient{}

	evaluator, err := NewNodeDrainEvaluator(cfg, &nodeSelectorInformers{nodes: nodes}, client)
	require.NoError(t, err)

	return evaluator.(*NodeDrainEvaluator), client
}

// quarantinedEvent builds the minimal event that reaches the drain path decision.
func quarantinedEvent(nodeName string) model.HealthEventWithStatus {
	return model.HealthEventWithStatus{
		HealthEvent: &protos.HealthEvent{Id: "event-1", NodeName: nodeName},
		HealthEventStatus: &protos.HealthEventStatus{
			NodeQuarantined:        string(model.Quarantined),
			UserPodsEvictionStatus: &protos.OperationStatus{Status: string(model.StatusInProgress)},
		},
	}
}

func labelledNode(name string, labels map[string]string) *v1.Node {
	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// TestEvaluateEventWithDatabase_NodeSelector_RoutesEachNodeToItsDrainPath
// checks that a scoped custom drain leaves the unmatched nodes on native eviction.
func TestEvaluateEventWithDatabase_NodeSelector_RoutesEachNodeToItsDrainPath(t *testing.T) {
	nodes := map[string]*v1.Node{
		"slurm-node": labelledNode("slurm-node", map[string]string{"scheduler": "slurm"}),
		"kueue-node": labelledNode("kueue-node", map[string]string{"scheduler": "kueue"}),
		"plain-node": labelledNode("plain-node", nil),
	}

	tests := []struct {
		name        string
		nodeName    string
		customDrain bool
	}{
		{"matching node uses custom drain", "slurm-node", true},
		{"other scheduler keeps native eviction", "kueue-node", false},
		{"unlabelled node keeps native eviction", "plain-node", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluator, client := newNodeSelectorEvaluator(t, "scheduler=slurm", nodes)

			result, err := evaluator.EvaluateEventWithDatabase(t.Context(), quarantinedEvent(tt.nodeName), nil, nil)
			require.NoError(t, err)

			if tt.customDrain {
				require.Equal(t, ActionCreateCR, result.Action)
				require.Equal(t, 1, client.existsCalls)
			} else {
				require.NotEqual(t, ActionCreateCR, result.Action)
				require.Zero(t, client.existsCalls)
			}
		})
	}
}

// TestEvaluateEventWithDatabase_NodeMissingFromCache_RetriesInsteadOfGuessing
// keeps an unresolvable node from silently falling back to the wrong drain path.
func TestEvaluateEventWithDatabase_NodeMissingFromCache_RetriesInsteadOfGuessing(t *testing.T) {
	evaluator, client := newNodeSelectorEvaluator(t, "scheduler=slurm", nil)

	result, err := evaluator.EvaluateEventWithDatabase(t.Context(), quarantinedEvent("absent-node"), nil, nil)
	require.NoError(t, err)
	require.Equal(t, ActionWait, result.Action)
	require.Equal(t, time.Minute, result.WaitDelay)
	require.Zero(t, client.existsCalls)
}

// TestEvaluateEventWithDatabase_NoNodeSelector_SendsEveryNodeToCustomDrain
// pins the pre-selector behaviour, including that the node cache is never read.
func TestEvaluateEventWithDatabase_NoNodeSelector_SendsEveryNodeToCustomDrain(t *testing.T) {
	evaluator, client := newNodeSelectorEvaluator(t, "", nil)
	evaluator.config.UserNamespaces = nil

	result, err := evaluator.EvaluateEventWithDatabase(t.Context(), quarantinedEvent("absent-node"), nil, nil)
	require.NoError(t, err)
	require.Equal(t, ActionCreateCR, result.Action)
	require.Equal(t, 1, client.existsCalls)
}
