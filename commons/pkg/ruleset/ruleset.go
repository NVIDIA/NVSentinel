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

// Package ruleset holds the shared rule set schema for node-drainer and
// fault-remediation.
//
// A rule set is a named list of conditions plus an outcome. The outcome says
// what that component does when the conditions match. The common fields are
// name, enabled, priority, and match. A component embeds RuleSet in its own
// type and adds one outcome field, such as drain or remediation.
//
// Nothing here evaluates a rule set. Validate and CheckUndecoded only check
// the configuration at startup.
package ruleset

const (
	// KindHealthEvent is a rule that reads the health event as "event".
	KindHealthEvent = "HealthEvent"
	// KindNode is a rule that reads the Node as "node".
	KindNode = "Node"

	// ruleSetsKey is the top-level configuration key both components use.
	ruleSetsKey = "ruleSets"
)

// Rule is one CEL condition. Kind is HealthEvent or Node.
type Rule struct {
	Kind       string `toml:"kind"       json:"kind"`
	Expression string `toml:"expression" json:"expression"`
}

// Match is the condition list. Exactly one of All or Any is set.
// All matches when every rule matches. Any matches when one rule matches.
type Match struct {
	All []Rule `toml:"all" json:"all,omitempty"`
	Any []Rule `toml:"any" json:"any,omitempty"`
}

// RuleSet is the part of a rule set that is the same in every component.
// A component embeds it, then adds the outcome field for its stage.
type RuleSet struct {
	Name string `toml:"name" json:"name"`
	// Enabled is true when the rule set is used. A missing value means true.
	// The field is a pointer because a bool would decode a missing key as false.
	Enabled  *bool `toml:"enabled"  json:"enabled,omitempty"`
	Priority int   `toml:"priority" json:"priority,omitempty"`
	Match    Match `toml:"match"    json:"match"`
}

// Stop is the shared "do nothing" field. A component outcome type embeds it.
type Stop struct {
	Stop bool `toml:"stop" json:"stop,omitempty"`
}

// Outcome is the component-specific result of a rule set.
//
// Stopped reports whether the outcome stops the stage. Validate checks only
// the outcome fields. For example, a remediation outcome rejects a value that
// sets both stop and action. A check that also reads the match or the
// component configuration goes in the check function of the package Validate.
type Outcome interface {
	Stopped() bool
	Validate() error
}

// Entry is one rule set as a component stores it.
//
// Common returns the shared fields. A component gets that method by embedding
// RuleSet. Outcome returns the component outcome. ok is false when the outcome
// key is absent. Return a nil interface in that case, not a nil pointer: a nil
// pointer stored in an interface is not nil, and Validate would then call
// Validate on it.
type Entry interface {
	Common() RuleSet
	Outcome() (outcome Outcome, ok bool)
}

// IsEnabled reports whether the rule set should be evaluated.
// A nil Enabled means the operator left the field out, and the default is true.
func (r RuleSet) IsEnabled() bool {
	return r.Enabled == nil || *r.Enabled
}

// Common returns the shared fields.
func (r RuleSet) Common() RuleSet {
	return r
}

// Stopped reports whether this outcome stops the stage.
func (s Stop) Stopped() bool {
	return s.Stop
}
