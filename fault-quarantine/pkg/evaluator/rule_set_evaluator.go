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
	"fmt"
	"log/slog"

	multierror "github.com/hashicorp/go-multierror"

	"github.com/nvidia/nvsentinel/commons/pkg/celrule"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
)

func InitializeRuleSetEvaluators(
	ruleSets []config.RuleSetMeta,
	nodeInformer *informer.NodeInformer,
) ([]RuleSetEvaluatorIface, error) {
	var (
		ruleSetEvals []RuleSetEvaluatorIface
		errs         *multierror.Error
	)

	for _, ruleSet := range ruleSets {
		if !ruleSet.Enabled {
			slog.Info("Skipping disabled ruleSet", "ruleSet", ruleSet.Name)
			continue
		}

		if len(ruleSet.Match.Any) > 0 {
			rules, err := createRules(ruleSet.Match.Any, nodeInformer)
			if err != nil {
				errs = multierror.Append(errs, err)
			} else {
				eval := NewAnyRuleSetEvaluator(rules, ruleSet)
				ruleSetEvals = append(ruleSetEvals, eval)

				slog.Debug("Initialized ruleSetEvaluator", "ruleSet", ruleSet)
			}
		}

		if len(ruleSet.Match.All) > 0 {
			rules, err := createRules(ruleSet.Match.All, nodeInformer)
			if err != nil {
				errs = multierror.Append(errs, err)
			} else {
				eval := NewAllRuleSetEvaluator(rules, ruleSet)
				ruleSetEvals = append(ruleSetEvals, eval)

				slog.Debug("Initialized ruleSetEvaluator", "ruleSet", ruleSet)
			}
		}
	}

	return ruleSetEvals, errs.ErrorOrNil()
}

func createRules(configRules []config.Rule, nodeInformer *informer.NodeInformer) ([]Rule, error) {
	rules := []Rule{}

	var errs *multierror.Error

	for _, configRule := range configRules {
		var (
			rule Rule
			err  error
		)

		switch configRule.Kind {
		// Add cases for other kinds of rules as needed
		case "HealthEvent":
			rule, err = celrule.NewHealthEventRule(configRule.Expression)

		case "Node":
			if nodeInformer == nil {
				err = fmt.Errorf("NodeInformer must be provided for Node rule kind")
			} else {
				rule, err = celrule.NewNodeRule(configRule.Expression, celrule.NodeGetterFunc(nodeInformer.GetRuleNode))
			}

		default:
			err = fmt.Errorf("unknown evaluator kind: %s", configRule.Kind)
		}

		if err != nil {
			errs = multierror.Append(errs, err)
			continue
		}

		rules = append(rules, rule)
	}

	return rules, errs.ErrorOrNil()
}
