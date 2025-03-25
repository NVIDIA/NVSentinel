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
	"fmt"

	multierror "github.com/hashicorp/go-multierror"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/config"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
)

func InitializeRuleSetEvaluators(ruleSets []config.RuleSet,
	client kubernetes.Interface) ([]RuleSetEvaluatorIface, error) {
	var (
		ruleSetEvals []RuleSetEvaluatorIface
		errs         *multierror.Error
	)

	for _, ruleSet := range ruleSets {
		// We can extend this to add different types of match based rules
		if len(ruleSet.Match.Any) > 0 {
			evaluators, err := createEvaluators(ruleSet.Match.Any, client)
			if err != nil {
				errs = multierror.Append(errs, err)
			} else {
				eval := NewAnyRuleSetEvaluator(evaluators, ruleSet)
				ruleSetEvals = append(ruleSetEvals, eval)

				klog.Infof("Initialized ruleSetEvaluator: %+v", ruleSet)
			}
		}

		if len(ruleSet.Match.All) > 0 {
			evaluators, err := createEvaluators(ruleSet.Match.All, client)
			if err != nil {
				errs = multierror.Append(errs, err)
			} else {
				eval := NewAllRuleSetEvaluator(evaluators, ruleSet)
				ruleSetEvals = append(ruleSetEvals, eval)

				klog.Infof("Initialized ruleSetEvaluator: %+v", ruleSet)
			}
		}
	}

	return ruleSetEvals, errs.ErrorOrNil()
}

func createEvaluators(rules []config.Rule, client kubernetes.Interface) ([]RuleEvaluator, error) {
	evaluators := []RuleEvaluator{}

	var errs *multierror.Error

	for _, rule := range rules {
		var eval RuleEvaluator

		var err error

		switch rule.Kind {
		// Add cases for other kinds of evaluators as needed
		case "HealthEvent":
			eval, err = NewHealthEventRuleEvaluator(rule.Expression)

		case "Node":
			eval, err = NewNodeRuleEvaluator(rule.Expression, client)

		default:
			err = fmt.Errorf("unknown evaluator kind: %s", rule.Kind)
		}

		if err != nil {
			errs = multierror.Append(errs, err)
			continue
		}

		evaluators = append(evaluators, eval)
	}

	return evaluators, errs.ErrorOrNil()
}
