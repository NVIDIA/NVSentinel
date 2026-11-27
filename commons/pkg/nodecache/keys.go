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

// Package nodecache decides which node fields a cache keeps.
//
// A node carries around a hundred labels. Holding every label for every node
// is most of the memory an informer uses on a large cluster. Rules name the
// few keys they read, and those rules are configuration, so the keys are known
// before the first node is cached.
package nodecache

import (
	"fmt"
	"maps"
	"slices"

	"github.com/nvidia/nvsentinel/commons/pkg/celrule"
)

// Path segments the cache can prune. A path into spec is not pruned: spec is
// small, and a Node rule keeps it whole. Status is never cached. The other
// metadata fields are small and a Node rule keeps them whole, except
// managedFields, which is large and kept only when a rule reads it.
const (
	metadataSegment      = "metadata"
	labelsSegment        = "labels"
	annotationsSegment   = "annotations"
	managedFieldsSegment = "managedFields"
	statusSegment        = "status"

	allRetained = "<all>"
)

// Keys is the set of label and annotation keys a cached node keeps, and
// whether it keeps metadata.managedFields.
//
// The zero value keeps every label, every annotation, and managedFields. That
// is deliberate. A caller that has not built a set must not prune: dropping a
// key makes a rule read the node as if the key were absent. NewPruningKeys
// starts from nothing and keeps only what the caller adds.
type Keys struct {
	labels             map[string]struct{}
	annotations        map[string]struct{}
	pruneLabels        bool
	pruneAnnotations   bool
	pruneManagedFields bool
	hasNodeRule        bool
	effects            []RuleEffect
}

// RuleEffect is what one Node expression asks the cache to keep.
//
// KeepAllLabels is true when the expression reads labels in a way that is not
// a fixed key, for example a prefix check or a key computed at runtime. The
// cache then keeps every label. ReadsStatus is true when the expression reads
// node status. The cache still drops status, so the rule sees an empty status.
type RuleEffect struct {
	Expression         string
	ParseError         error
	Incomplete         bool
	KeepAllLabels      bool
	KeepAllAnnotations bool
	ReadsStatus        bool
}

// NewPruningKeys returns a set that keeps no labels, no annotations, and no
// managedFields until the caller adds them. This is the set a component uses
// when it builds the cache from its own keys and from Node rules.
func NewPruningKeys() Keys {
	return Keys{
		labels:             make(map[string]struct{}),
		annotations:        make(map[string]struct{}),
		pruneLabels:        true,
		pruneAnnotations:   true,
		pruneManagedFields: true,
	}
}

// AddLabel keeps one label key. An empty key is ignored. A set that is not
// pruning already keeps every label, so the key is not recorded.
func (k *Keys) AddLabel(key string) {
	if key == "" || !k.pruneLabels {
		return
	}

	k.labels[key] = struct{}{}
}

// AddAnnotation keeps one annotation key. An empty key is ignored. A set that
// is not pruning already keeps every annotation, so the key is not recorded.
func (k *Keys) AddAnnotation(key string) {
	if key == "" || !k.pruneAnnotations {
		return
	}

	k.annotations[key] = struct{}{}
}

// AddNodeExpression records the node fields expression reads.
//
// An expression that does not parse, or that uses the node as a whole, keeps
// all of metadata. A rule that lost its key would treat the key as absent. Any
// expression passed here counts as a Node rule, including one that does not
// parse, so the caller keeps the node spec and metadata.
func (k *Keys) AddNodeExpression(expression string) RuleEffect {
	k.hasNodeRule = true

	effect := RuleEffect{Expression: expression}

	paths, isComplete, err := celrule.NodeFieldPaths(expression)
	if err != nil {
		effect.ParseError = err
		k.keepWholeMetadata(&effect)
		k.effects = append(k.effects, effect)

		return effect
	}

	if !isComplete {
		effect.Incomplete = true
		effect.ReadsStatus = true
		k.keepWholeMetadata(&effect)
		k.effects = append(k.effects, effect)

		return effect
	}

	for _, path := range paths {
		k.addPath(path, &effect)
	}

	k.effects = append(k.effects, effect)

	return effect
}

// PruneLabels reports whether the cache drops labels that were not named.
func (k Keys) PruneLabels() bool {
	return k.pruneLabels
}

// PruneAnnotations reports whether the cache drops annotations that were not named.
func (k Keys) PruneAnnotations() bool {
	return k.pruneAnnotations
}

// PruneManagedFields reports whether the cache drops metadata.managedFields,
// because no rule reads it.
func (k Keys) PruneManagedFields() bool {
	return k.pruneManagedFields
}

// HasNodeRule reports whether any Node expression was added.
// A component that keeps spec only for Node rules uses this.
func (k Keys) HasNodeRule() bool {
	return k.hasNodeRule
}

// Effects returns what each added Node expression asked the cache to keep.
func (k Keys) Effects() []RuleEffect {
	return slices.Clone(k.effects)
}

// RetainLabels returns the entries of in that this set keeps, plus extra.
// When this set is not pruning, in is returned unchanged, not copied.
func (k Keys) RetainLabels(in map[string]string, extra []string) map[string]string {
	if !k.pruneLabels {
		return in
	}

	return retain(in, k.labels, extra)
}

// RetainAnnotations returns the entries of in that this set keeps, plus extra.
// When this set is not pruning, in is returned unchanged, not copied.
func (k Keys) RetainAnnotations(in map[string]string, extra []string) map[string]string {
	if !k.pruneAnnotations {
		return in
	}

	return retain(in, k.annotations, extra)
}

// RetainedLabelNames is the label keys this set keeps, for a startup log.
// It is "<all>" when every label is kept.
func (k Keys) RetainedLabelNames() string {
	return retainedNames(k.labels, k.pruneLabels)
}

// RetainedAnnotationNames is the annotation keys this set keeps, for a startup log.
// It is "<all>" when every annotation is kept.
func (k Keys) RetainedAnnotationNames() string {
	return retainedNames(k.annotations, k.pruneAnnotations)
}

// retain returns the entries of in named by keys or extra.
// A nil map stays nil, so a caller that checks for a missing map still sees none.
// The retained keys are walked, not the map. There are far fewer keys than entries.
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

// addPath records one field path. Only the label and annotation maps are pruned,
// so a path into spec is nothing this set has to name.
func (k *Keys) addPath(path []string, effect *RuleEffect) {
	if len(path) == 0 {
		effect.ReadsStatus = true
		k.keepWholeMetadata(effect)

		return
	}

	switch path[0] {
	case statusSegment:
		effect.ReadsStatus = true
	case metadataSegment:
		k.addMetadataPath(path, effect)
	}
}

// addMetadataPath records a path that starts at metadata.
// The whole of metadata keeps both maps and managedFields. A path that stops
// at a map keeps every entry of that map. A longer path names one key. Other
// metadata fields, such as generation, need no key: a Node rule keeps them.
func (k *Keys) addMetadataPath(path []string, effect *RuleEffect) {
	if len(path) == 1 {
		k.keepWholeMetadata(effect)

		return
	}

	switch path[1] {
	case labelsSegment:
		addMapKey(path, &effect.KeepAllLabels, &k.pruneLabels, k.AddLabel)
	case annotationsSegment:
		addMapKey(path, &effect.KeepAllAnnotations, &k.pruneAnnotations, k.AddAnnotation)
	case managedFieldsSegment:
		k.pruneManagedFields = false
	}
}

// addMapKey records a path that reaches one map. A path that stops at the map
// reads every entry, so the map is no longer pruned. A longer path names a key.
func addMapKey(path []string, keepAll, prune *bool, addKey func(string)) {
	if len(path) == 2 {
		*keepAll = true
		*prune = false

		return
	}

	addKey(path[2])
}

func (k *Keys) keepWholeMetadata(effect *RuleEffect) {
	effect.KeepAllLabels = true
	effect.KeepAllAnnotations = true

	k.pruneLabels = false
	k.pruneAnnotations = false
	k.pruneManagedFields = false
}

func retainedNames(keys map[string]struct{}, prune bool) string {
	if !prune {
		return allRetained
	}

	return fmt.Sprint(slices.Sorted(maps.Keys(keys)))
}
