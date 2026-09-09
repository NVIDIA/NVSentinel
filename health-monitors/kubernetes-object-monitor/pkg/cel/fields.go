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
	"slices"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
)

// ResourceVar is the name of the CEL variable bound to the watched object.
const ResourceVar = "resource"

// LookupFunc is the name of the CEL function that reads a second object.
const LookupFunc = "lookup"

// ResourceFieldPaths returns the field paths of the resource variable that
// compiled reads, each as its own slice of segments.
// `resource.status.conditions.exists(c, c.type == "Ready")` yields
// [["status", "conditions"]].
//
// Paths are segmented rather than dotted because a segment can be a map key
// taken from a string literal, and Kubernetes map keys routinely contain dots:
// metadata.labels["nvidia.com/gpu.present"] is three segments, not five, and
// joining it would make the label indistinguishable from a nested field.
//
// A returned path stands for the entire subtree beneath it. That is what makes
// comprehensions safe to derive fields from: recording status.conditions covers
// every field of every element, so the per-element bindings need no handling of
// their own. The same holds for a computed index such as
// metadata.labels[resource.spec.nodeName], which records metadata.labels and
// therefore retains whichever key is present at runtime.
//
// isComplete is false when the expression uses the object as a whole rather
// than through a field access, as in size(resource), because no set of paths
// describes what such an expression reads. Callers must cache the object in
// full in that case: pruning against an incomplete field set silently changes
// evaluation results.
func ResourceFieldPaths(compiled *cel.Ast) (fieldPaths [][]string, isComplete bool) {
	if compiled == nil || compiled.NativeRep() == nil {
		return nil, false
	}

	w := walkExpression(compiled)
	if !w.ok {
		return nil, false
	}

	return sortedUniquePaths(w.paths), true
}

// walkExpression walks compiled and returns the walker holding what it read.
func walkExpression(compiled *cel.Ast) *fieldWalker {
	w := &fieldWalker{ok: true}
	w.walk(compiled.NativeRep().Expr())

	return w
}

// sortedUniquePaths orders paths and drops the duplicates.
func sortedUniquePaths(paths [][]string) [][]string {
	slices.SortFunc(paths, slices.Compare)

	return slices.CompactFunc(paths, slices.Equal)
}

// fieldWalker collects the field paths an expression graph reads, off the
// resource variable and off the objects lookup() returns. shadowed counts the
// enclosing comprehension bindings named after the resource variable, so an
// iteration or accumulator variable that shadows it is not mistaken for the
// object itself.
type fieldWalker struct {
	paths            [][]string
	lookupFieldPaths []lookupFieldPath
	shadowed         int
	ok               bool
}

func (w *fieldWalker) walk(e ast.Expr) {
	if e == nil || !w.ok {
		return
	}

	if w.recordChain(e) {
		return
	}

	w.walkChildren(e)
}

// recordChain records e if it is a chain of field accesses rooted at an object
// the cache can hold: the resource variable, or a lookup() call that names its
// apiVersion and kind with string literals. It reports whether e was one.
func (w *fieldWalker) recordChain(e ast.Expr) bool {
	base, path, _ := w.resolveChain(e)

	switch {
	case base == nil:
		return false

	case w.isResourceVar(base):
		if len(path) == 0 {
			w.ok = false

			return true
		}

		w.paths = append(w.paths, slices.Clone(path))
		w.walkIndexKeys(e)

		return true

	case isLookupCall(base):
		w.recordLookup(base.AsCall(), path)
		w.walkIndexKeys(e)
		// The arguments name the object to read, and reach the resource or a
		// further lookup to do so.
		w.walkCall(base.AsCall())

		return true

	default:
		return false
	}
}

// isResourceVar reports whether base is the resource variable itself rather
// than a comprehension binding that shadows its name.
func (w *fieldWalker) isResourceVar(base ast.Expr) bool {
	return base.Kind() == ast.IdentKind && base.AsIdent() == ResourceVar && w.shadowed == 0
}

// recordLookup records one field path taken off the object call returned. A
// call whose apiVersion or kind is computed is dropped: nothing can be cached
// for a GVK that is not known until the expression runs, so such a call reads
// through the API and needs no fields derived for it.
func (w *fieldWalker) recordLookup(call ast.CallExpr, path []string) {
	apiVersion, versionOK := stringLiteral(call.Args()[0])
	kind, kindOK := stringLiteral(call.Args()[1])

	if !versionOK || !kindOK {
		return
	}

	w.lookupFieldPaths = append(w.lookupFieldPaths, lookupFieldPath{
		apiVersion: apiVersion,
		kind:       kind,
		path:       slices.Clone(path),
	})
}

func (w *fieldWalker) walkChildren(e ast.Expr) {
	switch e.Kind() {
	case ast.SelectKind:
		w.walk(e.AsSelect().Operand())
	case ast.CallKind:
		w.walkCall(e.AsCall())
	case ast.ListKind:
		for _, element := range e.AsList().Elements() {
			w.walk(element)
		}
	case ast.MapKind:
		w.walkMapEntries(e.AsMap().Entries())
	case ast.StructKind:
		w.walkStructFields(e.AsStruct().Fields())
	case ast.ComprehensionKind:
		w.walkComprehension(e.AsComprehension())
	case ast.IdentKind, ast.LiteralKind, ast.UnspecifiedExprKind:
	}
}

func (w *fieldWalker) walkCall(call ast.CallExpr) {
	if call.IsMemberFunction() {
		w.walk(call.Target())
	}

	for _, arg := range call.Args() {
		w.walk(arg)
	}
}

func (w *fieldWalker) walkMapEntries(entries []ast.EntryExpr) {
	for _, entry := range entries {
		mapEntry := entry.AsMapEntry()
		w.walk(mapEntry.Key())
		w.walk(mapEntry.Value())
	}
}

func (w *fieldWalker) walkStructFields(fields []ast.EntryExpr) {
	for _, field := range fields {
		w.walk(field.AsStructField().Value())
	}
}

// walkComprehension walks a comprehension with its bindings scoped. The
// iteration range and the accumulator initialiser are evaluated in the
// enclosing scope; the loop body sees the iteration and accumulator variables,
// and the result expression sees only the accumulator.
func (w *fieldWalker) walkComprehension(c ast.ComprehensionExpr) {
	w.walk(c.IterRange())
	w.walk(c.AccuInit())

	w.pushBinding(c.AccuVar())
	w.pushBinding(c.IterVar())

	if c.HasIterVar2() {
		w.pushBinding(c.IterVar2())
	}

	w.walk(c.LoopCondition())
	w.walk(c.LoopStep())

	if c.HasIterVar2() {
		w.popBinding(c.IterVar2())
	}

	w.popBinding(c.IterVar())

	w.walk(c.Result())

	w.popBinding(c.AccuVar())
}

// walkIndexKeys walks the key expressions of the index operations inside an
// already recorded resource-rooted chain. The chain covers the values, but a
// computed key such as metadata.labels[resource.spec.nodeName] reads the
// resource in its own right.
func (w *fieldWalker) walkIndexKeys(e ast.Expr) {
	for w.ok {
		switch e.Kind() {
		case ast.SelectKind:
			e = e.AsSelect().Operand()
		case ast.CallKind:
			call := e.AsCall()
			if !isIndexCall(call) {
				return
			}

			w.walk(call.Args()[1])

			e = call.Args()[0]
		case ast.ComprehensionKind, ast.IdentKind, ast.ListKind,
			ast.LiteralKind, ast.MapKind, ast.StructKind, ast.UnspecifiedExprKind:
			return
		}
	}
}

// resolveChain interprets e as a chain of field accesses and returns the
// expression the chain is rooted at, along with the field path it reads from
// that root. base is nil when e is not a chain, so nothing roots it. An inexact
// path is one truncated by a computed index: the subtree at path is retained
// whole, so accesses beneath it are already covered and need not extend it.
func (w *fieldWalker) resolveChain(e ast.Expr) (base ast.Expr, path []string, exact bool) {
	switch e.Kind() {
	case ast.IdentKind:
		return e, nil, true
	case ast.SelectKind:
		return w.resolveSelectChain(e.AsSelect())
	case ast.CallKind:
		if call := e.AsCall(); isIndexCall(call) {
			return w.resolveIndexChain(call)
		}

		// Any other call ends the chain. Only lookup() roots one the cache can
		// serve, which the caller decides.
		return e, nil, true
	case ast.ComprehensionKind, ast.ListKind, ast.LiteralKind,
		ast.MapKind, ast.StructKind, ast.UnspecifiedExprKind:
		return nil, nil, false
	}

	return nil, nil, false
}

func (w *fieldWalker) resolveSelectChain(selectExpr ast.SelectExpr) (base ast.Expr, path []string, exact bool) {
	base, path, exact = w.resolveChain(selectExpr.Operand())
	if base == nil || !exact {
		return base, path, exact
	}

	return base, append(path, selectExpr.FieldName()), true
}

func (w *fieldWalker) resolveIndexChain(call ast.CallExpr) (base ast.Expr, path []string, exact bool) {
	base, path, exact = w.resolveChain(call.Args()[0])
	if base == nil || !exact {
		return base, path, exact
	}

	if key, ok := stringLiteral(call.Args()[1]); ok {
		return base, append(path, key), true
	}

	// A computed key can select any entry, so the whole subtree is retained.
	return base, path, false
}

// pushBinding enters a comprehension binding, so that one named after the
// resource variable shadows it for as long as it is in scope.
func (w *fieldWalker) pushBinding(name string) {
	if name == ResourceVar {
		w.shadowed++
	}
}

// popBinding leaves a binding pushBinding entered.
func (w *fieldWalker) popBinding(name string) {
	if name == ResourceVar {
		w.shadowed--
	}
}

func isIndexCall(call ast.CallExpr) bool {
	return call.FunctionName() == operators.Index && !call.IsMemberFunction() && len(call.Args()) == 2
}

func isLookupCall(e ast.Expr) bool {
	if e.Kind() != ast.CallKind {
		return false
	}

	call := e.AsCall()

	return call.FunctionName() == LookupFunc && !call.IsMemberFunction() && len(call.Args()) == lookupArgs
}

func stringLiteral(e ast.Expr) (string, bool) {
	if e.Kind() != ast.LiteralKind {
		return "", false
	}

	value, ok := e.AsLiteral().(types.String)
	if !ok {
		return "", false
	}

	return string(value), true
}
