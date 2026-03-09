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

package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/parser"
)

const (
	// ConditionTypeDrain is the pod condition type set by slurm-operator for Slurm DRAIN state.
	ConditionTypeDrain = "SlurmNodeStateDrain"
	// OperatorReasonPrefix is the prefix used by slurm-operator for its own drain reasons.
	// Reasons not starting with this are treated as external.
	OperatorReasonPrefix = "slurm-operator:"
)

// DrainEventPublisher publishes drain health events (interface for testing).
type DrainEventPublisher interface {
	PublishDrainEvents(
		ctx context.Context, reasons []parser.MatchedReason, nodeName string,
		isHealthy bool, podNamespace, podName string,
	) error
}

// podState tracks the last-published state for a pod.
type podState struct {
	message  string
	reasons  []parser.MatchedReason
	nodeName string
}

// DrainReconciler reconciles NodeSet pods and publishes health events for external Slurm drains.
type DrainReconciler struct {
	client.Client
	parser    *parser.Parser
	publisher DrainEventPublisher
	// matchStates: podKey -> last published state (for dedup and healthy transition).
	// Concurrency safety: controller-runtime's work queue deduplicates by key, so concurrent
	// reconciles for the same pod key do not occur regardless of MaxConcurrentReconciles.
	matchStates map[string]podState
	mu          sync.RWMutex
}

// NewDrainReconciler creates a DrainReconciler.
func NewDrainReconciler(c client.Client, pr *parser.Parser, pub DrainEventPublisher) *DrainReconciler {
	return &DrainReconciler{
		Client:      c,
		parser:      pr,
		publisher:   pub,
		matchStates: make(map[string]podState),
	}
}

func podKey(namespace, name string) string {
	if namespace == "" {
		return name
	}

	return namespace + "/" + name
}

// Reconcile processes a pod: detects external drain from conditions, parses reason, publishes events.
func (r *DrainReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pod := &corev1.Pod{}

	if err := r.Get(ctx, req.NamespacedName, pod); err != nil {
		if client.IgnoreNotFound(err) == nil {
			if err := r.cleanupDeletedPod(ctx, req.Namespace, req.Name); err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}

		metrics.ReconciliationErrors.WithLabelValues("get_pod_error").Inc()

		return ctrl.Result{}, fmt.Errorf("failed to get pod: %w", err)
	}

	key := podKey(req.Namespace, req.Name)
	nodeName := pod.Spec.NodeName
	externalMessage := r.getExternalDrainMessage(pod)

	if externalMessage == "" {
		return r.handleNoExternalDrain(ctx, key, nodeName, req.Namespace, req.Name)
	}

	return r.handleExternalDrain(ctx, key, nodeName, externalMessage, req.Namespace, req.Name)
}

func (r *DrainReconciler) getExternalDrainMessage(pod *corev1.Pod) string {
	drainCond, hasDrain := findDrainCondition(pod)
	if !hasDrain || drainCond.Status != corev1.ConditionTrue || drainCond.Message == "" {
		return ""
	}

	msg := strings.TrimSpace(drainCond.Message)
	if strings.HasPrefix(msg, OperatorReasonPrefix) {
		return ""
	}

	return msg
}

func (r *DrainReconciler) handleNoExternalDrain(
	ctx context.Context, key, nodeName, podNamespace, podName string,
) (ctrl.Result, error) {
	r.mu.RLock()
	prev, wasMatched := r.matchStates[key]
	r.mu.RUnlock()

	if !wasMatched {
		return ctrl.Result{}, nil
	}

	nn := prev.nodeName
	if nn == "" {
		nn = nodeName
	}

	if err := r.publisher.PublishDrainEvents(ctx, prev.reasons, nn, true, podNamespace, podName); err != nil {
		metrics.HealthEventsPublishErrors.WithLabelValues("grpc_error").Inc()
		slog.Error("Failed to publish healthy event", "pod", key, "node", nn, "error", err)

		return ctrl.Result{}, fmt.Errorf("failed to publish healthy event for pod %s: %w", key, err)
	}

	r.mu.Lock()
	delete(r.matchStates, key)
	r.mu.Unlock()
	slog.Info("Published healthy event, drain cleared", "pod", key, "node", nn)

	return ctrl.Result{}, nil
}

func (r *DrainReconciler) handleExternalDrain(
	ctx context.Context, key, nodeName, externalMessage, podNamespace, podName string,
) (ctrl.Result, error) {
	r.mu.RLock()
	prev, wasMatched := r.matchStates[key]
	r.mu.RUnlock()

	if wasMatched && prev.message == externalMessage {
		return ctrl.Result{}, nil
	}

	reasons := r.parser.Parse(externalMessage)
	if len(reasons) == 0 {
		// Message changed but no patterns match — if previously matched, transition to healthy.
		if wasMatched {
			return r.publishHealthyAndClear(ctx, key, prev, podNamespace, podName)
		}

		return ctrl.Result{}, nil
	}

	// If previously matched with a different message, publish healthy for old reasons first
	// to give the downstream pipeline a clean transition before new unhealthy events.
	if wasMatched {
		if err := r.publisher.PublishDrainEvents(
			ctx, prev.reasons, prev.nodeName, true, podNamespace, podName,
		); err != nil {
			metrics.HealthEventsPublishErrors.WithLabelValues("grpc_error").Inc()
			slog.Error("Failed to publish healthy event before re-match", "pod", key, "node", prev.nodeName, "error", err)

			return ctrl.Result{}, fmt.Errorf("failed to publish healthy event before re-match for pod %s: %w", key, err)
		}

		r.mu.Lock()
		delete(r.matchStates, key)
		r.mu.Unlock()
		slog.Info("Published healthy event for previous drain before re-match", "pod", key, "node", prev.nodeName)
	}

	for _, reason := range reasons {
		metrics.ParseMatches.WithLabelValues(reason.PatternName).Inc()
		metrics.ExternalDrainsDetected.WithLabelValues(reason.PatternName).Inc()
	}

	if err := r.publisher.PublishDrainEvents(
		ctx, reasons, nodeName, false, podNamespace, podName,
	); err != nil {
		metrics.HealthEventsPublishErrors.WithLabelValues("grpc_error").Inc()
		slog.Error("Failed to publish drain events", "pod", key, "node", nodeName, "error", err)

		return ctrl.Result{}, fmt.Errorf("failed to publish unhealthy events for pod %s: %w", key, err)
	}

	r.mu.Lock()
	r.matchStates[key] = podState{
		message:  externalMessage,
		reasons:  reasons,
		nodeName: nodeName,
	}
	r.mu.Unlock()
	slog.Info("Published unhealthy events for external drain", "pod", key, "node", nodeName, "message", externalMessage)

	return ctrl.Result{}, nil
}

func (r *DrainReconciler) publishHealthyAndClear(
	ctx context.Context, key string, prev podState, podNamespace, podName string,
) (ctrl.Result, error) {
	nn := prev.nodeName
	if err := r.publisher.PublishDrainEvents(ctx, prev.reasons, nn, true, podNamespace, podName); err != nil {
		metrics.HealthEventsPublishErrors.WithLabelValues("grpc_error").Inc()
		slog.Error("Failed to publish healthy event", "pod", key, "node", nn, "error", err)

		return ctrl.Result{}, fmt.Errorf("failed to publish healthy event for pod %s: %w", key, err)
	}

	r.mu.Lock()
	delete(r.matchStates, key)
	r.mu.Unlock()
	slog.Info("Published healthy event, drain no longer matches", "pod", key, "node", nn)

	return ctrl.Result{}, nil
}

func (r *DrainReconciler) cleanupDeletedPod(ctx context.Context, namespace, name string) error {
	key := podKey(namespace, name)

	r.mu.RLock()
	prev, wasMatched := r.matchStates[key]
	r.mu.RUnlock()

	if !wasMatched {
		return nil
	}

	slog.Info("Pod deleted, publishing healthy event", "pod", key, "node", prev.nodeName)

	if err := r.publisher.PublishDrainEvents(ctx, prev.reasons, prev.nodeName, true, namespace, name); err != nil {
		metrics.HealthEventsPublishErrors.WithLabelValues("grpc_error").Inc()
		slog.Error("Failed to publish healthy event for deleted pod", "pod", key, "error", err)

		return fmt.Errorf("failed to publish healthy event for deleted pod %s: %w", key, err)
	}

	r.mu.Lock()
	delete(r.matchStates, key)
	r.mu.Unlock()

	return nil
}

// findDrainCondition returns the condition with type SlurmNodeStateDrain and true if found.
func findDrainCondition(pod *corev1.Pod) (corev1.PodCondition, bool) {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodConditionType(ConditionTypeDrain) {
			return c, true
		}
	}

	return corev1.PodCondition{}, false
}
