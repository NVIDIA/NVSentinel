// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

package config

import (
	"errors"
	"fmt"

	"github.com/nvidia/nvsentinel/commons/pkg/ruleset"
)

// drainOutcomeKey is the only outcome key node-drainer accepts.
const drainOutcomeKey = "drain"

// DrainRuleSet is one entry of ruleSets. The shared fields come from ruleset.RuleSet.
// Drain is a pointer so a missing drain key stays nil. A nil pointer stored in an
// interface is not nil, so Outcome returns a nil interface when the key is absent.
type DrainRuleSet struct {
	ruleset.RuleSet

	Drain *DrainOutcome `toml:"drain" json:"drain,omitempty"`
}

// DrainOutcome is the drain result of a rule set. This release accepts only stop.
// method is absent on purpose: a method key stays undecoded, and startup rejects it
// until a later change adds the field.
type DrainOutcome struct {
	ruleset.Stop
}

var (
	_ ruleset.Entry   = DrainRuleSet{}
	_ ruleset.Outcome = DrainOutcome{}
)

var (
	// ErrDrainOutcome means the drain outcome is not stop: true.
	ErrDrainOutcome = errors.New("invalid drain outcome")
	// ErrStopWithHealthEventRule means a stop rule set has a HealthEvent rule.
	// Every event of a quarantine session reads the same Node, so only a Node rule
	// gives those events the same stop decision.
	ErrStopWithHealthEventRule = errors.New("stop rule set must use only Node rules")
	// ErrRuleSetsNotSupported means ruleSets is configured. This release validates
	// the field and then refuses it, so a rule set cannot be ignored silently.
	ErrRuleSetsNotSupported = errors.New("ruleSets is not supported in this release")
)

// Outcome returns the drain outcome. ok is false when the drain key is absent.
func (r DrainRuleSet) Outcome() (ruleset.Outcome, bool) {
	if r.Drain == nil {
		return nil, false
	}

	return r.Drain, true
}

// Validate accepts only a stop. There is no other drain outcome in this release.
func (o DrainOutcome) Validate() error {
	if o.Stopped() {
		return nil
	}

	return fmt.Errorf("%w: drain.stop must be true", ErrDrainOutcome)
}

// validateRuleSets checks the shared rule set fields and the drain outcome.
// A rule set with enabled set to false is checked too, so it fails at startup
// rather than when an operator enables it.
func validateRuleSets(ruleSets []DrainRuleSet) error {
	return ruleset.Validate(ruleSets, drainOutcomeKey, checkDrainRuleSet)
}

// checkDrainRuleSet rejects a HealthEvent rule. This release has no other drain
// outcome, so the check runs even when stop is false and both problems are reported.
func checkDrainRuleSet(ruleSet DrainRuleSet) error {
	if matchHasKind(ruleSet.Match, ruleset.KindHealthEvent) {
		return ErrStopWithHealthEventRule
	}

	return nil
}

func matchHasKind(match ruleset.Match, kind string) bool {
	return rulesHaveKind(match.All, kind) || rulesHaveKind(match.Any, kind)
}

func rulesHaveKind(rules []ruleset.Rule, kind string) bool {
	for _, rule := range rules {
		if rule.Kind == kind {
			return true
		}
	}

	return false
}
