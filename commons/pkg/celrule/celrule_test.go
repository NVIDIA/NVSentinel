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

package celrule

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestNodeRuleWithMetadataAndSpecOnly(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "slim-node",
			Labels:      map[string]string{"environment": "production"},
			Annotations: map[string]string{"maintenance": "false"},
		},
		Spec: corev1.NodeSpec{
			Unschedulable: true,
			Taints: []corev1.Taint{{
				Key:    "dedicated",
				Value:  "gpu",
				Effect: corev1.TaintEffectNoSchedule,
			}},
		},
	}

	rule, err := NewNodeRule(
		`node.metadata.name == "slim-node" &&
		 node.metadata.labels["environment"] == "production" &&
		 node.metadata.annotations["maintenance"] == "false" &&
		 node.spec.unschedulable &&
		 node.spec.taints.exists(t, t.key == "dedicated")`,
	)
	require.NoError(t, err)

	matched, err := rule.Evaluate(node)
	require.NoError(t, err)
	require.True(t, matched)
}

// Restoring the pruned maps must not invent one the node never carried, or a
// rule asking whether a node has any annotations at all would read the cache
// instead of the node.
func TestNodeRule_NodeWithNoAnnotations_ReportsThemAbsent(t *testing.T) {
	rule, err := NewNodeRule(`!has(node.metadata.annotations) && has(node.metadata.labels)`)
	require.NoError(t, err)

	matched, err := rule.Evaluate(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "bare-node",
			Labels: map[string]string{"kubernetes.io/hostname": "bare-node"},
		},
	})
	require.NoError(t, err)
	require.True(t, matched,
		"a nil annotations map must stay absent while a populated labels map stays present")
}

// A node whose labels and annotations were pruned to empty maps must still
// evaluate rules that read those maps by key.
func TestNodeRule_EmptyMaps_EvaluateGuards(t *testing.T) {
	rule, err := NewNodeRule(
		`!('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels) &&
		 !('quarantinedNodeUncordonedManually' in node.metadata.annotations)`,
	)
	require.NoError(t, err)

	matched, err := rule.Evaluate(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pruned-node",
			Labels:      map[string]string{},
			Annotations: map[string]string{},
		},
	})
	require.NoError(t, err)
	require.True(t, matched)
}

func TestNodeFieldPaths(t *testing.T) {
	paths, isComplete, err := NodeFieldPaths(
		`node.metadata.labels["environment"] == "production" && node.spec.unschedulable`)
	require.NoError(t, err)
	require.True(t, isComplete)
	require.ElementsMatch(t, [][]string{
		{"metadata", "labels", "environment"},
		{"spec", "unschedulable"},
	}, paths)

	_, isComplete, err = NodeFieldPaths(`has(node.metadata) && size(node) > 0`)
	require.NoError(t, err)
	require.False(t, isComplete)

	_, _, err = NodeFieldPaths("node.metadata ==")
	require.ErrorContains(t, err, "failed to parse expression")
}

func TestEvaluate(t *testing.T) {
	expression := "event.agent == 'GPU' && event.checkName == 'XidError' && ('31' in event.errorCode || '42' in event.errorCode)"
	rule, err := NewHealthEventRule(expression)
	require.NoError(t, err)

	matched, err := rule.Evaluate(&protos.HealthEvent{
		Agent:     "GPU",
		CheckName: "XidError",
		ErrorCode: []string{"31"},
	})
	require.NoError(t, err)
	require.True(t, matched)

	matched, err = rule.Evaluate(&protos.HealthEvent{
		Agent:     "GPU",
		CheckName: "XidError",
		ErrorCode: []string{"50"},
	})
	require.NoError(t, err)
	require.False(t, matched)
}

func TestHealthEventRule_EvaluationError(t *testing.T) {
	rule, err := NewHealthEventRule(`event.metadata["missing"].startsWith("value")`)
	require.NoError(t, err)

	_, err = rule.Evaluate(&protos.HealthEvent{})
	require.Error(t, err)
	require.True(t, IsEvaluationError(err))
}

func TestHealthEventRule_NonBooleanResult(t *testing.T) {
	rule, err := NewHealthEventRule(`event.agent`)
	require.NoError(t, err)

	_, err = rule.Evaluate(&protos.HealthEvent{Agent: "GPU"})
	require.ErrorContains(t, err, "expression did not return a boolean")
	require.True(t, IsEvaluationError(err))
}

func TestNewRule_InvalidExpression(t *testing.T) {
	_, err := NewHealthEventRule("event.agent ==")
	require.ErrorContains(t, err, "failed to parse expression")

	_, err = NewNodeRule("invalid.expression")
	require.ErrorContains(t, err, "failed to check expression")
}

func TestRoundTrip(t *testing.T) {
	eventTime := timestamppb.New(time.Now())
	event := &protos.HealthEvent{
		Id:                 "123",
		Version:            1,
		Agent:              "test-agent",
		ComponentClass:     "test-component",
		CheckName:          "test-check",
		IsFatal:            true,
		IsHealthy:          false,
		Message:            "test-message",
		RecommendedAction:  protos.RecommendedAction_RESTART_VM,
		ErrorCode:          []string{"E001", "E002"},
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "GPU-0"}},
		Metadata:           map[string]string{"key1": "value1"},
		GeneratedTimestamp: eventTime,
		NodeName:           "test-node",
	}

	result, err := RoundTrip(event)
	if err != nil {
		t.Fatalf("Failed to roundtrip event: %v", err)
	}

	expectedMap := map[string]any{
		"id":                "123",
		"version":           float64(1),
		"agent":             "test-agent",
		"componentClass":    "test-component",
		"checkName":         "test-check",
		"isFatal":           true,
		"isHealthy":         false,
		"message":           "test-message",
		"recommendedAction": float64(protos.RecommendedAction_RESTART_VM),
		"errorCode":         []any{"E001", "E002"},
		"entitiesImpacted": []any{
			map[string]any{
				"entityType":  "GPU",
				"entityValue": "GPU-0",
			},
		},
		"metadata": map[string]any{"key1": "value1"},
		"generatedTimestamp": map[string]any{
			"seconds": float64(eventTime.GetSeconds()),
			"nanos":   float64(eventTime.GetNanos()),
		},
		"nodeName":                "test-node",
		"processingStrategy":      float64(0),
		"quarantineOverrides":     nil,
		"drainOverrides":          nil,
		"customRecommendedAction": "",
	}

	if !reflect.DeepEqual(result, expectedMap) {
		t.Errorf("Expected map %v, got %v", expectedMap, result)
	}
}
