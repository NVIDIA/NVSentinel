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

package cel

import (
	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// lookupArgs is the arity of lookup(apiVersion, kind, namespace, name).
const lookupArgs = 4

// LookupTarget is a GVK an expression reads through lookup(), with the field
// paths it reads off the returned object.
type LookupTarget struct {
	APIVersion string
	Kind       string
	// Paths carry the meaning they do in ResourceFieldPaths: each is a slice of
	// segments standing for the whole subtree beneath it. They are nil when
	// Derivable is false.
	Paths [][]string
	// Derivable is false when the expression uses a returned object as a whole,
	// as in size(lookup(...)), or when the walk of the expression stopped
	// before the end, because no set of paths describes what it reads then. The
	// GVK must be read through the API in that case, since a pruned cache entry
	// would silently answer with fields it dropped.
	Derivable bool
}

// LookupTargets returns the GVKs compiled reads through lookup(), one entry per
// GVK however many calls name it, for the calls that give their apiVersion and
// kind as string literals. A call that computes either is absent: nothing can
// be cached for a GVK that is not known until the expression runs.
//
// No target is derivable once the walk has stopped at an expression that uses
// the watched object as a whole. The paths gathered up to that point describe
// the calls walked so far and no others, and a call beyond it may read further
// fields of a GVK already gathered, which pruning to those paths would drop.
func LookupTargets(compiled *cel.Ast) []LookupTarget {
	if compiled == nil || compiled.NativeRep() == nil {
		return nil
	}

	w := walkExpression(compiled)
	targets := mergeFieldPathsByGVK(w.lookupFieldPaths)

	if !w.ok {
		for i := range targets {
			targets[i].Derivable = false
			targets[i].Paths = nil
		}
	}

	return targets
}

// lookupFieldPath is one field path an expression takes off the object a
// lookup() call returned, together with the apiVersion and kind that call
// named. An empty path means the expression used the whole object.
type lookupFieldPath struct {
	apiVersion string
	kind       string
	path       []string
}

// mergeFieldPathsByGVK turns the field paths into one target per GVK, in the
// order the GVKs first appeared. A path that is empty leaves its GVK
// underivable, and the paths gathered for that GVK are then of no use.
func mergeFieldPathsByGVK(fieldPaths []lookupFieldPath) []LookupTarget {
	if len(fieldPaths) == 0 {
		return nil
	}

	// lookupTargets holds one target per GVK, in the order the GVKs first
	// appeared. positionOfGVK says where a GVK's target sits in lookupTargets.
	lookupTargets := make([]LookupTarget, 0, len(fieldPaths))
	positionOfGVK := make(map[schema.GroupVersionKind]int, len(fieldPaths))

	for _, fieldPath := range fieldPaths {
		gvk := schema.FromAPIVersionAndKind(fieldPath.apiVersion, fieldPath.kind)

		position, seen := positionOfGVK[gvk]
		if !seen {
			position = len(lookupTargets)
			positionOfGVK[gvk] = position

			lookupTargets = append(lookupTargets, LookupTarget{
				APIVersion: fieldPath.apiVersion,
				Kind:       fieldPath.kind,
				Derivable:  true,
			})
		}

		target := &lookupTargets[position]

		if len(fieldPath.path) == 0 {
			target.Derivable = false

			continue
		}

		target.Paths = append(target.Paths, fieldPath.path)
	}

	for i := range lookupTargets {
		target := &lookupTargets[i]

		if target.Derivable {
			target.Paths = sortedUniquePaths(target.Paths)
		} else {
			target.Paths = nil
		}
	}

	return lookupTargets
}
