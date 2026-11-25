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

package ruleset

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/celrule"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// ErrMissingInput means a rule needs the event or the Node, and the caller did not give it.
var ErrMissingInput = errors.New("missing rule input")

// Input is what the rules of one decision read. A HealthEvent rule reads Event, and a Node
// rule reads Node. The caller gets the Node: node-drainer from its informer, fault-remediation
// from its read for the event. A caller that has no Node waits before it evaluates, so a
// missing Node is not counted as a rule set error.
type Input struct {
	Event *protos.HealthEvent
	Node  *corev1.Node
}

// EvaluationError reports the rule set that could not be evaluated. It stops the decision:
// the rule set could have won, so neither a later rule set nor the default applies.
type EvaluationError struct {
	RuleSet string
	Err     error
}

func (e *EvaluationError) Error() string {
	return fmt.Sprintf("rule set %q: %v", e.RuleSet, e.Err)
}

func (e *EvaluationError) Unwrap() error { return e.Err }

// Evaluator holds the enabled rule sets of a component, compiled and in the order of evaluation.
type Evaluator[T Entry] struct {
	ruleSets []compiledRuleSet[T]
}

type compiledRuleSet[T Entry] struct {
	entry T
	name  string
	all   []compiledRule
	any   []compiledRule
}

// compiledRule holds one of the two rule kinds.
type compiledRule struct {
	event *celrule.HealthEventRule
	node  *celrule.NodeRule
}

// Compile compiles the enabled rule sets and orders them for Evaluate: the highest priority
// first, and the sequence of the configuration for equal priorities. A disabled rule set is
// left out. The caller runs Validate first. Compile reports only what keeps a rule from
// compiling.
//
// node-drainer evaluates its stop rule sets before the rule sets that select a method,
// whatever their priority. It compiles each group into its own Evaluator and evaluates the
// stop group first.
func Compile[T Entry](ruleSets []T) (*Evaluator[T], error) {
	evaluator := &Evaluator[T]{}

	var errs []error

	for _, entry := range ruleSets {
		common := entry.Common()
		if !common.IsEnabled() {
			continue
		}

		compiled, err := compileRuleSet(entry, common)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		evaluator.ruleSets = append(evaluator.ruleSets, compiled)
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	// A stable sort keeps the sequence of the configuration for equal priorities.
	slices.SortStableFunc(evaluator.ruleSets, func(a, b compiledRuleSet[T]) int {
		return cmp.Compare(b.entry.Common().Priority, a.entry.Common().Priority)
	})

	return evaluator, nil
}

func compileRuleSet[T Entry](entry T, common RuleSet) (compiledRuleSet[T], error) {
	compiled := compiledRuleSet[T]{entry: entry, name: common.Name}

	var err error

	if compiled.all, err = compileRules(common.Match.All); err != nil {
		return compiled, fmt.Errorf("rule set %q: match.all: %w", common.Name, err)
	}

	if compiled.any, err = compileRules(common.Match.Any); err != nil {
		return compiled, fmt.Errorf("rule set %q: match.any: %w", common.Name, err)
	}

	return compiled, nil
}

func compileRules(rules []Rule) ([]compiledRule, error) {
	compiled := make([]compiledRule, 0, len(rules))

	for i, rule := range rules {
		var (
			out compiledRule
			err error
		)

		switch rule.Kind {
		case KindHealthEvent:
			out.event, err = celrule.NewHealthEventRule(rule.Expression)
		case KindNode:
			out.node, err = celrule.NewNodeRule(rule.Expression)
		default:
			return nil, fmt.Errorf("[%d].kind: %w: %q", i, ErrRuleKind, rule.Kind)
		}

		if err != nil {
			return nil, fmt.Errorf("[%d].expression: %w: %w", i, ErrRuleExpression, err)
		}

		compiled = append(compiled, out)
	}

	return compiled, nil
}

// Evaluate returns the first rule set that matches the input, in the order of Compile.
// The rule sets after the winner are not evaluated. matched is false when no rule set
// matches, and the caller then uses its default.
//
// A rule set that cannot be evaluated before the winner stops the decision: Evaluate returns
// an *EvaluationError that names it, and no winner. The caller does not use the default,
// logs the error, and tries again.
func (e *Evaluator[T]) Evaluate(input Input) (winner T, matched bool, err error) {
	for _, ruleSet := range e.ruleSets {
		isMatch, err := ruleSet.matches(input)
		if err != nil {
			var zero T

			return zero, false, &EvaluationError{RuleSet: ruleSet.name, Err: err}
		}

		if isMatch {
			return ruleSet.entry, true, nil
		}
	}

	var zero T

	return zero, false, nil
}

// matches evaluates the match list of the rule set. Validate allows only one of all and any,
// and requires at least one rule in it.
func (r compiledRuleSet[T]) matches(input Input) (bool, error) {
	if len(r.all) > 0 {
		return evaluateAll(r.all, input)
	}

	return evaluateAny(r.any, input)
}

// evaluateAll gives the result of && in CEL, so the order of the rules does not change it.
// A rule that does not match decides the result, even when another rule fails.
func evaluateAll(rules []compiledRule, input Input) (bool, error) {
	var firstErr error

	for _, rule := range rules {
		isMatch, err := rule.evaluate(input)
		if err != nil {
			firstErr = cmp.Or(firstErr, err)
			continue
		}

		if !isMatch {
			return false, nil
		}
	}

	if firstErr != nil {
		return false, firstErr
	}

	return true, nil
}

// evaluateAny gives the result of || in CEL, so the order of the rules does not change it.
// A rule that matches decides the result, even when another rule fails.
func evaluateAny(rules []compiledRule, input Input) (bool, error) {
	var firstErr error

	for _, rule := range rules {
		isMatch, err := rule.evaluate(input)
		if err != nil {
			firstErr = cmp.Or(firstErr, err)
			continue
		}

		if isMatch {
			return true, nil
		}
	}

	return false, firstErr
}

func (r compiledRule) evaluate(input Input) (bool, error) {
	if r.event != nil {
		if input.Event == nil {
			return false, fmt.Errorf("%w: a %s rule needs the event", ErrMissingInput, KindHealthEvent)
		}

		return r.event.Evaluate(input.Event)
	}

	if input.Node == nil {
		return false, fmt.Errorf("%w: a %s rule needs the Node", ErrMissingInput, KindNode)
	}

	return r.node.Evaluate(input.Node)
}
