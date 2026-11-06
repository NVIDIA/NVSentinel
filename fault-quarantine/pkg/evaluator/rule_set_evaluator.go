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
	"fmt"
	"log/slog"

	multierror "github.com/hashicorp/go-multierror"

	"github.com/nvidia/nvsentinel/commons/pkg/celrule"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
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
			evaluators, err := createEvaluators(ruleSet.Match.Any, nodeInformer)
			if err != nil {
				errs = multierror.Append(errs, err)
			} else {
				eval := NewAnyRuleSetEvaluator(evaluators, ruleSet)
				ruleSetEvals = append(ruleSetEvals, eval)

				slog.Debug("Initialized ruleSetEvaluator", "ruleSet", ruleSet)
			}
		}

		if len(ruleSet.Match.All) > 0 {
			evaluators, err := createEvaluators(ruleSet.Match.All, nodeInformer)
			if err != nil {
				errs = multierror.Append(errs, err)
			} else {
				eval := NewAllRuleSetEvaluator(evaluators, ruleSet)
				ruleSetEvals = append(ruleSetEvals, eval)

				slog.Debug("Initialized ruleSetEvaluator", "ruleSet", ruleSet)
			}
		}
	}

	return ruleSetEvals, errs.ErrorOrNil()
}

func createEvaluators(rules []config.Rule, nodeInformer *informer.NodeInformer) ([]RuleEvaluator, error) {
	evaluators := []RuleEvaluator{}

	var errs *multierror.Error

	for _, rule := range rules {
		var eval RuleEvaluator

		var err error

		switch rule.Kind {
		// Add cases for other kinds of evaluators as needed
		case "HealthEvent":
			eval, err = newHealthEventRule(rule.Expression)

		case "Node":
			if nodeInformer == nil {
				err = fmt.Errorf("NodeInformer must be provided for Node rule kind")
			} else {
				eval, err = newNodeRule(rule.Expression, nodeInformer)
			}

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

type healthEventRule struct {
	rule *celrule.HealthEventRule
}

func newHealthEventRule(expression string) (RuleEvaluator, error) {
	rule, err := celrule.NewHealthEventRule(expression)
	if err != nil {
		return nil, err
	}

	return &healthEventRule{rule: rule}, nil
}

func (r *healthEventRule) Evaluate(_ context.Context, event *protos.HealthEvent) (bool, error) {
	return r.rule.Evaluate(event)
}

// nodeRule evaluates a Node rule against the node of the event. It reads the
// node through GetRuleNode, so cold start evaluates against the live node.
type nodeRule struct {
	rule         *celrule.NodeRule
	nodeInformer *informer.NodeInformer
}

func newNodeRule(expression string, nodeInformer *informer.NodeInformer) (RuleEvaluator, error) {
	rule, err := celrule.NewNodeRule(expression)
	if err != nil {
		return nil, err
	}

	return &nodeRule{rule: rule, nodeInformer: nodeInformer}, nil
}

func (r *nodeRule) Evaluate(ctx context.Context, event *protos.HealthEvent) (bool, error) {
	node, err := r.nodeInformer.GetRuleNode(ctx, event.NodeName)
	if err != nil {
		return false, fmt.Errorf("failed to get node metadata: %w", err)
	}

	return r.rule.Evaluate(node)
}
