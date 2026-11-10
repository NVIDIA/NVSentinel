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

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	annotation "github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/config"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/informers"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type nodeSelectorInformers struct {
	InformersInterface
	nodes     map[string]*v1.Node
	podChecks int
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
// The real check can force-delete pods, so each call is counted.
func (i *nodeSelectorInformers) CheckIfAllPodsAreEvictedInImmediateMode(context.Context, []string, string,
	time.Duration, *protos.Entity, ...informers.PodFilter) bool {
	i.podChecks++
	return true
}

// FindEvictablePodsInNamespaceAndNode reports the native path's node as already empty.
func (i *nodeSelectorInformers) FindEvictablePodsInNamespaceAndNode(context.Context, string, string, *protos.Entity,
	...informers.PodFilter) ([]*v1.Pod, error) {
	i.podChecks++
	return nil, nil
}

type countingCustomDrainClient struct {
	existsCalls       int
	nodeHasCR         bool
	nodeDrainComplete bool
	eventCRExists     bool
	eventCRComplete   bool
}

// ExistsForNode reports no CR by default, which makes custom drain ask for one to be created.
func (c *countingCustomDrainClient) ExistsForNode(context.Context, string) (bool, bool, error) {
	c.existsCalls++
	return c.nodeHasCR, c.nodeDrainComplete, nil
}

// GetCRStatus reports the CR of the evaluated event.
func (c *countingCustomDrainClient) GetCRStatus(context.Context, string) (bool, bool, error) {
	return c.eventCRExists, c.eventCRComplete, nil
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

			require.True(t, result.RecordDrainDecision)
			require.Equal(t, ActionWait, result.Action, "a new decision is stored before any drain action")

			if tt.customDrain {
				require.Equal(t, 1, client.existsCalls, "the read-only custom drain check runs before the store")
				require.Equal(t, model.DrainMethodCustom, result.DrainDecision.GetMethod())
				require.Equal(t, model.CustomDrainNodeSelectorRuleSet, result.DrainDecision.GetRuleSet())
			} else {
				require.Zero(t, client.existsCalls)
				require.Equal(t, model.DrainMethodBuiltIn, result.DrainDecision.GetMethod())
				require.Empty(t, result.DrainDecision.GetRuleSet())
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
	require.Nil(t, result.DrainDecision)
	require.False(t, result.RecordDrainDecision)
	require.Zero(t, client.existsCalls)
}

// TestEvaluateEventWithDatabase_NoNodeSelector_SendsEveryNodeToCustomDrain
// pins the pre-selector behaviour, including that the node cache is never read.
func TestEvaluateEventWithDatabase_NoNodeSelector_SendsEveryNodeToCustomDrain(t *testing.T) {
	evaluator, client := newNodeSelectorEvaluator(t, "", nil)
	evaluator.config.UserNamespaces = nil

	result, err := evaluator.EvaluateEventWithDatabase(t.Context(), quarantinedEvent("absent-node"), nil, nil)
	require.NoError(t, err)
	require.Equal(t, ActionWait, result.Action)
	require.Equal(t, model.DrainMethodCustom, result.DrainDecision.GetMethod())
	require.Empty(t, result.DrainDecision.GetRuleSet())
	require.True(t, result.RecordDrainDecision)
	require.Equal(t, 1, client.existsCalls)
}

// TestEvaluateEventWithDatabase_DrainDecision_KeepsOrWithholdsTheRecord
// covers each way a pass can gain, keep, or refuse a drain decision.
func TestEvaluateEventWithDatabase_DrainDecision_KeepsOrWithholdsTheRecord(t *testing.T) {
	slurmNodes := map[string]*v1.Node{
		"slurm-node": labelledNode("slurm-node", map[string]string{"scheduler": "slurm"}),
		"plain-node": labelledNode("plain-node", nil),
	}

	tests := []struct {
		name           string
		nodeSelector   string
		customEnabled  bool
		otherCRDrained bool
		eventCRDrained bool
		namespaces     []config.UserNamespace
		nodes          map[string]*v1.Node
		event          model.HealthEventWithStatus
		wantAction     DrainAction
		wantMethod     string
		wantRuleSet    string
		wantRecord     bool
		wantErrorType  string
	}{
		{
			name:          "selector match records custom and the selector rule set name",
			nodeSelector:  "scheduler=slurm",
			customEnabled: true,
			namespaces:    []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			nodes:         slurmNodes,
			event:         quarantinedEvent("slurm-node"),
			wantAction:    ActionWait,
			wantMethod:    model.DrainMethodCustom,
			wantRuleSet:   model.CustomDrainNodeSelectorRuleSet,
			wantRecord:    true,
		},
		{
			name:          "selector miss records built-in with an empty rule set",
			nodeSelector:  "scheduler=slurm",
			customEnabled: true,
			namespaces:    []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			nodes:         slurmNodes,
			event:         quarantinedEvent("plain-node"),
			wantAction:    ActionWait,
			wantMethod:    model.DrainMethodBuiltIn,
			wantRecord:    true,
		},
		{
			name:          "unscoped custom drain records custom with an empty rule set",
			customEnabled: true,
			event:         quarantinedEvent("any-node"),
			wantAction:    ActionWait,
			wantMethod:    model.DrainMethodCustom,
			wantRecord:    true,
		},
		{
			name:       "custom drain off records built-in",
			namespaces: []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			event:      quarantinedEvent("any-node"),
			wantAction: ActionWait,
			wantMethod: model.DrainMethodBuiltIn,
			wantRecord: true,
		},
		{
			name:           "node drained by another custom drain CR is already drained and records nothing",
			customEnabled:  true,
			otherCRDrained: true,
			event:          quarantinedEvent("any-node"),
			wantAction:     ActionMarkAlreadyDrained,
		},
		{
			name:           "completed CR of this event with no record records custom",
			customEnabled:  true,
			eventCRDrained: true,
			event:          quarantinedEvent("any-node"),
			wantAction:     ActionWait,
			wantMethod:     model.DrainMethodCustom,
			wantRecord:     true,
		},
		{
			name:           "recorded custom whose CR completed is already drained and keeps the record",
			customEnabled:  true,
			eventCRDrained: true,
			event:          eventWithDrainDecision("any-node", model.DrainMethodCustom, ""),
			wantAction:     ActionMarkAlreadyDrained,
			wantMethod:     model.DrainMethodCustom,
		},
		{
			name:          "recorded custom is kept after the label no longer matches",
			nodeSelector:  "scheduler=slurm",
			customEnabled: true,
			namespaces:    []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			nodes:         slurmNodes,
			event:         eventWithDrainDecision("plain-node", model.DrainMethodCustom, model.CustomDrainNodeSelectorRuleSet),
			wantAction:    ActionCreateCR,
			wantMethod:    model.DrainMethodCustom,
			wantRuleSet:   model.CustomDrainNodeSelectorRuleSet,
		},
		{
			name:          "recorded built-in is kept after the label starts matching",
			nodeSelector:  "scheduler=slurm",
			customEnabled: true,
			namespaces:    []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			nodes:         slurmNodes,
			event:         eventWithDrainDecision("slurm-node", model.DrainMethodBuiltIn, ""),
			wantAction:    ActionUpdateStatus,
			wantMethod:    model.DrainMethodBuiltIn,
		},
		{
			name:          "missing node waits and records nothing",
			nodeSelector:  "scheduler=slurm",
			customEnabled: true,
			namespaces:    []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			event:         quarantinedEvent("absent-node"),
			wantAction:    ActionWait,
		},
		{
			name:          "recorded custom with custom drain off waits",
			namespaces:    []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			event:         eventWithDrainDecision("plain-node", model.DrainMethodCustom, ""),
			wantAction:    ActionWait,
			wantErrorType: metrics.ErrorTypeRecordedCustomDrainUnavailable,
		},
		{
			name:          "recorded built-in under unscoped custom drain with no pod policies waits",
			customEnabled: true,
			event:         eventWithDrainDecision("plain-node", model.DrainMethodBuiltIn, ""),
			wantAction:    ActionWait,
			wantErrorType: metrics.ErrorTypeRecordedBuiltInNoPodPolicies,
		},
		{
			name:       "recorded built-in with custom drain off and no pod policies drains as without a record",
			event:      eventWithDrainDecision("plain-node", model.DrainMethodBuiltIn, ""),
			wantAction: ActionUpdateStatus,
			wantMethod: model.DrainMethodBuiltIn,
		},
		{
			name:          "recorded unknown method waits",
			namespaces:    []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			event:         eventWithDrainDecision("plain-node", "Replace", ""),
			wantAction:    ActionWait,
			wantErrorType: metrics.ErrorTypeRecordedDrainMethodUnknown,
		},
		{
			name:          "recorded decision with no method waits instead of choosing one",
			customEnabled: true,
			namespaces:    []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			nodeSelector:  "scheduler=slurm",
			nodes:         slurmNodes,
			event:         eventWithStoppedDrainDecision("slurm-node"),
			wantAction:    ActionWait,
			wantErrorType: metrics.ErrorTypeRecordedDrainMethodUnknown,
		},
		{
			name:       "healthy event records nothing",
			namespaces: []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			event:      healthyEvent("plain-node"),
			wantAction: ActionMarkAlreadyDrained,
		},
		{
			name:       "drain skip records nothing",
			namespaces: []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			event:      skippedEvent("plain-node"),
			wantAction: ActionMarkAlreadyDrained,
		},
		{
			name:       "terminal status records nothing",
			namespaces: []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
			event:      terminalEvent("plain-node"),
			wantAction: ActionSkip,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluator, client := newDecisionEvaluator(t, tt.customEnabled, tt.nodeSelector, tt.namespaces, tt.nodes)
			if tt.otherCRDrained || tt.eventCRDrained {
				client.nodeHasCR = true
				client.nodeDrainComplete = true
			}

			if tt.eventCRDrained {
				client.eventCRExists = true
				client.eventCRComplete = true
			}

			before := 0.0
			if tt.wantErrorType != "" {
				before = counterValue(t, metrics.ProcessingErrors, tt.wantErrorType, tt.event.HealthEvent.NodeName)
			}

			result, err := evaluator.EvaluateEventWithDatabase(t.Context(), tt.event, nil, nil)
			require.NoError(t, err)
			require.Equal(t, tt.wantAction, result.Action)
			require.Equal(t, tt.wantRecord, result.RecordDrainDecision)

			if tt.wantMethod == "" {
				require.Nil(t, result.DrainDecision)
			} else {
				require.Equal(t, tt.wantMethod, result.DrainDecision.GetMethod())
				require.Equal(t, tt.wantRuleSet, result.DrainDecision.GetRuleSet())
			}

			if tt.wantErrorType != "" {
				after := counterValue(t, metrics.ProcessingErrors, tt.wantErrorType, tt.event.HealthEvent.NodeName)
				require.Greater(t, after, before)
			}
		})
	}
}

// TestEvaluateEventWithDatabase_AlreadyDrained_DoesNotRecordDecision
// checks the annotation path that marks an event AlreadyDrained before a method is chosen.
func TestEvaluateEventWithDatabase_AlreadyDrained_DoesNotRecordDecision(t *testing.T) {
	const nodeName = "already-drained-node"

	previous := &protos.HealthEvent{Id: "earlier-event", NodeName: nodeName, Agent: "test", CheckName: "gpu"}
	events := annotation.NewHealthEventsAnnotationMap()
	events.Events[annotation.CreateEventKeyForEntity(previous, nil)] = previous
	raw, err := events.MarshalJSON()
	require.NoError(t, err)

	nodes := map[string]*v1.Node{
		nodeName: {
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				Annotations: map[string]string{
					common.QuarantineHealthEventAnnotationKey: string(raw),
				},
			},
		},
	}
	evaluator, _ := newDecisionEvaluator(t, false, "", []config.UserNamespace{
		{Name: "*", Mode: config.ModeImmediateEvict},
	}, nodes)

	quarantineStatus := datastore.Quarantined
	store := &completedDrainStore{event: datastore.HealthEventWithStatus{
		HealthEvent: previous,
		HealthEventStatus: datastore.HealthEventStatus{
			NodeQuarantined:        &quarantineStatus,
			UserPodsEvictionStatus: datastore.OperationStatus{Status: datastore.StatusSucceeded},
		},
	}}

	event := quarantinedEvent(nodeName)
	event.HealthEventStatus.NodeQuarantined = string(model.AlreadyQuarantined)

	result, err := evaluator.EvaluateEventWithDatabase(t.Context(), event, nil, store)
	require.NoError(t, err)
	require.Equal(t, ActionMarkAlreadyDrained, result.Action)
	require.Nil(t, result.DrainDecision)
	require.False(t, result.RecordDrainDecision)
}

// TestEvaluateEventWithDatabase_NewBuiltInDecision_ChecksNoPodsBeforeStore keeps the pod checks,
// which can force-delete pods, out of the pass that chooses the method. Only the pass that
// reads the stored decision runs them.
func TestEvaluateEventWithDatabase_NewBuiltInDecision_ChecksNoPodsBeforeStore(t *testing.T) {
	podInformers := &nodeSelectorInformers{}
	cfg := config.TomlConfig{
		SystemNamespaces: "kube-*",
		UserNamespaces:   []config.UserNamespace{{Name: "*", Mode: config.ModeImmediateEvict}},
	}
	evaluator, err := NewNodeDrainEvaluator(cfg, podInformers, nil)
	require.NoError(t, err)

	event := quarantinedEvent("plain-node")

	result, err := evaluator.EvaluateEventWithDatabase(t.Context(), event, nil, nil)
	require.NoError(t, err)
	require.True(t, result.RecordDrainDecision)
	require.Equal(t, ActionWait, result.Action)
	require.Equal(t, model.DrainMethodBuiltIn, result.DrainDecision.GetMethod())
	require.Zero(t, podInformers.podChecks, "no pod check may run before the decision is stored")

	event.HealthEventStatus.DrainDecision = result.DrainDecision

	result, err = evaluator.EvaluateEventWithDatabase(t.Context(), event, nil, nil)
	require.NoError(t, err)
	require.False(t, result.RecordDrainDecision)
	require.Equal(t, ActionUpdateStatus, result.Action)
	require.Positive(t, podInformers.podChecks, "the stored decision runs the pod checks")
}

func newDecisionEvaluator(t *testing.T, customEnabled bool, nodeSelector string,
	namespaces []config.UserNamespace, nodes map[string]*v1.Node) (*NodeDrainEvaluator, *countingCustomDrainClient) {
	t.Helper()

	cfg := config.TomlConfig{
		SystemNamespaces: "kube-*",
		UserNamespaces:   namespaces,
		CustomDrain: config.CustomDrainConfig{
			Enabled:               customEnabled,
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

	var client *countingCustomDrainClient
	var drainClient CustomDrainClientInterface

	if customEnabled {
		client = &countingCustomDrainClient{}
		drainClient = client
	}

	evaluator, err := NewNodeDrainEvaluator(cfg, &nodeSelectorInformers{nodes: nodes}, drainClient)
	require.NoError(t, err)

	return evaluator.(*NodeDrainEvaluator), client
}

func eventWithDrainDecision(nodeName, method, ruleSet string) model.HealthEventWithStatus {
	event := quarantinedEvent(nodeName)
	event.HealthEventStatus.DrainDecision = &protos.DrainDecision{Method: method, RuleSet: ruleSet}

	return event
}

// eventWithStoppedDrainDecision carries a stop, which has no method, as a newer version writes it.
func eventWithStoppedDrainDecision(nodeName string) model.HealthEventWithStatus {
	event := quarantinedEvent(nodeName)
	event.HealthEventStatus.DrainDecision = &protos.DrainDecision{Stopped: true}

	return event
}

func healthyEvent(nodeName string) model.HealthEventWithStatus {
	event := quarantinedEvent(nodeName)
	event.HealthEvent.IsHealthy = true

	return event
}

func skippedEvent(nodeName string) model.HealthEventWithStatus {
	event := quarantinedEvent(nodeName)
	event.HealthEvent.DrainOverrides = &protos.BehaviourOverrides{Skip: true}

	return event
}

func terminalEvent(nodeName string) model.HealthEventWithStatus {
	event := quarantinedEvent(nodeName)
	event.HealthEventStatus.UserPodsEvictionStatus.Status = string(model.StatusSucceeded)

	return event
}

func counterValue(t *testing.T, counterVec *prometheus.CounterVec, labelValues ...string) float64 {
	t.Helper()

	counter, err := counterVec.GetMetricWithLabelValues(labelValues...)
	require.NoError(t, err)

	metric := &dto.Metric{}
	require.NoError(t, counter.Write(metric))

	return metric.GetCounter().GetValue()
}

// completedDrainStore answers the already-drained lookup with one finished drain.
type completedDrainStore struct {
	datastore.HealthEventStore
	event datastore.HealthEventWithStatus
}

func (s *completedDrainStore) FindHealthEventsByQuery(context.Context,
	datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
	return []datastore.HealthEventWithStatus{s.event}, nil
}
