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

package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
)

// Outcomes of one request, the label of requestDuration.
const (
	outcomeOK       = "ok"
	outcomeRejected = "rejected"
	outcomeFailed   = "failed"
)

var (
	healthEventsReceived = promauto.NewCounter(prometheus.CounterOpts{
		Name: "platform_connector_health_events_received_total",
		Help: "The total number of health events that the platform connector has received",
	})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "platform_connector_request_duration_seconds",
		Help: "Duration of health event batch requests, by outcome: ok, rejected (the batch is invalid) or " +
			"failed (a connector did not accept the batch; the caller retries)",
		Buckets: prometheus.DefBuckets,
	}, []string{"outcome"})
)

// PlatformConnectorServer serves the PlatformConnector gRPC service for both
// roles of the binary. A batch is validated, run through the pipeline and
// handed to Connector, whose result is the reply. The node-local DaemonSet
// wires a set of ring buffers, so the reply is immediate and the connectors
// process the batch later with their own retries; the deployment platform
// connector wires the connectors themselves, so the reply waits for the
// datastore write.
type PlatformConnectorServer struct {
	pb.UnimplementedPlatformConnectorServer
	Pipeline  *pipeline.Pipeline
	Connector connectors.Connector
}

// ApplyEventDefaultsAndValidate fills per-event defaults in place and rejects
// batches that violate the request contract, so both roles accept exactly the
// same batches.
//
// An event without a GeneratedTimestamp is stamped with the arrival time:
// every consumer of the stored event reads the timestamp, and the health
// events analyzer cannot process an event that has none, so storing one would
// leave the analyzer stuck at that event on every restart.
func ApplyEventDefaultsAndValidate(events []*pb.HealthEvent) error {
	for _, event := range events {
		// Custom monitors that don't set processingStrategy will default to EXECUTE_REMEDIATION.
		if event.ProcessingStrategy == pb.ProcessingStrategy_UNSPECIFIED {
			event.ProcessingStrategy = pb.ProcessingStrategy_EXECUTE_REMEDIATION
		}

		if event.GeneratedTimestamp == nil {
			slog.Warn("HealthEvent has nil GeneratedTimestamp, stamping the arrival time",
				"node", event.NodeName, "agent", event.Agent, "check", event.CheckName)

			event.GeneratedTimestamp = timestamppb.Now()
		}

		if event.RecommendedAction == pb.RecommendedAction_CUSTOM && event.CustomRecommendedAction == "" {
			return status.Errorf(codes.InvalidArgument,
				"recommendedAction is CUSTOM but customRecommendedAction is empty (node=%s, agent=%s)",
				event.NodeName, event.Agent)
		}
	}

	return nil
}

// HealthEventOccurredV1 receives one batch of health events and answers once
// the connector has accepted it.
func (p *PlatformConnectorServer) HealthEventOccurredV1(ctx context.Context,
	he *pb.HealthEvents) (*empty.Empty, error) {
	start := time.Now()

	outcome, err := p.handle(ctx, he)
	requestDuration.WithLabelValues(outcome).Observe(time.Since(start).Seconds())

	if err != nil {
		return nil, err
	}

	return &empty.Empty{}, nil
}

// handle runs one batch to its reply and reports the outcome label.
func (p *PlatformConnectorServer) handle(ctx context.Context, he *pb.HealthEvents) (string, error) {
	// A caller that sent its span context in the request metadata (the
	// deployment platform connector's clients do) has every span below join
	// its trace; otherwise the trace starts here.
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = propagation.TraceContext{}.Extract(ctx, healthpub.MetadataCarrier(md))
	}

	ctx, span := tracing.StartSpan(ctx, "platform_connector.grpc.health_events_received")
	defer span.End()

	eventCount := len(he.GetEvents())
	span.SetAttributes(
		attribute.Int("platform_connector.grpc.event_count", eventCount),
	)

	slog.DebugContext(ctx, "Health events received", "events", he)
	healthEventsReceived.Add(float64(eventCount))

	if err := ApplyEventDefaultsAndValidate(he.GetEvents()); err != nil {
		return outcomeRejected, err
	}

	if p.Pipeline != nil {
		p.Pipeline.ProcessBatch(ctx, he.GetEvents())
	}

	if p.Connector != nil {
		if err := p.Connector.ProcessBatch(ctx, he); err != nil {
			tracing.RecordError(span, err)

			return outcomeFailed, batchFailure(ctx, err, eventCount)
		}
	}

	return outcomeOK, nil
}

// batchFailure turns a connector's failure into the reply: the caller's own
// cancellation when it gave up (it resends with the same key); a status the
// connector chose itself (a batch the datastore refuses as sent, which a
// resend would not change); Unavailable otherwise, so that the caller retries.
func batchFailure(ctx context.Context, err error, eventCount int) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}

	slog.ErrorContext(ctx, "Batch not acknowledged", "error", err, "eventCount", eventCount)

	if _, chosen := status.FromError(err); chosen {
		return err
	}

	return status.Errorf(codes.Unavailable, "batch not processed: %v", err)
}
