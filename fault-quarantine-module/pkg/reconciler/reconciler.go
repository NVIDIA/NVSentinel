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
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/evaluator"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/nodeinfo"
	"github.com/nvidia/nvsentinel/store-client-sdk/pkg/storewatcher"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type CircuitBreakerConfig struct {
	Namespace  string
	Name       string
	Percentage int
	Duration   time.Duration
}

type ReconcilerConfig struct {
	TomlConfig                            config.TomlConfig
	MongoHealthEventCollectionConfig      storewatcher.MongoDBConfig
	TokenConfig                           storewatcher.TokenConfig
	MongoPipeline                         mongo.Pipeline
	K8sClient                             K8sClientInterface
	DryRun                                bool
	CircuitBreakerEnabled                 bool
	UnprocessedEventsMetricUpdateInterval time.Duration
	CircuitBreaker                        CircuitBreakerConfig
}

type rulesetsConfig struct {
	TaintConfigMap     map[string]*config.Taint
	CordonConfigMap    map[string]bool
	RuleSetPriorityMap map[string]int
}

type Reconciler struct {
	config            ReconcilerConfig
	healthEventBuffer *common.HealthEventBuffer
	nodeInfo          *nodeinfo.NodeInfo
	// workSignal acts as a semaphore to wake up the reconcile loop
	workSignal chan struct{}
	// nodeAnnotationsCache caches node annotations to avoid repeated K8s API calls
	nodeAnnotationsCache sync.Map // map[string]map[string]string
	// cacheMutex protects cache operations during refresh to ensure consistency
	cacheMutex            sync.RWMutex
	lastProcessedObjectID atomic.Value // stores primitive.ObjectID
	cb                    breaker.CircuitBreaker
}

var (
	// Label keys
	cordonedByLabelKey        string
	cordonedReasonLabelKey    string
	cordonedTimestampLabelKey string

	uncordonedByLabelKey        string
	uncordonedReasonLabelkey    string
	uncordonedTimestampLabelKey string
)

func NewReconciler(ctx context.Context, cfg ReconcilerConfig, workSignal chan struct{}) *Reconciler {
	r := &Reconciler{
		config:            cfg,
		healthEventBuffer: common.NewHealthEventBuffer(ctx),
		nodeInfo:          nodeinfo.NewNodeInfo(workSignal),
		workSignal:        workSignal, // Store the signal channel
	}

	if cfg.CircuitBreakerEnabled {
		slog.Info("Initializing circuit breaker with config map %s in namespace %s",
			cfg.CircuitBreaker.Name, cfg.CircuitBreaker.Namespace)

		cb, err := breaker.NewSlidingWindowBreaker(ctx, breaker.Config{
			Window:         cfg.CircuitBreaker.Duration,
			TripPercentage: float64(cfg.CircuitBreaker.Percentage),
			GetTotalNodes:  cfg.K8sClient.GetTotalGpuNodes,
			EnsureConfigMap: func(c context.Context, initial breaker.State) error {
				return cfg.K8sClient.EnsureCircuitBreakerConfigMap(c,
					cfg.CircuitBreaker.Name, cfg.CircuitBreaker.Namespace, string(initial))
			},
			ReadStateFn: func(c context.Context) (breaker.State, error) {
				val, err := cfg.K8sClient.ReadCircuitBreakerState(c, cfg.CircuitBreaker.Name, cfg.CircuitBreaker.Namespace)
				if err != nil {
					slog.Error("Error reading circuit breaker state from config map",
						"name", cfg.CircuitBreaker.Name, "namespace", cfg.CircuitBreaker.Namespace, "error", err)
					return breaker.State(""), fmt.Errorf("failed to read circuit breaker state: %w", err)
				}
				return breaker.State(val), nil
			},
			WriteStateFn: func(c context.Context, s breaker.State) error {
				return cfg.K8sClient.WriteCircuitBreakerState(c, cfg.CircuitBreaker.Name, cfg.CircuitBreaker.Namespace, string(s))
			},
		})
		if err != nil {
			slog.Error("Failed to initialize circuit breaker", "error", err)
		}

		r.cb = cb
	} else {
		slog.Info("Circuit breaker is disabled, skipping initialization")

		r.cb = nil
	}

	return r
}

func (r *Reconciler) SetLabelKeys(labelKeyPrefix string) {
	cordonedByLabelKey = labelKeyPrefix + "cordon-by"
	cordonedReasonLabelKey = labelKeyPrefix + "cordon-reason"
	cordonedTimestampLabelKey = labelKeyPrefix + "cordon-timestamp"

	uncordonedByLabelKey = labelKeyPrefix + "uncordon-by"
	uncordonedReasonLabelkey = labelKeyPrefix + "uncordon-reason"
	uncordonedTimestampLabelKey = labelKeyPrefix + "uncordon-timestamp"
}

// nolint: cyclop, gocognit //fix this as part of NGCC-21793
func (r *Reconciler) Start(ctx context.Context) error {
	nodeInformer, err := informer.NewNodeInformer(r.config.K8sClient.GetK8sClient(),
		30*time.Minute, r.workSignal, r.nodeInfo)
	if err != nil {
		return fmt.Errorf("failed to create NodeInformer: %w", err)
	}

	// Set the callback to decrement the metric when a quarantined node with annotations is deleted
	nodeInformer.SetOnQuarantinedNodeDeletedCallback(func(nodeName string) {
		currentQuarantinedNodes.WithLabelValues(nodeName).Dec()
		slog.Info("Decremented currentQuarantinedNodes metric for deleted quarantined node", "node", nodeName)
	})

	// Set the callback to update the annotations cache when node annotations change
	nodeInformer.SetOnNodeAnnotationsChangedCallback(r.handleNodeAnnotationChange)

	// Set the callback to handle manual uncordon of quarantined nodes
	nodeInformer.SetOnManualUncordonCallback(r.handleManualUncordon)

	if fqClient, ok := r.config.K8sClient.(*FaultQuarantineClient); ok {
		fqClient.SetNodeInformer(nodeInformer)
	}

	ruleSetEvals, err := evaluator.InitializeRuleSetEvaluators(r.config.TomlConfig.RuleSets,
		r.config.K8sClient.GetK8sClient(), nodeInformer)
	if err != nil {
		return fmt.Errorf("failed to initialize all rule set evaluators: %w", err)
	}

	r.SetLabelKeys(r.config.TomlConfig.LabelPrefix)

	taintConfigMap := make(map[string]*config.Taint)
	cordonConfigMap := make(map[string]bool)
	ruleSetPriorityMap := make(map[string]int)

	// map ruleset name to taint and cordon configs
	for _, ruleSet := range r.config.TomlConfig.RuleSets {
		if ruleSet.Taint.Key != "" {
			taintConfigMap[ruleSet.Name] = &ruleSet.Taint
		}

		if ruleSet.Cordon.ShouldCordon {
			cordonConfigMap[ruleSet.Name] = true
		}

		if ruleSet.Priority > 0 {
			ruleSetPriorityMap[ruleSet.Name] = ruleSet.Priority
		}
	}

	rulesetsConfig := rulesetsConfig{
		TaintConfigMap:     taintConfigMap,
		CordonConfigMap:    cordonConfigMap,
		RuleSetPriorityMap: ruleSetPriorityMap,
	}

	watcher, err := storewatcher.NewChangeStreamWatcher(
		ctx,
		r.config.MongoHealthEventCollectionConfig,
		r.config.TokenConfig,
		r.config.MongoPipeline,
	)
	if err != nil {
		return fmt.Errorf("failed to create MongoDB change stream watcher: %w", err)
	}
	defer watcher.Close(ctx)

	healthEventCollection, err := storewatcher.GetCollectionClient(ctx, r.config.MongoHealthEventCollectionConfig)
	if err != nil {
		slog.Error(
			"Error initializing healthEventCollection client",
			"config", r.config.MongoHealthEventCollectionConfig,
			"error", err,
		)

		return fmt.Errorf("failed to get health event collection client: %w", err)
	}

	err = r.nodeInfo.BuildQuarantinedNodesMap(r.config.K8sClient.GetK8sClient())
	if err != nil {
		return fmt.Errorf("error fetching quarantined nodes: %w", err)
	} else {
		quarantinedNodesMap := r.nodeInfo.GetQuarantinedNodesCopy()

		for nodeName := range quarantinedNodesMap {
			currentQuarantinedNodes.WithLabelValues(nodeName).Inc()
		}

		slog.Info("Initial quarantinedNodesMap", "nodes", quarantinedNodesMap, "count", len(quarantinedNodesMap))
	}

	err = nodeInformer.Run(ctx.Done())
	if err != nil {
		return fmt.Errorf("failed to run NodeInformer: %w", err)
	}

	// Wait for NodeInformer cache to sync before processing any events
	slog.Info("Waiting for NodeInformer cache to sync before starting event processing...")

	for !nodeInformer.HasSynced() {
		select {
		case <-ctx.Done():
			slog.Warn("Context cancelled while waiting for node informer sync")
			return ctx.Err()
		case <-time.After(5 * time.Second): // Check periodically
			slog.Info("NodeInformer cache is not synced yet, waiting for 5 seconds")
		}
	}

	// Build initial node annotations cache
	if err := r.buildNodeAnnotationsCache(ctx); err != nil {
		// Continue anyway, individual API calls will be made as fallback
		return fmt.Errorf("failed to build initial node annotations cache: %w", err)
	}

	// If breaker is enabled and already tripped at startup, halt until restart/manual close
	if r.config.CircuitBreakerEnabled {
		if tripped, err := r.cb.IsTripped(ctx); err != nil {
			slog.Error("Error checking if circuit breaker is tripped", "error", err)
			<-ctx.Done()

			return fmt.Errorf("error checking if circuit breaker is tripped: %w", err)
		} else if tripped {
			slog.Error("Fault Quarantine circuit breaker is TRIPPED. Halting event dequeuing indefinitely.")
			<-ctx.Done()

			return fmt.Errorf("circuit breaker is tripped at startup")
		}
	}

	watcher.Start(ctx)

	slog.Info("Listening for events on the channel...")

	go func() {
		r.watchEvents(watcher)
		slog.Info("MongoDB event watcher stopped (context cancelled or connection closed)")
	}()

	// Start a goroutine to periodically update the unprocessed events metric
	go r.updateUnprocessedEventsMetric(ctx, watcher)

	// Process events in the main goroutine
	for {
		select {
		case <-ctx.Done():
			slog.Info("Context canceled. Exiting fault-quarantine event consumer.")
			return nil
		case <-r.workSignal: // Wait for a signal (semaphore acquired)
			// Only check circuit breaker if it's enabled
			if r.config.CircuitBreakerEnabled {
				if tripped, err := r.cb.IsTripped(ctx); err != nil {
					slog.Error("Error checking if circuit breaker is tripped", "error", err)
					return fmt.Errorf("error checking if circuit breaker is tripped: %w", err)
				} else if tripped {
					slog.Error("Circuit breaker TRIPPED. Halting event processing.")
					return fmt.Errorf("circuit breaker is tripped")
				}
			}
			// Get current queue length
			healthEventBufferLength := r.healthEventBuffer.Length()
			if healthEventBufferLength == 0 {
				slog.Debug("No events to process, skipping")
				continue
			}

			slog.Info("Processing batch of events", "count", healthEventBufferLength)

			// Process up to the current queue length
			for healthEventIndex := 0; healthEventIndex < healthEventBufferLength; {
				slog.Debug("Processing health event at index", "index", healthEventIndex)

				startTime := time.Now()
				currentEventInfo, _ := r.healthEventBuffer.Get(healthEventIndex)

				if currentEventInfo == nil {
					break
				}

				healthEventWithStatus := currentEventInfo.HealthEventWithStatus
				eventBson := currentEventInfo.EventBson

				// Check if event was already processed
				if healthEventIndex == 0 && currentEventInfo.HasProcessed {
					err := r.healthEventBuffer.RemoveAt(healthEventIndex)
					if err != nil {
						slog.Error("Error removing event",
							"checkName", healthEventWithStatus.HealthEvent.CheckName,
							"error", err)

						continue
					}

					if err := watcher.MarkProcessed(ctx); err != nil {
						processingErrors.WithLabelValues("mark_processed_error").Inc()

						slog.Error("Error updating resume token", "error", err)
					} else {
						slog.Info("Successfully marked event as processed", "node", healthEventWithStatus.HealthEvent.NodeName)
						/*
							Reason to reset healthEventIndex to 0 is that the current zeroth event is already processed and is deleted from
							the array so we need to start from the beginning of the array again hence healthEventIndex is reset to 0 and
							healthEventBufferLength is decremented by 1 because the element got deleted from the array on line number 226
						*/
						healthEventIndex = 0
						healthEventBufferLength--

						continue
					}
				}

				slog.Debug("Processing event %s at index %d", healthEventWithStatus.HealthEvent.CheckName, healthEventIndex)
				// Reason to increment healthEventIndex is that we want to process the next event in the next iteration
				healthEventIndex++

				isNodeQuarantined, ruleEvaluationResult := r.handleEvent(
					ctx,
					healthEventWithStatus,
					ruleSetEvals,
					rulesetsConfig,
				)

				if ruleEvaluationResult == common.RuleEvaluationRetryAgainInFuture {
					slog.Info(" Rule evaluation failed, will revaluate it in next iteration", "event", healthEventWithStatus)
					continue
				}

				if isNodeQuarantined == nil {
					// Status is nil, meaning we intentionally skipped processing this event
					// (e.g., healthy event without quarantine annotation or rule evaluation failed)
					slog.Debug("Skipped processing event for node, no status update needed",
						"node", healthEventWithStatus.HealthEvent.NodeName)

					currentEventInfo.HasProcessed = true

					r.storeEventObjectID(eventBson)

					duration := time.Since(startTime).Seconds()
					eventHandlingDuration.Observe(duration)
					totalEventsSkipped.Inc()

					continue
				}

				// Process events with status
				currentEventInfo.HasProcessed = true

				r.storeEventObjectID(eventBson)

				err := r.updateNodeQuarantineStatus(ctx, healthEventCollection, eventBson, isNodeQuarantined)
				if err != nil {
					slog.Error("Error updating Node quarantine status", "error", err)
					processingErrors.WithLabelValues("update_quarantine_status_error").Inc()
				} else if *isNodeQuarantined == model.Quarantined || *isNodeQuarantined == model.UnQuarantined {
					// Only count as successfully processed if there was an actual state change
					// AlreadyQuarantined means the event was skipped (already counted in handleEvent)
					totalEventsSuccessfullyProcessed.Inc()
				}

				duration := time.Since(startTime).Seconds()
				eventHandlingDuration.Observe(duration)
			}
		}
	}
}

// storeEventObjectID extracts the ObjectID from the event and stores it for metric tracking
func (r *Reconciler) storeEventObjectID(eventBson bson.M) {
	if fullDoc, ok := eventBson["fullDocument"].(bson.M); ok {
		if objID, ok := fullDoc["_id"].(primitive.ObjectID); ok {
			r.lastProcessedObjectID.Store(objID)
		}
	}
}

// updateUnprocessedEventsMetric periodically updates the EventBacklogSize metric
// based on the ObjectID of the last processed event
func (r *Reconciler) updateUnprocessedEventsMetric(ctx context.Context,
	watcher *storewatcher.ChangeStreamWatcher) {
	ticker := time.NewTicker(r.config.UnprocessedEventsMetricUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lastObjID := r.lastProcessedObjectID.Load()
			if lastObjID == nil {
				continue
			}

			objID, ok := lastObjID.(primitive.ObjectID)
			if !ok {
				continue
			}

			unprocessedCount, err := watcher.GetUnprocessedEventCount(ctx, objID)
			if err != nil {
				slog.Debug("Failed to get unprocessed event count", "error", err)
				continue
			}

			EventBacklogSize.Set(float64(unprocessedCount))
			slog.Debug("Updated unprocessed events metric", "count", unprocessedCount, "objectID", objID.Hex())
		}
	}
}

func (r *Reconciler) watchEvents(watcher *storewatcher.ChangeStreamWatcher) {
	for event := range watcher.Events() {
		totalEventsReceived.Inc()

		healthEventWithStatus := model.HealthEventWithStatus{}
		err := storewatcher.UnmarshalFullDocumentFromEvent(
			event,
			&healthEventWithStatus,
		)

		if err != nil {
			slog.Error("Failed to unmarshal event", "error", err)
			processingErrors.WithLabelValues("unmarshal_error").Inc()

			continue
		}

		slog.Debug("Enqueuing event", "event", healthEventWithStatus)
		r.healthEventBuffer.Add(&healthEventWithStatus, event)

		select {
		case r.workSignal <- struct{}{}:
			slog.Debug("Signalled work channel for new health event")
		default:
			slog.Debug("Work channel already signalled, skipping duplicate signal")
		}
	}
}

//nolint:cyclop,gocognit,nestif //fix this as part of NGCC-21793
func (r *Reconciler) handleEvent(
	ctx context.Context,
	event *model.HealthEventWithStatus,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
) (*model.Status, common.RuleEvaluationResult) {
	var status model.Status

	quarantineAnnotationExists := false

	// Get quarantine annotations from cache or API fallback
	annotations, annErr := r.getNodeQuarantineAnnotations(ctx, event.HealthEvent.NodeName)
	if annErr != nil {
		slog.Error("failed to fetch annotations for node %s: %+v",
			event.HealthEvent.NodeName, annErr)
	}

	if annErr == nil && annotations != nil {
		annotationVal, exists := annotations[common.QuarantineHealthEventAnnotationKey]

		if exists && annotationVal != "" {
			quarantineAnnotationExists = true
		}
	}

	if quarantineAnnotationExists {
		// The node was already quarantined by FQM earlier. Delegate to the
		// specialized handler which decides whether to keep it quarantined or
		// un-quarantine based on the incoming event.
		if r.handleQuarantinedNode(ctx, event.HealthEvent) {
			totalEventsSkipped.Inc()

			status = model.AlreadyQuarantined
		} else {
			status = model.UnQuarantined
		}

		return &status, common.RuleEvaluationNotApplicable
	}

	// For healthy events, if there's no existing quarantine annotation,
	// skip processing as there's no transition from unhealthy to healthy
	if event.HealthEvent.IsHealthy && !quarantineAnnotationExists {
		slog.Info("Skipping healthy event",
			"node", event.HealthEvent.NodeName,
			"event", event.HealthEvent)

		return nil, common.RuleEvaluationNotApplicable
	}

	type keyValTaint struct {
		Key   string
		Value string
	}

	var taintAppliedMap sync.Map

	var labelsMap sync.Map

	var isCordoned atomic.Bool

	var taintEffectPriorityMap sync.Map

	ruleEvaluationRetryInFuture := false

	for _, eval := range ruleSetEvals {
		taintConfig := rulesetsConfig.TaintConfigMap[eval.GetName()]
		if taintConfig != nil {
			keyVal := keyValTaint{
				Key:   taintConfig.Key,
				Value: taintConfig.Value,
			}
			// initialize maps
			taintAppliedMap.Store(keyVal, "")
			taintEffectPriorityMap.Store(keyVal, -1)
		}
	}

	var wg sync.WaitGroup

	if event.HealthEvent.QuarantineOverrides == nil ||
		!event.HealthEvent.QuarantineOverrides.Force {
		// Evaluate each ruleset in parallel
		for _, eval := range ruleSetEvals {
			wg.Add(1)

			go func(eval evaluator.RuleSetEvaluatorIface) {
				defer wg.Done()
				slog.Info("Handling event for ruleset", "event", event, "ruleset", eval.GetName())

				rulesetEvaluations.WithLabelValues(eval.GetName()).Inc()

				ruleEvaluatedResult, err := eval.Evaluate(event.HealthEvent)
				//nolint //ignore complex nesting blocks //fix this as part of NGCC-21793
				if ruleEvaluatedResult == common.RuleEvaluationSuccess {
					rulesetPassed.WithLabelValues(eval.GetName()).Inc()

					if shouldCordon := rulesetsConfig.CordonConfigMap[eval.GetName()]; shouldCordon {
						isCordoned.Store(true)

						newCordonReason := eval.GetName()

						if _, exist := labelsMap.Load(cordonedReasonLabelKey); exist {
							oldCordonReason, _ := labelsMap.Load(cordonedReasonLabelKey)
							newCordonReason = oldCordonReason.(string) + "-" + newCordonReason
						}

						labelsMap.Store(cordonedReasonLabelKey, formatCordonOrUncordonReasonValue(newCordonReason, 63))
					}

					taintConfig := rulesetsConfig.TaintConfigMap[eval.GetName()]
					// Apply taint and cordon based on configuration, if it is not already applied
					if taintConfig != nil {
						keyVal := keyValTaint{Key: taintConfig.Key, Value: taintConfig.Value}

						currentVal, _ := taintAppliedMap.Load(keyVal)
						currentEffect := currentVal.(string)

						currentPriorityVal, _ := taintEffectPriorityMap.Load(keyVal)
						currentPriority := currentPriorityVal.(int)

						newPriority := rulesetsConfig.RuleSetPriorityMap[eval.GetName()]

						// Update if no effect set yet or new priority is higher
						if currentEffect == "" || (currentEffect != "" && newPriority > currentPriority) {
							taintEffectPriorityMap.Store(keyVal, newPriority)
							taintAppliedMap.Store(keyVal, taintConfig.Effect)
						}
					}
				} else if err != nil {
					slog.Error("Error while evaluating event for ruleset", "event", event.HealthEvent, "ruleset", eval.GetName(), "error", err)

					processingErrors.WithLabelValues("ruleset_evaluation_error").Inc()

					rulesetFailed.WithLabelValues(eval.GetName()).Inc()
				} else if ruleEvaluatedResult == common.RuleEvaluationRetryAgainInFuture {

					slog.Debug("Rule evaluation not succeeded, will re-evaluate in next iteration", "event", event.HealthEvent)
					ruleEvaluationRetryInFuture = true

				} else {
					rulesetFailed.WithLabelValues(eval.GetName()).Inc()
				}
			}(eval)
		}

		wg.Wait()

		if ruleEvaluationRetryInFuture {
			return nil, common.RuleEvaluationRetryAgainInFuture
		}
	} else {
		isCordoned.Store(true)
		labelsMap.LoadOrStore(cordonedByLabelKey, event.HealthEvent.Agent+"-"+event.HealthEvent.Metadata["creator_id"])
		labelsMap.Store(cordonedReasonLabelKey,
			formatCordonOrUncordonReasonValue(event.HealthEvent.Message, 63))
	}

	taintsToBeApplied := []config.Taint{}
	// Check the taint map and collect the taints which are to be applied
	taintAppliedMap.Range(func(k, v interface{}) bool {
		keyVal := k.(keyValTaint)
		effect := v.(string)

		if effect != "" {
			taintsToBeApplied = append(taintsToBeApplied, config.Taint{
				Key:    keyVal.Key,
				Value:  keyVal.Value,
				Effect: effect,
			})
		}

		return true
	})

	// collect annotations to be applied if any
	annotationsMap := map[string]string{}

	if len(taintsToBeApplied) > 0 {
		// store the taints applied as an annotation
		taintsJsonStr, err := json.Marshal(taintsToBeApplied)
		if err != nil {
			slog.Error("Error marshalling taints", "taints", taintsToBeApplied, "event", event, "error", err)
		} else {
			annotationsMap[common.QuarantineHealthEventAppliedTaintsAnnotationKey] = string(taintsJsonStr)
		}
	}

	if isCordoned.Load() {
		// store cordon as an annotation
		annotationsMap[common.QuarantineHealthEventIsCordonedAnnotationKey] =
			common.QuarantineHealthEventIsCordonedAnnotationValueTrue

		labelsMap.LoadOrStore(cordonedByLabelKey, common.ServiceName)

		labelsMap.Store(cordonedTimestampLabelKey, time.Now().UTC().Format("2006-01-02T15-04-05Z"))
		labelsMap.Store(string(statemanager.NVSentinelStateLabelKey), string(statemanager.QuarantinedLabelValue))
	}

	isNodeQuarantined := (len(taintsToBeApplied) > 0 || isCordoned.Load())

	//nolint //ignore complex nested block //fix this as part of NGCC-21793
	if isNodeQuarantined {
		// Record an event to sliding window before actually quarantining
		if r.config.CircuitBreakerEnabled && (event.HealthEvent.QuarantineOverrides == nil ||
			!event.HealthEvent.QuarantineOverrides.Force) {
			r.cb.AddCordonEvent(event.HealthEvent.NodeName)
		}

		// Create health events structure for the new quarantine with sanitized health event
		healthEvents := healthEventsAnnotation.NewHealthEventsAnnotationMap()
		updated := healthEvents.AddOrUpdateEvent(event.HealthEvent)

		if !updated {
			slog.Info("Health event already exists for node, skipping quarantine", "event", event.HealthEvent, "node", event.HealthEvent.NodeName)
			return nil, common.RuleEvaluationNotApplicable
		}

		eventJsonStr, err := json.Marshal(healthEvents)
		if err != nil {
			slog.Error("Error marshalling health events", "error", err)
		} else {
			annotationsMap[common.QuarantineHealthEventAnnotationKey] = string(eventJsonStr)
		}

		labels := map[string]string{}
		labelsMap.Range(func(key, value any) bool {
			strKey, okKey := key.(string)
			strValue, okValue := value.(string)
			if okKey && okValue {
				labels[strKey] = strValue
			}
			return true
		})

		// Remove manual uncordon annotation if present before applying new quarantine
		r.removeManualUncordonAnnotationIfPresent(ctx, event.HealthEvent.NodeName, annotations)

		if !r.config.CircuitBreakerEnabled {
			slog.Info("Circuit breaker is disabled, proceeding with quarantine action for node without circuit breaker protection", "node", event.HealthEvent.NodeName)
		}

		if err := r.config.K8sClient.TaintAndCordonNodeAndSetAnnotations(
			ctx,
			event.HealthEvent.NodeName,
			taintsToBeApplied,
			isCordoned.Load(),
			annotationsMap,
			labels,
		); err != nil {
			slog.Error("Error updating node", "event", event.HealthEvent, "error", err)

			processingErrors.WithLabelValues("taint_and_cordon_error").Inc()

			isNodeQuarantined = false
		} else {
			totalNodesQuarantined.WithLabelValues(event.HealthEvent.NodeName).Inc()
			currentQuarantinedNodes.WithLabelValues(event.HealthEvent.NodeName).Inc()

			// Update cache with the new annotations that were just added to the node
			// This ensures subsequent events in the same batch see the updated annotations
			r.updateCacheWithQuarantineAnnotations(event.HealthEvent.NodeName, annotationsMap)

			// update the map here so that later we can refer to it and update the quarantined nodes
			r.nodeInfo.MarkNodeQuarantineStatusCache(event.HealthEvent.NodeName, isNodeQuarantined, true)

			for _, taint := range taintsToBeApplied {
				taintsApplied.WithLabelValues(taint.Key, taint.Effect).Inc()
			}

			if isCordoned.Load() {
				cordonsApplied.Inc()
			}
		}
	}

	if isNodeQuarantined {
		status = model.Quarantined
	} else {
		return nil, common.RuleEvaluationNotApplicable
	}

	return &status, common.RuleEvaluationNotApplicable
}

func (r *Reconciler) handleQuarantinedNode(
	ctx context.Context,
	event *protos.HealthEvent,
) bool {
	// Get and validate health events quarantine annotations
	healthEventsAnnotationMap, annotations, err := r.getAndValidateHealthEventsQuarantineAnnotations(ctx, event)
	if err != nil {
		processingErrors.WithLabelValues("get_node_annotations_error").Inc()
		// Error cases return true to keep node quarantined, or false if no annotation exists
		return err.Error() != "no quarantine annotation"
	}

	// Check if any entities from this event are already tracked
	_, hasExistingCheck := healthEventsAnnotationMap.GetEvent(event)

	if !event.IsHealthy {
		// Handle unhealthy event - add new entity failures
		added := healthEventsAnnotationMap.AddOrUpdateEvent(event)

		if added {
			slog.Info("Added entity failures for check on node",
				"check", event.CheckName,
				"node", event.NodeName,
				"trackedEntities", healthEventsAnnotationMap.Count())

			// Update the annotation with the new entity failures
			if err := r.updateHealthEventsQuarantineAnnotation(ctx, event.NodeName, healthEventsAnnotationMap); err != nil {
				slog.Error("Failed to update health events annotation", "error", err)
				return true
			}
		} else {
			slog.Debug("All entities already tracked for check %s on node %s",
				event.CheckName, event.NodeName)
		}

		// Node remains quarantined
		return true
	}

	// Handle healthy event
	if !hasExistingCheck {
		slog.Debug("Received healthy event for untracked check %s on node %s (other checks may still be failing)",
			event.CheckName, event.NodeName)
		return true
	}

	// Remove the specific entities that have recovered
	// With entity-level tracking, each entity is handled independently
	removedCount := healthEventsAnnotationMap.RemoveEvent(event)

	if removedCount > 0 {
		slog.Info("Removed recovered entities for check on node",
			"removedCount", removedCount,
			"check", event.CheckName,
			"node", event.NodeName,
			"remainingEntities", healthEventsAnnotationMap.Count())
	} else {
		slog.Debug("No matching entities to remove for check %s on node %s",
			event.CheckName, event.NodeName)
	}

	// Check if all checks have recovered
	if healthEventsAnnotationMap.IsEmpty() {
		// All checks recovered - uncordon the node
		slog.Info("All health checks recovered for node, proceeding with uncordon",
			"node", event.NodeName)
		return r.performUncordon(ctx, event, annotations)
	}

	// Update the annotation with the modified health events structure
	if err := r.updateHealthEventsQuarantineAnnotation(ctx, event.NodeName, healthEventsAnnotationMap); err != nil {
		slog.Error("Failed to update health events annotation after recovery", "error", err)
		return true
	}

	// Node remains quarantined as there are still failing checks
	slog.Info("Node remains quarantined with failing checks",
		"node", event.NodeName,
		"failingChecksCount", healthEventsAnnotationMap.Count(),
		"checks", healthEventsAnnotationMap.GetAllCheckNames())

	return true
}

func (r *Reconciler) getAndValidateHealthEventsQuarantineAnnotations(
	ctx context.Context,
	event *protos.HealthEvent,
) (*healthEventsAnnotation.HealthEventsAnnotationMap, map[string]string, error) {
	annotations, err := r.getNodeQuarantineAnnotations(ctx, event.NodeName)
	if err != nil {
		slog.Error("Error getting node annotations", "event", event, "error", err)
		processingErrors.WithLabelValues("get_node_annotations_error").Inc()

		return nil, nil, fmt.Errorf("failed to get annotations")
	}

	quarantineAnnotationStr, exists := annotations[common.QuarantineHealthEventAnnotationKey]
	if !exists || quarantineAnnotationStr == "" {
		slog.Info("No quarantine annotation found for node", "node", event.NodeName)
		return nil, nil, fmt.Errorf("no quarantine annotation")
	}

	// Try to unmarshal as HealthEventsAnnotationMap first
	var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap

	err = json.Unmarshal([]byte(quarantineAnnotationStr), &healthEventsMap)
	if err != nil {
		// Fallback: try to unmarshal as single HealthEvent for backward compatibility
		var singleHealthEvent protos.HealthEvent

		if err2 := json.Unmarshal([]byte(quarantineAnnotationStr), &singleHealthEvent); err2 == nil {
			// Convert single event to health events structure
			slog.Info("Converting single health event to health events structure for node", "node", event.NodeName)

			healthEventsMap = *healthEventsAnnotation.NewHealthEventsAnnotationMap()
			healthEventsMap.AddOrUpdateEvent(&singleHealthEvent)

			// Update the annotation to new format for consistency
			if err := r.updateHealthEventsQuarantineAnnotation(ctx, event.NodeName, &healthEventsMap); err != nil {
				slog.Warn("Failed to update annotation to new format", "error", err)
			}
		} else {
			slog.Error("error unmarshalling annotation for node %s: %+v", event.NodeName, err)
			return nil, nil, fmt.Errorf("failed to unmarshal annotation")
		}
	}

	return &healthEventsMap, annotations, nil
}

func (r *Reconciler) updateHealthEventsQuarantineAnnotation(
	ctx context.Context,
	nodeName string,
	healthEvents *healthEventsAnnotation.HealthEventsAnnotationMap,
) error {
	annotationBytes, err := json.Marshal(healthEvents)
	if err != nil {
		slog.Error("Error marshalling health events annotation", "error", err)
		return fmt.Errorf("failed to marshal health events: %w", err)
	}

	annotationsToUpdate := map[string]string{
		common.QuarantineHealthEventAnnotationKey: string(annotationBytes),
	}

	if err := r.config.K8sClient.UpdateNodeAnnotations(ctx, nodeName, annotationsToUpdate); err != nil {
		slog.Error("Error updating node annotations for multi-event", "error", err)
		return fmt.Errorf("failed to update node annotations for multi-event on %s: %w", nodeName, err)
	}

	slog.Info("Updated health events quarantine annotation for node %s - %d checks tracked",
		nodeName, healthEvents.Count())

	// Update cache
	r.updateCacheWithQuarantineAnnotations(nodeName, annotationsToUpdate)

	return nil
}

func (r *Reconciler) performUncordon(
	ctx context.Context,
	event *protos.HealthEvent,
	annotations map[string]string,
) bool {
	slog.Info("All entities recovered for check %s on node %s - proceeding with uncordon",
		event.CheckName, event.NodeName)

	// Prepare uncordon parameters
	taintsToBeRemoved, annotationsToBeRemoved, isUnCordon, labelsMap, err := r.prepareUncordonParams(
		event, annotations)
	if err != nil {
		slog.Error("Error preparing uncordon params", "event", event, "error", err)
		return true
	}

	// Nothing to uncordon
	if len(taintsToBeRemoved) == 0 && !isUnCordon {
		return false
	}

	// Add the main quarantine annotation to removal list
	annotationsToBeRemoved = append(annotationsToBeRemoved, common.QuarantineHealthEventAnnotationKey)

	if !r.config.CircuitBreakerEnabled {
		slog.Info("Circuit breaker is disabled, proceeding with unquarantine action for node", "node", event.NodeName)
	}

	if err := r.config.K8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(
		ctx,
		event.NodeName,
		taintsToBeRemoved,
		isUnCordon,
		annotationsToBeRemoved,
		[]string{cordonedByLabelKey, cordonedReasonLabelKey, cordonedTimestampLabelKey, statemanager.NVSentinelStateLabelKey},
		labelsMap,
	); err != nil {
		slog.Error("Error updating node", "event", event, "error", err)
		processingErrors.WithLabelValues("untaint_and_uncordon_error").Inc()

		return true
	}

	r.updateUncordonMetricsAndCache(event.NodeName, taintsToBeRemoved, isUnCordon, annotationsToBeRemoved)

	return false
}

// prepareUncordonParams prepares parameters for uncordoning a node
func (r *Reconciler) prepareUncordonParams(
	event *protos.HealthEvent,
	annotations map[string]string,
) ([]config.Taint, []string, bool, map[string]string, error) {
	var (
		annotationsToBeRemoved = []string{}
		taintsToBeRemoved      []config.Taint
		isUnCordon             = false
		labelsMap              = map[string]string{}
	)

	// Check taints
	quarantineAnnotationEventTaintsAppliedStr, taintsExists :=
		annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey]
	if taintsExists && quarantineAnnotationEventTaintsAppliedStr != "" {
		annotationsToBeRemoved = append(annotationsToBeRemoved,
			common.QuarantineHealthEventAppliedTaintsAnnotationKey)

		err := json.Unmarshal([]byte(quarantineAnnotationEventTaintsAppliedStr), &taintsToBeRemoved)
		if err != nil {
			slog.Error("Error unmarshalling taints annotation",
				"annotation", quarantineAnnotationEventTaintsAppliedStr, "event", event, "error", err)
			return nil, nil, false, nil, fmt.Errorf("failed to unmarshal taints annotation: %w", err)
		}
	}

	// Check cordon status
	quarantineAnnotationEventIsCordonStr, cordonExists :=
		annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]
	if cordonExists && quarantineAnnotationEventIsCordonStr == common.QuarantineHealthEventIsCordonedAnnotationValueTrue {
		isUnCordon = true

		annotationsToBeRemoved = append(annotationsToBeRemoved,
			common.QuarantineHealthEventIsCordonedAnnotationKey)
		labelsMap[uncordonedByLabelKey] = common.ServiceName
		labelsMap[uncordonedTimestampLabelKey] = time.Now().UTC().Format("2006-01-02T15-04-05Z")
	}

	return taintsToBeRemoved, annotationsToBeRemoved, isUnCordon, labelsMap, nil
}

// updateUncordonMetricsAndCache updates metrics and cache after uncordoning
func (r *Reconciler) updateUncordonMetricsAndCache(
	nodeName string,
	taintsToBeRemoved []config.Taint,
	isUnCordon bool,
	annotationsToBeRemoved []string,
) {
	totalNodesUnquarantined.WithLabelValues(nodeName).Inc()
	currentQuarantinedNodes.WithLabelValues(nodeName).Dec()
	slog.Info("Decremented currentQuarantinedNodes metric for unquarantined node", "node", nodeName)

	// Update cache
	r.updateCacheWithUnquarantineAnnotations(nodeName, annotationsToBeRemoved)
	r.nodeInfo.MarkNodeQuarantineStatusCache(nodeName, false, false)

	// Update taint metrics
	for _, taint := range taintsToBeRemoved {
		taintsRemoved.WithLabelValues(taint.Key, taint.Effect).Inc()
	}

	if isUnCordon {
		cordonsRemoved.Inc()
	}
}

func (r *Reconciler) updateNodeQuarantineStatus(
	ctx context.Context,
	healthEventCollection *mongo.Collection,
	event bson.M,
	nodeQuarantinedStatus *model.Status,
) error {
	if nodeQuarantinedStatus == nil {
		return fmt.Errorf("nodeQuarantinedStatus is nil")
	}

	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}

	filter := bson.M{"_id": document["_id"]}

	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.nodequarantined": *nodeQuarantinedStatus,
		},
	}

	if _, err := healthEventCollection.UpdateOne(ctx, filter, update); err != nil {
		return fmt.Errorf("error updating document with _id: %v, error: %w", document["_id"], err)
	}

	slog.Info("Document updated", "_id", document["_id"], "nodeQuarantinedStatus", *nodeQuarantinedStatus)

	return nil
}

func formatCordonOrUncordonReasonValue(input string, length int) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

	formatted := re.ReplaceAllString(input, "-")

	if len(formatted) > length {
		formatted = formatted[:length]
	}

	// Ensure it starts and ends with an alphanumeric character
	formatted = strings.Trim(formatted, "-")

	return formatted
}

// getNodeQuarantineAnnotations retrieves quarantine annotations from cache or API fallback
func (r *Reconciler) getNodeQuarantineAnnotations(ctx context.Context, nodeName string) (map[string]string, error) {
	// Try to get annotations from cache first
	r.cacheMutex.RLock()
	cached, ok := r.nodeAnnotationsCache.Load(nodeName)
	r.cacheMutex.RUnlock()

	if ok {
		orig := cached.(map[string]string)
		// Create a defensive copy to prevent external mutations
		dup := make(map[string]string, len(orig))
		for k, v := range orig {
			dup[k] = v
		}

		slog.Debug("Using cached annotations for node", "node", nodeName)

		return dup, nil
	}

	// Fall back to API call if not in cache
	return r.fetchAndCacheQuarantineAnnotations(ctx, nodeName)
}

// fetchAndCacheQuarantineAnnotations fetches all annotations from API and caches only quarantine ones
func (r *Reconciler) fetchAndCacheQuarantineAnnotations(ctx context.Context,
	nodeName string) (map[string]string, error) {
	allAnnotations, err := r.config.K8sClient.GetNodeAnnotations(ctx, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node annotations for %s: %w", nodeName, err)
	}

	// Extract and store only quarantine annotations in cache
	quarantineAnnotations := make(map[string]string)
	quarantineKeys := []string{
		common.QuarantineHealthEventAnnotationKey,
		common.QuarantineHealthEventAppliedTaintsAnnotationKey,
		common.QuarantineHealthEventIsCordonedAnnotationKey,
		common.QuarantinedNodeUncordonedManuallyAnnotationKey,
	}

	for _, key := range quarantineKeys {
		if value, exists := allAnnotations[key]; exists {
			quarantineAnnotations[key] = value
		}
	}

	// Store all nodes in cache (even with empty quarantine annotations)
	// This prevents repeated API calls for the same node
	r.cacheMutex.Lock()
	r.nodeAnnotationsCache.Store(nodeName, quarantineAnnotations)
	r.cacheMutex.Unlock()

	if len(quarantineAnnotations) > 0 {
		slog.Debug("Cached quarantine annotations for node", "node", nodeName)
	}

	// Return a defensive copy to prevent external mutations of the cached map
	returnCopy := make(map[string]string, len(quarantineAnnotations))
	for k, v := range quarantineAnnotations {
		returnCopy[k] = v
	}

	return returnCopy, nil
}

// handleNodeAnnotationChange updates the cached annotations for a node when notified by the informer
func (r *Reconciler) handleNodeAnnotationChange(nodeName string, annotations map[string]string) {
	r.cacheMutex.Lock()
	defer r.cacheMutex.Unlock()

	if annotations == nil {
		// Node was deleted, remove from cache
		r.nodeAnnotationsCache.Delete(nodeName)
		slog.Debug("Removed annotations from cache for deleted node", "node", nodeName)

		return
	}

	// Since we only cache quarantine annotations and the informer only sends quarantine annotations,
	// we can simply replace the entire cache entry
	// Store all nodes in cache (even with empty quarantine annotations) to prevent API calls
	r.nodeAnnotationsCache.Store(nodeName, annotations)

	if len(annotations) > 0 {
		slog.Debug("Updated quarantine annotations in cache for node", "node", nodeName)
	} else {
		slog.Debug("Updated cache for node (no quarantine annotations)", "node", nodeName)
	}
}

// updateCacheWithQuarantineAnnotations updates the cached annotations for a node
// after quarantine annotations have been added to the actual node
func (r *Reconciler) updateCacheWithQuarantineAnnotations(nodeName string, newAnnotations map[string]string) {
	r.cacheMutex.Lock()
	defer r.cacheMutex.Unlock()

	if cached, ok := r.nodeAnnotationsCache.Load(nodeName); ok {
		// Create a copy of the existing cached annotations
		annotations := make(map[string]string)
		for k, v := range cached.(map[string]string) {
			annotations[k] = v
		}

		// Add the new quarantine annotations
		for key, value := range newAnnotations {
			annotations[key] = value
		}

		// Update the cache with the modified annotations
		r.nodeAnnotationsCache.Store(nodeName, annotations)
		slog.Debug("Updated cache", "node", nodeName, "annotations", newAnnotations)
	} else {
		// If not in cache, store a copy of the new annotations to prevent external mutations
		annotationsCopy := make(map[string]string, len(newAnnotations))
		for k, v := range newAnnotations {
			annotationsCopy[k] = v
		}

		r.nodeAnnotationsCache.Store(nodeName, annotationsCopy)
		slog.Debug("Stored new annotations in cache", "node", nodeName, "annotations", newAnnotations)
	}
}

// updateCacheWithUnquarantineAnnotations updates the cached annotations for a node
// after quarantine annotations have been removed from the actual node
func (r *Reconciler) updateCacheWithUnquarantineAnnotations(nodeName string, removedAnnotationKeys []string) {
	r.cacheMutex.Lock()
	defer r.cacheMutex.Unlock()

	if cached, ok := r.nodeAnnotationsCache.Load(nodeName); ok {
		// Create a copy of the existing cached annotations
		annotations := make(map[string]string)
		for k, v := range cached.(map[string]string) {
			annotations[k] = v
		}

		// Remove the specified annotation keys
		for _, key := range removedAnnotationKeys {
			delete(annotations, key)
		}

		// Update the cache with the modified annotations
		r.nodeAnnotationsCache.Store(nodeName, annotations)
		slog.Debug("Updated cache for node %s, removed annotation keys: %v", nodeName, removedAnnotationKeys)
	} else {
		// If not in cache, nothing to remove - this shouldn't happen in normal flow
		slog.Debug("No cache entry found for node during unquarantine annotation update", "node", nodeName)
	}
}

// buildNodeAnnotationsCache fetches all nodes and their annotations to populate the cache
func (r *Reconciler) buildNodeAnnotationsCache(ctx context.Context) error {
	slog.Info("Building node annotations cache...")

	startTime := time.Now()

	nodeList, err := r.config.K8sClient.GetK8sClient().CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}

	// List of quarantine annotation keys we care about
	quarantineKeys := []string{
		common.QuarantineHealthEventAnnotationKey,
		common.QuarantineHealthEventAppliedTaintsAnnotationKey,
		common.QuarantineHealthEventIsCordonedAnnotationKey,
		common.QuarantinedNodeUncordonedManuallyAnnotationKey,
	}

	// Use write lock for bulk cache population
	r.cacheMutex.Lock()
	defer r.cacheMutex.Unlock()

	nodeCount := 0

	for _, node := range nodeList.Items {
		// Extract only the quarantine annotations
		quarantineAnnotations := make(map[string]string)

		if node.Annotations != nil {
			for _, key := range quarantineKeys {
				if value, exists := node.Annotations[key]; exists {
					quarantineAnnotations[key] = value
				}
			}
		}

		// Store all nodes in cache (even with empty quarantine annotations)
		// This prevents API calls for nodes without quarantine annotations
		r.nodeAnnotationsCache.Store(node.Name, quarantineAnnotations)

		if len(quarantineAnnotations) > 0 {
			slog.Debug("Cached quarantine annotations", "node", node.Name, "annotations", quarantineAnnotations)
		}

		nodeCount++
	}

	fetchDuration := time.Since(startTime)
	slog.Info("Successfully built cache with quarantine annotations", "nodeCount", nodeCount, "duration", fetchDuration)

	return nil
}

// removeManualUncordonAnnotationIfPresent removes the manual uncordon annotation from a node
// if it exists. This is called before applying a new quarantine to ensure clean state.
func (r *Reconciler) removeManualUncordonAnnotationIfPresent(ctx context.Context, nodeName string,
	annotations map[string]string) {
	if annotations == nil {
		return
	}

	if _, hasManualUncordon := annotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey]; hasManualUncordon {
		slog.Info("Removing manual uncordon annotation from node before applying new quarantine", "node", nodeName)

		// Remove the manual uncordon annotation before applying quarantine
		if err := r.config.K8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(
			ctx,
			nodeName,
			nil,   // No taints to remove
			false, // Not uncordoning
			[]string{common.QuarantinedNodeUncordonedManuallyAnnotationKey}, // Remove manual uncordon annotation
			nil, // No labels to remove
			nil, // No labels to add
		); err != nil {
			slog.Error("Failed to remove manual uncordon annotation from node", "node", nodeName)
		} else {
			// Update cache to remove the manual uncordon annotation
			r.updateCacheWithUnquarantineAnnotations(nodeName,
				[]string{common.QuarantinedNodeUncordonedManuallyAnnotationKey})
		}
	}
}

// handleManualUncordon handles the case when a node is manually uncordoned while having FQ annotations
func (r *Reconciler) handleManualUncordon(nodeName string) error {
	ctx := context.Background()

	slog.Info("Handling manual uncordon for node", "node", nodeName)

	// Get the current annotations from cache or API fallback
	annotations, err := r.getNodeQuarantineAnnotations(ctx, nodeName)
	if err != nil {
		return fmt.Errorf("failed to get annotations for manually uncordoned node %s: %w", nodeName, err)
	}

	// Check which FQ annotations exist and need to be removed
	annotationsToRemove := []string{}

	var taintsToRemove []config.Taint

	// Check for taints annotation
	taintsKey := common.QuarantineHealthEventAppliedTaintsAnnotationKey
	if taintsStr, exists := annotations[taintsKey]; exists && taintsStr != "" {
		annotationsToRemove = append(annotationsToRemove, taintsKey)

		// Parse taints to remove them
		if err := json.Unmarshal([]byte(taintsStr), &taintsToRemove); err != nil {
			return fmt.Errorf("failed to unmarshal taints for manually uncordoned node %s: %w", nodeName, err)
		}
	}

	// Remove all FQ-related annotations
	if _, exists := annotations[common.QuarantineHealthEventAnnotationKey]; exists {
		annotationsToRemove = append(annotationsToRemove, common.QuarantineHealthEventAnnotationKey)
	}

	if _, exists := annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]; exists {
		annotationsToRemove = append(annotationsToRemove, common.QuarantineHealthEventIsCordonedAnnotationKey)
	}

	// Add the manual uncordon annotation
	newAnnotations := map[string]string{
		common.QuarantinedNodeUncordonedManuallyAnnotationKey: common.QuarantinedNodeUncordonedManuallyAnnotationValue,
	}

	// Update the node: remove FQ annotations and any remaining taints
	if err := r.config.K8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(
		ctx,
		nodeName,
		taintsToRemove,
		false, // Node is already uncordoned manually, so we don't need to uncordon again
		annotationsToRemove,
		[]string{statemanager.NVSentinelStateLabelKey},
		nil, // No labels to add
	); err != nil {
		processingErrors.WithLabelValues("manual_uncordon_cleanup_error").Inc()

		return fmt.Errorf("failed to clean up annotations for manually uncordoned node %s: %w", nodeName, err)
	}

	// Add the new annotation
	if err := r.config.K8sClient.TaintAndCordonNodeAndSetAnnotations(
		ctx,
		nodeName,
		nil,   // No taints to add
		false, // No cordon to add
		newAnnotations,
		nil, // No labels to add
	); err != nil {
		return fmt.Errorf("failed to add manual uncordon annotation to node %s: %w", nodeName, err)
	}

	currentQuarantinedNodes.WithLabelValues(nodeName).Dec()
	slog.Info("Decremented currentQuarantinedNodes metric for manually uncordoned node", "node", nodeName)

	// Update internal state immediately to be consistent with the metric.
	// This ensures the state is correct even before the subsequent update event is processed.
	// Note: The subsequent update event will call updateNodeQuarantineStatus, but it won't
	// actually update the cache since we've already set it to the correct state here.
	r.nodeInfo.MarkNodeQuarantineStatusCache(nodeName, false, false)

	// Note: We don't need to manually update the annotation cache here because
	// after we update the node, it will trigger another update event in the NodeInformer
	// which will call onNodeAnnotationsChanged to update the cache

	slog.Info("Successfully handled manual uncordon for node", "node", nodeName)

	return nil
}
