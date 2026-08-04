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

package dedup

import (
	"context"
	"log/slog"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// Name is the pipeline registry name for the deduplication transformer.
const Name = "Deduplicator"

// Deduplicator marks repeated health events as STORE_ONLY within a tracker suppression window.
type Deduplicator struct {
	tracker *tracker
	include map[string]bool
	cancel  context.CancelFunc
}

// NewDeduplicator creates a transformer backed by tracker and a check-name include list.
func NewDeduplicator(
	tracker *tracker,
	includeChecks []string,
	cancel ...context.CancelFunc,
) *Deduplicator {
	include := make(map[string]bool, len(includeChecks))
	for _, check := range includeChecks {
		include[check] = true
	}

	d := &Deduplicator{
		tracker: tracker,
		include: include,
	}
	if len(cancel) > 0 {
		d.cancel = cancel[0]
	}

	return d
}

// Close stops the background eviction loop, if this transformer owns one.
func (d *Deduplicator) Close() error {
	if d.cancel != nil {
		d.cancel()
	}

	return nil
}

// Name returns the pipeline stage name.
func (d *Deduplicator) Name() string {
	return Name
}

// Transform downgrades duplicate unhealthy events to STORE_ONLY so they are persisted
// but do not create Kubernetes-side remediation effects.
func (d *Deduplicator) Transform(ctx context.Context, event *pb.HealthEvent) error {
	if len(d.include) > 0 && !d.include[event.GetCheckName()] {
		return nil
	}

	if event.GetIsHealthy() {
		clearedUnhealthy := d.tracker.clearUnhealthyCounterpart(event)
		if clearedUnhealthy {
			slog.InfoContext(ctx, "Healthy event cleared unhealthy dedup counterpart",
				"node", event.GetNodeName(),
				"check", event.GetCheckName(),
				"err_code", errCodeLabel(event))
		}

		return nil
	}

	if d.tracker.checkAndMark(event) {
		dedupStoreAndAnalyseCounter.WithLabelValues(
			event.GetCheckName(),
			event.GetNodeName(),
			errCodeLabel(event),
		).Inc()
		event.ProcessingStrategy = pb.ProcessingStrategy_STORE_AND_ANALYSE
		slog.InfoContext(ctx, "Duplicate health event marked STORE_AND_ANALYSE by deduplication",
			"node", event.GetNodeName(),
			"check", event.GetCheckName(),
			"err_code", errCodeLabel(event))

		return nil
	}

	return nil
}

func startEvictExpired(ctx context.Context, tracker *tracker, interval time.Duration) {
	ticker := time.NewTicker(interval)

	go func() {
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tracker.evictExpired()
			}
		}
	}()
}
