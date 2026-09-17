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

// reconcileRequest is the workqueue item. It names the event rather than carrying it, so a
// queued item does not retain the decoded change-stream document; the worker fetches the
// document when it runs.
//
// resumeToken is the live event's change-stream position, held as a string because
// workqueue.TypedInterface requires a comparable key and a []byte is not comparable. Go
// strings hold arbitrary bytes, so the conversion round-trips a raw token unchanged.
// The token has to travel with the item because fault-remediation only checkpoints an event
// once the work is finalised — an event parked behind an in-progress maintenance CR must not
// advance the stream position. Cold-start items carry no token.
type reconcileRequest struct {
	documentID  string
	resumeToken string
}

// Reconcile fetches the health event named by the request and reconciles it. The live
// change-stream path and cold start both queue document IDs, so both arrive here.
func (r *FaultRemediationReconciler) Reconcile(
	ctx context.Context,
	request reconcileRequest,
) (ctrl.Result, error) {
	documentID := request.documentID
	resumeToken := []byte(request.resumeToken)

	if documentID == "" {
		metrics.ProcessingErrors.WithLabelValues("invalid_request", "unknown").Inc()
		slog.ErrorContext(ctx, "Dropping reconcile request without a document ID")

		return ctrl.Result{}, nil
	}

	healthEvents, err := r.healthEventStore.FindHealthEventsByQuery(
		ctx,
		query.New().Build(query.Eq("_id", documentID)),
	)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("fetch_error", "unknown").Inc()
		slog.ErrorContext(ctx, "Failed to fetch health event",
			"eventID", documentID,
			"error", err)

		return ctrl.Result{}, fmt.Errorf("fetch health event %s: %w", documentID, err)
	}

	// A live event can be deleted between being queued and being fetched. Dropping it is
	// terminal, so the stream has to advance past it or the position stalls behind an event
	// that will never load. Cold-start items carry no token and safeMarkProcessed ignores
	// them, which is what keeps an empty token from advancing the checkpoint to the live
	// cursor position.
	if len(healthEvents) == 0 {
		metrics.ProcessingErrors.WithLabelValues("event_unavailable", "unknown").Inc()
		slog.WarnContext(ctx, "Skipping deleted health event", "eventID", documentID)

		return r.markProcessedOrError(ctx, r.Watcher, datastore.EventWithToken{ResumeToken: resumeToken}, "unknown")
	}

	if len(healthEvents[0].RawEvent) == 0 {
		metrics.ProcessingErrors.WithLabelValues("event_unavailable", "unknown").Inc()
		slog.WarnContext(ctx, "Skipping health event without a raw document", "eventID", documentID)

		return r.markProcessedOrError(ctx, r.Watcher, datastore.EventWithToken{ResumeToken: resumeToken}, "unknown")
	}

	return r.reconcileEvent(ctx, &datastore.EventWithToken{
		Event:       healthEvents[0].RawEvent,
		ResumeToken: resumeToken,
	})
}
