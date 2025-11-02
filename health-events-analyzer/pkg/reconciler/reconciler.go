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
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	platform_connectors "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/client"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/helper"
)

const (
	maxRetries int           = 5
	delay      time.Duration = 10 * time.Second
)

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
	return &Reconciler{config: cfg}
}

// Start begins the reconciliation process by listening to change stream events
// and processing them accordingly.
func (r *Reconciler) Start(ctx context.Context) error {
	// Use standardized datastore client initialization
	bundle, err := helper.NewDatastoreClientFromConfig(ctx, "health-events-analyzer", *r.config.DataStoreConfig, r.config.Pipeline)
	if err != nil {
		return fmt.Errorf("failed to create datastore client bundle: %w", err)
	}
	defer bundle.Close(ctx)

	r.databaseClient = bundle.DatabaseClient

	// Create and configure the unified EventProcessor
	processorConfig := client.EventProcessorConfig{
		MaxRetries:    maxRetries,
		RetryDelay:    delay,
		EnableMetrics: true,
		MetricsLabels: map[string]string{"module": "health-events-analyzer"},
	}

	r.eventProcessor = client.NewEventProcessor(bundle.ChangeStreamWatcher, bundle.DatabaseClient, processorConfig)

	// Set the event handler for processing health events
	r.eventProcessor.SetEventHandler(client.EventHandlerFunc(r.processHealthEvent))

	slog.Info("Starting health events analyzer with unified event processor...")

	// Start the event processor
	return r.eventProcessor.Start(ctx)
}

// processHealthEvent handles individual health events and implements the EventHandler interface
func (r *Reconciler) processHealthEvent(ctx context.Context, event *model.HealthEventWithStatus) error {
	startTime := time.Now()

	slog.Debug("Received event", "event", event)

	// Track event reception metrics
	totalEventsReceived.WithLabelValues(event.HealthEvent.EntitiesImpacted[0].EntityValue).Inc()

	// Process the event using existing business logic
	publishedNewEvent, err := r.handleEvent(ctx, event)
	if err != nil {
		// Log error but let the EventProcessor handle retry logic
		totalEventProcessingError.WithLabelValues("handle_event_error").Inc()
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


func (r *Reconciler) handleEvent(ctx context.Context, event *model.HealthEventWithStatus) (bool, error) {
	for _, rule := range r.config.HealthEventsAnalyzerRules.Rules {
		// Check if current event matches any sequence criteria in the rule
		if matchesAnySequenceCriteria(rule, *event) && r.evaluateRule(ctx, rule, *event) {
			slog.Debug("Rule matched for event", "rule", rule.Name, "event", event)

			actionVal, ok := platform_connectors.RecommendedAction_value[rule.RecommendedAction]
			if !ok {
				slog.Warn("Invalid recommended_action '%s' in rule '%s'; defaulting to NONE", rule.RecommendedAction, rule.Name)

				actionVal = int32(platform_connectors.RecommendedAction_NONE)
			}

			err := r.config.Publisher.Publish(ctx, event.HealthEvent, platform_connectors.RecommendedAction(actionVal))
			if err != nil {
				slog.Error("Error in publishing the new fatal event", "error", err)
				publisher.FatalEventPublishingError.WithLabelValues("event_publishing_to_UDS_error").Inc()

				return false, fmt.Errorf("failed to publish fatal event: %w", err)
			}

			return true, nil
		}

		slog.Debug("Rule didn't meet criteria", "rule", rule.Name)
	}

	slog.Info("No rule matched for event", "event", event)

	return false, nil
}

// matchesAnySequenceCriteria checks if the current event matches any sequence criteria in the rule
func matchesAnySequenceCriteria(rule config.HealthEventsAnalyzerRule,
	healthEventWithStatus model.HealthEventWithStatus) bool {
	for _, seq := range rule.Sequence {
		if matchesSequenceCriteria(seq.Criteria, healthEventWithStatus.HealthEvent) {
			return true
		}
	}

	return false
}

// matchesSequenceCriteria checks if the current event matches a specific sequence criteria
func matchesSequenceCriteria(criteria map[string]interface{}, event *platform_connectors.HealthEvent) bool {
	for key, value := range criteria {
		strValue, ok := value.(string)
		if ok && len(strValue) > 5 && strValue[:5] == "this." {
			continue
		}

		actualValue := getValueFromPath(key, event)
		if actualValue == nil || actualValue != value {
			return false
		}
	}

	return true
}

// getValueFromPath extracts a value from the event using a dot-notation path
//
//nolint:cyclop, gocognit // todo
func getValueFromPath(path string, event *platform_connectors.HealthEvent) interface{} {
	parts := strings.Split(path, ".")

	if len(parts) > 0 && parts[0] == "healthevent" {
		parts = parts[1:]
	}

	if len(parts) == 0 {
		return nil
	}

	rootField := strings.ToLower(parts[0])

	if len(parts) == 1 {
		val := reflect.ValueOf(event).Elem()

		// Find the field by name case-insensitive
		for i := 0; i < val.NumField(); i++ {
			field := val.Type().Field(i)
			if strings.EqualFold(field.Name, rootField) {
				return val.Field(i).Interface()
			}
		}
	}

	if strings.EqualFold(rootField, "errorcode") && len(parts) > 1 {
		if idx, err := strconv.Atoi(parts[1]); err == nil && idx < len(event.ErrorCode) {
			return event.ErrorCode[idx]
		}

		return nil
	}

	if strings.EqualFold(rootField, "entitiesimpacted") && len(parts) > 2 {
		if idx, err := strconv.Atoi(parts[1]); err == nil && idx < len(event.EntitiesImpacted) {
			entity := event.EntitiesImpacted[idx]
			subField := strings.ToLower(parts[2])

			entityVal := reflect.ValueOf(entity).Elem()
			for i := 0; i < entityVal.NumField(); i++ {
				field := entityVal.Type().Field(i)
				if strings.EqualFold(field.Name, subField) {
					return entityVal.Field(i).Interface()
				}
			}
		}

		return nil
	}

	// Handle metadata map
	if strings.EqualFold(rootField, "metadata") && len(parts) > 1 {
		metadataKey := parts[1]
		if value, exists := event.Metadata[metadataKey]; exists {
			return value
		}
	}

	if strings.EqualFold(rootField, "generatedtimestamp") && len(parts) > 1 && event.GeneratedTimestamp != nil {
		subField := strings.ToLower(parts[1])

		timestampVal := reflect.ValueOf(event.GeneratedTimestamp).Elem()
		for i := 0; i < timestampVal.NumField(); i++ {
			field := timestampVal.Type().Field(i)
			if strings.EqualFold(field.Name, subField) {
				return timestampVal.Field(i).Interface()
			}
		}
	}

	return nil
}

func (r *Reconciler) evaluateRule(ctx context.Context, rule config.HealthEventsAnalyzerRule,
	healthEventWithStatus model.HealthEventWithStatus) bool {
	slog.Debug("Evaluating rule for event", "rule", rule.Name, "event", healthEventWithStatus)

	timeWindow, err := time.ParseDuration(rule.TimeWindow)
	if err != nil {
		slog.Error("Failed to parse time window", "error", err)
		totalEventProcessingError.WithLabelValues("parse_time_window_error").Inc()

		return false
	}

	// Check each sequence condition individually
	for i, seq := range rule.Sequence {
		slog.Debug("Evaluating sequence", "sequence", seq)

		// Create filter criteria
		filter := make(map[string]interface{})

		// Time window filter using database-agnostic filter building
		timeThreshold := time.Now().UTC().Add(-timeWindow).Unix()
		timeFilter := client.NewFilterBuilder().Gte("healthevent.generatedtimestamp.seconds", timeThreshold).Build()

		// Merge time filter into main filter
		for k, v := range timeFilter.(map[string]interface{}) {
			filter[k] = v
		}

		// Add sequence-specific criteria
		for key, value := range seq.Criteria {
			strValue, ok := value.(string)
			if ok && len(strValue) > 5 && strValue[:5] == "this." {
				fieldPath := strValue[5:] // Skip "this."
				filter[key] = getValueFromPath(fieldPath, healthEventWithStatus.HealthEvent)
			} else {
				filter[key] = value
			}
		}

		// Count documents matching this sequence
		count, err := r.databaseClient.CountDocuments(ctx, filter, nil)
		if err != nil {
			slog.Error("Failed to count documents for sequence", "sequence", i, "error", err)
			totalEventProcessingError.WithLabelValues("count_documents_error").Inc()

			return false
		}

		// Check if count meets the required threshold
		if count < int64(seq.ErrorCount) {
			slog.Debug("Sequence condition not met", "sequence", i, "count", count, "required", seq.ErrorCount)
			return false
		}

		slog.Debug("Sequence condition met", "sequence", i, "count", count, "required", seq.ErrorCount)
	}

	slog.Debug("All sequence conditions met for rule", "rule", rule.Name)

	return true
}
