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
	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/klog"
)

type ReconcilerConfig struct {
	TomlConfig                       config.TomlConfig
	MongoHealthEventCollectionConfig storewatcher.MongoDBConfig
	TokenConfig                      storewatcher.TokenConfig
	MongoPipeline                    mongo.Pipeline
	K8sClient                        K8sClientInterface
	DryRun                           bool
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
		quarantinedNodesMap := r.nodeInfo.GetQuarantinedNodesMap()
		nodesCount := len(*quarantinedNodesMap)

		// Increment metrics based on the count of quarantined nodes
		for i := 0; i < nodesCount; i++ {
			currentQuarantinedNodes.Inc()
			totalNodesQuarantined.Inc()
		}

		klog.Infof("Initial quarantinedNodesMap is: %+v", quarantinedNodesMap)
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

	watcher.Start(ctx)

	klog.Info("Listening for events on the channel...")

	go r.watchEvents(watcher)

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

				if isNodeQuarantined != nil {
					currentEventInfo.HasProcessed = true
				}

				errFlag := false

				if err := r.updateNodeQuarantineStatus(ctx, healthEventCollection, eventBson, isNodeQuarantined); err != nil {
					klog.Errorf("Error updating Node quarantine status: %+v", err)
					processingErrors.WithLabelValues("update_quarantine_status_error").Inc()

					errFlag = true
				}

				if !errFlag {
					totalEventsSuccessfullyProcessed.Inc()
				}

				duration := time.Since(startTime).Seconds()

				eventHandlingDuration.Observe(duration)
			}
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

// nolint: cyclop, gocognit //fix this as part of NGCC-21793
func (r *Reconciler) handleEvent(
	ctx context.Context,
	event *storeconnector.HealthEventWithStatus,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
) (*storeconnector.Status, common.RuleEvaluationResult) {
	var status storeconnector.Status

	quarantineAnnotationExists := false

	annotations, annErr := r.config.K8sClient.GetNodeAnnotations(ctx, event.HealthEvent.NodeName)
	if annErr != nil {
		klog.Errorf("failed to fetch annotations for node %s: %+v",
			event.HealthEvent.NodeName, annErr)
	} else {
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

	// A node should be considered "already quarantined" for status reporting
	// if it is already marked as quarantined in our in-memory cache (e.g. it
	// was cordoned manually) even though the FQM annotation is not present
	// yet.
	treatStatusAsAlreadyQuarantined := r.nodeInfo.GetNodeQuarantineStatusCache(event.HealthEvent.NodeName)

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

		labelsMap.Store(cordonedByLabelKey, common.ServiceName)
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

			// update the map here so that later we can refer to it and update the quarantined nodes
			r.nodeInfo.MarkNodeQuarantineStatusCache(event.HealthEvent.NodeName, isNodeQuarantined)

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

	// Override status if node was already cordoned manually (no FQM annotation)
	if treatStatusAsAlreadyQuarantined {
		status = storeconnector.AlreadyQuarantined
	}

	return &status, common.RuleEvaluationNotApplicable
}

// nolint: cyclop //fix this as part of NGCC-21793
func (r *Reconciler) handleQuarantinedNode(
	ctx context.Context,
	event *platformconnectorprotos.HealthEvent,
) bool {
	annotations, err := r.config.K8sClient.GetNodeAnnotations(ctx, event.NodeName)
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

			// Update the quarantinedNodesMap to reflect the node is no longer quarantined
			r.nodeInfo.MarkNodeQuarantineStatusCache(event.NodeName, false)
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
