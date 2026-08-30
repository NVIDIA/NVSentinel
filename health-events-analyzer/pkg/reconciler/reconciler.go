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
	"fmt"
	"log/slog"
	"sync"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/analyzer"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/parser"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// No retry constants needed - EventProcessor no longer retries internally

const (
	// agentName identifies this component in health events it publishes, and is
	// used to filter out its own events when watching the store.
	agentName = "health-events-analyzer"
	// fieldProcessingStrategy is the stored document field holding the
	// per-event processing strategy.
	fieldProcessingStrategy = "healthevent.processingstrategy"
	// fieldNodeName is the stored document field used to scope rule evaluation
	// to the node that produced the incoming event.
	fieldNodeName = "healthevent.nodename"
	// fieldGeneratedTimestamp is used to preserve created-at fallback semantics
	// for legacy events without a generated timestamp.
	fieldGeneratedTimestamp = "healthevent.generatedtimestamp"
	// aggregationOperatorGT is the greater-than operator used in generated
	// analyzer aggregation stages.
	aggregationOperatorGT = "$gt"
	aggregationOperatorOr = "$or"
)

type HealthEventsAnalyzerReconcilerConfig struct {
	DataStoreConfig           *datastore.DataStoreConfig
	Pipeline                  any
	HealthEventsAnalyzerRules *config.TomlConfig
	Publisher                 *publisher.PublisherConfig
}

type Reconciler struct {
	config             HealthEventsAnalyzerReconcilerConfig
	datastore          datastore.DataStore
	databaseClient     client.DatabaseClient // MongoDB-specific client for aggregation
	eventProcessor     client.EventProcessor
	xidDetector        *analyzer.XidBurstDetector // PostgreSQL-specific XID burst detection
	useXidDetector     bool                       // True if using PostgreSQL
	provider           datastore.DataStoreProvider
	recoveryMu         sync.RWMutex
	recoveryBoundaries map[string]recoveryBoundary
	recoveryLoaded     map[string]struct{}
	derivedStates      map[string]derivedState
	recoveryPoll       time.Duration
	recoveryRepublish  time.Duration
	recoveryTimeout    time.Duration
}

func NewReconciler(cfg HealthEventsAnalyzerReconcilerConfig) *Reconciler {
	return &Reconciler{
		config: cfg,
	}
}

// Start begins the reconciliation process by listening to change stream events
// and processing them accordingly.
func (r *Reconciler) Start(ctx context.Context) error {
	// Create datastore using NEW abstraction
	ds, err := datastore.NewDataStore(ctx, *r.config.DataStoreConfig)
	if err != nil {
		return fmt.Errorf("failed to create datastore: %w", err)
	}
	defer ds.Close(ctx)

	r.datastore = ds
	r.provider = ds.Provider()

	// Check if using PostgreSQL and enable XID burst detector
	if ds.Provider() == datastore.ProviderPostgreSQL {
		slog.DebugContext(ctx, "PostgreSQL detected - enabling Go-based XID burst detection")

		// Extract XID burst detector config from the RepeatedXidError rule's pipeline
		xidConfig := r.extractXidDetectorConfig()
		r.xidDetector = analyzer.NewXidBurstDetectorWithConfig(xidConfig)
		r.useXidDetector = true
	} else {
		slog.DebugContext(ctx, "MongoDB detected - using pipeline-based XID detection")

		r.useXidDetector = false
	}

	// Get database client and change stream watcher from datastore
	datastoreAdapter, ok := ds.(interface {
		GetDatabaseClient() client.DatabaseClient
		CreateChangeStreamWatcher(
			ctx context.Context, clientName string, pipeline any,
		) (datastore.ChangeStreamWatcher, error)
	})
	if !ok {
		return fmt.Errorf("datastore does not support required operations (GetDatabaseClient and CreateChangeStreamWatcher)")
	}

	r.databaseClient = datastoreAdapter.GetDatabaseClient()

	changeStreamWatcher, err := datastoreAdapter.CreateChangeStreamWatcher(
		ctx, agentName, r.config.Pipeline)
	if err != nil {
		return fmt.Errorf("failed to create change stream watcher: %w", err)
	}

	// Unwrap for EventProcessor compatibility
	type unwrapper interface {
		Unwrap() client.ChangeStreamWatcher
	}

	unwrapable, ok := changeStreamWatcher.(unwrapper)
	if !ok {
		return fmt.Errorf("watcher does not support unwrapping to client.ChangeStreamWatcher")
	}

	oldWatcher := unwrapable.Unwrap()

	// The handler owns bounded retries. If an event remains uncheckpointed, the
	// processor stops so a later event cannot advance the resume token past it.
	processorConfig := client.EventProcessorConfig{
		EnableMetrics:        true,
		MetricsLabels:        map[string]string{"module": agentName},
		MarkProcessedOnError: false, // IMPORTANT: Don't mark failed events as processed
	}

	r.eventProcessor = client.NewEventProcessor(oldWatcher, r.databaseClient, processorConfig)

	// Set the event handler for processing health events
	r.eventProcessor.SetEventHandler(client.EventHandlerFunc(r.processHealthEvent))

	slog.InfoContext(ctx, "Starting health events analyzer with unified event processor...")

	// Start the event processor
	return r.eventProcessor.Start(ctx)
}

// processHealthEvent handles individual health events and implements the EventHandler interface
func (r *Reconciler) processHealthEvent(ctx context.Context, event *datamodels.HealthEventWithStatus) error {
	startTime := time.Now()

	traceID := tracing.TraceIDFromMetadata(event.HealthEvent.GetMetadata())
	parentSpanID := tracing.ParentSpanID(event.HealthEventStatus.SpanIds, tracing.ServicePlatformConnector)

	ctx, span := tracing.StartSpanWithLinkFromTraceContext(
		ctx, traceID, parentSpanID, "health_events_analyzer.process_health_event")
	defer span.End()

	// Track event reception metrics
	// Use nodeName as label value, fall back to first entity if available
	labelValue := event.HealthEvent.NodeName

	if labelValue == "" && len(event.HealthEvent.EntitiesImpacted) > 0 {
		labelValue = event.HealthEvent.EntitiesImpacted[0].EntityValue
	}

	if labelValue == "" {
		labelValue = "unknown"
	}

	totalEventsReceived.WithLabelValues(labelValue).Inc()

	// Process the event using existing business logic
	publishedNewEvent, err := r.handleEvent(ctx, event)
	if err != nil {
		// Return error - EventProcessor will NOT mark as processed
		// Event will be retried on next pod restart
		totalEventProcessingError.WithLabelValues("handle_event_error").Inc()
		slog.ErrorContext(ctx, "Failed to process health event", "error", err, "nodeName", labelValue)

		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "handle_event_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return fmt.Errorf("failed to handle event: %w", err)
	}

	// Track success metrics
	totalEventsSuccessfullyProcessed.Inc()

	span.SetAttributes(
		attribute.Bool("health_events_analyzer.event.published", publishedNewEvent),
	)

	r.recordPublishedEvent(ctx, event.HealthEvent, publishedNewEvent)

	// Track processing duration
	duration := time.Since(startTime).Seconds()
	eventHandlingDuration.Observe(duration)

	return nil
}

func (r *Reconciler) recordPublishedEvent(ctx context.Context, event *protos.HealthEvent, published bool) {
	if !published {
		slog.InfoContext(ctx, "No derived event published.")

		return
	}

	if event.IsHealthy {
		slog.InfoContext(ctx, "Derived recovery event published.")

		return
	}

	slog.InfoContext(ctx, "New fatal event published.")

	if len(event.EntitiesImpacted) == 0 {
		slog.WarnContext(ctx, "Fatal event published but EntitiesImpacted is empty, using 'unknown' for metrics")
		fatalEventsPublishedTotal.WithLabelValues("unknown").Inc()

		return
	}

	fatalEventsPublishedTotal.WithLabelValues(event.EntitiesImpacted[0].EntityValue).Inc()
}

func (r *Reconciler) handleEvent(ctx context.Context, event *datamodels.HealthEventWithStatus) (bool, error) {
	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.handle_event")
	defer span.End()

	var multiErr *multierror.Error

	publishedNewEvent := false

	// Healthy events are admitted only for configured recovery mappings. Keep
	// the PostgreSQL XID detector on its existing unhealthy-event input.
	if !event.HealthEvent.IsHealthy {
		published, err := r.handleXidDetector(ctx, event)
		if err != nil {
			multiErr = multierror.Append(multiErr, err)
		}

		if published {
			publishedNewEvent = true
		}
	}

	published, err := r.processHealthState(ctx, event, span)
	if err != nil {
		multiErr = multierror.Append(multiErr, err)
	}

	publishedNewEvent = published || publishedNewEvent

	if multiErr.ErrorOrNil() != nil {
		slog.ErrorContext(ctx, "Error in handling the event", "error", multiErr)
		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "handle_event_error"),
			attribute.String("health_events_analyzer.error.message", multiErr.Error()),
		)
		tracing.RecordError(span, multiErr.ErrorOrNil())

		return publishedNewEvent, fmt.Errorf("error in handling the event: %w", multiErr)
	}

	return publishedNewEvent, nil
}

func (r *Reconciler) processHealthState(
	ctx context.Context,
	event *datamodels.HealthEventWithStatus,
	span trace.Span,
) (bool, error) {
	if event.HealthEvent.IsHealthy {
		return r.handleRecoveryEvents(ctx, event)
	}

	return r.processConfiguredRules(ctx, event, span)
}

func (r *Reconciler) processConfiguredRules(
	ctx context.Context,
	event *datamodels.HealthEventWithStatus,
	span trace.Span,
) (bool, error) {
	var multiErr *multierror.Error

	publishedAny := false

	for _, rule := range r.config.HealthEventsAnalyzerRules.Rules {
		if !rule.EvaluateRule {
			slog.InfoContext(ctx, "Skipping rule evaluation", "rule_name", rule.Name)

			continue
		}

		published, err := r.processRule(ctx, rule, event)
		if err != nil {
			if client.IsPermanentError(err) {
				slog.ErrorContext(ctx, "Skipping rule after deterministic evaluation failure",
					"rule_name", rule.Name, "error", err)
				totalEventProcessingError.WithLabelValues("permanent_rule_error").Inc()
				span.AddEvent("permanent_rule_error", trace.WithAttributes(
					attribute.String("health_events_analyzer.error.message", err.Error()),
				))

				continue
			}

			multiErr = multierror.Append(multiErr, err)
			span.AddEvent("rule_evaluation_error", trace.WithAttributes(
				attribute.String("health_events_analyzer.error.type", "rule_evaluation_error"),
				attribute.String("health_events_analyzer.error.message", err.Error()),
			))

			continue
		}

		publishedAny = published || publishedAny
	}

	return publishedAny, multiErr.ErrorOrNil()
}

// handleXidDetector handles XID burst detection and history clearing
func (r *Reconciler) handleXidDetector(ctx context.Context, event *datamodels.HealthEventWithStatus) (bool, error) {
	if !r.useXidDetector {
		return false, nil
	}

	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.handle_xid_detector")
	defer span.End()

	// Check for GPU XID errors and detect burst patterns
	if r.shouldProcessXidEvent(event.HealthEvent) {
		published, err := r.processXidBurstDetection(ctx, event.HealthEvent)
		if err != nil {
			slog.ErrorContext(ctx, "Error processing XID burst detection", "error", err)
			span.SetAttributes(
				attribute.String("health_events_analyzer.error.type", "xid_burst_detection_error"),
				attribute.String("health_events_analyzer.error.message", err.Error()),
			)
			tracing.RecordError(span, err)

			return false, err
		}

		span.SetAttributes(attribute.Bool("health_events_analyzer.published_event", published))

		return published, nil
	}

	return false, nil
}

// processRule handles the processing of a single rule against an event
func (r *Reconciler) processRule(ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	event *datamodels.HealthEventWithStatus) (bool, error) {
	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.evaluate_rule")
	defer span.End()

	span.SetAttributes(
		attribute.String("health_events_analyzer.rule.name", rule.Name),
		attribute.String("health_events_analyzer.rule.recommended_action", rule.RecommendedAction),
	)

	startTime := time.Now()

	// Validate all sequences from DB docs
	matchedSequences, err := r.validateAllSequenceCriteria(ctx, rule, *event)
	if err != nil {
		slog.ErrorContext(ctx, "Error in validating all sequence criteria", "error", err)
		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "validate_sequence_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return false, fmt.Errorf("error in validating all sequence criteria: %w", err)
	}

	duration := time.Since(startTime).Seconds()
	mongoQueryExecutionDuration.WithLabelValues(rule.Name).Observe(duration)

	span.AddEvent("rule_evaluation_duration_seconds", trace.WithAttributes(
		attribute.Float64("rule_evaluation_duration_seconds", duration),
	))

	if !matchedSequences {
		return false, nil
	}

	identity, recoveryEnabled := recoveryIdentityForEvent(rule, event.HealthEvent)

	if rule.Recovery != nil && !recoveryEnabled {
		slog.WarnContext(ctx, "Rule match does not contain the configured recovery scope; "+
			"publishing without automatic recovery",
			"rule_name", rule.Name,
			"node", event.HealthEvent.NodeName,
			"entity_types", rule.Recovery.EntityTypes)
	}

	ruleMatchedTotal.WithLabelValues(rule.Name, event.HealthEvent.NodeName).Inc()

	published, err := r.publishRuleMatch(ctx, rule, event, identity, recoveryEnabled)
	if err != nil {
		slog.ErrorContext(ctx, "Error in publishing the matched event", "error", err)
		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "publish_matched_event_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return false, fmt.Errorf("error in publishing the matched event: %w", err)
	}

	return published, nil
}

func (r *Reconciler) publishRuleMatch(
	ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	event *datamodels.HealthEventWithStatus,
	identity recoveryIdentity,
	recoveryEnabled bool,
) (bool, error) {
	if !recoveryEnabled {
		if err := r.publishMatchedEvent(ctx, rule, event.HealthEvent); err != nil {
			return false, err
		}

		return true, nil
	}

	persistedBoundary, published, err := r.publishFaultUntilStored(ctx, event, rule, identity)
	if err != nil {
		return false, err
	}

	r.rememberDerivedState(rule.Name, identity, derivedState{
		boundary:  persistedBoundary,
		isHealthy: false,
	})

	return published, nil
}

// publishMatchedEvent publishes an event when a rule matches
func (r *Reconciler) publishMatchedEvent(ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	event *protos.HealthEvent) error {
	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.publish_matched_event")
	defer span.End()

	actionVal := r.getRecommendedActionValue(rule.RecommendedAction, rule.Name)

	err := r.config.Publisher.Publish(ctx, event, protos.RecommendedAction(actionVal),
		rule.Name, rule.Message, &rule)
	if err != nil {
		slog.ErrorContext(ctx, "Error in publishing the new fatal event", "error", err)
		span.SetAttributes(
			attribute.Bool("health_events_analyzer.event.published", false),
			attribute.String("health_events_analyzer.error.type", "publish_event_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)

		tracing.RecordError(span, err)

		return fmt.Errorf("error in publishing the new fatal event: %w", err)
	}

	slog.InfoContext(ctx, "New event successfully published for matching rule", "rule_name", rule.Name)

	return nil
}

// getRecommendedActionValue returns the action value, with fallback to RecommendedAction_CONTACT_SUPPORT if invalid
func (r *Reconciler) getRecommendedActionValue(recommendedAction, ruleName string) int32 {
	actionVal, ok := protos.RecommendedAction_value[recommendedAction]
	if !ok {
		defaultAction := int32(protos.RecommendedAction_CONTACT_SUPPORT)
		slog.Warn("Invalid recommended_action in rule; defaulting to CONTACT_SUPPORT",
			"recommended_action", recommendedAction,
			"rule_name", ruleName,
			"default_action", protos.RecommendedAction_name[defaultAction])

		return defaultAction
	}

	return actionVal
}

func (r *Reconciler) validateAllSequenceCriteria(ctx context.Context, rule config.HealthEventsAnalyzerRule,
	healthEventWithStatus datamodels.HealthEventWithStatus) (bool, error) {
	// Execute aggregation with tracing
	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.mongo.aggregate")
	defer span.End()

	slog.InfoContext(ctx, "→ Evaluating rule for event",
		"rule_name", rule.Name,
		"node", healthEventWithStatus.HealthEvent.NodeName,
		"error_code", healthEventWithStatus.HealthEvent.ErrorCode,
		"agent", healthEventWithStatus.HealthEvent.Agent)

	boundary, err := r.recoveryBoundaryForEvent(ctx, rule, healthEventWithStatus.HealthEvent)
	if err != nil {
		return false, fmt.Errorf("find recovery boundary: %w", err)
	}

	// Build aggregation pipeline from stages
	pipelineStages, err := r.getPipelineStages(rule, healthEventWithStatus, boundary)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to build pipeline stages", "error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "build_pipeline_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)

		return false, fmt.Errorf("failed to build pipeline stages: %w", err)
	}

	var result []map[string]any

	slog.DebugContext(ctx, "Executing aggregation pipeline",
		"rule_name", rule.Name, "pipeline_stages_count", len(pipelineStages))

	queryPipeline := any(pipelineStages)
	if rule.Recovery != nil {
		queryPipeline = client.WithExtendedFilters(pipelineStages)
	}

	cursor, err := r.databaseClient.Aggregate(ctx, queryPipeline)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to execute aggregation pipeline", "error", err, "rule_name", rule.Name)
		totalEventProcessingError.WithLabelValues("execute_pipeline_error").Inc()

		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "execute_pipeline_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return false, classifyRuleDatastoreError(
			fmt.Errorf("failed to execute aggregation pipeline: %w", err),
		)
	}

	defer cursor.Close(ctx)

	if err = cursor.All(ctx, &result); err != nil {
		slog.ErrorContext(ctx, "Failed to decode cursor", "error", err, "rule_name", rule.Name)
		totalEventProcessingError.WithLabelValues("decode_cursor_error").Inc()

		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "decode_cursor_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return false, classifyRuleDatastoreError(fmt.Errorf("failed to decode cursor: %w", err))
	}

	slog.DebugContext(ctx, "Aggregation pipeline completed", "rule_name", rule.Name, "result_count", len(result))

	// Check if we have results (rule matched)
	if len(result) > 0 {
		// Check for explicit ruleMatched field (used in tests and by SequenceFacet pipelines)
		if matched, ok := result[0]["ruleMatched"].(bool); ok {
			if matched {
				slog.InfoContext(ctx, "Rule matched via ruleMatched field",
					"rule_name", rule.Name,
					"node", healthEventWithStatus.HealthEvent.NodeName)

				return true, nil
			}

			slog.InfoContext(ctx, "Rule did not match (ruleMatched=false)",
				"rule_name", rule.Name,
				"node", healthEventWithStatus.HealthEvent.NodeName,
				"result", result[0])

			return false, nil
		}

		return true, nil
	}

	slog.InfoContext(ctx, "Rule did not match (no results)",
		"rule_name", rule.Name,
		"node", healthEventWithStatus.HealthEvent.NodeName)

	return false, nil
}

// getPipelineStages converts rule stages to aggregation pipeline stages
func (r *Reconciler) getPipelineStages(
	rule config.HealthEventsAnalyzerRule,
	healthEventWithStatus datamodels.HealthEventWithStatus,
	boundary *recoveryBoundary,
) ([]map[string]any, error) {
	// Always start with mandatory filters. The agent filter prevents the analyzer
	// from matching its own generated events, while the node filter limits each
	// rule evaluation to events from the node that produced the incoming event.
	// Keeping the node predicate in the first stage lets the datastore use its
	// node-prefixed HealthEvents index before evaluating configured rule stages.
	mandatoryMatch := map[string]any{
		"healthevent.agent": map[string]any{"$ne": agentName},
		fieldNodeName:       healthEventWithStatus.HealthEvent.NodeName,
		aggregationOperatorOr: []any{
			map[string]any{
				fieldProcessingStrategy: int32(protos.ProcessingStrategy_UNSPECIFIED),
			},
			map[string]any{
				fieldProcessingStrategy: int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION),
			},
			map[string]any{
				fieldProcessingStrategy: int32(protos.ProcessingStrategy_STORE_AND_ANALYSE),
			},
			map[string]any{
				fieldProcessingStrategy: map[string]any{"$exists": false},
			},
		},
	}

	if boundary != nil {
		if !boundary.createdAt.IsZero() {
			mandatoryMatch["createdAt"] = map[string]any{aggregationOperatorGT: boundary.createdAt}
		}

		if boundary.generated != nil {
			mandatoryMatch["$and"] = []any{
				map[string]any{
					aggregationOperatorOr: []any{
						map[string]any{
							fieldGeneratedTimestamp: map[string]any{"$exists": false},
						},
						map[string]any{
							"$expr": generatedAfterExpression(boundary.generated),
						},
					},
				},
			}
		}
	}

	pipeline := []map[string]any{
		{
			"$match": mandatoryMatch,
		},
	}

	for i, stageStr := range rule.Stage {
		// Parse the stage and resolve "this." references
		stageMap, err := parser.ParseSequenceStage(stageStr, healthEventWithStatus)
		if err != nil {
			slog.Error("Failed to parse stage", "stage_index", i, "error", err, "stage_string", stageStr)
			totalEventProcessingError.WithLabelValues("parse_stage_error").Inc()

			return nil, client.PermanentError(fmt.Errorf("failed to parse stage %d: %w", i, err))
		}

		slog.Debug("Parsed aggregation stage", "rule_name", rule.Name, "stage_index", i)

		pipeline = append(pipeline, stageMap)
	}

	return pipeline, nil
}

func classifyRuleDatastoreError(err error) error {
	if datastore.IsDeterministicError(err) {
		return client.PermanentError(err)
	}

	return err
}

func generatedAfterExpression(timestamp *timestamppb.Timestamp) map[string]any {
	return map[string]any{
		aggregationOperatorOr: []any{
			map[string]any{
				aggregationOperatorGT: []any{"$healthevent.generatedtimestamp.seconds", timestamp.Seconds},
			},
			map[string]any{
				"$and": []any{
					map[string]any{
						"$eq": []any{"$healthevent.generatedtimestamp.seconds", timestamp.Seconds},
					},
					map[string]any{
						aggregationOperatorGT: []any{"$healthevent.generatedtimestamp.nanos", int64(timestamp.Nanos)},
					},
				},
			},
		},
	}
}

// shouldProcessXidEvent checks if an event should be processed by the XID burst detector
func (r *Reconciler) shouldProcessXidEvent(event *protos.HealthEvent) bool {
	// Only process GPU XID errors (unhealthy GPU events with error codes)
	return event != nil &&
		event.ComponentClass == "GPU" &&
		!event.IsHealthy &&
		len(event.ErrorCode) > 0 &&
		event.Agent != agentName // Don't process our own events
}

// processXidBurstDetection processes GPU XID events through the burst detector
// and publishes RepeatedXidError events when burst patterns are detected
func (r *Reconciler) processXidBurstDetection(ctx context.Context, event *protos.HealthEvent) (bool, error) {
	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.xid.burst_detection")
	defer span.End()

	shouldTrigger, burstCount := r.xidDetector.ProcessEvent(event)

	span.SetAttributes(
		attribute.Bool("health_events_analyzer.xid.burst_detected", shouldTrigger),
		attribute.Int("health_events_analyzer.xid.burst_count", burstCount),
	)

	if !shouldTrigger {
		slog.DebugContext(ctx, "XID event processed but no burst pattern detected",
			"node", event.NodeName,
			"xid", event.ErrorCode[0],
			"burstCount", burstCount)

		return false, nil
	}

	// Burst pattern detected - publish RepeatedXidError event
	xidCode := event.ErrorCode[0]
	slog.InfoContext(ctx, "RepeatedXidError detected - publishing alert",
		"node", event.NodeName,
		"xid", xidCode,
		"burstCount", burstCount)

	// Use the publisher to create and publish the RepeatedXidError event
	// The publisher will set agent, checkName, isHealthy, isFatal, and recommendedAction
	err := r.config.Publisher.Publish(ctx, event, protos.RecommendedAction_CONTACT_SUPPORT, "RepeatedXidError",
		event.Message, nil)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to publish RepeatedXidError event",
			"error", err,
			"node", event.NodeName,
			"xid", xidCode)

		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "xid_publish_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return false, fmt.Errorf("failed to publish RepeatedXidError event: %w", err)
	}

	span.SetAttributes(
		attribute.Bool("health_events_analyzer.event.published", true),
		attribute.String("health_events_analyzer.event.published_rule", "RepeatedXidError"),
	)

	slog.InfoContext(ctx, "Successfully published RepeatedXidError event",
		"node", event.NodeName,
		"xid", xidCode,
		"burstCount", burstCount)

	// NOTE: We do NOT clear history here. The MongoDB pipeline is stateless and
	// queries the DB each time, so multiple XIDs in the same burst can trigger
	// if they each appear in 2+ bursts. Clearing history here would prevent that.
	// History is only cleared when a healthy event is received.

	// Track metrics
	ruleMatchedTotal.WithLabelValues("RepeatedXidError", event.NodeName).Inc()

	if len(event.EntitiesImpacted) > 0 {
		fatalEventsPublishedTotal.WithLabelValues(event.EntitiesImpacted[0].EntityValue).Inc()
	} else {
		fatalEventsPublishedTotal.WithLabelValues("unknown").Inc()
	}

	return true, nil
}

// extractXidDetectorConfig extracts the XID burst detector configuration from the
// RepeatedXidError rule's MongoDB aggregation pipeline stages.
// This ensures the Go-based detector uses the same parameters as configured in the ConfigMap.
func (r *Reconciler) extractXidDetectorConfig() analyzer.XidBurstDetectorConfig {
	// Find the RepeatedXidError rule
	for _, rule := range r.config.HealthEventsAnalyzerRules.Rules {
		if rule.Name == "RepeatedXidError" {
			slog.Info("Found RepeatedXidError rule, parsing pipeline for XID detector config")

			return analyzer.ParseXidConfigFromPipeline(rule.Stage)
		}
	}

	// If no RepeatedXidError rule found, use defaults
	slog.Warn("RepeatedXidError rule not found in config, using default XID detector settings")

	return analyzer.DefaultXidBurstDetectorConfig()
}
