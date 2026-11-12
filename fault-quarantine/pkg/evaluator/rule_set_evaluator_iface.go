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

package evaluator

import (
	"context"

	"github.com/nvidia/nvsentinel/commons/pkg/celrule"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
)

// RuleEvaluator evaluates a compiled rule from commons/pkg/celrule.
type RuleEvaluator interface {
	Evaluate(context.Context, *protos.HealthEvent) (bool, error)
}

// evaluateRule converts a celrule result. An error in the expression is
// permanent for the event, so cold start does not retry it.
func evaluateRule(
	ctx context.Context,
	evaluator RuleEvaluator,
	event *protos.HealthEvent,
) (common.RuleEvaluationResult, error) {
	matched, err := evaluator.Evaluate(ctx, event)
	if celrule.IsEvaluationError(err) {
		err = coldstart.PermanentError(err)
	}

	if matched {
		return common.RuleEvaluationSuccess, err
	}

	return common.RuleEvaluationFailed, err
}

// Interfaces and base structs
type RuleSetEvaluatorIface interface {
	Evaluate(context.Context, *protos.HealthEvent) (common.RuleEvaluationResult, error)
	GetName() string
}

type baseRuleSetEvaluator struct {
	Version  string
	Name     string
	Priority int
}

func (baseEval *baseRuleSetEvaluator) GetName() string {
	return baseEval.Name
}
