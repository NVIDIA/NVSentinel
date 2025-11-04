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
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/nvidia/nvsentinel/store-client/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/mongo"
)

const (
	maxRetries int           = 5
	delay      time.Duration = 10 * time.Second
)

type CollectionInterface interface {
	Aggregate(ctx context.Context, pipeline interface{}, opts ...*options.AggregateOptions) (*mongo.Cursor, error)
}

type HealthEventsAnalyzerReconcilerConfig struct {
	MongoHealthEventCollectionConfig storewatcher.MongoDBConfig
	TokenConfig                      storewatcher.TokenConfig
	MongoPipeline                    mongo.Pipeline
	HealthEventsAnalyzerRules        *config.TomlConfig
	Publisher                        *publisher.PublisherConfig
	CollectionClient                 CollectionInterface
}

type Reconciler struct {
	config HealthEventsAnalyzerReconcilerConfig
}

func NewReconciler(cfg HealthEventsAnalyzerReconcilerConfig) *Reconciler {
	return &Reconciler{config: cfg}
}

// Start begins the reconciliation process by listening to change stream events
// and processing them accordingly.
func (r *Reconciler) Start(ctx context.Context) error {
	watcher, err := storewatcher.NewChangeStreamWatcher(
		ctx,
		r.config.MongoHealthEventCollectionConfig,
		r.config.TokenConfig,
		r.config.MongoPipeline,
	)
	if err != nil {
		return fmt.Errorf("failed to create change stream watcher: %w", err)
	}
	defer watcher.Close(ctx)

	r.config.CollectionClient, err = storewatcher.GetCollectionClient(ctx, r.config.MongoHealthEventCollectionConfig)
	if err != nil {
		slog.Error(
			"Error initializing healthEventCollection client",
			"config", r.config.MongoHealthEventCollectionConfig,
			"error", err,
		)

		return fmt.Errorf("failed to initialize healthEventCollection client: %w", err)
	}

	watcher.Start(ctx)

	slog.Info("Listening for events on the channel...")

	for event := range watcher.Events() {
		startTime := time.Now()

		document := event["fullDocument"].(bson.M)

		healthEventWithStatus := model.HealthEventWithStatus{}
		if err := storewatcher.UnmarshalFullDocumentFromEvent(
			event,
			&healthEventWithStatus,
		); err != nil {
			slog.Error("Failed to unmarshal event", "error", err)
			totalEventProcessingError.WithLabelValues("unamrshal_doc_error").Inc()

			if err := watcher.MarkProcessed(ctx); err != nil {
				slog.Error("Error updating resume token", "error", err)
			}

			continue
		}

		slog.Debug("Received event", "event", healthEventWithStatus)

		totalEventsReceived.WithLabelValues(healthEventWithStatus.HealthEvent.EntitiesImpacted[0].EntityValue).Inc()

		var err error

		var publishedNewEvent bool

		for i := 1; i <= maxRetries; i++ {
			slog.Debug("Handling event", "attempt", i, "eventID", document["_id"])

			publishedNewEvent, err = r.handleEvent(ctx, &healthEventWithStatus)
			if err == nil {
				totalEventsSuccessfullyProcessed.Inc()

				if publishedNewEvent {
					slog.Info("New fatal event published.")
					fatalEventsPublishedTotal.WithLabelValues(healthEventWithStatus.HealthEvent.EntitiesImpacted[0].EntityValue).Inc()
				} else {
					slog.Info("Fatal event is not published, rule set criteria didn't match.")
				}

				break
			}

			slog.Error("Error in handling the event", "eventID", document["_id"], "error", err)

			totalEventProcessingError.WithLabelValues("handle_event_error").Inc()

			time.Sleep(delay)
		}

		if err != nil {
			slog.Error("Max attempt reached, error in handling the event", "eventID", document["_id"], "error", err)
		}

		duration := time.Since(startTime).Seconds()

		eventHandlingDuration.Observe(duration)
	}

	return nil
}

func (r *Reconciler) handleEvent(ctx context.Context, event *model.HealthEventWithStatus) (bool, error) {
	for _, rule := range r.config.HealthEventsAnalyzerRules.Rules {
		// Check if current event matches any sequence criteria in the rule
		if matchesAnySequenceCriteria(rule, *event) && r.evaluateRule(ctx, rule, *event) {
			slog.Debug("Rule matched for event", "rule", rule.Name, "event", event)

			actionVal, ok := platform_connectors.RecommendedAction_value[rule.RecommendedAction]
			if !ok {
				slog.Warn("Invalid recommended_action in rule; defaulting to NONE",
					"recommended_action", rule.RecommendedAction,
					"rule", rule.Name)

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

	// Create facets for each sequence
	facets := bson.D{}

	for i, seq := range rule.Sequence {
		slog.Debug("Evaluating sequence", "sequence", seq)

		facetName := "sequence_" + strconv.Itoa(i)

		matchCriteria, err := parseSequenceString(seq.Criteria, healthEventWithStatus.HealthEvent)
		if err != nil {
			slog.Error("Failed to parse sequence criteria", "error", err)

			totalEventProcessingError.WithLabelValues("parse_criteria_error").Inc()

			continue
		}

		facets = append(facets, bson.E{
			Key: facetName,
			Value: bson.A{
				bson.D{{Key: "$match", Value: bson.D{
					{Key: "healthevent.generatedtimestamp.seconds", Value: bson.D{
						{Key: "$gte", Value: time.Now().UTC().Add(-timeWindow).Unix()},
					}},
				}}},
				bson.D{{Key: "$match", Value: matchCriteria}},
				bson.D{{Key: "$count", Value: "count"}},
			},
		})
	}

	pipeline := mongo.Pipeline{
		{{Key: "$facet", Value: facets}},
		{{Key: "$project", Value: bson.D{
			{Key: "ruleMatched", Value: bson.D{
				{Key: "$and", Value: func() bson.A {
					conditions := make(bson.A, len(rule.Sequence))
					for i, seq := range rule.Sequence {
						facetName := "sequence_" + strconv.Itoa(i)
						conditions[i] = bson.D{
							{Key: "$gte", Value: bson.A{
								bson.D{{Key: "$arrayElemAt", Value: bson.A{"$" + facetName + ".count", 0}}},
								seq.ErrorCount,
							}},
						}
					}
					return conditions
				}()},
			}},
		}}},
	}

	var result []bson.M

	cursor, err := r.config.CollectionClient.Aggregate(ctx, pipeline)
	if err != nil {
		slog.Error("Failed to execute aggregation pipeline", "error", err)
		totalEventProcessingError.WithLabelValues("execute_pipeline_error").Inc()

		return false
	}
	defer cursor.Close(ctx)

	if err = cursor.All(ctx, &result); err != nil {
		slog.Error("Failed to decode cursor", "error", err)
		totalEventProcessingError.WithLabelValues("decode_cursor_error").Inc()

		return false
	}

	if len(result) > 0 {
		// Check if all criteria are met
		if matched, ok := result[0]["ruleMatched"].(bool); ok && matched {
			slog.Debug("All sequence conditions met for rule", "rule", rule.Name)
			return true
		}
	}

	return false
}

// parseSequenceString converts a criteria string into a BSON document for MongoDB queries
func parseSequenceString(criteria map[string]interface{}, event *platform_connectors.HealthEvent) (bson.D, error) {
	doc := bson.D{}

	for key, value := range criteria {
		strValue, ok := value.(string)
		if ok && len(strValue) > 5 && strValue[:5] == "this." {
			fieldPath := strValue[5:] // Skip "this."
			doc = append(doc, bson.E{Key: key, Value: getValueFromPath(fieldPath, event)})
		} else {
			doc = append(doc, bson.E{Key: key, Value: value})
		}
	}

	return doc, nil
}
