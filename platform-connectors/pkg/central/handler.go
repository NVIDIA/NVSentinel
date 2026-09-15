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

package central

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// clientKeyPattern is the accepted shape of the client-supplied batch
// idempotency key. The key lands inside a server-derived composite, so its
// alphabet and length are a contract; "#" is the composite's separator.
var clientKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// clientIdempotencyKey reads and validates the batch's idempotency-key
// header. The header is mandatory: monitors retry over the network, and
// without the key a resent batch would be stored twice.
func clientIdempotencyKey(md metadata.MD) (string, error) {
	keys := md.Get(healthpub.IdempotencyKeyHeader)

	if len(keys) > 1 {
		return "", status.Error(codes.InvalidArgument, "multiple idempotency-key headers")
	}

	if len(keys) == 0 || keys[0] == "" {
		return "", status.Error(codes.InvalidArgument, "idempotency-key header is required")
	}

	if !clientKeyPattern.MatchString(keys[0]) {
		return "", status.Error(codes.InvalidArgument, "idempotency-key must match ^[A-Za-z0-9._:-]{1,128}$")
	}

	return keys[0], nil
}

// stampIdempotencyKeys writes the server-derived per-event key
// podUID#clientKey#eventIndex into each event's metadata, always overwriting
// any inbound value: the stored key is scoped to the authenticated caller, so
// an incoming value is never trusted.
func stampIdempotencyKeys(he *pb.HealthEvents, podUID, clientKey string) {
	prefix := podUID + "#" + clientKey + "#"

	for i, ev := range he.GetEvents() {
		if ev.Metadata == nil {
			ev.Metadata = map[string]string{}
		}

		ev.Metadata[datastore.HealthEventIdempotencyKeyMetadataField] = prefix + strconv.Itoa(i)
	}
}

// storeWriter is the datastore side of a request: the store connector's
// InsertBatch.
type storeWriter interface {
	InsertBatch(ctx context.Context, he *pb.HealthEvents) (string, error)
}

// batchProcessor is a connector called once per batch with no queue in
// between: the k8s connector's ProcessBatch and the gRPC sink's SendBatch.
type batchProcessor interface {
	ProcessBatch(ctx context.Context, he *pb.HealthEvents) error
}

// writeServer serves the PlatformConnector gRPC service for the deployment
// platform connector. A request is validated, keyed and run through the
// pipeline; then the datastore write and the node condition update run in
// parallel and the reply waits for both. The write decides the reply; the
// condition update is best effort inside a bounded wait, as it is today.
type writeServer struct {
	pb.UnimplementedPlatformConnectorServer
	pipeline *pipeline.Pipeline
	store    storeWriter
	// conditions is nil when the k8s connector is disabled in the shared
	// platform connector config.
	conditions batchProcessor
	// sink is nil unless the optional ADR-033 gRPC sink is enabled.
	sink batchProcessor
	// prom is nil unless the optional Prometheus connector is enabled; it
	// counts every accepted batch in health_events_total, as the node-local
	// role counts every batch it receives.
	prom             batchProcessor
	conditionTimeout time.Duration
	// pipelineTimeout bounds the pipeline for one batch as a whole, chiefly
	// the node metadata reads of a cross-node batch with many uncached nodes.
	// A batch whose nodes were not all read inside it is deferred with
	// Unavailable, so the caller resends it once the reads that were started
	// have warmed the cache, instead of the unread nodes passing the
	// managed-label gate.
	pipelineTimeout time.Duration
	// ready gates writes on the idempotency index: a replica that
	// has not verified the index refuses batches, so a resend is never stored
	// twice before the index exists. Taking the replica out of the Service is
	// not enough, because established connections keep sending to it.
	ready *readiness
}

func (w *writeServer) HealthEventOccurredV1(ctx context.Context, he *pb.HealthEvents) (*emptypb.Empty, error) {
	start := time.Now()

	outcome, err := w.handle(ctx, he)
	requests.WithLabelValues(outcome).Observe(time.Since(start).Seconds())

	if err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

// handle runs one batch to its reply and reports the outcome label.
func (w *writeServer) handle(ctx context.Context, he *pb.HealthEvents) (string, error) {
	caller := callerFromContext(ctx)
	if caller == nil {
		return outcomeRejected, status.Error(codes.Internal, "caller identity missing from context")
	}

	md, _ := metadata.FromIncomingContext(ctx)

	// The monitor's span context arrives in the metadata; extract it first so
	// everything below joins the trace the monitor started.
	ctx = propagation.TraceContext{}.Extract(ctx, healthpub.MetadataCarrier(md))
	ctx, span := tracing.StartSpan(ctx, "platform_connector.deployment.health_event_occurred")

	defer span.End()

	clientKey, err := clientIdempotencyKey(md)
	if err != nil {
		return outcomeRejected, err
	}

	if err := server.ApplyEventDefaultsAndValidate(he.GetEvents()); err != nil {
		return outcomeRejected, err
	}

	if !w.ready.writesAllowed() {
		return outcomeFailed, status.Error(codes.Unavailable, "idempotency index not verified on this replica; retry")
	}

	stampIdempotencyKeys(he, caller.PodUID, clientKey)

	slog.DebugContext(ctx, "Health events received",
		"caller", caller.Username, "node", caller.NodeName, "eventCount", len(he.GetEvents()))

	if err := w.runPipeline(ctx, he, caller.PodUID, clientKey); err != nil {
		return w.deferBatch(ctx, err, caller.Username, len(he.GetEvents()))
	}

	if w.prom != nil {
		// Counted after the pipeline, like the node-local role counts what
		// reaches its ring buffer, so the labels the overrides transformer
		// rewrites match in both roles. ProcessBatch never fails.
		_ = w.prom.ProcessBatch(ctx, he)
	}

	outcome, storeErr := w.writeAndUpdate(ctx, he)
	if storeErr != nil {
		tracing.RecordError(span, storeErr)

		return outcomeFailed, w.writeFailure(ctx, storeErr, caller.Username, len(he.GetEvents()))
	}

	return outcome, nil
}

// runPipeline runs the batch through the pipeline inside the batch budget and
// stamps the server-owned keys again afterwards: transformers copy node labels
// into the event metadata, and the first stamp stays because dedup reads it to
// recognise a resend. It returns an error when the node metadata of the batch
// was not all read inside the budget; that is decided before any transformer
// runs, so dedup never remembers events of a batch that was not stored.
func (w *writeServer) runPipeline(ctx context.Context, he *pb.HealthEvents, podUID, clientKey string) error {
	// The write below keeps the request context; only the pipeline is bounded.
	pipelineCtx, cancel := context.WithTimeout(ctx, w.pipelineTimeout)
	defer cancel()

	if err := w.pipeline.Prewarm(pipelineCtx, he.GetEvents()); err != nil {
		return err
	}

	for _, ev := range he.GetEvents() {
		w.pipeline.Process(pipelineCtx, ev)
	}

	stampIdempotencyKeys(he, podUID, clientKey)

	return nil
}

// deferBatch answers a batch whose node metadata was not all read inside the
// pipeline budget: nothing was written, so the caller resends it with the same
// key and the next attempt finds the cache warmed by the reads that were
// started. The caller's own cancellation is reported as such.
func (w *writeServer) deferBatch(ctx context.Context, err error, caller string, eventCount int) (string, error) {
	if ctx.Err() != nil {
		return outcomeFailed, status.FromContextError(ctx.Err()).Err()
	}

	slog.WarnContext(ctx, "Deferring batch: node metadata not read within the pipeline budget",
		"caller", caller, "eventCount", eventCount, "error", err)

	return outcomeDeferred, status.Error(codes.Unavailable,
		"node metadata not read within the pipeline budget; resend: "+err.Error())
}

// writeFailure turns a failed write into the reply: the caller's own
// cancellation when it gave up (it resends with the same key), Unavailable
// otherwise so that it retries.
func (w *writeServer) writeFailure(ctx context.Context, storeErr error, caller string, eventCount int) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}

	slog.ErrorContext(ctx, "Datastore write failed, batch not acknowledged",
		"error", storeErr, "caller", caller, "eventCount", eventCount)

	return status.Errorf(codes.Unavailable, "datastore write failed: %v", storeErr)
}

// writeAndUpdate runs the datastore write, the node condition update and the
// optional sink in parallel and waits for all of them; a failed write cuts the
// other two short, since the batch is then not acknowledged and will be
// resent, and the reply must not wait for them. Only the write's error
// is returned; the others are best effort and are counted where they fail.
func (w *writeServer) writeAndUpdate(ctx context.Context, he *pb.HealthEvents) (string, error) {
	var wg sync.WaitGroup

	sideCtx, cancelSide := context.WithCancel(ctx)
	defer cancelSide()

	if w.conditions != nil {
		wg.Go(func() { w.updateConditions(sideCtx, he) })
	}

	if w.sink != nil {
		wg.Go(func() {
			if err := w.sink.ProcessBatch(sideCtx, he); err != nil {
				slog.WarnContext(ctx, "gRPC sink forward failed", "error", err)
			}
		})
	}

	// The write runs here, on the request goroutine, alongside the two above.
	outcome, storeErr := w.store.InsertBatch(ctx, he)
	if storeErr != nil {
		cancelSide()
	}

	wg.Wait()

	return outcome, storeErr
}

// updateConditions applies the batch to node conditions and Kubernetes Events
// inside the bounded wait. The k8s connector keeps its own retries for
// conflicts and passing errors; a failure past them is counted and logged,
// and the node shows the old state until the monitor next reports.
func (w *writeServer) updateConditions(ctx context.Context, he *pb.HealthEvents) {
	cctx, cancel := context.WithTimeout(ctx, w.conditionTimeout)
	defer cancel()

	err := w.conditions.ProcessBatch(cctx, he)
	if err == nil {
		return
	}

	if ctx.Err() != nil {
		// The caller gave up on the request or its write failed: either way
		// the batch is not acknowledged and will be resent; not a condition
		// update failure.
		slog.DebugContext(ctx, "Node condition update cut short with the request", "error", err)

		return
	}

	reason := conditionFailed
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		reason = conditionTimedOut
	}

	conditionUpdateFailures.WithLabelValues(reason).Inc()
	slog.WarnContext(ctx, "Node condition update did not land; batch acknowledged anyway",
		"reason", reason, "error", err, "eventCount", len(he.GetEvents()))
}
