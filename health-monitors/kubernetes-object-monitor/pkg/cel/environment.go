// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LookupCache is what lookup() needs of the manager's cache: to read an object
// from it, and to ask whether the informer behind a GVK has caught up, since a
// read of one that has not blocks until it does. cache.Cache satisfies it.
type LookupCache interface {
	client.Reader
	GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error)
}

type Environment struct {
	env *cel.Env
	// reader backs lookup() for every GVK the cache holds no entry for. It must
	// read straight from the API server: a cached read of a GVK with no entry
	// starts a cluster-wide informer for it on demand and holds it in full.
	reader client.Reader
	// objectCache and cachedGVKs back lookup() for the GVKs UseCacheForLookups
	// named. reportedFallback holds the GVKs whose fallback to the API server
	// has been logged, so a misconfiguration is logged once rather than per
	// evaluation. All three are written before the manager starts and read
	// under evalMu, which Evaluate holds while lookup() runs.
	objectCache      LookupCache
	cachedGVKs       map[schema.GroupVersionKind]bool
	reportedFallback map[schema.GroupVersionKind]bool
	evalMu           sync.Mutex
	ctx              context.Context
}

// NewEnvironment returns an Environment that compiles and evaluates policy
// expressions. The reader backs lookup(), and must read straight from the API
// server: pass mgr.GetAPIReader(), not mgr.GetClient(). Call
// UseCacheForLookups to serve the GVKs that do have a cache entry from it.
func NewEnvironment(r client.Reader) (*Environment, error) {
	e := &Environment{
		reader:           r,
		reportedFallback: make(map[schema.GroupVersionKind]bool),
	}

	env, err := cel.NewEnv(
		cel.Variable(ResourceVar, cel.DynType),
		cel.Variable("now", cel.TimestampType),
		cel.Function("lookup",
			cel.Overload("lookup_string_string_string_string",
				[]*cel.Type{cel.StringType, cel.StringType, cel.StringType, cel.StringType},
				cel.DynType,
				cel.FunctionBinding(e.lookup),
			),
		),
	)
	if err != nil {
		slog.Error("Failed to create CEL environment", "error", err)
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	e.env = env

	return e, nil
}

// NewCompilerEnvironment returns an Environment that declares the same
// variables and functions as NewEnvironment but has no reader behind lookup().
// It exists because the cache options are built before the manager, and
// deriving the fields a policy reads needs a compiled AST at that point.
//
// Only Compile may be called on it. Evaluating an expression that calls
// lookup() has nothing to resolve against.
func NewCompilerEnvironment() (*Environment, error) {
	return NewEnvironment(nil)
}

// UseCacheForLookups routes lookup() reads of gvks through objectCache, which
// is the manager's cache.
//
// Only a GVK whose cache entry retains every field the policies read off a
// looked-up object may be named. Reading an entry pruned without regard for
// those fields returns an object missing them, which CEL cannot tell from an
// object that never had them, so the policy would quietly evaluate against
// absent fields.
func (e *Environment) UseCacheForLookups(objectCache LookupCache, gvks []schema.GroupVersionKind) {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()

	e.objectCache = objectCache
	e.cachedGVKs = make(map[schema.GroupVersionKind]bool, len(gvks))

	for _, gvk := range gvks {
		e.cachedGVKs[gvk] = true
	}

	slog.Info("lookup() reads these GVKs from the cache", "gvks", gvks)
}

func (e *Environment) Compile(expression string) (*cel.Ast, error) {
	ast, issues := e.env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		slog.Error("Failed to compile CEL expression", "error", issues.Err())
		return nil, fmt.Errorf("CEL compilation failed: %w", issues.Err())
	}

	slog.Info("Successfully compiled CEL expression", "expression", expression)

	return ast, nil
}

func (e *Environment) Evaluate(ast *cel.Ast, resource any, ctx context.Context) (ref.Val, error) {
	e.evalMu.Lock()
	defer e.evalMu.Unlock()

	e.ctx = ctx

	prg, err := e.env.Program(ast)
	if err != nil {
		slog.Error("Failed to create CEL program", "error", err)
		return nil, fmt.Errorf("failed to create CEL program: %w", err)
	}

	result, _, err := prg.ContextEval(ctx, map[string]any{
		"resource": resource,
		"now":      time.Now(),
	})
	if err != nil {
		slog.Error("Failed to evaluate CEL expression", "error", err)
		return nil, fmt.Errorf("CEL evaluation failed: %w", err)
	}

	slog.Info("Successfully evaluated CEL expression", "result", result)

	return result, nil
}

func (e *Environment) lookup(args ...ref.Val) ref.Val {
	if len(args) != 4 {
		slog.Error("Lookup requires 4 arguments: version, kind, namespace, name")
		return types.NewErr("lookup requires 4 arguments: version, kind, namespace, name")
	}

	version, ok := args[0].(types.String)
	if !ok {
		slog.Error("Lookup arg[0] (version) must be string")
		return types.NewErr("lookup arg[0] (version) must be string")
	}

	kind, ok := args[1].(types.String)
	if !ok {
		slog.Error("Lookup arg[1] (kind) must be string")
		return types.NewErr("lookup arg[1] (kind) must be string")
	}

	namespace, ok := args[2].(types.String)
	if !ok {
		slog.Error("Lookup arg[2] (namespace) must be string")
		return types.NewErr("lookup arg[2] (namespace) must be string")
	}

	name, ok := args[3].(types.String)
	if !ok {
		slog.Error("Lookup arg[3] (name) must be string")
		return types.NewErr("lookup arg[3] (name) must be string")
	}

	ctx := e.ctx

	obj := &unstructured.Unstructured{}

	obj.SetAPIVersion(string(version))
	obj.SetKind(string(kind))

	key := client.ObjectKey{
		Namespace: string(namespace),
		Name:      string(name),
	}

	if err := e.getForLookup(ctx, key, obj); err != nil {
		slog.Error("Failed to get object for lookup", "error", err)
		return types.NullValue
	}

	slog.Info("Successfully got object for lookup", "object", obj.Object)

	return types.DefaultTypeAdapter.NativeToValue(obj.Object)
}

// getForLookup reads the named object into obj, from the cache when its GVK has
// an entry that retains what the policies read off it and an informer that has
// caught up, and from the API server otherwise.
//
// A cached read falls back to the API on anything but a missing object, which
// keeps a policy answering as it did before the cache was asked at all.
func (e *Environment) getForLookup(
	ctx context.Context,
	key client.ObjectKey,
	obj *unstructured.Unstructured,
) error {
	gvk := obj.GroupVersionKind()

	if !e.cachedGVKs[gvk] || !e.cacheHasCaughtUp(ctx, obj) {
		return e.reader.Get(ctx, key, obj)
	}

	err := e.objectCache.Get(ctx, key, obj)
	if err == nil || apierrors.IsNotFound(err) {
		return err
	}

	e.reportFallback(gvk, err)

	obj.SetGroupVersionKind(gvk)

	return e.reader.Get(ctx, key, obj)
}

// cacheHasCaughtUp reports whether the cache can answer for obj's GVK without
// waiting, and creates the informer behind it on the first call.
//
// Waiting is what has to be avoided. That informer lists every object of the
// GVK before it can answer, and a GVK whose cluster-wide list and watch a
// deployment withheld never answers at all; since Evaluate is serialised, a
// read that waits on either holds up every evaluation in the process and not
// just its own. Reads go to the API server until the informer has caught up,
// and for the life of the process if it cannot.
func (e *Environment) cacheHasCaughtUp(ctx context.Context, obj *unstructured.Unstructured) bool {
	informer, err := e.objectCache.GetInformer(ctx, obj, cache.BlockUntilSynced(false))
	if err != nil {
		e.reportFallback(obj.GroupVersionKind(), err)

		return false
	}

	return informer.HasSynced()
}

// reportFallback reports a GVK read through the API server although it has a
// cache entry, once per GVK: a misconfiguration that persists would otherwise
// log on every evaluation.
func (e *Environment) reportFallback(gvk schema.GroupVersionKind, err error) {
	if e.reportedFallback[gvk] {
		return
	}

	e.reportedFallback[gvk] = true

	slog.Warn("Reading lookup() through the API server: the cache cannot serve this GVK. "+
		"Serving it from the cache needs cluster-wide list and watch on it",
		"gvk", gvk.String(), "error", err)
}
