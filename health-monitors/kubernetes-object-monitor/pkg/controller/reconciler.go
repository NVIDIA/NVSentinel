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
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/annotations"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/policy"
)

type HealthEventPublisher interface {
	PublishHealthEvent(
		ctx context.Context,
		policy *config.Policy,
		nodeName string,
		isHealthy bool,
		resourceInfo *config.ResourceInfo,
	) error
}

type ResourceReconciler struct {
	client.Client
	apiReader     client.Reader
	evaluator     *policy.Evaluator
	publisher     HealthEventPublisher
	annotationMgr *annotations.Manager
	policies      []config.Policy
	gvk           schema.GroupVersionKind
	// matchStates maps stateKey -> nodeName
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
	return NewResourceReconcilerWithReader(c, c, evaluator, pub, annotationMgr, policies, gvk)
}

func NewResourceReconcilerWithReader(
	c client.Client,
	apiReader client.Reader,
	evaluator *policy.Evaluator,
	pub HealthEventPublisher,
	annotationMgr *annotations.Manager,
	policies []config.Policy,
	gvk schema.GroupVersionKind,
) *ResourceReconciler {
	return &ResourceReconciler{
		Client:        c,
		apiReader:     apiReader,
		evaluator:     evaluator,
		publisher:     pub,
		annotationMgr: annotationMgr,
		policies:      policies,
		gvk:           gvk,
		matchStates:   make(map[string]string),
	}
}

// LoadState reloads persisted policy match state from node annotations.
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

// LoadStateWithScope reloads persisted policy match state, verifies referenced
// resources, and recovers deletions that occurred while the monitor was stopped.
func (r *ResourceReconciler) LoadStateWithScope(ctx context.Context, resourceNamespaced bool) error {
	allMatches, err := r.annotationMgr.LoadAllMatches(ctx)
	if err != nil {
		return fmt.Errorf("failed to load match state: %w", err)
	}

	loadedMatches := make(map[string]string)
	requests := make(map[string]ctrl.Request)

	for stateKey, nodeName := range allMatches {
		req, ok := r.requestFromStateKey(stateKey, resourceNamespaced)
		if !ok {
			continue
		}

		loadedMatches[stateKey] = nodeName
		requests[stateKey] = req
	}

	r.matchStatesMu.Lock()
	r.matchStates = maps.Clone(loadedMatches)
	r.matchStatesMu.Unlock()

	for stateKey, req := range requests {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(r.gvk)

		if err := r.apiReader.Get(ctx, req.NamespacedName, obj); err != nil {
			if apierrors.IsNotFound(err) {
				if err := r.cleanupDeletedResource(ctx, req); err != nil {
					return fmt.Errorf("failed to recover deleted match %q: %w", stateKey, err)
				}

				continue
			}

			return fmt.Errorf("failed to verify loaded match %q: %w", stateKey, err)
		}
	}

	r.matchStatesMu.RLock()
	remainingMatches := len(r.matchStates)
	r.matchStatesMu.RUnlock()

	slog.Info("Loaded policy match state from annotations", "gvk", r.gvk.String(), "matches", remainingMatches)

	return nil
}

func (r *ResourceReconciler) requestFromStateKey(stateKey string, resourceNamespaced bool) (ctrl.Request, bool) {
	parts := strings.Split(stateKey, "/")

	resourcePartCount := 1
	if resourceNamespaced {
		resourcePartCount = 2
	}

	if len(parts) <= resourcePartCount {
		return ctrl.Request{}, false
	}

	policyName := strings.Join(parts[:len(parts)-resourcePartCount], "/")
	if !r.hasEnabledPolicy(policyName) {
		return ctrl.Request{}, false
	}

	resourceParts := parts[len(parts)-resourcePartCount:]

	if resourceNamespaced {
		req := ctrl.Request{
			NamespacedName: client.ObjectKey{
				Namespace: resourceParts[0],
				Name:      resourceParts[1],
			},
		}

		return req, resourceParts[0] != "" && resourceParts[1] != ""
	}

	return ctrl.Request{
		NamespacedName: client.ObjectKey{Name: resourceParts[0]},
	}, resourceParts[0] != ""
}

func (r *ResourceReconciler) hasEnabledPolicy(name string) bool {
	for i := range r.policies {
		if r.policies[i].Enabled && r.policies[i].Name == name {
			return true
		}
	}

	return false
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

		if !p.Resource.MatchesNamespace(obj.GetNamespace()) {
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
		if err := r.cleanupDeletedResource(ctx, req); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	metrics.ReconciliationErrors.WithLabelValues(r.gvk.Kind, "get_resource_error").Inc()

	return ctrl.Result{}, fmt.Errorf("failed to get resource: %w", err)
}

// cleanupDeletedResource handles cleanup when a resource is deleted.
// when a pod is deleted, we uncordon the node immediately.
// This means if a replacement pod comes up unhealthy, it will be re-cordoned.
func (r *ResourceReconciler) cleanupDeletedResource(ctx context.Context, req ctrl.Request) error {
	slog.Info("Cleaning up deleted resource", "resource", req.NamespacedName)

	for _, p := range r.policies {
		if !p.Enabled {
			continue
		}

		if !p.Resource.MatchesNamespace(req.Namespace) {
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
			// For Node resources, the deleted resource is the node itself.
			// In this case, there's no point publishing a healthy event or trying to
			// remove annotations - the node is gone.
			isNodeResource := r.gvk.Kind == "Node" && nodeName == req.Name
			if isNodeResource {
				slog.Info("Node deleted, cleaning up internal state without publishing",
					"policy", p.Name, "node", nodeName)

				r.matchStatesMu.Lock()
				delete(r.matchStates, stateKey)
				r.matchStatesMu.Unlock()

				continue
			}

			resourceInfo := &config.ResourceInfo{
				Group:     r.gvk.Group,
				Version:   r.gvk.Version,
				Kind:      r.gvk.Kind,
				Namespace: req.Namespace,
				Name:      req.Name,
			}

			// Pod deleted - publish healthy event to uncordon the node
			slog.Info("Resource deleted, publishing healthy event to uncordon node",
				"policy", p.Name, "resource", req.NamespacedName, "node", nodeName)

			if err := r.handleHealthyTransition(ctx, &p, nodeName, stateKey, resourceInfo); err != nil {
				return fmt.Errorf(
					"failed to publish recovery for deleted resource %s with policy %q: %w",
					req.NamespacedName,
					p.Name,
					err,
				)
			}
		}
	}

	return nil
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

	resourceInfo := &config.ResourceInfo{
		Group:     r.gvk.Group,
		Version:   r.gvk.Version,
		Kind:      r.gvk.Kind,
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}

	if matched && !wasMatched {
		return r.handleUnhealthyTransition(ctx, p, nodeName, stateKey, resourceInfo)
	}

	if !matched && wasMatched {
		return r.handleHealthyTransition(ctx, p, storedNodeName, stateKey, resourceInfo)
	}

	return nil
}

func (r *ResourceReconciler) handleUnhealthyTransition(
	ctx context.Context,
	p *config.Policy,
	nodeName string,
	stateKey string,
	resourceInfo *config.ResourceInfo,
) error {
	if err := r.publisher.PublishHealthEvent(ctx, p, nodeName, false, resourceInfo); err != nil {
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
	resourceInfo *config.ResourceInfo,
) error {
	if err := r.publisher.PublishHealthEvent(ctx, p, nodeName, true, resourceInfo); err != nil {
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

// getStateKey generates the state key for tracking: policyName/namespace/resourceName
func (r *ResourceReconciler) getStateKey(p *config.Policy, obj *unstructured.Unstructured) string {
	if obj.GetNamespace() != "" {
		return fmt.Sprintf("%s/%s/%s", p.Name, obj.GetNamespace(), obj.GetName())
	}

	return fmt.Sprintf("%s/%s", p.Name, obj.GetName())
}
