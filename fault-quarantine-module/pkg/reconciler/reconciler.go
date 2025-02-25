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

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/config"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/fault-quarantine-module/pkg/evaluator"
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
}

type rulesetsConfig struct {
	TaintConfigMap     map[string]*config.Taint
	CordonConfigMap    map[string]bool
	RuleSetPriorityMap map[string]int
}

type Reconciler struct {
	config ReconcilerConfig
}

const (
	// Annotation keys for storing event on node which causes node to be cordoned or tainted
	quarantineHealthEventAnnotationKey                 = "quarantineHealthEvent"
	quarantineHealthEventAppliedTaintsAnnotationKey    = "quarantineHealthEventAppliedTaints"
	quarantineHealthEventIsCordonedAnnotationKey       = "quarantineHealthEventIsCordoned"
	quarantineHealthEventIsCordonedAnnotationValueTrue = "True"

	serviceName = "NVSentinel"
)

var (
	// Label keys
	cordonedByLabelKey        string
	cordonedReasonLabelKey    string
	cordonedTimestampLabelKey string

	uncordonedByLabelKey        string
	uncordonedReasonLabelkey    string
	uncordonedTimestampLabelKey string
)

func NewReconciler(cfg ReconcilerConfig) *Reconciler {
	return &Reconciler{config: cfg}
}

// nolint: cyclop, gocognit //fix this as part of NGCC-21793
func (r *Reconciler) Start(ctx context.Context) {
	ruleSetEvals, err := evaluator.InitializeRuleSetEvaluators(r.config.TomlConfig.RuleSets)
	if err != nil {
		klog.Fatalf("failed to initialize all rule set evaluators: %+v", err)
	}

	labelKeyPrefix := r.config.TomlConfig.LabelPrefix

	cordonedByLabelKey = labelKeyPrefix + "cordon-by"
	cordonedReasonLabelKey = labelKeyPrefix + "cordon-reason"
	cordonedTimestampLabelKey = labelKeyPrefix + "cordon-timestamp"

	uncordonedByLabelKey = labelKeyPrefix + "uncordon-by"
	uncordonedReasonLabelkey = labelKeyPrefix + "uncordon-reason"
	uncordonedTimestampLabelKey = labelKeyPrefix + "uncordon-timestamp"

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

	watcher.Start(ctx)

	quarantinedNodesMap := make(map[string]bool)

	quarantinedNodesList, err := r.config.K8sClient.GetNodesWithAnnotation(ctx, quarantineHealthEventAnnotationKey)
	if err != nil {
		klog.Fatalf("error fetching quarantined nodes: %+v", err)
	}

	for _, node := range quarantinedNodesList {
		quarantinedNodesMap[node] = true
	}

	klog.Infof("Initial quarantinedNodesMap is: %+v", quarantinedNodesMap)

	klog.Info("Listening for events on the channel...")

	for event := range watcher.Events() {
		totalEventsReceived.Inc()

		startTime := time.Now()
		healthEventWithStatus := storeconnector.HealthEventWithStatus{}

		if err := storewatcher.UnmarshalFullDocumentFromEvent(
			event,
			&healthEventWithStatus,
		); err != nil {
			klog.Errorf("Failed to unmarshal event: %+v", err)

			processingErrors.WithLabelValues("unmarshal_error").Inc()

			if err := watcher.MarkProcessed(ctx); err != nil {
				klog.Errorf("Error updating resume token: %+v", err)

				processingErrors.WithLabelValues("mark_processed_error").Inc()
			}

			continue
		}

		isNodeQuarantined := r.handleEvent(
			ctx,
			healthEventWithStatus.HealthEvent,
			ruleSetEvals,
			rulesetsConfig,
			quarantinedNodesMap,
		)

		errFlag := false

		if err := r.updateNodeQuarantineStatus(ctx, healthEventCollection, event, isNodeQuarantined); err != nil {
			klog.Errorf("Error updating Node quarantine status: %+v", err)

			processingErrors.WithLabelValues("update_quarantine_status_error").Inc()

			errFlag = true
		}

		if err := watcher.MarkProcessed(ctx); err != nil {
			klog.Errorf("Error updating resume token: %+v", err)

			processingErrors.WithLabelValues("mark_processed_error").Inc()

			errFlag = true
		}

		if !errFlag {
			totalEventsSuccessfullyProcessed.Inc()
		}

		duration := time.Since(startTime).Seconds()

		eventHandlingDuration.Observe(duration)
	}
}

// nolint: cyclop, gocognit //fix this as part of NGCC-21793
func (r *Reconciler) handleEvent(
	ctx context.Context,
	event *platformconnectorprotos.HealthEvent,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
	quarantinedNodesMap map[string]bool,
) bool {
	if quarantinedNodesMap[event.NodeName] {
		return r.handleQuarantinedNode(ctx, event, quarantinedNodesMap)
	}

	type keyValTaint struct {
		Key   string
		Value string
	}

	var taintAppliedMap sync.Map

	var labelsMap sync.Map

	var isCordoned atomic.Bool

	var taintEffectPriorityMap sync.Map

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

			ok, err := eval.Evaluate(event)
			//nolint //ignore complex nesting blocks //fix this as part of NGCC-21793
			if ok {
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
				klog.Errorf("error while evaluating for event: %+v for ruleset: %+v: %+v", event, eval.GetName(), err)

				processingErrors.WithLabelValues("ruleset_evaluation_error").Inc()

				rulesetFailed.WithLabelValues(eval.GetName()).Inc()
			} else {
				rulesetFailed.WithLabelValues(eval.GetName()).Inc()
			}
		}(eval)
	}

	wg.Wait()

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
			annotationsMap[quarantineHealthEventAppliedTaintsAnnotationKey] = string(taintsJsonStr)
		}
	}

	if isCordoned.Load() {
		// store cordon as an annotation
		annotationsMap[quarantineHealthEventIsCordonedAnnotationKey] = quarantineHealthEventIsCordonedAnnotationValueTrue

		labelsMap.Store(cordonedByLabelKey, serviceName)
		labelsMap.Store(cordonedTimestampLabelKey, time.Now().UTC().Format("2006-01-02T15-04-05Z"))
	}

	isNodeQuarantined := (len(taintsToBeApplied) > 0 || isCordoned.Load())

	//nolint //ignore complex nested block //fix this as part of NGCC-21793
	if isNodeQuarantined {
		eventJsonStr, err := json.Marshal(event)
		if err != nil {
			klog.Errorf("error while marshalling event %+v: %+v", event, err)
		} else {
			annotationsMap[quarantineHealthEventAnnotationKey] = string(eventJsonStr)
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
			event.NodeName,
			taintsToBeApplied,
			isCordoned.Load(),
			annotationsMap,
			labels,
		); err != nil {
			klog.Errorf("error while updating node for event: %+v: %+v", event, err)

			processingErrors.WithLabelValues("taint_and_cordon_error").Inc()

			isNodeQuarantined = false
		} else {
			totalNodesQuarantined.Inc()
			currentQuarantinedNodes.Inc()

			for _, taint := range taintsToBeApplied {
				taintsApplied.WithLabelValues(taint.Key, taint.Effect).Inc()
			}

			if isCordoned.Load() {
				cordonsApplied.Inc()
			}
		}
	}

	// update the map here so that later we can refer to it and update the quarantined nodes
	quarantinedNodesMap[event.NodeName] = isNodeQuarantined

	return isNodeQuarantined
}

// nolint: cyclop //fix this as part of NGCC-21793
func (r *Reconciler) handleQuarantinedNode(
	ctx context.Context,
	event *platformconnectorprotos.HealthEvent,
	quarantinedNodesMap map[string]bool,
) bool {
	annotations, err := r.config.K8sClient.GetNodeAnnotations(ctx, event.NodeName)
	if err != nil {
		klog.Errorf("error while getting node annotations for event: %+v: %+v", event, err)
		processingErrors.WithLabelValues("get_node_annotations_error").Inc()

		return true
	}

	labelsMap := map[string]string{}

	quarantineAnnotationEvent, exists := annotations[quarantineHealthEventAnnotationKey]
	if !exists || quarantineAnnotationEvent == "" {
		// No quarantine annotation found, node is not considered quarantined
		quarantinedNodesMap[event.NodeName] = false
		return false
	}

	//nolint //ignore complexity of nested block //fix this as part of NGCC-21793
	if compareHealthEventWithAnnotationEventToCheckUnQuarantine(event, quarantineAnnotationEvent) {
		// Check if we need to remove taints and remove them
		quarantineAnnotationEventTaintsAppliedStr, taintsExists :=
			annotations[quarantineHealthEventAppliedTaintsAnnotationKey]

		// Check if we need to uncordon
		quarantineAnnotationEventIsCordonStr, cordonExists := annotations[quarantineHealthEventIsCordonedAnnotationKey]

		var taintsToBeRemoved []config.Taint

		annotationsToBeRemoved := []string{}

		isUnCordon := false

		if taintsExists && quarantineAnnotationEventTaintsAppliedStr != "" {
			annotationsToBeRemoved = append(annotationsToBeRemoved, quarantineHealthEventAppliedTaintsAnnotationKey)

			err = json.Unmarshal([]byte(quarantineAnnotationEventTaintsAppliedStr), &taintsToBeRemoved)
			if err != nil {
				klog.Errorf("error while unmarshalling taints annotation %+v for event: %+v: %+v",
					quarantineAnnotationEventTaintsAppliedStr, event, err)

				// Node remains quarantined due to unmarshalling error
				return true
			}
		}

		if cordonExists && quarantineAnnotationEventIsCordonStr == quarantineHealthEventIsCordonedAnnotationValueTrue {
			isUnCordon = true

			annotationsToBeRemoved = append(annotationsToBeRemoved, quarantineHealthEventIsCordonedAnnotationKey)
			labelsMap[uncordonedByLabelKey] = serviceName
			labelsMap[uncordonedTimestampLabelKey] = time.Now().UTC().Format("2006-01-02T15-04-05Z")
		}

		if len(taintsToBeRemoved) > 0 || isUnCordon {
			annotationsToBeRemoved = append(annotationsToBeRemoved, quarantineHealthEventAnnotationKey)

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

			for _, taint := range taintsToBeRemoved {
				taintsRemoved.WithLabelValues(taint.Key, taint.Effect).Inc()
			}

			if isUnCordon {
				cordonsRemoved.Inc()
			}
		}

		// Update the quarantinedNodesMap to reflect the node is no longer quarantined
		quarantinedNodesMap[event.NodeName] = false

		return false
	}

	// If quarantineAnnotationEvent is present but doesn't match the criteria to unquarantine
	return true
}

func (r *Reconciler) updateNodeQuarantineStatus(
	ctx context.Context,
	healthEventCollection *mongo.Collection,
	event bson.M,
	isQuarantined bool,
) error {
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}

	filter := bson.M{"_id": document["_id"]}

	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.nodequarantined": isQuarantined,
		},
	}

	if _, err := healthEventCollection.UpdateOne(ctx, filter, update); err != nil {
		return fmt.Errorf("error updating document with _id: %v, error: %w", document["_id"], err)
	}

	klog.Infof("Document with _id: %v has been updated with status %t", document["_id"], isQuarantined)

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
