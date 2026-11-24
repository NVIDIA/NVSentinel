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

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/events"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// drainScopeMismatchError is the error_type of fault_remediation_processing_errors_total for an event whose
// partial drain does not cover the action.
const drainScopeMismatchError = "drain_scope_mismatch"

// ErrDrainDoesNotCoverAction means that node-drainer drained only the pods of one device, and the action
// does not repair only that device. Running it would stop pods that were not drained.
var ErrDrainDoesNotCoverAction = errors.New("the partial drain does not cover the remediation action")

// drainCoversAction reports whether the drain that node-drainer recorded for the event covers the action
// (ADR-071). A drain of the full node covers any action. A partial drain covers only an action scoped to the
// same entity. An event without a drain decision was drained by a version that did not record one, so it is
// not checked.
func drainCoversAction(decision *protos.DrainDecision, groupConfig *common.EquivalenceGroupConfig) bool {
	drained := decision.GetScope()
	if drained.GetEntityType() == "" {
		return true
	}

	return groupConfig.ImpactedEntityScope == drained.GetEntityType() &&
		groupConfig.ImpactedEntityScopeValue == drained.GetEntityValue()
}

// tryRejectUncoveredEvent returns (result, err, true) when the event's partial drain does not cover its
// action. The event is retried with ErrDrainDoesNotCoverAction and gets no CR and no decision, so a fix to
// the configuration lets it continue while the quarantine holds the event. Once the node is deleted, or its
// quarantine no longer holds the event, the event is closed without remediation, so that a later fix does
// not create a CR for a node that is back in service. A CR of the same equivalence group that already covers
// the event holds or closes it instead. Otherwise it returns (zero, nil, false).
func (r *FaultRemediationReconciler) tryRejectUncoveredEvent(
	ctx context.Context,
	span oteltrace.Span,
	healthEventWithStatus *events.HealthEventDoc,
	groupConfig *common.EquivalenceGroupConfig,
	eventWithToken datastore.EventWithToken,
	watcherInstance datastore.ChangeStreamWatcher,
	healthEventStore datastore.HealthEventStore,
) (ctrl.Result, error, bool) {
	if drainCoversAction(healthEventWithStatus.HealthEventStatus.GetDrainDecision(), groupConfig) {
		return ctrl.Result{}, nil, false
	}

	nodeName := healthEventWithStatus.HealthEvent.GetNodeName()

	// A deleted node has no active quarantine.
	active, err := r.nodeHasActiveQuarantine(ctx, healthEventWithStatus.HealthEvent)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check the quarantine of node %s: %w", nodeName, err), true
	}

	if !active {
		slog.WarnContext(ctx, "Quarantine no longer holds the event, closing it without remediation",
			"node", nodeName, "event", healthEventWithStatus.ID)

		res, err := r.markEventTerminalAndProcessed(ctx, healthEventStore, eventWithToken, watcherInstance, nodeName, false)

		return res, err, true
	}

	// A CR of the event's equivalence group may already cover it, for example a reboot that another event of the
	// session started. That CR holds or closes the event, as for any other event. No CR is created here.
	decision, err := r.checkExistingCRStatus(ctx, healthEventWithStatus.HealthEvent, healthEventWithStatus.CreatedAt,
		groupConfig)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error checking existing CR status: %w", err), true
	}

	if !decision.shouldCreate {
		res, err := r.handleEventCoveredByExistingCR(ctx, decision, eventWithToken, watcherInstance, healthEventStore,
			nodeName)

		return res, err, true
	}

	return ctrl.Result{}, recordDrainScopeMismatch(ctx, span, healthEventWithStatus, groupConfig), true
}

// recordDrainScopeMismatch logs and counts an event whose partial drain does not cover its action, and
// returns ErrDrainDoesNotCoverAction.
func recordDrainScopeMismatch(ctx context.Context, span oteltrace.Span, healthEventWithStatus *events.HealthEventDoc,
	groupConfig *common.EquivalenceGroupConfig) error {
	nodeName := healthEventWithStatus.HealthEvent.GetNodeName()
	action := model.GetEffectiveActionName(healthEventWithStatus.HealthEvent)
	drained := healthEventWithStatus.HealthEventStatus.GetDrainDecision().GetScope()

	err := fmt.Errorf("%w: event %s drained only %s %s, and action %s acts on %s",
		ErrDrainDoesNotCoverAction, healthEventWithStatus.ID, drained.GetEntityType(), drained.GetEntityValue(),
		action, actionScopeDescription(groupConfig))

	metrics.ProcessingErrors.WithLabelValues(drainScopeMismatchError, nodeName).Inc()
	slog.ErrorContext(ctx, "Partial drain does not cover the remediation action, retrying event",
		"node", nodeName,
		"event", healthEventWithStatus.ID,
		"action", action,
		"drainEntityType", drained.GetEntityType(),
		"drainEntityValue", drained.GetEntityValue(),
		"actionEntityType", groupConfig.ImpactedEntityScope,
		"actionEntityValue", groupConfig.ImpactedEntityScopeValue)
	tracing.RecordError(span, err)
	span.SetAttributes(
		attribute.String("fault_remediation.error.type", drainScopeMismatchError),
		attribute.String("fault_remediation.error.message", err.Error()),
	)

	return err
}

func actionScopeDescription(groupConfig *common.EquivalenceGroupConfig) string {
	if groupConfig.ImpactedEntityScope == "" {
		return "the full node"
	}

	return groupConfig.ImpactedEntityScope + " " + groupConfig.ImpactedEntityScopeValue
}
