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
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/nvidia/nvsentinel/commons/pkg/celrule"
)

var (
	// ErrInvalidName means a rule set name is empty or is not a DNS-1123 label.
	ErrInvalidName = errors.New("invalid rule set name")
	// ErrDuplicateName means two rule sets in one component use the same name.
	ErrDuplicateName = errors.New("duplicate rule set name")
	// ErrMatch means match does not set exactly one non-empty list.
	ErrMatch = errors.New("invalid match")
	// ErrRuleKind means a rule kind is not HealthEvent or Node.
	ErrRuleKind = errors.New("invalid rule kind")
	// ErrRuleExpression means a CEL expression is empty, does not compile, or
	// does not return a boolean.
	ErrRuleExpression = errors.New("invalid rule expression")
	// ErrMissingOutcome means the rule set has no outcome field.
	ErrMissingOutcome = errors.New("missing outcome")
	// ErrUnknownKey means the TOML has a key under ruleSets that the config
	// type did not decode.
	ErrUnknownKey = errors.New("unknown rule set key")
	// ErrMixedRootKey means the TOML spells the ruleSets root key in more
	// than one case, for example ruleSets and RuleSets.
	ErrMixedRootKey = errors.New("rule set root key spelled in more than one case")
)

// Validate checks the shared rule set fields and each outcome.
//
// outcomeKey is the key the component accepts, for example "drain" or
// "remediation". It is named in the error when a rule set has no outcome.
// The caller checks unknown keys with CheckUndecoded before Validate, because
// the decoder has already dropped them.
//
// check holds the component checks that need more than the outcome: the whole
// rule set, or the component configuration. For example, node-drainer rejects
// a HealthEvent rule in a stop rule set, and fault-remediation rejects an
// action that is not in maintenance.actions. check runs only for a rule set
// that has an outcome, so it can read the outcome without a nil check. Pass
// nil when the component has no such checks.
//
// Every rule set is checked, including one with enabled set to false, so a
// broken rule set fails at startup rather than when an operator enables it.
// The returned error contains each problem. errors.Is matches the sentinels.
func Validate[T Entry](ruleSets []T, outcomeKey string, check func(T) error) error {
	var errs []error

	seen := make(map[string]int, len(ruleSets))

	for i, ruleSet := range ruleSets {
		errs = append(errs, validateEntry(i, ruleSet, outcomeKey, check, seen)...)
	}

	return errors.Join(errs...)
}

func validateEntry[T Entry](index int, entry T, outcomeKey string, check func(T) error, seen map[string]int) []error {
	common := entry.Common()
	prefix := ruleSetPrefix(index, common.Name)

	var errs []error

	if err := validateName(prefix, common.Name); err != nil {
		errs = append(errs, err)
	}

	if first, ok := seen[common.Name]; ok {
		errs = append(errs, fmt.Errorf("%s: %w: same name as %s[%d]",
			prefix, ErrDuplicateName, ruleSetsKey, first))
	} else {
		seen[common.Name] = index
	}

	errs = append(errs, validateMatch(prefix, common.Match)...)
	errs = append(errs, validateOutcome(prefix, entry, outcomeKey, check)...)

	return errs
}

func validateName(prefix, name string) error {
	msgs := validation.IsDNS1123Label(name)
	if len(msgs) == 0 {
		return nil
	}

	return fmt.Errorf("%s.name: %w: %s", prefix, ErrInvalidName, strings.Join(msgs, ", "))
}

func validateMatch(prefix string, match Match) []error {
	hasAll := match.All != nil
	hasAny := match.Any != nil
	shapeErr := fmt.Errorf("%s.match: %w: must set exactly one of all or any", prefix, ErrMatch)

	if !hasAll && !hasAny {
		return []error{shapeErr}
	}

	var errs []error

	if hasAll && hasAny {
		errs = append(errs, shapeErr)
	}

	if hasAll {
		errs = append(errs, validateRules(prefix, "all", match.All)...)
	}

	if hasAny {
		errs = append(errs, validateRules(prefix, "any", match.Any)...)
	}

	return errs
}

func validateRules(prefix, list string, rules []Rule) []error {
	if len(rules) == 0 {
		return []error{fmt.Errorf("%s.match.%s: %w: must contain at least one rule", prefix, list, ErrMatch)}
	}

	var errs []error

	for i, rule := range rules {
		rulePrefix := fmt.Sprintf("%s.match.%s[%d]", prefix, list, i)
		errs = append(errs, validateRule(rulePrefix, rule)...)
	}

	return errs
}

func validateRule(prefix string, rule Rule) []error {
	var errs []error

	variable, err := kindVariable(rule.Kind)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s.kind: %w: %q", prefix, err, rule.Kind))
	}

	if rule.Expression == "" {
		errs = append(errs, fmt.Errorf("%s.expression: %w: must not be empty", prefix, ErrRuleExpression))
	}

	if len(errs) > 0 {
		return errs
	}

	if err := celrule.CheckBooleanExpression(variable, rule.Expression); err != nil {
		return []error{fmt.Errorf("%s.expression: %w: %w", prefix, ErrRuleExpression, err)}
	}

	return nil
}

func kindVariable(kind string) (string, error) {
	switch kind {
	case KindHealthEvent:
		return celrule.EventVar, nil
	case KindNode:
		return celrule.NodeVar, nil
	default:
		return "", ErrRuleKind
	}
}

func validateOutcome[T Entry](prefix string, entry T, outcomeKey string, check func(T) error) []error {
	outcome, ok := entry.Outcome()
	if !ok || outcome == nil {
		return []error{fmt.Errorf("%s: %w %q", prefix, ErrMissingOutcome, outcomeKey)}
	}

	var errs []error

	if err := outcome.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", prefix, err))
	}

	if check == nil {
		return errs
	}

	if err := check(entry); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", prefix, err))
	}

	return errs
}

func ruleSetPrefix(index int, name string) string {
	if name == "" {
		return fmt.Sprintf("%s[%d]", ruleSetsKey, index)
	}

	return fmt.Sprintf("%s[%d] %q", ruleSetsKey, index, name)
}
