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
	"slices"

	v1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"

	cordonlabels "github.com/nvidia/nvsentinel/commons/pkg/labels"
	nodefields "github.com/nvidia/nvsentinel/commons/pkg/nodecache"
	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
)

// nodeRuleKind is the Rule.Kind whose expressions read the cached node.
const nodeRuleKind = "Node"

// Operational are the keys fault-quarantine reads or removes on its own
// account, which no expression mentions and so nothing can derive.
type Operational struct {
	// GPUNodeLabelKey is the label GetNodeCounts selects on to size the
	// circuit breaker's denominator. It is a label selector against the
	// lister, so the key has to be present on the cached object or the
	// selector matches nothing and the breaker sees an empty fleet.
	GPUNodeLabelKey string
}

// Keys names the label and annotation keys a cached node retains.
//
// The zero value retains every key. That is deliberate: a caller that has not
// derived a set cannot accidentally prune one, and every way of failing to
// derive a set has to land here rather than on an empty set. Pruning to an
// empty set would make the shipped opt-out rules evaluate as though the label
// were absent, and fault-quarantine would cordon nodes their owners excluded.
type Keys struct {
	inner nodefields.Keys
}

// Derive returns the keys a cached node must retain for cfg's rules and for
// fault-quarantine's own reads and removals.
//
// Rule sets are walked whether or not they are enabled. The evaluator skips
// disabled ones, so this over-retains for them, which costs a few map entries
// and cannot make a rule evaluate against a key the cache dropped.
func Derive(cfg config.TomlConfig, operational Operational) Keys {
	keys := Keys{inner: nodefields.NewPruningKeys()}

	keys.addOperational(cfg, operational)

	for _, expression := range nodeRuleExpressions(cfg) {
		logExpressionEffect(keys.inner.AddNodeExpression(expression))
	}

	keys.log()

	return keys
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
		k.inner.AddAnnotation(key)
	}

	k.inner.AddLabel(statemanager.NVSentinelStateLabelKey)

	if operational.GPUNodeLabelKey != "" {
		k.inner.AddLabel(operational.GPUNodeLabelKey)
	}

	// The prefix comes from cfg because that is what the reconciler builds these
	// keys from. Taking it from anywhere else would retain keys under one prefix
	// while fault-quarantine reads and removes them under another.
	for _, suffix := range []string{
		cordonlabels.CordonedBySuffix,
		cordonlabels.CordonedReasonSuffix,
		cordonlabels.CordonedTimestampSuffix,
		cordonlabels.UncordonedBySuffix,
		cordonlabels.UncordonedReasonSuffix,
		cordonlabels.UncordonedTimestampSuffix,
	} {
		k.inner.AddLabel(cordonlabels.Key(cfg.LabelPrefix, suffix))
	}

	for _, ruleSet := range cfg.RuleSets {
		if ruleSet.Label.Key != "" {
			k.inner.AddLabel(ruleSet.Label.Key)
		}
	}
}

// logExpressionEffect reports a rule the cache cannot prune against.
// The same messages were logged when this derivation lived in this package.
func logExpressionEffect(effect nodefields.RuleEffect) {
	if effect.ParseError != nil {
		slog.Error("Caching whole node labels and annotations: a Node rule does not parse",
			"expression", effect.Expression, "error", effect.ParseError)

		return
	}

	if effect.Incomplete {
		slog.Info("Caching whole node labels and annotations: a Node rule uses the node as a whole",
			"expression", effect.Expression)
	}
}

// log reports what the cache will keep.
func (k Keys) log() {
	slog.Info("Node cache transform derived from rule CEL",
		"retainedLabels", k.inner.RetainedLabelNames(),
		"retainedAnnotations", k.inner.RetainedAnnotationNames())
}

// Transform returns the node informer's cache transform. It clears the status,
// which is the bulk of a node's bytes, and prunes the label and annotation
// maps to the retained keys, which is the bulk of its map entries. It also
// drops managedFields, unless a rule reads it.
//
// Spec and the rest of metadata are left whole. Spec is small, and the cordon
// path, manual-untaint detection and pre-existing taint marking all read it.
func (k Keys) Transform() toolscache.TransformFunc {
	return func(obj any) (any, error) {
		node, ok := obj.(*v1.Node)
		if !ok {
			return nil, fmt.Errorf("expected node object, got %T", obj)
		}

		node.Status = v1.NodeStatus{}

		if k.inner.PruneManagedFields() {
			node.ManagedFields = nil
		}

		// Read before pruning, so this does not depend on the applied-labels
		// annotation being one of the retained keys.
		sessionLabels, sessionLabelsKnown := appliedLabelKeys(node.Annotations)

		node.Annotations = k.inner.RetainAnnotations(node.Annotations, nil)

		if k.inner.PruneLabels() && sessionLabelsKnown {
			node.Labels = k.inner.RetainLabels(node.Labels, sessionLabels)
		}

		return node, nil
	}
}

// appliedLabelKeys returns the label keys named by the applied-labels
// annotation, which unquarantine cleanup removes from the node.
//
// addOperational already retains the label each configured rule set applies,
// so this covers the ones configuration no longer mentions: a rule set renamed,
// edited or removed since the node was quarantined leaves a label whose key
// survives only in this annotation. Cleanup still has to remove it, and a
// removal cannot diff against a key the cache dropped, so it is read per
// object. Only an already-quarantined node carries the annotation, so the
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
