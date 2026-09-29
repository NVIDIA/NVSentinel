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
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
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

		kube, err = kubernetes.NewForConfig(restConfig)
		if err != nil {
			return fmt.Errorf("create Kubernetes client for annotation recovery: %w", err)
		}
	}

	controller, err := newNodeRecoveryController(kube, r.config.HealthEventsAnalyzerRules,
		r.reconcileNodeRecovery, recoveryRequestResyncPeriod)
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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	unlock, err := r.nodeProcessing.acquire(ctx, nodeName)
	if err != nil {
		return err
	}
	defer unlock()

	node, err := r.nodeRecovery.nodes.Get(nodeName)
	if apierrors.IsNotFound(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read recovery node: %w", err)
	}

	var result error
	for _, rule := range r.config.HealthEventsAnalyzerRules.Rules {
		result = errors.Join(result, r.processRuleAnnotation(ctx, node, rule))
	}

	return result
}

func (r *Reconciler) processRuleAnnotation(ctx context.Context, node *corev1.Node,
	rule config.HealthEventsAnalyzerRule) error {
	if !rule.EvaluateRule || rule.Recovery == nil {
		return nil
	}

	key := rule.Recovery.AnnotationKey

	value := node.Annotations[key]
	if value == "" {
		return nil
	}

	request, err := parseAnnotationRecovery(node, rule, value, time.Now())
	if err != nil {
		return r.nodeRecovery.report(ctx, node, key, value, "RecoveryInvalid",
			fmt.Sprintf("Rule %s: %s", rule.Name, err), true)
	}

	requestID := request.HealthEvent.Metadata[annotationRequestKey]

	terminalKey := node.Name + "\x00" + rule.Name
	if previous, found := r.terminalRequests.Load(terminalKey); found && previous == requestID {
		return r.reportRecoveryFailure(ctx, node, rule, value, requestID, healthpub.ErrPublishRejected)
	}

	recovered, err := r.recoverFromAnnotation(ctx, request, rule)
	if err != nil {
		return r.reportRecoveryFailure(ctx, node, rule, value, requestID, err)
	}

	reason := "RecoverySkipped"
	if recovered > 0 {
		reason = "RecoveryCompleted"
	}

	return r.nodeRecovery.report(ctx, node, key, value, reason,
		fmt.Sprintf("Rule %s: %d recovered identities stored for verification time %s. "+
			"Annotation retained and periodically rechecked.",
			rule.Name, recovered, request.HealthEvent.Metadata[annotationVerifiedAtKey]), false)
}

func (r *Reconciler) reportRecoveryFailure(ctx context.Context, node *corev1.Node,
	rule config.HealthEventsAnalyzerRule, value, requestID string, err error) error {
	slog.ErrorContext(ctx, "Annotation recovery failed", "node", node.Name, "rule", rule.Name, "error", err)

	if errors.Is(err, healthpub.ErrPublishRejected) {
		r.terminalRequests.Store(node.Name+"\x00"+rule.Name, requestID)
	}

	if reportErr := r.nodeRecovery.report(ctx, node, rule.Recovery.AnnotationKey, value, "RecoveryFailed",
		fmt.Sprintf("Rule %s recovery failed; see analyzer logs. Annotation retained.", rule.Name), true); reportErr != nil {
		return reportErr
	}

	if errors.Is(err, healthpub.ErrPublishRejected) {
		return nil
	}

	return err
}

func (r *Reconciler) recoverFromAnnotation(ctx context.Context, request *datamodels.HealthEventWithStatus,
	rule config.HealthEventsAnalyzerRule) (int, error) {
	states, err := r.derivedStatesForNode(ctx, rule, request.HealthEvent.NodeName)
	if err != nil {
		return 0, err
	}

	requested, scoped := recoveryIdentityForEvent(rule, request.HealthEvent)
	recovered := 0

	for _, state := range states {
		identity, _ := recoveryIdentityForEvent(rule, state.HealthEvent)
		if scoped && identity.key != requested.key {
			continue
		}

		if state.HealthEvent.IsHealthy {
			if state.HealthEvent.Metadata[annotationRequestKey] == request.HealthEvent.Metadata[annotationRequestKey] {
				recovered++
			}

			continue
		}

		if !request.HealthEvent.GeneratedTimestamp.AsTime().After(state.HealthEvent.GeneratedTimestamp.AsTime()) {
			continue
		}

		event := recoveryEvent(state.HealthEvent, identity.entities, request.HealthEvent.Metadata)

		if err := r.publishRecoveryUntilStored(ctx, event, rule, identity); err != nil {
			return recovered, err
		}

		recovered++
	}

	return recovered, nil
}

func recoveryEvent(fault *protos.HealthEvent, entities []*protos.Entity,
	metadata map[string]string) *protos.HealthEvent {
	event := proto.Clone(fault).(*protos.HealthEvent)

	event.EntitiesImpacted = entities
	if event.Metadata == nil {
		event.Metadata = make(map[string]string)
	}

	for key, value := range metadata {
		event.Metadata[key] = value
	}

	return event
}

func (r *Reconciler) publishRecoveryUntilStored(ctx context.Context, event *protos.HealthEvent,
	rule config.HealthEventsAnalyzerRule, identity recoveryIdentity) error {
	// Direct-mode Publish already confirms durable storage. Socket mode only
	// confirms queue acceptance, so check storage immediately and then poll.
	_, err := r.config.Publisher.PublishRecovery(ctx, event, rule)
	if err != nil {
		return err
	}

	if r.config.Publisher.AcknowledgesStorage() {
		return nil
	}

	return r.waitForRecoveryStorage(ctx, event, rule, identity)
}

func (r *Reconciler) waitForRecoveryStorage(ctx context.Context, event *protos.HealthEvent,
	rule config.HealthEventsAnalyzerRule, identity recoveryIdentity) error {
	poll, republish := r.recoveryPoll, r.recoveryRepublish
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}

	if republish <= 0 {
		republish = 30 * time.Second
	}

	nextPublish := time.Now().Add(republish)

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		found, err := r.recoveryStored(ctx, event, rule, identity)
		if err != nil {
			return err
		}

		if found {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		if !time.Now().Before(nextPublish) {
			if _, err := r.config.Publisher.PublishRecovery(ctx, event, rule); err != nil {
				return err
			}

			nextPublish = time.Now().Add(republish)
		}
	}
}
