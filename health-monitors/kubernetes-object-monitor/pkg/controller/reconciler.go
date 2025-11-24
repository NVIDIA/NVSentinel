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
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"

	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/annotations"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type HealthEventPublisher interface {
	PublishHealthEvent(ctx context.Context, policy *config.Policy, nodeName string, isHealthy bool) error
}

type ResourceReconciler struct {
	client.Client
	evaluator     *policy.Evaluator
	publisher     HealthEventPublisher
	annotationMgr *annotations.Manager
	policies      []config.Policy
	gvk           schema.GroupVersionKind
	matchStates   map[string]string
	matchStatesMu sync.RWMutex
}

func NewResourceReconciler(
	c client.Client,
	evaluator *policy.Evaluator,
	pub HealthEventPublisher,
	annotationMgr *annotations.Manager,
	policies []config.Policy,
	gvk schema.GroupVersionKind,
) *ResourceReconciler {
	return &ResourceReconciler{
		Client:        c,
		evaluator:     evaluator,
		publisher:     pub,
		annotationMgr: annotationMgr,
		policies:      policies,
		gvk:           gvk,
		matchStates:   make(map[string]string),
	}
}

func (r *ResourceReconciler) LoadState(ctx context.Context) error {
	allMatches, err := r.annotationMgr.LoadAllMatches(ctx)
	if err != nil {
		return fmt.Errorf("failed to load match state: %w", err)
	}

	r.matchStatesMu.Lock()
	defer r.matchStatesMu.Unlock()

	maps.Copy(r.matchStates, allMatches)

	slog.Info("Loaded policy match state from annotations", "gvk", r.gvk.String(), "matches", len(allMatches))

	return nil
}

func (r *ResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.gvk)

	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return r.handleGetError(ctx, err, req)
	}

	for _, p := range r.policies {
		if !p.Enabled {
			continue
		}

		if err := r.reconcilePolicy(ctx, &p, obj); err != nil {
			slog.Error("Failed to reconcile policy", "policy", p.Name, "resource", req.NamespacedName, "error", err)
			metrics.ReconciliationErrors.WithLabelValues(r.gvk.Kind, "policy_reconcile_error").Inc()
		}
	}

	return ctrl.Result{}, nil
}

func (r *ResourceReconciler) handleGetError(ctx context.Context, err error, req ctrl.Request) (ctrl.Result, error) {
	if client.IgnoreNotFound(err) == nil {
		r.cleanupDeletedResource(ctx, req)
		return ctrl.Result{}, nil
	}

	metrics.ReconciliationErrors.WithLabelValues(r.gvk.Kind, "get_resource_error").Inc()

	return ctrl.Result{}, fmt.Errorf("failed to get resource: %w", err)
}

func (r *ResourceReconciler) cleanupDeletedResource(ctx context.Context, req ctrl.Request) {
	slog.Info("Cleaning up deleted resource", "resource", req.NamespacedName)

	for _, p := range r.policies {
		if !p.Enabled {
			continue
		}

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(r.gvk)
		obj.SetNamespace(req.Namespace)
		obj.SetName(req.Name)

		stateKey := r.getStateKey(&p, obj)

		r.matchStatesMu.RLock()
		nodeName, wasMatched := r.matchStates[stateKey]
		r.matchStatesMu.RUnlock()

		if wasMatched {
			if err := r.publisher.PublishHealthEvent(ctx, &p, nodeName, true); err != nil {
				slog.Error("Failed to publish healthy event for deleted resource",
					"policy", p.Name,
					"resource", req.NamespacedName,
					"node", nodeName,
					"error", err)
				metrics.HealthEventsPublishErrors.WithLabelValues(p.Name, "grpc_error").Inc()
			}

			slog.Debug("Removing match state for deleted resource",
				"resource", req.NamespacedName,
				"node", nodeName,
				"policy", p.Name,
				"stateKey", stateKey)

			r.matchStatesMu.Lock()
			delete(r.matchStates, stateKey)
			r.matchStatesMu.Unlock()

			if err := r.annotationMgr.RemoveMatch(ctx, nodeName, stateKey); err != nil {
				slog.Error("Failed to remove match state from annotation", "node", nodeName, "stateKey", stateKey, "error", err)
			}
		}
	}
}

func (r *ResourceReconciler) reconcilePolicy(
	ctx context.Context,
	p *config.Policy,
	obj *unstructured.Unstructured,
) error {
	matched, err := r.evaluator.EvaluatePredicate(ctx, p.Name, obj)
	if err != nil {
		metrics.PolicyEvaluationErrors.WithLabelValues(p.Name, "cel_error").Inc()
		return fmt.Errorf("predicate evaluation failed: %w", err)
	}

	nodeName, err := r.evaluator.EvaluateNodeAssociation(ctx, p.Name, obj)
	if err != nil {
		metrics.PolicyEvaluationErrors.WithLabelValues(p.Name, "node_association_error").Inc()
		return fmt.Errorf("node association evaluation failed: %w", err)
	}

	if nodeName == "" {
		nodeName = obj.GetName()
	}

	stateKey := r.getStateKey(p, obj)

	r.matchStatesMu.RLock()
	storedNodeName, wasMatched := r.matchStates[stateKey]
	r.matchStatesMu.RUnlock()

	if matched && !wasMatched {
		return r.handleUnhealthyTransition(ctx, p, nodeName, stateKey)
	}

	if !matched && wasMatched {
		return r.handleHealthyTransition(ctx, p, storedNodeName, stateKey)
	}

	return nil
}

func (r *ResourceReconciler) handleUnhealthyTransition(
	ctx context.Context,
	p *config.Policy,
	nodeName string,
	stateKey string,
) error {
	if err := r.publisher.PublishHealthEvent(ctx, p, nodeName, false); err != nil {
		metrics.HealthEventsPublishErrors.WithLabelValues(p.Name, "grpc_error").Inc()
		return fmt.Errorf("failed to publish unhealthy event: %w", err)
	}

	r.matchStatesMu.Lock()
	r.matchStates[stateKey] = nodeName
	r.matchStatesMu.Unlock()

	if err := r.annotationMgr.AddMatch(ctx, nodeName, stateKey, nodeName); err != nil {
		slog.Error("Failed to persist match state to annotation", "node", nodeName, "stateKey", stateKey, "error", err)
	}

	metrics.PolicyMatches.WithLabelValues(p.Name, nodeName, r.gvk.Kind).Inc()

	return nil
}

func (r *ResourceReconciler) handleHealthyTransition(
	ctx context.Context,
	p *config.Policy,
	nodeName string,
	stateKey string,
) error {
	if err := r.publisher.PublishHealthEvent(ctx, p, nodeName, true); err != nil {
		metrics.HealthEventsPublishErrors.WithLabelValues(p.Name, "grpc_error").Inc()
		return fmt.Errorf("failed to publish healthy event: %w", err)
	}

	r.matchStatesMu.Lock()
	delete(r.matchStates, stateKey)
	r.matchStatesMu.Unlock()

	if err := r.annotationMgr.RemoveMatch(ctx, nodeName, stateKey); err != nil {
		slog.Error("Failed to remove match state from annotation", "node", nodeName, "stateKey", stateKey, "error", err)
	}

	return nil
}

func (r *ResourceReconciler) getStateKey(p *config.Policy, obj *unstructured.Unstructured) string {
	if obj.GetNamespace() != "" {
		return fmt.Sprintf("%s/%s/%s", p.Name, obj.GetNamespace(), obj.GetName())
	}

	return fmt.Sprintf("%s/%s", p.Name, obj.GetName())
}
