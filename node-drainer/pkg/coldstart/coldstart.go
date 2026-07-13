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

// Package coldstart re-processes health events that were in-progress or quarantined
// while node-drainer was offline. On restart node-drainer's in-memory cancellation
// state is lost, so this package also guards against replaying quarantine records
// whose session has already ended (see Handle / quarantineSessionResolver).
package coldstart

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/eventutil"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/queue"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
)

const batchSize = 1000

// Dependencies bundles the collaborators cold start needs. It is intentionally
// decoupled from initializer.Components so the package stays independently testable.
type Dependencies struct {
	QueueManager       queue.EventQueueManager
	DatabaseClient     client.DatabaseClient
	HealthEventStore   datastore.HealthEventStore
	ColdStartAfterTime time.Time
}

// Handle re-processes events that were in-progress or quarantined during a restart.
// Events are fetched in bounded batches via FindHealthEventsByQueryBatched to prevent
// unbounded memory usage. All matching events are loaded (not just latest per node)
// because a single node can have multiple concurrent partial drains.
//
// Cold start deliberately does NOT trust the datastore match alone. Node-drainer's
// live cancellation state (cancelledNodes) is in-memory and is lost on restart, so a
// quarantine record with an empty drain status can linger even after fault-quarantine
// has already unquarantined the node. Replaying those records marks a healthy,
// uncordoned node drain-succeeded, which fault-remediation then turns into an orphaned
// remediation-failed label. To avoid that, each quarantine record is checked against
// later UnQuarantined/Cancelled events for the same node; stale records are tombstoned
// instead of re-queued.
func Handle(ctx context.Context, deps Dependencies) error {
	slog.InfoContext(ctx, "Querying for events requiring processing")

	q := coldStartQuery(deps.ColdStartAfterTime)

	dbAdapter := &dataStoreAdapter{DatabaseClient: deps.DatabaseClient}
	resolver := newQuarantineSessionResolver(deps.HealthEventStore)

	err := deps.HealthEventStore.FindHealthEventsByQueryBatched(ctx, q, batchSize,
		func(batch []datastore.HealthEventWithStatus) error {
			slog.Info("Processing cold start batch", "count", len(batch))

			for i := range batch {
				processColdStartEvent(ctx, deps, dbAdapter, resolver, batch[i])
			}

			return nil
		})
	if err != nil {
		return fmt.Errorf("failed to process cold start events: %w", err)
	}

	slog.InfoContext(ctx, "Cold start processing completed")

	return nil
}

// coldStartQuery returns the query for events that may still require draining after a
// restart: in-progress drains and quarantined/already-quarantined events that were
// never processed. Note that this intentionally matches records regardless of whether
// their quarantine session has since ended; that filtering happens per-event so we can
// also tombstone stale records (see Handle).
func coldStartQuery(coldStartAfter time.Time) *query.Builder {
	condition := query.Or(
		// Events that were in-progress
		query.Eq("healtheventstatus.userpodsevictionstatus.status", string(model.StatusInProgress)),

		// Quarantined events that haven't been processed yet
		query.And(
			query.Eq("healtheventstatus.nodequarantined", string(model.Quarantined)),
			query.In("healtheventstatus.userpodsevictionstatus.status", []interface{}{"", string(model.StatusNotStarted)}),
		),

		// AlreadyQuarantined events that haven't been processed yet
		query.And(
			query.Eq("healtheventstatus.nodequarantined", string(model.AlreadyQuarantined)),
			query.In("healtheventstatus.userpodsevictionstatus.status", []interface{}{"", string(model.StatusNotStarted)}),
		),
	)

	if !coldStartAfter.IsZero() {
		condition = query.And(
			query.Gt("createdAt", coldStartAfter),
			condition,
		)
	}

	return query.New().Build(condition)
}

// processColdStartEvent evaluates a single cold-start candidate: it either tombstones a
// stale quarantine record whose session already ended, or re-queues the event for the
// normal drain pipeline.
func processColdStartEvent(
	ctx context.Context,
	deps Dependencies,
	dbAdapter *dataStoreAdapter,
	resolver *quarantineSessionResolver,
	he datastore.HealthEventWithStatus,
) {
	parsed, nodeName, documentID, ok := parseColdStartEvent(ctx, he.RawEvent)
	if !ok {
		return
	}

	if skipStaleColdStartEvent(ctx, resolver, dbAdapter, parsed, nodeName, documentID, he.CreatedAt) {
		return
	}

	if enqueueErr := deps.QueueManager.EnqueueEventGeneric(
		ctx, nodeName, he.RawEvent, dbAdapter, deps.HealthEventStore, documentID); enqueueErr != nil {
		slog.Error("Failed to enqueue cold start event", "error", enqueueErr, "nodeName", nodeName)
	} else {
		slog.InfoContext(ctx, "Re-queued event from cold start", "nodeName", nodeName)
	}
}

// parseColdStartEvent validates and extracts the fields needed to process a cold-start
// candidate. It returns ok=false (after logging) when the event is malformed and should
// be skipped.
func parseColdStartEvent(
	ctx context.Context, event datastore.Event,
) (parsed model.HealthEventWithStatus, nodeName string, documentID interface{}, ok bool) {
	if len(event) == 0 {
		slog.ErrorContext(ctx, "RawEvent is empty, skipping cold start event")

		return parsed, "", nil, false
	}

	parsed, err := eventutil.ParseHealthEventFromEvent(event)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to parse health event from cold start event", "error", err)

		return parsed, "", nil, false
	}

	if parsed.HealthEvent == nil {
		slog.ErrorContext(ctx, "Health event is nil in cold start event")

		return parsed, "", nil, false
	}

	nodeName = parsed.HealthEvent.GetNodeName()
	if nodeName == "" {
		slog.ErrorContext(ctx, "Node name is empty in cold start event")

		return parsed, "", nil, false
	}

	documentID, err = utils.ExtractDocumentIDNative(event)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to extract document ID from cold start event", "error", err)

		return parsed, "", nil, false
	}

	return parsed, nodeName, documentID, true
}

// skipStaleColdStartEvent reports whether a quarantine record should be skipped because
// its quarantine session has already ended. When it decides to skip, it also tombstones
// the record so future restarts do not replay it. It returns false (do not skip) for
// non-quarantine records and when session state cannot be determined, so genuine drains
// are never silently dropped.
func skipStaleColdStartEvent(
	ctx context.Context,
	resolver *quarantineSessionResolver,
	dbAdapter *dataStoreAdapter,
	parsed model.HealthEventWithStatus,
	nodeName string,
	documentID interface{},
	eventCreatedAt time.Time,
) bool {
	// Staleness only applies to active quarantine records. UnQuarantined/Cancelled are
	// the session-end signals we compare against, so they must never be tombstoned.
	if !isActiveQuarantineStatus(parsed.HealthEventStatus.NodeQuarantined) {
		return false
	}

	ended, err := resolver.quarantineSessionEnded(ctx, nodeName, eventCreatedAt)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to determine quarantine session state, re-queueing event",
			"error", err, "nodeName", nodeName)

		return false
	}

	if !ended {
		return false
	}

	slog.InfoContext(ctx, "Skipping stale cold start event from an ended quarantine session",
		"nodeName", nodeName, "eventCreatedAt", eventCreatedAt)

	if markErr := tombstoneStaleQuarantineEvent(ctx, dbAdapter, documentID); markErr != nil {
		slog.ErrorContext(ctx, "Failed to tombstone stale cold start event",
			"error", markErr, "nodeName", nodeName)
	}

	return true
}

// isActiveQuarantineStatus reports whether the node-quarantined status represents an
// active quarantine (as opposed to a session-ending UnQuarantined/Cancelled marker).
func isActiveQuarantineStatus(status string) bool {
	return status == string(model.Quarantined) || status == string(model.AlreadyQuarantined)
}

// tombstoneStaleQuarantineEvent marks a stale quarantine record's drain status as
// Cancelled. This removes it from future cold-start queries and, because its
// nodequarantined stays Quarantined/AlreadyQuarantined with a non-drained status, it
// does not match fault-remediation's change-stream pipeline, so no remediation label
// is applied.
func tombstoneStaleQuarantineEvent(ctx context.Context, dbClient client.DatabaseClient, documentID interface{}) error {
	filter := map[string]any{"_id": documentID}
	update := map[string]any{
		"$set": map[string]any{
			"healtheventstatus.userpodsevictionstatus.status": string(model.Cancelled),
		},
	}

	if _, err := dbClient.UpdateDocument(ctx, filter, update); err != nil {
		return fmt.Errorf("failed to tombstone stale quarantine event %v: %w", documentID, err)
	}

	return nil
}

// dataStoreAdapter adapts client.DatabaseClient to queue.DataStore.
type dataStoreAdapter struct {
	client.DatabaseClient
}

func (d *dataStoreAdapter) FindDocument(ctx context.Context, filter interface{},
	options *client.FindOneOptions) (client.SingleResult, error) {
	return d.FindOne(ctx, filter, options)
}

func (d *dataStoreAdapter) FindDocuments(ctx context.Context, filter interface{},
	options *client.FindOptions) (client.Cursor, error) {
	return d.Find(ctx, filter, options)
}
