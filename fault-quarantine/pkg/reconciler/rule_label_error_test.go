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

package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/evaluator"
)

type stubRuleSetEvaluator struct {
	name   string
	result common.RuleEvaluationResult
	err    error
}

func (s *stubRuleSetEvaluator) Evaluate(*protos.HealthEvent) (common.RuleEvaluationResult, error) {
	return s.result, s.err
}

func (s *stubRuleSetEvaluator) GetName() string    { return s.name }
func (s *stubRuleSetEvaluator) GetVersion() string { return "1" }
func (s *stubRuleSetEvaluator) GetPriority() int   { return 0 }

// TestApplyRuleLabelsForEvent_EvaluationError_ContinuesToNextEvaluator verifies that later evaluators still apply labels.
func TestApplyRuleLabelsForEvent_EvaluationError_ContinuesToNextEvaluator(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 20*time.Second)
	defer cancel()

	nodeName := "rule-label-evaluation-error-" + generateShortTestID()
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	r, _, _, _ := setupE2EReconciler(t, ctx, config.TomlConfig{}, nil)
	rulesets := rulesetsConfig{
		LabelConfigMap: map[string]*config.Label{
			"low":  {Key: "nvidia.com/gpu-fault", Value: "degraded"},
			"high": {Key: "nvidia.com/gpu-fault", Value: "critical"},
		},
		RuleSetPriorityMap: map[string]int{"low": 5, "high": 10},
		RuleSetOrderMap:    map[string]int{"low": 0, "broken": 1, "high": 2},
	}
	evaluators := []evaluator.RuleSetEvaluatorIface{
		&stubRuleSetEvaluator{name: "low", result: common.RuleEvaluationSuccess},
		&stubRuleSetEvaluator{name: "broken", err: errors.New("evaluation failed")},
		&stubRuleSetEvaluator{name: "high", result: common.RuleEvaluationSuccess},
	}

	err := r.applyRuleLabelsForEvent(ctx, &protos.HealthEvent{NodeName: nodeName}, evaluators, rulesets)
	require.NoError(t, err)

	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "critical", node.Labels["nvidia.com/gpu-fault"])
}
