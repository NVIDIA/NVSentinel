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

package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

func (r *Reconciler) runProcessors(ctx context.Context) error {
	if !r.config.HealthEventsAnalyzerRules.HasAnnotationRecovery() {
		return r.eventProcessor.Start(ctx)
	}

	kube := r.config.KubernetesClient
	if kube == nil {
		restConfig, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("load Kubernetes configuration for annotation recovery: %w", err)
		}

		restConfig.Timeout = 30 * time.Second

		kube, err = kubernetes.NewForConfig(restConfig)
		if err != nil {
			return fmt.Errorf("create Kubernetes client for annotation recovery: %w", err)
		}
	}

	controller, err := newNodeRecoveryController(kube, r.config.HealthEventsAnalyzerRules, r.reconcileNodeRecovery)
	if err != nil {
		return err
	}

	r.nodeRecovery = controller

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	group, groupCtx := errgroup.WithContext(runCtx)
	group.Go(func() error { return controller.run(groupCtx, r.config.Workers) })
	group.Go(func() error {
		defer cancel()

		select {
		case <-groupCtx.Done():
			return groupCtx.Err()
		case <-controller.ready:
		}

		return r.eventProcessor.Start(groupCtx)
	})

	return group.Wait()
}

func (r *Reconciler) reconcileNodeRecovery(ctx context.Context, nodeName string) error {
	unlock, err := r.nodeProcessing.acquire(ctx, nodeName)
	if err != nil {
		return fmt.Errorf("lock node recovery: %w", err)
	}
	defer unlock()

	return r.processNodeAnnotations(ctx, nodeName)
}

// Called with the same node lock used by source-event processing.
func (r *Reconciler) processNodeAnnotations(ctx context.Context, nodeName string) error {
	node, err := r.nodeRecovery.nodes.Get(nodeName)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read node recovery annotations: %w", err)
	}

	r.observeNodeUID(node)

	for _, rule := range r.config.HealthEventsAnalyzerRules.Rules {
		if err := r.processRuleAnnotation(ctx, node, rule); err != nil {
			return err
		}
	}

	return nil
}

func (r *Reconciler) processRuleAnnotation(ctx context.Context, node *corev1.Node,
	rule config.HealthEventsAnalyzerRule,
) error {
	if !rule.EvaluateRule || rule.Recovery == nil || rule.Recovery.AnnotationKey == "" {
		return nil
	}

	value := node.Annotations[rule.Recovery.AnnotationKey]
	if value == "" {
		return nil
	}

	source, err := parseAnnotationRecovery(node, rule, value, time.Now())
	if err != nil {
		slog.ErrorContext(ctx, "Invalid recovery annotation; correct the request",
			"node", node.Name, "rule", rule.Name, "error", err)

		return nil
	}

	if err := r.recoverFromAnnotation(ctx, source, rule); err != nil {
		if client.IsPermanentError(err) {
			slog.ErrorContext(ctx, "Recovery annotation retained after permanent stored-state failure",
				"node", node.Name, "rule", rule.Name, "error", err)

			return nil
		}

		return err
	}

	return r.nodeRecovery.removeRequest(ctx, node, rule.Recovery.AnnotationKey, value)
}

func (r *Reconciler) recoverFromAnnotation(ctx context.Context, source *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule,
) error {
	identity, nodeWide, valid := recoveryIdentityForSource(rule, source.HealthEvent)
	if !valid {
		return fmt.Errorf("annotation recovery identity does not match rule %q", rule.Name)
	}

	targets, err := r.recoveryTargets(ctx, rule, identity, nodeWide)
	if err != nil {
		return fmt.Errorf("read annotation recovery targets: %w", err)
	}

	sourceBoundary := boundaryFromEvent(source)
	for _, target := range targets {
		if target.state.isHealthy || !boundaryAfter(sourceBoundary, target.state.boundary) {
			continue
		}

		event := proto.Clone(source.HealthEvent).(*protos.HealthEvent)
		event.ComponentClass = target.state.componentClass
		event.Version = target.state.version
		scopedSource := *source
		scopedSource.HealthEvent = event

		storedBoundary, published, err := r.publishRecoveryUntilStored(ctx, &scopedSource, rule, target.identity)
		if err != nil {
			return fmt.Errorf("publish annotation recovery for rule %q: %w", rule.Name, err)
		}

		r.rememberRecoveryBoundary(rule.Name, target.identity, sourceBoundary)
		r.rememberDerivedState(rule.Name, target.identity, derivedState{
			boundary: storedBoundary, isHealthy: true,
			componentClass: target.state.componentClass, version: target.state.version,
		})

		if published {
			recoveryEventsPublishedTotal.WithLabelValues(rule.Name, string(rule.Recovery.Scope)).Inc()
		}
	}

	return nil
}

// A completed annotation request leaves its boundary in the derived healthy
// event. No intermediate healthy source event is needed for restart recovery.
func (r *Reconciler) latestAnnotationRecovery(ctx context.Context, rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity,
) (*datamodels.HealthEventWithStatus, error) {
	if r.nodeRecovery == nil {
		return nil, fmt.Errorf("annotation recovery requires a synchronized node cache")
	}

	node, err := r.nodeRecovery.nodes.Get(identity.nodeName)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read recovery node identity: %w", err)
	}

	latest, err := r.findLatestMatchingEvent(ctx, &rule, &identity, rule.Name, "annotation_recovery",
		r.recoveryLookupFilter(agentName, rule.Name, identity.nodeName),
		func(candidate *datamodels.HealthEventWithStatus) bool {
			return annotationRecoveryMatches(candidate, rule, identity, string(node.UID))
		})
	if err != nil || latest == nil {
		return nil, err
	}

	return annotationRecoverySource(latest, node)
}

func annotationRecoveryMatches(candidate *datamodels.HealthEventWithStatus, rule config.HealthEventsAnalyzerRule,
	identity recoveryIdentity, nodeUID string,
) bool {
	event := candidate.HealthEvent
	if !event.IsHealthy || event.Metadata[annotationMetadataKey] != rule.Recovery.AnnotationKey ||
		event.Metadata[annotationNodeUIDKey] != nodeUID || event.Metadata[annotationRequestKey] == "" {
		return false
	}

	candidateIdentity, valid := recoveryIdentityForEvent(rule, event)

	return valid && candidateIdentity.key == identity.key
}

func annotationRecoverySource(latest *datamodels.HealthEventWithStatus, node *corev1.Node) (
	*datamodels.HealthEventWithStatus, error,
) {
	timestamp := latest.HealthEvent.Metadata[publisher.SourceGeneratedTimestampMetadataKey]

	recoveredAt, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return nil, client.PermanentError(fmt.Errorf("decode stored annotation recovery boundary: %w", err))
	}

	if recoveredAt.Before(node.CreationTimestamp.Time) ||
		recoveredAt.After(latest.HealthEvent.GeneratedTimestamp.AsTime()) {
		return nil, client.PermanentError(
			fmt.Errorf("stored annotation recovery boundary is outside the node lifetime or recovery time"),
		)
	}

	latest.CreatedAt = recoveredAt
	latest.HealthEvent = proto.Clone(latest.HealthEvent).(*protos.HealthEvent)
	latest.HealthEvent.GeneratedTimestamp = timestamppb.New(recoveredAt)

	return latest, nil
}

func (r *Reconciler) observeNodeUID(node *corev1.Node) {
	r.recoveryMu.Lock()
	defer r.recoveryMu.Unlock()

	if r.nodeUIDs == nil {
		r.nodeUIDs = make(map[string]string)
	}

	previous := r.nodeUIDs[node.Name]

	r.nodeUIDs[node.Name] = string(node.UID)
	if previous == "" || previous == string(node.UID) {
		return
	}

	for key := range r.recoveryLoaded {
		if recoveryKeyForNode(key, node.Name) {
			delete(r.recoveryLoaded, key)
		}
	}

	for key := range r.recoveryBoundaries {
		if recoveryKeyForNode(key, node.Name) {
			delete(r.recoveryBoundaries, key)
		}
	}

	for key := range r.derivedStates {
		if recoveryKeyForNode(key, node.Name) {
			delete(r.derivedStates, key)
		}
	}
}

func recoveryKeyForNode(key, node string) bool {
	_, identity, _ := strings.Cut(key, "\x00")
	return identity == node || strings.HasPrefix(identity, node+"|")
}
