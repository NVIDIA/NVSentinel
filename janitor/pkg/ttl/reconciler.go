// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package ttl

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Option configures a Reconciler at construction time.
type Option[T client.Object] func(*Reconciler[T])

// WithDefaultTTL sets the system-wide default TTL applied to resources that
// don't already carry a TTL annotation. A zero duration disables the default
// (explicit per-resource annotations still work).
func WithDefaultTTL[T client.Object](d time.Duration) Option[T] {
	return func(r *Reconciler[T]) {
		r.defaultTTL = d
	}
}

// WithClock injects a custom clock for deterministic testing.
// A nil clock is ignored so the caller cannot accidentally clear the default.
func WithClock[T client.Object](c Clock) Option[T] {
	return func(r *Reconciler[T]) {
		if c == nil {
			return
		}

		r.clock = c
	}
}

// WithMetrics registers a callback invoked once per successful TTL deletion,
// letting the caller increment a component-scoped metric (e.g. a Prometheus
// counter) without the ttl package taking a dependency on the metrics layout.
// A nil fn is ignored so the caller cannot accidentally clear the default no-op.
func WithMetrics[T client.Object](fn func(kind string)) Option[T] {
	return func(r *Reconciler[T]) {
		if fn == nil {
			return
		}

		r.onDeleted = fn
	}
}

// Reconciler is a generic TTL reconciler for any cluster-scoped
// Kubernetes resource type T. A separate Reconciler is instantiated per CRD
// via Setup, and each is hard-bound to its type at compile time.
type Reconciler[T client.Object] struct {
	client.Client
	defaultTTL time.Duration
	clock      Clock
	onDeleted  func(kind string)
	kind       string
}

// NewReconciler constructs a Reconciler for type T.
func NewReconciler[T client.Object](c client.Client, opts ...Option[T]) *Reconciler[T] {
	r := &Reconciler[T]{
		Client:    c,
		clock:     clock.RealClock{},
		onDeleted: func(string) {},
		kind:      kindFromType[T](),
	}

	for _, opt := range opts {
		opt(r)
	}

	return r
}

// Reconcile implements the controller-runtime reconcile.Reconciler interface.
// It fetches the object, delegates the decision to Process, persists any
// annotation mutations, and deletes the object when it has expired.
func (r *Reconciler[T]) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := newZeroRef[T]()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	result, err := Process(ctx, obj, r.defaultTTL, r.clock)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("process %s %q: %w", r.kind, obj.GetName(), err)
	}

	if result.Changed {
		if err := r.applyAnnotations(ctx, obj); err != nil {
			return ctrl.Result{}, fmt.Errorf("update %s %q annotations: %w", r.kind, obj.GetName(), err)
		}
	}

	switch result.Action {
	case ActionExpired:
		slog.InfoContext(ctx, "deleting expired resource",
			"kind", r.kind, "name", obj.GetName(),
			"expiry", obj.GetAnnotations()[ExpiryAnnotation])

		if err := r.Delete(ctx, obj); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(
				fmt.Errorf("delete expired %s %q: %w", r.kind, obj.GetName(), err))
		}

		r.onDeleted(r.kind)

		return ctrl.Result{}, nil
	case ActionRequeue:
		return ctrl.Result{RequeueAfter: result.RequeueAfter}, nil
	case ActionNoop:
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{}, nil
	}
}

// Ensure Reconciler implements reconcile.Reconciler.
var _ reconcile.Reconciler = (*Reconciler[client.Object])(nil)

// applyAnnotations writes the TTL annotations from obj, refetching on
// conflict so a racing writer (e.g. GPUReset's finalizer) doesn't block us.
func (r *Reconciler[T]) applyAnnotations(ctx context.Context, obj T) error {
	mine := obj.GetAnnotations()
	key := client.ObjectKeyFromObject(obj)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := newZeroRef[T]()
		if err := r.Get(ctx, key, fresh); err != nil {
			return err
		}

		annots := fresh.GetAnnotations()
		if annots == nil {
			annots = map[string]string{}
		}

		for _, k := range []string{TTLAnnotation, ExpiryAnnotation} {
			if v, ok := mine[k]; ok {
				annots[k] = v
			}
		}

		fresh.SetAnnotations(annots)

		return r.Update(ctx, fresh)
	})
}

// newZeroRef returns a zero-value instance of T. For T = *RebootNode,
// it returns &RebootNode{}.
func newZeroRef[T client.Object]() T {
	var zero T

	t := reflect.TypeOf(zero).Elem()

	return reflect.New(t).Interface().(T)
}

// kindFromType returns the short name of the concrete type T for use as a
// metrics label and log field. For T = *RebootNode, it returns "RebootNode".
func kindFromType[T client.Object]() string {
	var zero T

	t := reflect.TypeOf(zero)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	return t.Name()
}
