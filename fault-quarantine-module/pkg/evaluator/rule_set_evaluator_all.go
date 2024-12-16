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
	multierror "github.com/hashicorp/go-multierror"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/config"
	platformconnectorprotos "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
)

type AllRuleSetEvaluator struct {
	evaluators []RuleEvaluator
	baseRuleSetEvaluator
}

func (allEval *AllRuleSetEvaluator) Evaluate(healthEvent *platformconnectorprotos.HealthEvent) (bool, error) {
	var errs *multierror.Error

	for _, evaluator := range allEval.evaluators {
		eval, err := evaluator.Evaluate(healthEvent)
		if err != nil {
			errs = multierror.Append(errs, err)
		}

		if !eval {
			return false, errs.ErrorOrNil()
		}
	}

	return true, errs.ErrorOrNil()
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
