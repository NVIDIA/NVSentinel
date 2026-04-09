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
	"time"

	multierror "github.com/hashicorp/go-multierror"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

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

type HealthEventsAnalyzerReconcilerConfig struct {
	DataStoreConfig           *datastore.DataStoreConfig
	Pipeline                  interface{}
	HealthEventsAnalyzerRules *config.TomlConfig
	Publisher                 *publisher.PublisherConfig
}

type Reconciler struct {
	config         HealthEventsAnalyzerReconcilerConfig
	datastore      datastore.DataStore
	databaseClient client.DatabaseClient // MongoDB-specific client for aggregation
	eventProcessor client.EventProcessor
	xidDetector    *analyzer.XidBurstDetector // PostgreSQL-specific XID burst detection
	useXidDetector bool                       // True if using PostgreSQL
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
			ctx context.Context, clientName string, pipeline interface{},
		) (datastore.ChangeStreamWatcher, error)
	})
	if !ok {
		return fmt.Errorf("datastore does not support required operations (GetDatabaseClient and CreateChangeStreamWatcher)")
	}

	r.databaseClient = datastoreAdapter.GetDatabaseClient()

	changeStreamWatcher, err := datastoreAdapter.CreateChangeStreamWatcher(
		ctx, "health-events-analyzer", r.config.Pipeline)
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

	// Create and configure the unified EventProcessor
	// Note: EventProcessor no longer retries internally to prevent blocking the event stream
	// Failed events will be retried on next pod restart (via resume token)
	processorConfig := client.EventProcessorConfig{
		EnableMetrics:        true,
		MetricsLabels:        map[string]string{"module": "health-events-analyzer"},
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

	if publishedNewEvent {
		slog.InfoContext(ctx, "New fatal event published.")
		// Only track entity-specific metrics if EntitiesImpacted is not empty
		if len(event.HealthEvent.EntitiesImpacted) > 0 {
			fatalEventsPublishedTotal.WithLabelValues(event.HealthEvent.EntitiesImpacted[0].EntityValue).Inc()
		} else {
			slog.WarnContext(ctx, "Fatal event published but EntitiesImpacted is empty, using 'unknown' for metrics")
			fatalEventsPublishedTotal.WithLabelValues("unknown").Inc()
		}
	} else {
		slog.InfoContext(ctx, "Fatal event is not published, rule set criteria didn't match.")
	}

	// Track processing duration
	duration := time.Since(startTime).Seconds()
	eventHandlingDuration.Observe(duration)

	return nil
}

func (r *Reconciler) handleEvent(ctx context.Context, event *datamodels.HealthEventWithStatus) (bool, error) {
	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.handle_event")
	defer span.End()

	var multiErr *multierror.Error

	publishedNewEvent := false

	// Handle XID detector operations (clear on healthy, detect bursts on unhealthy)
	published, err := r.handleXidDetector(ctx, event)
	if err != nil {
		multiErr = multierror.Append(multiErr, err)
	}

	if published {
		publishedNewEvent = true
	}

	// Process regular rules
	for _, rule := range r.config.HealthEventsAnalyzerRules.Rules {
		if !rule.EvaluateRule {
			slog.InfoContext(ctx, "Skipping rule evaluation", "rule_name", rule.Name)
			continue
		}

		published, err := r.processRule(ctx, rule, event)
		if err != nil {
			multiErr = multierror.Append(multiErr, err)
			span.AddEvent("rule_evaluation_error", trace.WithAttributes(
				attribute.String("health_events_analyzer.error.type", "rule_evaluation_error"),
				attribute.String("health_events_analyzer.error.message", err.Error()),
			))

			continue
		}

		if published {
			publishedNewEvent = true
		}
	}

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

// handleXidDetector handles XID burst detection and history clearing
func (r *Reconciler) handleXidDetector(ctx context.Context, event *datamodels.HealthEventWithStatus) (bool, error) {
	if !r.useXidDetector {
		return false, nil
	}

	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.handle_xid_detector")
	defer span.End()

	// Clear XID burst history when a healthy GPU event is received
	if r.shouldClearXidHistory(event.HealthEvent) {
		r.xidDetector.ClearNodeHistory(event.HealthEvent.NodeName)
		span.SetAttributes(attribute.Bool("health_events_analyzer.xid.history_cleared", true))
		slog.InfoContext(ctx, "Cleared XID burst history for node due to healthy GPU event",
			"node", event.HealthEvent.NodeName)
	}

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

	err = r.publishMatchedEvent(ctx, rule, event)
	if err != nil {
		slog.ErrorContext(ctx, "Error in publishing the matched event", "error", err)
		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "publish_matched_event_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return false, fmt.Errorf("error in publishing the matched event: %w", err)
	}

	return true, nil
}

// publishMatchedEvent publishes an event when a rule matches
func (r *Reconciler) publishMatchedEvent(ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	event *datamodels.HealthEventWithStatus) error {
	ctx, span := tracing.StartSpan(ctx, "health_events_analyzer.publish_matched_event")
	defer span.End()

	ruleMatchedTotal.WithLabelValues(rule.Name, event.HealthEvent.NodeName).Inc()

	actionVal := r.getRecommendedActionValue(rule.RecommendedAction, rule.Name)

	err := r.config.Publisher.Publish(ctx, event.HealthEvent, protos.RecommendedAction(actionVal),
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

	// Build aggregation pipeline from stages
	pipelineStages, err := r.getPipelineStages(rule, healthEventWithStatus)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to build pipeline stages", "error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "build_pipeline_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)

		return false, fmt.Errorf("failed to build pipeline stages: %w", err)
	}

	var result []map[string]interface{}

	slog.DebugContext(ctx, "Executing aggregation pipeline",
		"rule_name", rule.Name, "pipeline_stages_count", len(pipelineStages))

	cursor, err := r.databaseClient.Aggregate(ctx, pipelineStages)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to execute aggregation pipeline", "error", err, "rule_name", rule.Name)
		totalEventProcessingError.WithLabelValues("execute_pipeline_error").Inc()

		span.SetAttributes(
			attribute.String("health_events_analyzer.error.type", "execute_pipeline_error"),
			attribute.String("health_events_analyzer.error.message", err.Error()),
		)
		tracing.RecordError(span, err)

		return false, fmt.Errorf("failed to execute aggregation pipeline: %w", err)
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

		return false, fmt.Errorf("failed to decode cursor: %w", err)
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
) ([]map[string]interface{}, error) {
	// CRITICAL: Always start with agent filter to exclude events from health-events-analyzer itself
	// This prevents the analyzer from matching its own generated events, which would cause
	// infinite loops and incorrect rule evaluations
	pipeline := []map[string]interface{}{
		{
			"$match": map[string]interface{}{
				"healthevent.agent": map[string]interface{}{"$ne": "health-events-analyzer"},
				"healthevent.processingstrategy": map[string]interface{}{
					"$eq": int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION),
				},
			},
		},
	}

	for i, stageStr := range rule.Stage {
		// Parse the stage and resolve "this." references
		stageMap, err := parser.ParseSequenceStage(stageStr, healthEventWithStatus)
		if err != nil {
			slog.Error("Failed to parse stage", "stage_index", i, "error", err, "stage_string", stageStr)
			totalEventProcessingError.WithLabelValues("parse_stage_error").Inc()

			return nil, fmt.Errorf("failed to parse stage %d: %w", i, err)
		}

		slog.Debug("Parsed aggregation stage", "rule_name", rule.Name, "stage_index", i)

		pipeline = append(pipeline, stageMap)
	}

	return pipeline, nil
}

// shouldProcessXidEvent checks if an event should be processed by the XID burst detector
func (r *Reconciler) shouldProcessXidEvent(event *protos.HealthEvent) bool {
	// Only process GPU XID errors (unhealthy GPU events with error codes)
	return event != nil &&
		event.ComponentClass == "GPU" &&
		!event.IsHealthy &&
		len(event.ErrorCode) > 0 &&
		event.Agent != "health-events-analyzer" // Don't process our own events
}

// shouldClearXidHistory checks if a healthy GPU event should clear the XID burst history
// This ensures that when a GPU is healthy again, we don't keep triggering RepeatedXidError
// based on stale XID history from before the recovery
func (r *Reconciler) shouldClearXidHistory(event *protos.HealthEvent) bool {
	return event != nil &&
		event.ComponentClass == "GPU" &&
		event.IsHealthy &&
		event.Agent != "health-events-analyzer" // Don't process our own events
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
