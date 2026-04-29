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

// Package ttl provides a generic, annotation-driven TTL reconciler for
// cluster-scoped Custom Resources. It is used by the janitor binary to
// auto-delete completed maintenance CRs (RebootNode, GPUReset, TerminateNode)
// once they are past their TTL.
//
// See docs/designs/037-janitor-cr-ttl-cleanup.md for the design rationale.
package ttl

import (
	"context"
	"log/slog"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Annotation keys used by the TTL reconciler.
const (
	// TTLAnnotation specifies the time-to-live duration for the resource
	// (e.g. "336h"). If absent, the system default is applied on first reconcile.
	TTLAnnotation = "nvsentinel.nvidia.com/ttl"

	// ExpiryAnnotation stores the calculated expiry time in RFC3339 format.
	// It is written on first reconcile and never recomputed, so clock skew
	// and controller restarts don't shift the expiry forward.
	ExpiryAnnotation = "nvsentinel.nvidia.com/expiry"

	// PreserveAnnotation, when set to "true", prevents TTL-based deletion.
	// This is the per-CR escape hatch.
	PreserveAnnotation = "nvsentinel.nvidia.com/preserve"
)

// maxRequeueInterval caps how long the reconciler will sleep between checks,
// so annotation edits, clock changes, or configuration changes are picked up
// within at most this window.
const maxRequeueInterval = time.Hour

// Clock abstracts time.Now for testability.
type Clock interface {
	Now() time.Time
}

// Action describes what the caller should do after Process returns.
type Action int

const (
	// ActionNoop means no action needed (resource is deleting, preserved,
	// or TTL is effectively disabled).
	ActionNoop Action = iota

	// ActionRequeue means the resource is not yet expired; the caller should
	// requeue after the returned RequeueAfter.
	ActionRequeue

	// ActionExpired means the resource has expired and should be deleted.
	ActionExpired
)

// Result is returned by Process to tell the caller what happened.
type Result struct {
	// Action the caller should take.
	Action Action

	// RequeueAfter is set when Action == ActionRequeue.
	RequeueAfter time.Duration

	// Changed is true when annotations were mutated in memory and the caller
	// should persist them via Update.
	Changed bool
}

// Process applies the TTL state machine to obj in memory. It mutates
// annotations but does NOT call Update or Delete — the caller acts on the
// returned Result.
//
// The defaultTTL is applied only when the object has no TTL annotation yet.
// A zero defaultTTL means "no automatic default" — objects without an explicit
// TTL annotation are left alone.
func Process(ctx context.Context, obj client.Object, defaultTTL time.Duration, clock Clock) (Result, error) {
	if skipEarly(ctx, obj) {
		return Result{Action: ActionNoop}, nil
	}

	if obj.GetAnnotations() == nil {
		obj.SetAnnotations(map[string]string{})
	}

	changed := applyDefaultTTLIfNeeded(ctx, obj, defaultTTL)

	ttlDuration, ok := readTTL(ctx, obj)
	if !ok {
		return Result{Action: ActionNoop, Changed: changed}, nil
	}

	expiryTime, setExpiry := getOrCalculateExpiry(ctx, obj, ttlDuration, clock)
	changed = changed || setExpiry

	if !clock.Now().Before(expiryTime) {
		slog.InfoContext(ctx, "resource has expired",
			"name", obj.GetName(), "expiry", expiryTime.Format(time.RFC3339))

		return Result{Action: ActionExpired, Changed: changed}, nil
	}

	remaining := expiryTime.Sub(clock.Now())
	if remaining > maxRequeueInterval {
		remaining = maxRequeueInterval
	}

	return Result{Action: ActionRequeue, RequeueAfter: remaining, Changed: changed}, nil
}

// skipEarly returns true when the resource is being deleted or has been
// explicitly preserved, meaning the TTL state machine should take no action.
func skipEarly(ctx context.Context, obj client.Object) bool {
	if !obj.GetDeletionTimestamp().IsZero() {
		slog.DebugContext(ctx, "resource is being deleted, skipping TTL processing",
			"name", obj.GetName())

		return true
	}

	if obj.GetAnnotations()[PreserveAnnotation] == "true" {
		slog.DebugContext(ctx, "resource has preserve annotation, skipping TTL processing",
			"name", obj.GetName())

		return true
	}

	return false
}

// readTTL returns the parsed TTL annotation, or (0, false) when no usable TTL
// is set. A TTL of zero (or negative) is treated as "no TTL" to mirror the
// defaultTTL = 0 semantics.
func readTTL(ctx context.Context, obj client.Object) (time.Duration, bool) {
	ttlStr, hasTTL := obj.GetAnnotations()[TTLAnnotation]
	if !hasTTL {
		return 0, false
	}

	d, err := time.ParseDuration(ttlStr)
	if err != nil {
		slog.WarnContext(ctx, "invalid TTL duration, ignoring",
			"name", obj.GetName(), "ttl", ttlStr, "error", err)

		return 0, false
	}

	if d <= 0 {
		return 0, false
	}

	return d, true
}

// applyDefaultTTLIfNeeded sets the TTL annotation to the system default when
// it is missing. Returns true when the annotation was added.
func applyDefaultTTLIfNeeded(ctx context.Context, obj client.Object, defaultTTL time.Duration) bool {
	annotations := obj.GetAnnotations()
	if _, ok := annotations[TTLAnnotation]; ok {
		return false
	}

	if defaultTTL <= 0 {
		return false
	}

	annotations[TTLAnnotation] = defaultTTL.String()
	slog.InfoContext(ctx, "applying default TTL to resource",
		"name", obj.GetName(), "ttl", defaultTTL)

	return true
}

// getOrCalculateExpiry returns the expiry time, reading it from the annotation
// if present (and parseable) or computing it from clock.Now() + ttl. The
// second return value is true when a new expiry was written to annotations.
func getOrCalculateExpiry(
	ctx context.Context, obj client.Object, ttl time.Duration, clock Clock,
) (time.Time, bool) {
	annotations := obj.GetAnnotations()

	if existing, ok := annotations[ExpiryAnnotation]; ok {
		if t, err := time.Parse(time.RFC3339, existing); err == nil {
			return t, false
		}

		slog.WarnContext(ctx, "invalid expiry annotation, recomputing",
			"name", obj.GetName(), "expiry", existing)
	}

	expiry := clock.Now().Add(ttl)
	annotations[ExpiryAnnotation] = expiry.Format(time.RFC3339)

	return expiry, true
}
