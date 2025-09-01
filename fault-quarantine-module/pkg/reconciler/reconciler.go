// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/common"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/config"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/evaluator"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/informer"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/nodeinfo"
	storeconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"
	platformconnectorprotos "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
)

type ReconcilerConfig struct {
	TomlConfig                            config.TomlConfig
	MongoHealthEventCollectionConfig      storewatcher.MongoDBConfig
	TokenConfig                           storewatcher.TokenConfig
	MongoPipeline                         mongo.Pipeline
	K8sClient                             K8sClientInterface
	DryRun                                bool
	UnprocessedEventsMetricUpdateInterval time.Duration
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
	return &Reconciler{
		config:            cfg,
		healthEventBuffer: common.NewHealthEventBuffer(ctx),
		nodeInfo:          nodeinfo.NewNodeInfo(workSignal),
		workSignal:        workSignal, // Store the signal channel
	}
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
func (r *Reconciler) Start(ctx context.Context) {
	nodeInformer, err := informer.NewNodeInformer(r.config.K8sClient.GetK8sClient(),
		30*time.Minute, r.workSignal, r.nodeInfo)
	if err != nil {
		klog.Fatalf("failed to initialize node informer: %+v", err)
	}

	// Set the callback to decrement the metric when a quarantined node with annotations is deleted
	nodeInformer.SetOnQuarantinedNodeDeletedCallback(func(nodeName string) {
		currentQuarantinedNodes.Dec()
		klog.Infof("Decremented currentQuarantinedNodes metric for deleted quarantined node: %s", nodeName)
	})

	// Set the callback to update the annotations cache when node annotations change
	nodeInformer.SetOnNodeAnnotationsChangedCallback(r.handleNodeAnnotationChange)

	// Set the callback to handle manual uncordon of quarantined nodes
	nodeInformer.SetOnManualUncordonCallback(r.handleManualUncordon)

	ruleSetEvals, err := evaluator.InitializeRuleSetEvaluators(r.config.TomlConfig.RuleSets,
		r.config.K8sClient.GetK8sClient(), nodeInformer)
	if err != nil {
		klog.Fatalf("failed to initialize all rule set evaluators: %+v", err)
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
		klog.Fatalf("failed to create change stream watcher: %+v", err)
	}
	defer watcher.Close(ctx)

	healthEventCollection, err := storewatcher.GetCollectionClient(ctx, r.config.MongoHealthEventCollectionConfig)
	if err != nil {
		klog.Fatalf(
			"error initializing healthEventCollection client with config %+v for mongodb: %+v",
			r.config.MongoHealthEventCollectionConfig,
			err,
		)
	}

	err = r.nodeInfo.BuildQuarantinedNodesMap(r.config.K8sClient.GetK8sClient())
	if err != nil {
		klog.Fatalf("error fetching quarantined nodes: %+v", err)
	} else {
		quarantinedNodesMap := r.nodeInfo.GetQuarantinedNodesCopy()
		nodesCount := len(quarantinedNodesMap)

		// Set the gauge to the current number of quarantined nodes
		currentQuarantinedNodes.Set(float64(nodesCount))

		klog.Infof("Initial quarantinedNodesMap is: %+v, total of %d nodes", quarantinedNodesMap, nodesCount)
	}

	err = nodeInformer.Run(ctx.Done())
	if err != nil {
		klog.Fatalf("failed to run node informer: %+v", err)
	}

	// Wait for NodeInformer cache to sync before processing any events
	klog.Info("Waiting for NodeInformer cache to sync before starting event processing...")

	for !nodeInformer.HasSynced() {
		select {
		case <-ctx.Done():
			klog.Warning("Context cancelled while waiting for node informer sync")
			return // Exit if context is cancelled during wait
		case <-time.After(5 * time.Second): // Check periodically
			klog.Infof("NodeInformer cache is not synced yet, waiting for 5 seconds")
		}
	}

	// Build initial node annotations cache
	if err := r.buildNodeAnnotationsCache(ctx); err != nil {
		// Continue anyway, individual API calls will be made as fallback
		klog.Errorf("Failed to build initial node annotations cache: %v", err)
	}

	watcher.Start(ctx)

	klog.Info("Listening for events on the channel...")

	go r.watchEvents(watcher)

	// Start a goroutine to periodically update the unprocessed events metric
	go r.updateUnprocessedEventsMetric(ctx, watcher)

	// Process events in the main goroutine
	for {
		select {
		case <-ctx.Done():
			klog.Info("Context canceled. Exiting fault-quarantine event consumer.")
			return
		case <-r.workSignal: // Wait for a signal (semaphore acquired)
			// Get current queue length
			healthEventBufferLength := r.healthEventBuffer.Length()
			if healthEventBufferLength == 0 {
				klog.V(4).Infof("No events to process, skipping")
				continue
			}

			klog.Infof("Processing batch of %d events", healthEventBufferLength)

			// Process up to the current queue length
			for healthEventIndex := 0; healthEventIndex < healthEventBufferLength; {
				klog.V(3).Infof("healthEventIndex is %d", healthEventIndex)

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
						klog.Errorf("Error removing event %s with error: %+v", healthEventWithStatus.HealthEvent.CheckName, err)
						continue
					}

					if err := watcher.MarkProcessed(ctx); err != nil {
						processingErrors.WithLabelValues("mark_processed_error").Inc()

						klog.Fatalf("Error updating resume token: %+v", err)
					} else {
						klog.Infof("Successfully marked event %s as processed", healthEventWithStatus.HealthEvent.NodeName)
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

				klog.V(3).Infof("Processing event %s at index %d", healthEventWithStatus.HealthEvent.CheckName, healthEventIndex)
				// Reason to increment healthEventIndex is that we want to process the next event in the next iteration
				healthEventIndex++

				isNodeQuarantined, ruleEvaluationResult := r.handleEvent(
					ctx,
					healthEventWithStatus,
					ruleSetEvals,
					rulesetsConfig,
				)

				if ruleEvaluationResult == common.RuleEvaluationRetryAgainInFuture {
					klog.Infof(" Rule evaluation failed, will revaluate it in next iteration \n%+v", healthEventWithStatus)
					continue
				}

				if isNodeQuarantined == nil {
					// Status is nil, meaning we intentionally skipped processing this event
					// (e.g., healthy event without quarantine annotation)
					// This was already counted as skipped in handleEvent, so don't count again
					klog.V(2).Infof("Skipped processing event for node %s, no status update needed",
						healthEventWithStatus.HealthEvent.NodeName)

					currentEventInfo.HasProcessed = true

					r.storeEventObjectID(eventBson)

					duration := time.Since(startTime).Seconds()
					eventHandlingDuration.Observe(duration)

					continue
				}

				// Process events with status
				currentEventInfo.HasProcessed = true

				r.storeEventObjectID(eventBson)

				err := r.updateNodeQuarantineStatus(ctx, healthEventCollection, eventBson, isNodeQuarantined)
				if err != nil {
					klog.Errorf("Error updating Node quarantine status: %+v", err)
					processingErrors.WithLabelValues("update_quarantine_status_error").Inc()
				} else if *isNodeQuarantined == storeconnector.Quarantined || *isNodeQuarantined == storeconnector.UnQuarantined {
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
				klog.V(3).Infof("Failed to get unprocessed event count: %v", err)
				continue
			}

			EventBacklogSize.Set(float64(unprocessedCount))
			klog.V(3).Infof("Updated unprocessed events metric: %d events after ObjectID %v",
				unprocessedCount, objID.Hex())
		}
	}
}

func (r *Reconciler) watchEvents(watcher *storewatcher.ChangeStreamWatcher) {
	for event := range watcher.Events() {
		totalEventsReceived.Inc()

		healthEventWithStatus := storeconnector.HealthEventWithStatus{}
		err := storewatcher.UnmarshalFullDocumentFromEvent(
			event,
			&healthEventWithStatus,
		)

		if err != nil {
			klog.Errorf("Failed to unmarshal event: %+v", err)
			processingErrors.WithLabelValues("unmarshal_error").Inc()

			continue
		}

		klog.V(3).Infof("Enqueuing event: %+v", healthEventWithStatus)
		r.healthEventBuffer.Add(&healthEventWithStatus, event)
		r.workSignal <- struct{}{}
	}
}

//nolint:cyclop,gocognit,nestif //fix this as part of NGCC-21793
func (r *Reconciler) handleEvent(
	ctx context.Context,
	event *storeconnector.HealthEventWithStatus,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
) (*storeconnector.Status, common.RuleEvaluationResult) {
	var status storeconnector.Status

	quarantineAnnotationExists := false

	// Get quarantine annotations from cache or API fallback
	annotations, annErr := r.getNodeQuarantineAnnotations(ctx, event.HealthEvent.NodeName)
	if annErr != nil {
		klog.Errorf("failed to fetch annotations for node %s: %+v",
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

			status = storeconnector.AlreadyQuarantined
		} else {
			status = storeconnector.UnQuarantined
		}

		return &status, common.RuleEvaluationNotApplicable
	}

	// For healthy events, if there's no existing quarantine annotation,
	// skip processing as there's no transition from unhealthy to healthy
	if event.HealthEvent.IsHealthy && !quarantineAnnotationExists {
		klog.Infof("Skipping healthy event for node %s as there's no existing quarantine annotation, Event: %+v",
			event.HealthEvent.NodeName, event.HealthEvent)
		totalEventsSkipped.Inc()

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
				klog.Infof("Handling event: %+v for ruleset: %+v", event, eval.GetName())

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
					klog.Errorf("error while evaluating for event: %+v for ruleset: %+v: %+v", event.HealthEvent, eval.GetName(), err)

					processingErrors.WithLabelValues("ruleset_evaluation_error").Inc()

					rulesetFailed.WithLabelValues(eval.GetName()).Inc()
				} else if ruleEvaluatedResult == common.RuleEvaluationRetryAgainInFuture {

					klog.V(2).Infof("RuleEvaluation not succeeded , will revaluate it in next iteration \n%+v", event.HealthEvent)
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
			klog.Errorf("error while marshalling taints %+v for event: %+v: %+v", taintsToBeApplied, event, err)
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
	}

	isNodeQuarantined := (len(taintsToBeApplied) > 0 || isCordoned.Load())

	//nolint //ignore complex nested block //fix this as part of NGCC-21793
	if isNodeQuarantined {
		eventJsonStr, err := json.Marshal(event.HealthEvent)
		if err != nil {
			klog.Errorf("error while marshalling event %+v: %+v", event.HealthEvent, err)
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

		if err := r.config.K8sClient.TaintAndCordonNodeAndSetAnnotations(
			ctx,
			event.HealthEvent.NodeName,
			taintsToBeApplied,
			isCordoned.Load(),
			annotationsMap,
			labels,
		); err != nil {
			klog.Errorf("error while updating node for event: %+v: %+v", event.HealthEvent, err)

			processingErrors.WithLabelValues("taint_and_cordon_error").Inc()

			isNodeQuarantined = false
		} else {
			totalNodesQuarantined.Inc()
			currentQuarantinedNodes.Inc()

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
		status = storeconnector.Quarantined
	} else {
		status = storeconnector.UnQuarantined
	}

	return &status, common.RuleEvaluationNotApplicable
}

// nolint: cyclop //fix this as part of NGCC-21793
func (r *Reconciler) handleQuarantinedNode(
	ctx context.Context,
	event *platformconnectorprotos.HealthEvent,
) bool {
	// Get quarantine annotations from cache or API fallback
	annotations, err := r.getNodeQuarantineAnnotations(ctx, event.NodeName)
	if err != nil {
		klog.Errorf("error while getting node annotations for event: %+v: %+v", event, err)
		processingErrors.WithLabelValues("get_node_annotations_error").Inc()

		return true
	}

	labelsMap := map[string]string{}
	quarantineAnnotationEvent, exists := annotations[common.QuarantineHealthEventAnnotationKey]

	if !exists || quarantineAnnotationEvent == "" {
		klog.Infof("No quarantine annotation found for node %s", event.NodeName)
		return false
	}

	//nolint //ignore complexity of nested block //fix this as part of NGCC-21793
	if compareHealthEventWithAnnotationEventToCheckUnQuarantine(event, quarantineAnnotationEvent) {
		// Check if we need to remove taints and remove them
		quarantineAnnotationEventTaintsAppliedStr, taintsExists :=
			annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey]

		// Check if we need to uncordon
		quarantineAnnotationEventIsCordonStr, cordonExists := annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]

		var taintsToBeRemoved []config.Taint

		annotationsToBeRemoved := []string{}

		isUnCordon := false

		if taintsExists && quarantineAnnotationEventTaintsAppliedStr != "" {
			annotationsToBeRemoved = append(annotationsToBeRemoved, common.QuarantineHealthEventAppliedTaintsAnnotationKey)

			err = json.Unmarshal([]byte(quarantineAnnotationEventTaintsAppliedStr), &taintsToBeRemoved)
			if err != nil {
				klog.Errorf("error while unmarshalling taints annotation %+v for event: %+v: %+v",
					quarantineAnnotationEventTaintsAppliedStr, event, err)

				// Node remains quarantined due to unmarshalling error
				return true
			}
		}

		if cordonExists && quarantineAnnotationEventIsCordonStr == common.QuarantineHealthEventIsCordonedAnnotationValueTrue {
			isUnCordon = true
			annotationsToBeRemoved = append(annotationsToBeRemoved, common.QuarantineHealthEventIsCordonedAnnotationKey)
			labelsMap[uncordonedByLabelKey] = common.ServiceName
			labelsMap[uncordonedTimestampLabelKey] = time.Now().UTC().Format("2006-01-02T15-04-05Z")
		}

		if len(taintsToBeRemoved) > 0 || isUnCordon {
			annotationsToBeRemoved = append(annotationsToBeRemoved, common.QuarantineHealthEventAnnotationKey)

			if err := r.config.K8sClient.UnTaintAndUnCordonNodeAndRemoveAnnotations(
				ctx,
				event.NodeName,
				taintsToBeRemoved,
				isUnCordon,
				annotationsToBeRemoved,
				[]string{cordonedByLabelKey, cordonedReasonLabelKey, cordonedTimestampLabelKey}, labelsMap,
			); err != nil {
				klog.Errorf("error while updating node for event: %+v: %+v", event, err)
				processingErrors.WithLabelValues("untaint_and_uncordon_error").Inc()

				return true
			}

			totalNodesUnquarantined.Inc()
			currentQuarantinedNodes.Dec()

			// Update cache by removing the annotations that were just removed from the node
			// This ensures subsequent events in the same batch see the updated annotations
			r.updateCacheWithUnquarantineAnnotations(event.NodeName, annotationsToBeRemoved)

			// Update the quarantinedNodesMap to reflect the node is no longer quarantined
			r.nodeInfo.MarkNodeQuarantineStatusCache(event.NodeName, false, false)
			for _, taint := range taintsToBeRemoved {
				taintsRemoved.WithLabelValues(taint.Key, taint.Effect).Inc()
			}

			if isUnCordon {
				cordonsRemoved.Inc()
			}
		}
		return false
	}

	// If quarantineAnnotationEvent is present but doesn't match the criteria to unquarantine
	return true
}

func (r *Reconciler) updateNodeQuarantineStatus(
	ctx context.Context,
	healthEventCollection *mongo.Collection,
	event bson.M,
	nodeQuarantinedStatus *storeconnector.Status,
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

	klog.Infof("Document with _id: %v has been updated with status %s", document["_id"], *nodeQuarantinedStatus)

	return nil
}

func compareHealthEventWithAnnotationEventToCheckUnQuarantine(
	event *platformconnectorprotos.HealthEvent,
	annotationEventStr string,
) bool {
	var annotationEvent platformconnectorprotos.HealthEvent

	err := json.Unmarshal([]byte(annotationEventStr), &annotationEvent)
	if err != nil {
		klog.Errorf("error while unmarshalling annotation event string %s: %+v", annotationEventStr, err)
		return false
	}

	if event.Agent != annotationEvent.Agent ||
		event.CheckName != annotationEvent.CheckName ||
		event.ComponentClass != annotationEvent.ComponentClass ||
		event.NodeName != annotationEvent.NodeName ||
		!areAnnotationEntitiesSubsetOfEventEntities(event.EntitiesImpacted, annotationEvent.EntitiesImpacted) ||
		event.Version != annotationEvent.Version {
		return false
	}

	return event.IsHealthy
}

// checks if all Entity objects in annotation event are present in passed event regardless of order
func areAnnotationEntitiesSubsetOfEventEntities(
	eventEntities,
	annotationEventEntities []*platformconnectorprotos.Entity,
) bool {
	if len(eventEntities) == 0 {
		return true
	}

	type key struct {
		EntityType  string
		EntityValue string
	}

	counts := make(map[key]int)

	for _, entity := range eventEntities {
		k := key{EntityType: entity.EntityType, EntityValue: entity.EntityValue}
		counts[k]++
	}

	for _, entity := range annotationEventEntities {
		k := key{EntityType: entity.EntityType, EntityValue: entity.EntityValue}

		if counts[k] == 0 {
			return false
		}

		counts[k]--
	}

	return true
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

		klog.V(5).Infof("Using cached annotations for node %s", nodeName)

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
		return nil, err
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
		klog.V(4).Infof("Cached quarantine annotations for node %s", nodeName)
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
		klog.V(4).Infof("Removed annotations from cache for deleted node %s", nodeName)

		return
	}

	// Since we only cache quarantine annotations and the informer only sends quarantine annotations,
	// we can simply replace the entire cache entry
	// Store all nodes in cache (even with empty quarantine annotations) to prevent API calls
	r.nodeAnnotationsCache.Store(nodeName, annotations)

	if len(annotations) > 0 {
		klog.V(4).Infof("Updated quarantine annotations in cache for node %s", nodeName)
	} else {
		klog.V(4).Infof("Updated cache for node %s (no quarantine annotations)", nodeName)
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
		klog.V(4).Infof("Updated cache for node %s with quarantine annotations: %v", nodeName, newAnnotations)
	} else {
		// If not in cache, store a copy of the new annotations to prevent external mutations
		annotationsCopy := make(map[string]string, len(newAnnotations))
		for k, v := range newAnnotations {
			annotationsCopy[k] = v
		}

		r.nodeAnnotationsCache.Store(nodeName, annotationsCopy)
		klog.V(4).Infof("Stored new annotations in cache for node %s: %v", nodeName, newAnnotations)
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
		klog.V(4).Infof("Updated cache for node %s, removed annotation keys: %v", nodeName, removedAnnotationKeys)
	} else {
		// If not in cache, nothing to remove - this shouldn't happen in normal flow
		klog.V(4).Infof("No cache entry found for node %s during unquarantine annotation update", nodeName)
	}
}

// buildNodeAnnotationsCache fetches all nodes and their annotations to populate the cache
func (r *Reconciler) buildNodeAnnotationsCache(ctx context.Context) error {
	klog.Info("Building node annotations cache...")

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
			klog.V(4).Infof("Cached quarantine annotations for node %s: %v", node.Name, quarantineAnnotations)
		}

		nodeCount++
	}

	fetchDuration := time.Since(startTime)
	klog.Infof("Successfully built cache with quarantine annotations for %d nodes in %v", nodeCount, fetchDuration)

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
		klog.Infof("Removing manual uncordon annotation from node %s before applying new quarantine", nodeName)

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
			klog.Errorf("Failed to remove manual uncordon annotation from node %s: %v", nodeName, err)
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

	klog.Infof("Handling manual uncordon for node: %s", nodeName)

	// Get the current annotations from cache or API fallback
	annotations, err := r.getNodeQuarantineAnnotations(ctx, nodeName)
	if err != nil {
		klog.Errorf("Failed to get annotations for manually uncordoned node %s: %v", nodeName, err)
		return err
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
			klog.Errorf("Failed to unmarshal taints for manually uncordoned node %s: %v", nodeName, err)
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
		nil, // No labels to remove
		nil, // No labels to add
	); err != nil {
		klog.Errorf("Failed to clean up annotations for manually uncordoned node %s: %v", nodeName, err)
		processingErrors.WithLabelValues("manual_uncordon_cleanup_error").Inc()

		return err
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
		klog.Errorf("Failed to add manual uncordon annotation to node %s: %v", nodeName, err)
		return err
	}

	currentQuarantinedNodes.Dec()

	// Update internal state immediately to be consistent with the metric.
	// This ensures the state is correct even before the subsequent update event is processed.
	// Note: The subsequent update event will call updateNodeQuarantineStatus, but it won't
	// actually update the cache since we've already set it to the correct state here.
	r.nodeInfo.MarkNodeQuarantineStatusCache(nodeName, false, false)

	// Note: We don't need to manually update the annotation cache here because
	// after we update the node, it will trigger another update event in the NodeInformer
	// which will call onNodeAnnotationsChanged to update the cache

	klog.Infof("Successfully handled manual uncordon for node %s", nodeName)

	return nil
}
