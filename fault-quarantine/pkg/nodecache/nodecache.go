// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package nodecache decides what a cached node keeps, and prunes it to that.
//
// A node carries around a hundred labels and a dozen annotations, and the
// per-entry cost of holding them dominates what the informer cache costs at
// fleet scale. The Node rules read a handful of keys by name, and they are
// configuration read at startup, so the keys are knowable before the first
// object is cached.
//
// Pruning is only safe against the whole of what fault-quarantine reads, which
// is wider than what the rules read. It also removes keys, and a removal is a
// merge-patch diff against the cached object: a key the cache dropped cannot
// appear in the diff, so deleting it silently does nothing. Every key that can
// be removed therefore has to be retained as well.
package nodecache

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	v1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"

	"github.com/nvidia/nvsentinel/commons/pkg/celfields"
	cordonlabels "github.com/nvidia/nvsentinel/commons/pkg/labels"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
)

// nodeObjKey is the CEL variable the Node rules are evaluated against. It
// matches the binding in pkg/evaluator, which is what makes the paths derived
// here describe the same reads the rules perform.
const nodeObjKey = "node"

// nodeRuleKind is the Rule.Kind whose expressions read the cached node.
const nodeRuleKind = "Node"

// Path segments of the two maps this package prunes.
const (
	metadataSegment    = "metadata"
	labelsSegment      = "labels"
	annotationsSegment = "annotations"
)

// Operational are the keys fault-quarantine reads or removes on its own
// account, which no expression mentions and so nothing can derive.
type Operational struct {
	// GPUNodeLabelKey is the label GetNodeCounts selects on to size the
	// circuit breaker's denominator. It is a label selector against the
	// lister, so the key has to be present on the cached object or the
	// selector matches nothing and the breaker sees an empty fleet.
	GPUNodeLabelKey string
	// LabelPrefix builds the cordon and uncordon bookkeeping label keys.
	// handleUncordon reads the cordon-reason key, and manual cleanup removes
	// the rest.
	LabelPrefix string
}

// Keys names the label and annotation keys a cached node retains.
//
// The zero value retains every key. That is deliberate: a caller that has not
// derived a set cannot accidentally prune one, and every way of failing to
// derive a set has to land here rather than on an empty set. Pruning to an
// empty set would make the shipped opt-out rules evaluate as though the label
// were absent, and fault-quarantine would cordon nodes their owners excluded.
type Keys struct {
	labels           map[string]struct{}
	annotations      map[string]struct{}
	pruneLabels      bool
	pruneAnnotations bool
}

// Derive returns the keys a cached node must retain for cfg's rules and for
// fault-quarantine's own reads and removals.
//
// Rule sets are walked whether or not they are enabled. The evaluator skips
// disabled ones, so this over-retains for them, which costs a few map entries
// and cannot make a rule evaluate against a key the cache dropped.
func Derive(cfg config.TomlConfig, operational Operational) Keys {
	keys := Keys{
		labels:           make(map[string]struct{}),
		annotations:      make(map[string]struct{}),
		pruneLabels:      true,
		pruneAnnotations: true,
	}

	keys.addOperational(cfg, operational)

	env, err := newRuleEnv()
	if err != nil {
		// Nothing can be derived without an environment to compile against, so
		// the maps stay whole. pkg/evaluator builds the same environment and
		// fails startup on the same error.
		slog.Error("Caching whole node labels and annotations: CEL environment unavailable", "error", err)

		return Keys{}
	}

	for _, expression := range nodeRuleExpressions(cfg) {
		keys.addExpression(env, expression)
	}

	keys.log()

	return keys
}

// newRuleEnv builds the environment pkg/evaluator compiles Node rules in. The
// declarations have to match, or a path derived here would not correspond to
// the read the rule performs.
func newRuleEnv() (*cel.Env, error) {
	env, err := cel.NewEnv(
		cel.Variable(nodeObjKey, cel.AnyType),
		ext.Strings(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	return env, nil
}

// nodeRuleExpressions returns every expression evaluated against a node, from
// the quarantine rule sets and the validation rule sets alike.
func nodeRuleExpressions(cfg config.TomlConfig) []string {
	var expressions []string

	appendFrom := func(meta config.RuleSetMeta) {
		for _, rule := range slices.Concat(meta.Match.Any, meta.Match.All) {
			if rule.Kind == nodeRuleKind {
				expressions = append(expressions, rule.Expression)
			}
		}
	}

	for _, ruleSet := range cfg.RuleSets {
		appendFrom(ruleSet.RuleSetMeta)
	}

	for _, ruleSet := range cfg.Validation.RuleSets {
		appendFrom(ruleSet.RuleSetMeta)
	}

	return expressions
}

// addOperational records the keys that do not come from any expression.
func (k *Keys) addOperational(cfg config.TomlConfig, operational Operational) {
	for _, key := range common.QuarantineAnnotationKeys {
		k.annotations[key] = struct{}{}
	}

	k.labels[statemanager.NVSentinelStateLabelKey] = struct{}{}

	if operational.GPUNodeLabelKey != "" {
		k.labels[operational.GPUNodeLabelKey] = struct{}{}
	}

	for _, suffix := range []string{
		cordonlabels.CordonedBySuffix,
		cordonlabels.CordonedReasonSuffix,
		cordonlabels.CordonedTimestampSuffix,
		cordonlabels.UncordonedBySuffix,
		cordonlabels.UncordonedReasonSuffix,
		cordonlabels.UncordonedTimestampSuffix,
	} {
		k.labels[cordonlabels.Key(operational.LabelPrefix, suffix)] = struct{}{}
	}

	for _, ruleSet := range cfg.RuleSets {
		if ruleSet.Label.Key != "" {
			k.labels[ruleSet.Label.Key] = struct{}{}
		}
	}
}

// addExpression records what one expression reads off the node. An expression
// that cannot be compiled, or whose reads cannot be described by a set of
// paths, leaves both maps whole.
func (k *Keys) addExpression(env *cel.Env, expression string) {
	ast, issues := env.Parse(expression)
	if issues != nil && issues.Err() != nil {
		slog.Error("Caching whole node labels and annotations: a Node rule does not parse",
			"expression", expression, "error", issues.Err())
		k.retainAllLabels()
		k.retainAllAnnotations()

		return
	}

	paths, isComplete := celfields.FieldPaths(ast, nodeObjKey)
	if !isComplete {
		slog.Info("Caching whole node labels and annotations: a Node rule uses the node as a whole",
			"expression", expression)
		k.retainAllLabels()
		k.retainAllAnnotations()

		return
	}

	for _, path := range paths {
		k.addPath(path)
	}
}

// addPath records one derived field path. Only the label and annotation maps
// are pruned, so a path into spec or status is nothing this has to keep.
func (k *Keys) addPath(path []string) {
	if len(path) == 0 {
		// The whole object is read, which no set of keys describes.
		k.retainAllLabels()
		k.retainAllAnnotations()

		return
	}

	if path[0] != metadataSegment {
		return
	}

	if len(path) == 1 {
		// The whole of metadata is read, so both maps come with it.
		k.retainAllLabels()
		k.retainAllAnnotations()

		return
	}

	switch path[1] {
	case labelsSegment:
		k.addMapPath(path, k.labels, k.retainAllLabels)
	case annotationsSegment:
		k.addMapPath(path, k.annotations, k.retainAllAnnotations)
	}
}

// addMapPath records a path that reaches one of the pruned maps. A path that
// stops at the map itself reads every entry; one that continues names a key.
func (k *Keys) addMapPath(path []string, into map[string]struct{}, retainAll func()) {
	if len(path) == 2 {
		retainAll()

		return
	}

	into[path[2]] = struct{}{}
}

func (k *Keys) retainAllLabels() {
	k.pruneLabels = false
}

func (k *Keys) retainAllAnnotations() {
	k.pruneAnnotations = false
}

// log reports what the cache will keep. An operator who edits a ruleset needs
// to see the effect without reading the code.
func (k *Keys) log() {
	slog.Info("Node cache transform derived from rule CEL",
		"retainedLabels", retainedForLog(k.labels, k.pruneLabels),
		"retainedAnnotations", retainedForLog(k.annotations, k.pruneAnnotations))
}

func retainedForLog(keys map[string]struct{}, prune bool) string {
	if !prune {
		return "<all>"
	}

	return fmt.Sprint(slices.Sorted(maps.Keys(keys)))
}

// Transform returns the node informer's cache transform. It clears the status,
// which is the bulk of a node's bytes, and prunes the label and annotation
// maps to the retained keys, which is the bulk of its map entries.
//
// Spec and the identity metadata are left whole. Spec is small, and the cordon
// path, manual-untaint detection and pre-existing taint marking all read it.
func (k Keys) Transform() toolscache.TransformFunc {
	return func(obj any) (any, error) {
		node, ok := obj.(*v1.Node)
		if !ok {
			return nil, fmt.Errorf("expected node object, got %T", obj)
		}

		node.Status = v1.NodeStatus{}

		// Read before pruning, so this does not depend on the applied-labels
		// annotation being one of the retained keys.
		sessionLabels, sessionLabelsKnown := appliedLabelKeys(node.Annotations)

		if k.pruneAnnotations {
			node.Annotations = retain(node.Annotations, k.annotations, nil)
		}

		if k.pruneLabels && sessionLabelsKnown {
			node.Labels = retain(node.Labels, k.labels, sessionLabels)
		}

		return node, nil
	}
}

// appliedLabelKeys returns the label keys named by the applied-labels
// annotation, which manual unquarantine cleanup removes from the node.
//
// The keys are operator-chosen and recorded on the node itself, so they are
// not knowable when the retained set is derived. They are read per object
// instead. Only an already-quarantined node carries this annotation, so the
// unquarantined majority that drives the cache size still prunes fully.
//
// known is false when the annotation is present but does not parse, in which
// case the labels of that one node are left whole rather than guessed at.
func appliedLabelKeys(annotations map[string]string) (keys []string, known bool) {
	value, present := annotations[common.QuarantineHealthEventAppliedLabelsAnnotationKey]
	if !present || value == "" {
		return nil, true
	}

	var appliedLabels []config.AppliedLabel

	if err := json.Unmarshal([]byte(value), &appliedLabels); err != nil {
		slog.Warn("Caching whole node labels: the applied-labels annotation does not parse",
			"error", err)

		return nil, false
	}

	keys = make([]string, 0, len(appliedLabels))

	for _, label := range appliedLabels {
		if label.Key != "" {
			keys = append(keys, label.Key)
		}
	}

	return keys, true
}

// retain returns the entries of in named by keys or extra, and preserves a nil
// map as nil so callers that test for absence behave as they did before.
//
// The retained keys are iterated rather than the map, because the point of
// this is that there are far fewer of them than there are entries.
func retain(in map[string]string, keys map[string]struct{}, extra []string) map[string]string {
	if in == nil {
		return nil
	}

	out := make(map[string]string, len(keys)+len(extra))

	for key := range keys {
		if value, present := in[key]; present {
			out[key] = value
		}
	}

	for _, key := range extra {
		if value, present := in[key]; present {
			out[key] = value
		}
	}

	return out
}
