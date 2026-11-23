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

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/events"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/metrics"
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

// checkDrainCoversAction returns ErrDrainDoesNotCoverAction when the event's partial drain does not cover
// its action. The caller then retries the event without creating a CR or recording a decision, so a fix to
// the configuration lets the event continue.
func checkDrainCoversAction(ctx context.Context, span oteltrace.Span, healthEventWithStatus *events.HealthEventDoc,
	groupConfig *common.EquivalenceGroupConfig) error {
	decision := healthEventWithStatus.HealthEventStatus.GetDrainDecision()
	if drainCoversAction(decision, groupConfig) {
		return nil
	}

	nodeName := healthEventWithStatus.HealthEvent.GetNodeName()
	action := model.GetEffectiveActionName(healthEventWithStatus.HealthEvent)
	drained := decision.GetScope()

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
