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

package initializer

import (
	"log/slog"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"

	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

const metadataKey = "metadata"

// alwaysRetainedPaths survive pruning for every watched GVK regardless of what
// the policies read.
var alwaysRetainedPaths = [][]string{
	{"apiVersion"},
	{"kind"},
	{metadataKey, "name"},
	{metadataKey, "namespace"},
	{metadataKey, "uid"},
	{metadataKey, "resourceVersion"},
	{metadataKey, "deletionTimestamp"},
}

// gvkCacheEntry is how one GVK is cached, derived from the CEL of the enabled
// policies.
type gvkCacheEntry struct {
	// transform prunes the cache entry to the fields the policies read, and is
	// nil when the objects must be cached in full.
	transform toolscache.TransformFunc
	// servesLookups reports whether a lookup() of this GVK reads the cache or the API server.
	servesLookups bool
}

// gvkFieldPaths is what the enabled policies read off one GVK, in each of the
// two roles it can play. wholeWatch and wholeLookup name a policy that reads
// the object as a whole in that role, which no set of paths describes.
type gvkFieldPaths struct {
	watched     bool
	watchPaths  [][]string
	wholeWatch  string
	lookedUp    bool
	lookupPaths [][]string
	wholeLookup string
}

// buildCacheEntries decides two things for each kind the enabled policies use:
// which of its fields the cache keeps, and whether lookup() may read it from
// the cache. Both follow from the fields the policy expressions read.
//
// A policy uses a kind by watching it or by passing it to lookup().
func buildCacheEntries(
	compiler *celenv.Environment,
	policies []config.Policy,
) map[schema.GroupVersionKind]gvkCacheEntry {
	derived := derivePolicyFieldPaths(compiler, policies)
	entries := make(map[schema.GroupVersionKind]gvkCacheEntry, len(derived))

	for gvk, fieldPaths := range derived {
		if !fieldPaths.watched && fieldPaths.wholeLookup != "" {
			// Nothing to cache: no policy watches this GVK, and the lookup that
			// names it reads the returned object as a whole.
			slog.Info("Reading lookup() through the API: it uses the whole object",
				"gvk", gvk.String(), "policy", fieldPaths.wholeLookup)

			continue
		}

		entries[gvk] = fieldPaths.cacheEntryFor(gvk)
	}

	return entries
}

// fieldPathsByGVK is what the policies read, per GVK.
type fieldPathsByGVK map[schema.GroupVersionKind]*gvkFieldPaths

// forGVK returns the entry for gvk, adding an empty one if it has none yet.
func (f fieldPathsByGVK) forGVK(gvk schema.GroupVersionKind) *gvkFieldPaths {
	fieldPaths := f[gvk]
	if fieldPaths == nil {
		fieldPaths = &gvkFieldPaths{}
		f[gvk] = fieldPaths
	}

	return fieldPaths
}

// derivePolicyFieldPaths compiles every expression of every enabled policy and
// records what it reads, off the object the policy watches and off the objects
// its lookup() calls return.
//
// A compile failure counts as reading the watched object whole rather than
// being reported: policy.NewEvaluator compiles the same expression with the
// policy name for context and fails startup there.
func derivePolicyFieldPaths(compiler *celenv.Environment, policies []config.Policy) fieldPathsByGVK {
	derived := make(fieldPathsByGVK)

	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}

		watched := derived.forGVK(policyGVK(policy))
		watched.watched = true

		for _, expression := range policyExpressions(policy) {
			compiled, err := compiler.Compile(expression)
			if err != nil {
				watched.wholeWatch = policy.Name

				continue
			}

			if paths, ok := celenv.ResourceFieldPaths(compiled); ok {
				watched.watchPaths = append(watched.watchPaths, paths...)
			} else {
				watched.wholeWatch = policy.Name
			}

			derived.recordLookupPaths(policy.Name, celenv.LookupTargets(compiled))
		}
	}

	return derived
}

// recordLookupPaths records what the policy reads off each GVK it looks up.
func (f fieldPathsByGVK) recordLookupPaths(policyName string, targets []celenv.LookupTarget) {
	for _, target := range targets {
		gvk := schema.FromAPIVersionAndKind(target.APIVersion, target.Kind)

		lookedUp := f.forGVK(gvk)
		lookedUp.lookedUp = true

		if !target.Derivable {
			lookedUp.wholeLookup = policyName

			continue
		}

		lookedUp.lookupPaths = append(lookedUp.lookupPaths, target.Paths...)
	}
}

// cacheEntryFor decides how the GVK is cached from what the policies read off
// it.
func (f *gvkFieldPaths) cacheEntryFor(gvk schema.GroupVersionKind) gvkCacheEntry {
	if f.wholeWatch != "" {
		slog.Info("Caching full objects: policy fields could not be derived from CEL",
			"gvk", gvk.String(), "policy", f.wholeWatch, "servesLookups", f.lookedUp)

		// Every field is there, so a lookup finds whatever it reads.
		return gvkCacheEntry{servesLookups: f.lookedUp}
	}

	servesLookups := f.lookedUp && f.wholeLookup == ""

	tree := newFieldTree(alwaysRetainedPaths)

	for _, path := range f.watchPaths {
		tree.insert(path)
	}

	if servesLookups {
		for _, path := range f.lookupPaths {
			tree.insert(path)
		}
	}

	if f.wholeLookup != "" {
		slog.Info("Reading lookup() through the API: it uses the whole object",
			"gvk", gvk.String(), "policy", f.wholeLookup)
	}

	slog.Info("Cache transform derived from policy CEL",
		"gvk", gvk.String(),
		"retainedFields", strings.Join(tree.retainedPaths(), " "),
		"servesLookups", servesLookups)

	return gvkCacheEntry{transform: newFieldPruningTransform(tree), servesLookups: servesLookups}
}

func policyExpressions(p config.Policy) []string {
	if p.NodeAssociation == nil {
		return []string{p.Predicate.Expression}
	}

	return []string{p.Predicate.Expression, p.NodeAssociation.Expression}
}

// newFieldPruningTransform returns a cache.TransformFunc that strips every
// field the tree does not retain.
func newFieldPruningTransform(tree *fieldTree) toolscache.TransformFunc {
	return func(in any) (any, error) {
		obj, ok := in.(*unstructured.Unstructured)
		if !ok || obj == nil || obj.Object == nil {
			// Tombstones and structured objects are passed through: a
			// partially understood object is worse than a whole one.
			return in, nil
		}

		obj.Object = prune(obj.Object, tree)

		return obj, nil
	}
}

// fieldTree is a prefix tree of the field paths a cached object must retain.
// keepSubtree marks a node whose value is retained whole, which is what makes
// inserting status.conditions.type beneath status.conditions a no-op.
type fieldTree struct {
	keepSubtree bool
	children    map[string]*fieldTree
}

func newFieldTree(paths [][]string) *fieldTree {
	tree := &fieldTree{}
	for _, path := range paths {
		tree.insert(path)
	}

	return tree
}

// insert records a path as a sequence of segments, descending a level per
// segment and creating the nodes it passes through. Segments are never split or
// joined, so a map key containing dots stays a single level of the tree.
func (t *fieldTree) insert(segments []string) {
	node := t

	for _, segment := range segments {
		if node.keepSubtree {
			return
		}

		if node.children == nil {
			node.children = make(map[string]*fieldTree)
		}

		child := node.children[segment]
		if child == nil {
			child = &fieldTree{}
			node.children[segment] = child
		}

		node = child
	}

	node.keepSubtree = true
	node.children = nil
}

// retainedPaths renders the retained paths for logging, in sorted order and
// collapsed so that a retained subtree is reported once. The dotted form is for
// operators to read and is not used to prune, which is why it can be lossy for
// a map key that contains dots.
func (t *fieldTree) retainedPaths() []string {
	var out []string

	for name, child := range t.children {
		out = child.appendRetainedPaths(out, name)
	}

	slices.Sort(out)

	return out
}

// appendRetainedPaths appends the dotted paths retained at or beneath t, each
// carrying prefix, the path by which t was reached.
func (t *fieldTree) appendRetainedPaths(out []string, prefix string) []string {
	if t.keepSubtree || len(t.children) == 0 {
		return append(out, prefix)
	}

	for name, child := range t.children {
		out = child.appendRetainedPaths(out, prefix+"."+name)
	}

	return out
}

// prune returns a copy of obj holding only the fields the tree retains. Values
// under a retained path are carried over by reference; the maps that are dropped
// are the bulk of what an unstructured object costs, because every field of
// every object is a map entry with a boxed value.
func prune(obj map[string]any, tree *fieldTree) map[string]any {
	if tree == nil || tree.keepSubtree {
		return obj
	}

	pruned := make(map[string]any, len(tree.children))

	for name, child := range tree.children {
		if value, present := obj[name]; present {
			pruned[name] = pruneValue(value, child)
		}
	}

	return pruned
}

// pruneValue returns value with the fields tree does not retain removed.
func pruneValue(value any, tree *fieldTree) any {
	if tree.keepSubtree {
		return value
	}

	nested, ok := value.(map[string]any)
	if !ok {
		// A path continues past a value that is not an object, so keep the
		// value whole rather than guess at its shape.
		return value
	}

	return prune(nested, tree)
}
