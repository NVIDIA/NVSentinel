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

package healthevents

import (
	"context"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

// TTLController reconciles HealthEvent resources to delete them after TTL expiry.
// It watches for events in Resolved or Cancelled phase and deletes them after
// the configured TTL period has elapsed.
type TTLController struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// PolicyConfigMapName is the name of the ConfigMap containing TTL policy.
	PolicyConfigMapName string

	// PolicyConfigMapNamespace is the namespace of the policy ConfigMap.
	PolicyConfigMapNamespace string

	// DefaultTTL is the default TTL if ConfigMap is not found.
	DefaultTTL time.Duration

	// MaxConcurrentReconciles is the maximum number of concurrent Reconciles.
	MaxConcurrentReconciles int

	// BatchSize is the maximum number of events to delete per cycle.
	BatchSize int
}

// TTLPolicy holds the parsed TTL configuration.
type TTLPolicy struct {
	TTLAfterResolved    time.Duration
	RetentionMode       string // "standard" or "compliance"
	ComplianceRetention time.Duration
	BatchSize           int
	CleanupInterval     time.Duration
	MaxEventsPerNode    int
	MaxTotalEvents      int
}

// DefaultTTLPolicy returns the default TTL policy.
func DefaultTTLPolicy() TTLPolicy {
	return TTLPolicy{
		TTLAfterResolved:    168 * time.Hour, // 7 days
		RetentionMode:       "standard",
		ComplianceRetention: 2160 * time.Hour, // 90 days
		BatchSize:           100,
		CleanupInterval:     5 * time.Minute,
		MaxEventsPerNode:    0, // unlimited
		MaxTotalEvents:      0, // unlimited
	}
}

// +kubebuilder:rbac:groups=nvsentinel.nvidia.com,resources=healthevents,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles HealthEvent TTL cleanup.
// It checks if the event has exceeded its TTL and deletes it if so.
func (r *TTLController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := klog.FromContext(ctx).WithValues("healthevent", req.Name)
	log.V(2).Info("Reconciling HealthEvent for TTL")

	// Fetch the HealthEvent
	var healthEvent nvsentinelv1alpha1.HealthEvent
	if err := r.Get(ctx, req.NamespacedName, &healthEvent); err != nil {
		if apierrors.IsNotFound(err) {
			// Already deleted
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get HealthEvent")
		return ctrl.Result{}, err
	}

	// Only process resolved or cancelled events
	if healthEvent.Status.Phase != nvsentinelv1alpha1.PhaseResolved &&
		healthEvent.Status.Phase != nvsentinelv1alpha1.PhaseCancelled {
		log.V(2).Info("Skipping event not in terminal phase", "phase", healthEvent.Status.Phase)
		return ctrl.Result{}, nil
	}

	// Get TTL policy
	policy := r.getPolicy(ctx)

	// Calculate effective TTL
	ttl := policy.TTLAfterResolved
	if policy.RetentionMode == "compliance" {
		ttl = policy.ComplianceRetention
	}

	// TTL of 0 means disabled
	if ttl == 0 {
		log.V(2).Info("TTL cleanup disabled")
		return ctrl.Result{}, nil
	}

	// Check if event has expired
	resolvedAt := healthEvent.Status.ResolvedAt
	if resolvedAt == nil {
		// Use creation time if resolvedAt not set
		resolvedAt = &healthEvent.CreationTimestamp
	}

	age := time.Since(resolvedAt.Time)
	if age < ttl {
		// Not yet expired, requeue for later
		requeueAfter := ttl - age + time.Minute // Add buffer
		log.V(2).Info("Event not yet expired, requeueing",
			"age", age.Round(time.Second),
			"ttl", ttl,
			"requeueAfter", requeueAfter.Round(time.Second),
		)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	// Delete the event
	log.Info("Deleting expired HealthEvent",
		"age", age.Round(time.Second),
		"ttl", ttl,
		"nodeName", healthEvent.Spec.NodeName,
	)

	if err := r.Delete(ctx, &healthEvent); err != nil {
		if apierrors.IsNotFound(err) {
			// Already deleted
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to delete HealthEvent")
		return ctrl.Result{}, err
	}

	// Record metrics
	ttlDeletionsTotal.WithLabelValues(healthEvent.Spec.NodeName, string(healthEvent.Status.Phase)).Inc()

	log.Info("Deleted expired HealthEvent", "name", healthEvent.Name)
	return ctrl.Result{}, nil
}

// getPolicy reads the TTL policy from ConfigMap or returns defaults.
func (r *TTLController) getPolicy(ctx context.Context) TTLPolicy {
	log := klog.FromContext(ctx)
	policy := DefaultTTLPolicy()

	// Override with controller's DefaultTTL if set
	if r.DefaultTTL != 0 {
		policy.TTLAfterResolved = r.DefaultTTL
	}

	if r.PolicyConfigMapName == "" || r.PolicyConfigMapNamespace == "" {
		return policy
	}

	var cm corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{
		Name:      r.PolicyConfigMapName,
		Namespace: r.PolicyConfigMapNamespace,
	}, &cm)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to get policy ConfigMap, using defaults")
		}
		return policy
	}

	// Parse TTL after resolved
	if val, ok := cm.Data["ttlAfterResolved"]; ok {
		if d, err := time.ParseDuration(val); err == nil {
			policy.TTLAfterResolved = d
		} else {
			log.V(1).Info("Invalid ttlAfterResolved, using default", "value", val)
		}
	}

	// Parse retention mode
	if val, ok := cm.Data["retentionMode"]; ok {
		if val == "standard" || val == "compliance" {
			policy.RetentionMode = val
		}
	}

	// Parse compliance retention
	if val, ok := cm.Data["complianceRetention"]; ok {
		if d, err := time.ParseDuration(val); err == nil {
			policy.ComplianceRetention = d
		}
	}

	// Parse batch size
	if val, ok := cm.Data["batchSize"]; ok {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			policy.BatchSize = n
		}
	}

	// Parse cleanup interval
	if val, ok := cm.Data["cleanupInterval"]; ok {
		if d, err := time.ParseDuration(val); err == nil {
			policy.CleanupInterval = d
		}
	}

	// Parse max events per node
	if val, ok := cm.Data["maxEventsPerNode"]; ok {
		if n, err := strconv.Atoi(val); err == nil && n >= 0 {
			policy.MaxEventsPerNode = n
		}
	}

	// Parse max total events
	if val, ok := cm.Data["maxTotalEvents"]; ok {
		if n, err := strconv.Atoi(val); err == nil && n >= 0 {
			policy.MaxTotalEvents = n
		}
	}

	return policy
}

// SetupWithManager sets up the controller with the Manager.
// Note: We intentionally do NOT use phase-based predicates because status subresource
// updates may not trigger watch events reliably. Instead, we filter by phase in Reconcile.
func (r *TTLController) SetupWithManager(mgr ctrl.Manager) error {
	// Register metrics
	registerTTLMetrics()

	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent == 0 {
		maxConcurrent = 1 // TTL cleanup is not latency-sensitive
	}

	if r.DefaultTTL == 0 {
		r.DefaultTTL = 168 * time.Hour // 7 days
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&nvsentinelv1alpha1.HealthEvent{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxConcurrent,
		}).
		Named("ttl").
		Complete(r)
}

// RunPeriodicCleanup runs periodic cleanup of old events.
// This is useful for catching events that might have been missed.
func (r *TTLController) RunPeriodicCleanup(ctx context.Context) {
	log := klog.FromContext(ctx).WithName("ttl-cleanup")

	// Get initial policy for interval
	policy := r.getPolicy(ctx)
	ticker := time.NewTicker(policy.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.cleanupExpiredEvents(ctx, log)

			// Update ticker if policy changed
			newPolicy := r.getPolicy(ctx)
			if newPolicy.CleanupInterval != policy.CleanupInterval {
				ticker.Reset(newPolicy.CleanupInterval)
				policy = newPolicy
			}
		}
	}
}

// cleanupExpiredEvents lists and deletes expired events in batches.
func (r *TTLController) cleanupExpiredEvents(ctx context.Context, log klog.Logger) {
	policy := r.getPolicy(ctx)

	// TTL of 0 means disabled
	ttl := policy.TTLAfterResolved
	if policy.RetentionMode == "compliance" {
		ttl = policy.ComplianceRetention
	}
	if ttl == 0 {
		return
	}

	// List all resolved/cancelled events
	var events nvsentinelv1alpha1.HealthEventList
	if err := r.List(ctx, &events); err != nil {
		log.Error(err, "Failed to list HealthEvents for cleanup")
		return
	}

	deleted := 0
	for i := range events.Items {
		if deleted >= policy.BatchSize {
			log.V(1).Info("Reached batch size limit", "deleted", deleted)
			break
		}

		event := &events.Items[i]

		// Skip non-terminal events
		if event.Status.Phase != nvsentinelv1alpha1.PhaseResolved &&
			event.Status.Phase != nvsentinelv1alpha1.PhaseCancelled {
			continue
		}

		// Check expiry
		resolvedAt := event.Status.ResolvedAt
		if resolvedAt == nil {
			resolvedAt = &event.CreationTimestamp
		}

		if time.Since(resolvedAt.Time) < ttl {
			continue
		}

		// Delete
		if err := r.Delete(ctx, event); err != nil {
			if !apierrors.IsNotFound(err) {
				log.Error(err, "Failed to delete expired event", "name", event.Name)
			}
			continue
		}

		deleted++
		ttlDeletionsTotal.WithLabelValues(event.Spec.NodeName, string(event.Status.Phase)).Inc()
	}

	if deleted > 0 {
		log.Info("Periodic cleanup completed", "deleted", deleted)
	}
}
