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
	multierror "github.com/hashicorp/go-multierror"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
)

type AllRuleSetEvaluator struct {
	evaluators []RuleEvaluator
	baseRuleSetEvaluator
}

func (allEval *AllRuleSetEvaluator) Evaluate(
	healthEvent *protos.HealthEvent) (common.RuleEvaluationResult, error) {
	var errs *multierror.Error

	for _, evaluator := range allEval.evaluators {
		ruleEvaluatedResult, err := evaluator.Evaluate(healthEvent)
		if err != nil {
			errs = multierror.Append(errs, err)
		}

		if ruleEvaluatedResult != common.RuleEvaluationSuccess {
			return ruleEvaluatedResult, errs.ErrorOrNil()
		}
	}

	return common.RuleEvaluationSuccess, errs.ErrorOrNil()
}

func NewAllRuleSetEvaluator(evaluators []RuleEvaluator, ruleset config.RuleSet) *AllRuleSetEvaluator {
	return &AllRuleSetEvaluator{
		baseRuleSetEvaluator: baseRuleSetEvaluator{
			Name:     ruleset.Name,
			Version:  ruleset.Version,
			Priority: ruleset.Priority,
		},
		evaluators: evaluators,
	}
}
