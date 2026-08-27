// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
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

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/nvidia/nvsentinel/fault-remediation/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

type reconcileRequest struct {
	event      *datastore.EventWithToken
	documentID string
}

type controllerReconciler struct {
	reconciler *FaultRemediationReconciler
}

func (c *controllerReconciler) Reconcile(
	ctx context.Context,
	request reconcileRequest,
) (ctrl.Result, error) {
	if request.event != nil {
		return c.reconciler.Reconcile(ctx, request.event)
	}

	return c.reconcileColdStartEvent(ctx, request.documentID)
}

func (c *controllerReconciler) reconcileColdStartEvent(
	ctx context.Context,
	documentID string,
) (ctrl.Result, error) {
	if documentID == "" {
		metrics.ProcessingErrors.WithLabelValues("invalid_cold_start_request", "unknown").Inc()
		slog.ErrorContext(ctx, "Dropping cold-start reconcile request without a document ID")

		return ctrl.Result{}, nil
	}

	healthEvents, err := c.reconciler.healthEventStore.FindHealthEventsByQuery(
		ctx,
		query.New().Build(query.Eq("_id", documentID)),
	)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("cold_start_fetch_error", "unknown").Inc()
		slog.ErrorContext(ctx, "Failed to fetch cold-start health event",
			"eventID", documentID,
			"error", err)

		return ctrl.Result{}, fmt.Errorf("fetch cold-start health event %s: %w", documentID, err)
	}

	if len(healthEvents) == 0 {
		metrics.ProcessingErrors.WithLabelValues("cold_start_event_unavailable", "unknown").Inc()
		slog.WarnContext(ctx, "Skipping deleted cold-start health event", "eventID", documentID)

		return ctrl.Result{}, nil
	}

	if len(healthEvents[0].RawEvent) == 0 {
		metrics.ProcessingErrors.WithLabelValues("cold_start_event_unavailable", "unknown").Inc()
		slog.WarnContext(ctx, "Skipping cold-start health event without a raw document", "eventID", documentID)

		return ctrl.Result{}, nil
	}

	return c.reconciler.Reconcile(ctx, &datastore.EventWithToken{Event: healthEvents[0].RawEvent})
}
