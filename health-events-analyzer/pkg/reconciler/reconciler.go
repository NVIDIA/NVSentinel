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
	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/parser"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/helper"
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
	databaseClient client.DatabaseClient
	eventProcessor client.EventProcessor
}

func NewReconciler(cfg HealthEventsAnalyzerReconcilerConfig) *Reconciler {
	return &Reconciler{
		config: cfg,
	}
}

// Start begins the reconciliation process by listening to change stream events
// and processing them accordingly.
func (r *Reconciler) Start(ctx context.Context) error {
	// Use standardized datastore client initialization
	bundle, err := helper.NewDatastoreClientFromConfig(
		ctx, "health-events-analyzer", *r.config.DataStoreConfig, r.config.Pipeline,
	)
	if err != nil {
		return fmt.Errorf("failed to create datastore client bundle: %w", err)
	}
	defer bundle.Close(ctx)

	r.databaseClient = bundle.DatabaseClient

	// Create and configure the unified EventProcessor
	// Note: EventProcessor no longer retries internally to prevent blocking the event stream
	// Failed events will be retried on next pod restart (via resume token)
	processorConfig := client.EventProcessorConfig{
		EnableMetrics:        true,
		MetricsLabels:        map[string]string{"module": "health-events-analyzer"},
		MarkProcessedOnError: false, // IMPORTANT: Don't mark failed events as processed
	}

	r.eventProcessor = client.NewEventProcessor(bundle.ChangeStreamWatcher, bundle.DatabaseClient, processorConfig)

	// Set the event handler for processing health events
	r.eventProcessor.SetEventHandler(client.EventHandlerFunc(r.processHealthEvent))

	slog.Info("Starting health events analyzer with unified event processor...")

	// Start the event processor
	return r.eventProcessor.Start(ctx)
}

// processHealthEvent handles individual health events and implements the EventHandler interface
func (r *Reconciler) processHealthEvent(ctx context.Context, event *datamodels.HealthEventWithStatus) error {
	startTime := time.Now()

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
		slog.Error("Failed to process health event", "error", err, "nodeName", labelValue)

		return fmt.Errorf("failed to handle event: %w", err)
	}

	// Track success metrics
	totalEventsSuccessfullyProcessed.Inc()

	if publishedNewEvent {
		slog.Info("New fatal event published.")
		fatalEventsPublishedTotal.WithLabelValues(event.HealthEvent.EntitiesImpacted[0].EntityValue).Inc()
	} else {
		slog.Info("Fatal event is not published, rule set criteria didn't match.")
	}

	// Track processing duration
	duration := time.Since(startTime).Seconds()
	eventHandlingDuration.Observe(duration)

	return nil
}

func (r *Reconciler) handleEvent(ctx context.Context, event *datamodels.HealthEventWithStatus) (bool, error) {
	var multiErr *multierror.Error

	publishedNewEvent := false

	for _, rule := range r.config.HealthEventsAnalyzerRules.Rules {
		published, err := r.processRule(ctx, rule, event)
		if err != nil {
			multiErr = multierror.Append(multiErr, err)
			continue
		}

		if published {
			publishedNewEvent = true
		}
	}

	if multiErr.ErrorOrNil() != nil {
		slog.Error("Error in handling the event", "error", multiErr)
		return publishedNewEvent, fmt.Errorf("error in handling the event: %w", multiErr)
	}

	return publishedNewEvent, nil
}

// processRule handles the processing of a single rule against an event
func (r *Reconciler) processRule(ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	event *datamodels.HealthEventWithStatus) (bool, error) {
	// Validate all sequences from DB docs
	matchedSequences, err := r.validateAllSequenceCriteria(ctx, rule, *event)
	if err != nil {
		slog.Error("Error in validating all sequence criteria", "error", err)
		return false, fmt.Errorf("error in validating all sequence criteria: %w", err)
	}

	if !matchedSequences {
		return false, nil
	}

	err = r.publishMatchedEvent(ctx, rule, event)
	if err != nil {
		slog.Error("Error in publishing the matched event", "error", err)
		return false, fmt.Errorf("error in publishing the matched event: %w", err)
	}

	return true, nil
}

// publishMatchedEvent publishes an event when a rule matches
func (r *Reconciler) publishMatchedEvent(ctx context.Context,
	rule config.HealthEventsAnalyzerRule,
	event *datamodels.HealthEventWithStatus) error {
	slog.Info("Rule matched for event", "rule_name", rule.Name, "event", event)
	ruleMatchedTotal.WithLabelValues(rule.Name, event.HealthEvent.NodeName).Inc()

	actionVal := r.getRecommendedActionValue(rule.RecommendedAction, rule.Name)

	// No need to clone here - Publisher.Publish already clones the event
	// The EventProcessor creates a fresh stack variable for each event, so no mutation risk
	err := r.config.Publisher.Publish(ctx, event.HealthEvent, protos.RecommendedAction(actionVal), rule.Name)
	if err != nil {
		slog.Error("Error in publishing the new fatal event", "error", err)
		return fmt.Errorf("error in publishing the new fatal event: %w", err)
	}

	slog.Info("New event successfully published for matching rule", "rule_name", rule.Name)

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
	slog.Info("→ Evaluating rule for event",
		"rule_name", rule.Name,
		"node", healthEventWithStatus.HealthEvent.NodeName,
		"error_code", healthEventWithStatus.HealthEvent.ErrorCode,
		"agent", healthEventWithStatus.HealthEvent.Agent)

	// Build aggregation pipeline from stages
	pipelineStages, err := r.getPipelineStages(rule, healthEventWithStatus)
	if err != nil {
		slog.Error("Failed to build pipeline stages", "error", err)
		totalEventProcessingError.WithLabelValues("build_pipeline_error").Inc()

		return false, fmt.Errorf("failed to build pipeline stages: %w", err)
	}

	var result []map[string]interface{}

	// Execute aggregation using store-client abstraction
	cursor, err := r.databaseClient.Aggregate(ctx, pipelineStages)
	if err != nil {
		slog.Error("Failed to execute aggregation pipeline", "error", err, "rule_name", rule.Name)
		totalEventProcessingError.WithLabelValues("execute_pipeline_error").Inc()

		return false, fmt.Errorf("failed to execute aggregation pipeline: %w", err)
	}

	defer cursor.Close(ctx)

	if err = cursor.All(ctx, &result); err != nil {
		slog.Error("Failed to decode cursor", "error", err, "rule_name", rule.Name)
		totalEventProcessingError.WithLabelValues("decode_cursor_error").Inc()

		return false, fmt.Errorf("failed to decode cursor: %w", err)
	}

	// Check if we have results (rule matched)
	if len(result) > 0 {
		// Check for explicit ruleMatched field (used in tests and by SequenceFacet pipelines)
		if matched, ok := result[0]["ruleMatched"].(bool); ok {
			if matched {
				slog.Info("✓ Rule matched via ruleMatched field",
					"rule_name", rule.Name,
					"node", healthEventWithStatus.HealthEvent.NodeName)

				return true, nil
			}

			slog.Info("✗ Rule did not match (ruleMatched=false)",
				"rule_name", rule.Name,
				"node", healthEventWithStatus.HealthEvent.NodeName,
				"result", result[0])

			return false, nil
		}

		// For Stage-based pipelines, presence of results indicates a match
		slog.Info("✓ Rule matched via results existence",
			"rule_name", rule.Name,
			"node", healthEventWithStatus.HealthEvent.NodeName,
			"result_count", len(result))

		return true, nil
	}

	slog.Info("✗ Rule did not match (no results)",
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

		pipeline = append(pipeline, stageMap)
	}

	return pipeline, nil
}
