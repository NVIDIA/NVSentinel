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

package evaluator

import (
	"reflect"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	platformconnectorprotos "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
)

func TestEvaluate(t *testing.T) {
	expression := "event.agent == 'GPU' && event.checkName == 'XidError' && ('31' in event.errorCode || '42' in event.errorCode)"
	evaluator, err := NewHealthEventRuleEvaluator(expression)
	if err != nil {
		t.Fatalf("Failed to create HealthEventRuleEvaluator: %v", err)
	}

	eventTrue := &platformconnectorprotos.HealthEvent{
		Agent:     "GPU",
		CheckName: "XidError",
		ErrorCode: []string{"31"},
	}

	result, err := evaluator.Evaluate(eventTrue)
	if err != nil {
		t.Fatalf("Failed to evaluate expression: %v", err)
	}

	if !result {
		t.Errorf("Expected evaluation result to be true, got false")
	}

	eventFalse := &platformconnectorprotos.HealthEvent{
		Agent:     "GPU",
		CheckName: "XidError",
		ErrorCode: []string{"50"},
	}

	result, err = evaluator.Evaluate(eventFalse)
	if err != nil {
		t.Fatalf("Failed to evaluate expression: %v", err)
	}

	if result {
		t.Errorf("Expected evaluation result to be false, got true")
	}
}

func TestRoundTrip(t *testing.T) {
	eventTime := timestamppb.New(time.Now())
	event := &platformconnectorprotos.HealthEvent{
		Version:            1,
		Agent:              "test-agent",
		ComponentClass:     "test-component",
		CheckName:          "test-check",
		IsFatal:            true,
		IsHealthy:          false,
		Message:            "test-message",
		RecommendedAction:  platformconnectorprotos.RecommenedAction_NODE_REBOOT,
		ErrorCode:          []string{"E001", "E002"},
		EntitiesImpacted:   []*platformconnectorprotos.Entity{{EntityType: "GPU", EntityValue: "GPU-0"}},
		Metadata:           map[string]string{"key1": "value1"},
		GeneratedTimestamp: eventTime,
		NodeName:           "test-node",
	}

	result, err := RoundTrip(event)
	if err != nil {
		t.Fatalf("Failed to roundtrip event: %v", err)
	}

	expectedMap := map[string]interface{}{
		"version":           float64(1),
		"agent":             "test-agent",
		"componentClass":    "test-component",
		"checkName":         "test-check",
		"isFatal":           true,
		"isHealthy":         false,
		"message":           "test-message",
		"recommendedAction": float64(platformconnectorprotos.RecommenedAction_NODE_REBOOT),
		"errorCode":         []interface{}{"E001", "E002"},
		"entitiesImpacted": []interface{}{
			map[string]interface{}{
				"entityType":  "GPU",
				"entityValue": "GPU-0",
			},
		},
		"metadata": map[string]interface{}{"key1": "value1"},
		"generatedTimestamp": map[string]interface{}{
			"seconds": float64(eventTime.GetSeconds()),
			"nanos":   float64(eventTime.GetNanos()),
		},
		"nodeName": "test-node",
	}

	if !reflect.DeepEqual(result, expectedMap) {
		t.Errorf("Expected map %v, got %v", expectedMap, result)
	}
}
