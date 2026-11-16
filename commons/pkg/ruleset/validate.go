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
)

// Validate checks the shared rule set fields and each outcome.
//
// outcomeKey is the key the component accepts, for example "drain" or
// "remediation". It is named in the error when a rule set has no outcome.
// The caller checks unknown keys with CheckUndecoded before Validate, because
// the decoder has already dropped them.
//
// Every rule set is checked, including one with enabled set to false, so a
// broken rule set fails at startup rather than when an operator enables it.
// The returned error contains each problem. errors.Is matches the sentinels.
func Validate[T Entry](ruleSets []T, outcomeKey string) error {
	var errs []error

	seen := make(map[string]int, len(ruleSets))

	for i, ruleSet := range ruleSets {
		errs = append(errs, validateEntry(i, ruleSet, outcomeKey, seen)...)
	}

	return errors.Join(errs...)
}

func validateEntry[T Entry](index int, entry T, outcomeKey string, seen map[string]int) []error {
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

	outcome, ok := entry.Outcome()
	if !ok || outcome == nil {
		errs = append(errs, fmt.Errorf("%s: %w %q", prefix, ErrMissingOutcome, outcomeKey))

		return errs
	}

	if err := outcome.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", prefix, err))
	}

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

	if hasAll == hasAny {
		return []error{fmt.Errorf("%s.match: %w: must set exactly one of all or any", prefix, ErrMatch)}
	}

	rules, list := matchList(match, hasAny)
	if len(rules) == 0 {
		return []error{fmt.Errorf("%s.match.%s: %w: must contain at least one rule", prefix, list, ErrMatch)}
	}

	var errs []error

	for i, rule := range rules {
		rulePrefix := fmt.Sprintf("%s.match.%s[%d]", prefix, list, i)
		if err := validateRule(rulePrefix, rule); err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

func matchList(match Match, useAny bool) (rules []Rule, list string) {
	if useAny {
		return match.Any, "any"
	}

	return match.All, "all"
}

func validateRule(prefix string, rule Rule) error {
	variable, err := kindVariable(rule.Kind)
	if err != nil {
		return fmt.Errorf("%s.kind: %w: %q", prefix, err, rule.Kind)
	}

	if rule.Expression == "" {
		return fmt.Errorf("%s.expression: %w: must not be empty", prefix, ErrRuleExpression)
	}

	if err := celrule.CheckBooleanExpression(variable, rule.Expression); err != nil {
		return fmt.Errorf("%s.expression: %w: %w", prefix, ErrRuleExpression, err)
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

func ruleSetPrefix(index int, name string) string {
	if name == "" {
		return fmt.Sprintf("%s[%d]", ruleSetsKey, index)
	}

	return fmt.Sprintf("%s[%d] %q", ruleSetsKey, index, name)
}
