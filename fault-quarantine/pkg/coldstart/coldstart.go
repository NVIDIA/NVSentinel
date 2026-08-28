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

// Package coldstart recovers processable health events that fault-quarantine
// did not handle before its change-stream position was lost.
package coldstart

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/query"
)

const batchSize = 1000

type recoveryContextKey struct{}

// WithRecoveryContext marks event processing as cold-start replay. Consumers
// can use it to bypass eventually-consistent caches between ordered events.
func WithRecoveryContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, recoveryContextKey{}, true)
}

// IsRecoveryContext reports whether the current event came from cold start.
func IsRecoveryContext(ctx context.Context) bool {
	recovering, _ := ctx.Value(recoveryContextKey{}).(bool)

	return recovering
}

type ProcessResult string

const (
	ProcessResultProcessed ProcessResult = "processed"
	ProcessResultSkipped   ProcessResult = "skipped"
	ProcessResultInvalid   ProcessResult = "invalid"
	ProcessResultFailed    ProcessResult = "failed"
)

type EventProcessor interface {
	ProcessStoredEvent(ctx context.Context, event datastore.Event) (ProcessResult, error)
}

type Dependencies struct {
	HealthEventStore   datastore.HealthEventStore
	EventProcessor     EventProcessor
	ColdStartAfterTime time.Time
}

// Handle replays unresolved events in creation order. Processing stops on a
// transient failure so the pod can restart without moving on to live events.
// Invalid stored documents are counted and skipped because retrying them cannot
// make them valid.
func Handle(ctx context.Context, deps Dependencies) error {
	if deps.HealthEventStore == nil {
		return fmt.Errorf("health event store is required")
	}

	if deps.EventProcessor == nil {
		return fmt.Errorf("event processor is required")
	}

	startedAt := time.Now()
	defer func() {
		metrics.ColdStartDuration.Observe(time.Since(startedAt).Seconds())
	}()

	slog.InfoContext(ctx, "Recovering unresolved fault-quarantine events")

	err := deps.HealthEventStore.FindHealthEventsByQueryBatched(
		ctx,
		coldStartQuery(deps.ColdStartAfterTime),
		batchSize,
		func(events []datastore.HealthEventWithStatus) error {
			for i := range events {
				result, err := deps.EventProcessor.ProcessStoredEvent(ctx, events[i].RawEvent)
				if err != nil {
					metrics.ColdStartEvents.WithLabelValues(string(ProcessResultFailed)).Inc()

					return fmt.Errorf("failed to recover stored event: %w", err)
				}

				metrics.ColdStartEvents.WithLabelValues(string(result)).Inc()
			}

			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("fault-quarantine cold start failed: %w", err)
	}

	slog.InfoContext(ctx, "Fault-quarantine event recovery completed")

	return nil
}

func coldStartQuery(coldStartAfter time.Time) *query.Builder {
	strategy := int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION)
	unresolved := query.Or(
		query.Eq("healtheventstatus.nodequarantined", nil),
		query.Eq("healtheventstatus.nodequarantined", ""),
		query.Eq("healtheventstatus.nodequarantined", string(model.StatusNotStarted)),
	)
	processable := query.Or(
		query.Eq("healthevent.processingstrategy", strategy),
		query.Eq("healthevent.processingStrategy", strategy),
		query.And(
			query.Eq("healthevent.processingstrategy", nil),
			query.Eq("healthevent.processingStrategy", nil),
		),
	)

	condition := query.And(unresolved, processable)

	if !coldStartAfter.IsZero() {
		condition = query.And(query.Gt("createdAt", coldStartAfter), condition)
	}

	return query.New().Build(condition)
}
