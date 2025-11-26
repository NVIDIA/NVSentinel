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
	"errors"
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
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/breaker"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/evaluator"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/eventwatcher"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/healthEventsAnnotation"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	corev1 "k8s.io/api/core/v1"
)

type ReconcilerConfig struct {
	TomlConfig            config.TomlConfig
	DryRun                bool
	CircuitBreakerEnabled bool
	DataStoreConfig       *datastore.DataStoreConfig
	TokenConfig           client.TokenConfig
	DatabasePipeline      interface{}
}

type rulesetsConfig struct {
	TaintConfigMap     map[string]*config.Taint
	CordonConfigMap    map[string]bool
	RuleSetPriorityMap map[string]int
}

// keyValTaint represents a taint key-value pair used for deduplication and priority tracking
type keyValTaint struct {
	Key   string
	Value string
}

type Reconciler struct {
	config                ReconcilerConfig
	k8sClient             *informer.FaultQuarantineClient
	lastProcessedObjectID atomic.Value
	cb                    breaker.CircuitBreaker
	eventWatcher          eventwatcher.EventWatcherInterface
	taintInitKeys         []keyValTaint // Pre-computed taint keys for map initialization
	taintUpdateMu         sync.Mutex    // Protects taint priority updates

	// Label keys
	cordonedByLabelKey        string
	cordonedReasonLabelKey    string
	cordonedTimestampLabelKey string

	uncordonedByLabelKey        string
	uncordonedReasonLabelKey    string
	uncordonedTimestampLabelKey string
}

var (
	// Compile regex once at package initialization for efficiency
	labelValueRegex = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

	// Sentinel errors for better error handling
	errNoQuarantineAnnotation = fmt.Errorf("no quarantine annotation")
)

func NewReconciler(
	cfg ReconcilerConfig,
	k8sClient *informer.FaultQuarantineClient,
	circuitBreaker breaker.CircuitBreaker,
) *Reconciler {
	r := &Reconciler{
		config:    cfg,
		k8sClient: k8sClient,
		cb:        circuitBreaker,
	}

	return r
}

func (r *Reconciler) SetLabelKeys(labelKeyPrefix string) {
	r.cordonedByLabelKey = labelKeyPrefix + "cordon-by"
	r.cordonedReasonLabelKey = labelKeyPrefix + "cordon-reason"
	r.cordonedTimestampLabelKey = labelKeyPrefix + "cordon-timestamp"

	r.uncordonedByLabelKey = labelKeyPrefix + "uncordon-by"
	r.uncordonedReasonLabelKey = labelKeyPrefix + "uncordon-reason"
	r.uncordonedTimestampLabelKey = labelKeyPrefix + "uncordon-timestamp"
}

func (r *Reconciler) StoreLastProcessedObjectID(objID string) {
	r.lastProcessedObjectID.Store(objID)
}

func (r *Reconciler) LoadLastProcessedObjectID() (string, bool) {
	lastObjID := r.lastProcessedObjectID.Load()
	if lastObjID == nil {
		return "", false
	}

	objID, ok := lastObjID.(string)

	return objID, ok
}

func (r *Reconciler) SetEventWatcher(eventWatcher eventwatcher.EventWatcherInterface) {
	r.eventWatcher = eventWatcher
}

func (r *Reconciler) Start(ctx context.Context) error {
	ds, err := datastore.NewDataStore(ctx, *r.config.DataStoreConfig)
	if err != nil {
		return fmt.Errorf("failed to create datastore: %w", err)
	}
	defer ds.Close(ctx)

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

	databaseClient := datastoreAdapter.GetDatabaseClient()

	changeStreamWatcher, err := datastoreAdapter.CreateChangeStreamWatcher(
		ctx, "fault-quarantine", r.config.DatabasePipeline)
	if err != nil {
		return fmt.Errorf("failed to create change stream watcher: %w", err)
	}

	// Unwrap to get client.ChangeStreamWatcher for EventWatcher compatibility
	type unwrapper interface {
		Unwrap() client.ChangeStreamWatcher
	}

	unwrapable, ok := changeStreamWatcher.(unwrapper)
	if !ok {
		return fmt.Errorf("watcher does not support unwrapping to client.ChangeStreamWatcher")
	}

	oldWatcher := unwrapable.Unwrap()

	// Create event watcher with the new signature
	r.eventWatcher = eventwatcher.NewEventWatcher(
		oldWatcher,
		databaseClient,
		time.Second*30, // 30 second metric update interval
		r,              // Reconciler implements LastProcessedObjectIDStore interface
	)

	r.setupNodeInformerCallbacks()

	ruleSetEvals, err := r.initializeRuleSetEvaluators()
	if err != nil {
		return fmt.Errorf("failed to initialize rule set evaluators: %w", err)
	}

	r.setupLabelKeys()

	rulesetsConfig := r.buildRulesetsConfig()

	r.precomputeTaintInitKeys(ruleSetEvals, rulesetsConfig)

	if !r.k8sClient.NodeInformer.WaitForSync(ctx) {
		return fmt.Errorf("failed to sync NodeInformer cache")
	}

	r.initializeQuarantineMetrics()

	if err := r.checkCircuitBreakerAtStartup(ctx); err != nil {
		return err
	}

	r.eventWatcher.SetProcessEventCallback(
		func(ctx context.Context, event *model.HealthEventWithStatus) *model.Status {
			return r.ProcessEvent(ctx, event, ruleSetEvals, rulesetsConfig)
		},
	)

	if err := r.eventWatcher.Start(ctx); err != nil {
		return fmt.Errorf("event watcher failed: %w", err)
	}

	slog.Info("Event watcher stopped, exiting fault-quarantine reconciler.")

	return nil
}

// setupNodeInformerCallbacks configures callbacks on the already-created node informer
func (r *Reconciler) setupNodeInformerCallbacks() {
	r.k8sClient.NodeInformer.SetOnQuarantinedNodeDeletedCallback(func(nodeName string) {
		metrics.CurrentQuarantinedNodes.WithLabelValues(nodeName).Set(0)
		slog.Info("Set currentQuarantinedNodes to 0 for deleted quarantined node", "node", nodeName)
	})

	r.k8sClient.NodeInformer.SetOnManualUncordonCallback(r.handleManualUncordon)
}

// initializeRuleSetEvaluators initializes all rule set evaluators from config
func (r *Reconciler) initializeRuleSetEvaluators() ([]evaluator.RuleSetEvaluatorIface, error) {
	ruleSetEvals, err := evaluator.InitializeRuleSetEvaluators(r.config.TomlConfig.RuleSets, r.k8sClient.NodeInformer)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize all rule set evaluators: %w", err)
	}

	return ruleSetEvals, nil
}

// setupLabelKeys configures label keys for cordon/uncordon tracking
func (r *Reconciler) setupLabelKeys() {
	r.SetLabelKeys(r.config.TomlConfig.LabelPrefix)
	r.k8sClient.SetLabelKeys(r.cordonedReasonLabelKey, r.uncordonedReasonLabelKey)
}

// buildRulesetsConfig builds the rulesets configuration maps from TOML config
func (r *Reconciler) buildRulesetsConfig() rulesetsConfig {
	taintConfigMap := make(map[string]*config.Taint)
	cordonConfigMap := make(map[string]bool)
	ruleSetPriorityMap := make(map[string]int)

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

	return rulesetsConfig{
		TaintConfigMap:     taintConfigMap,
		CordonConfigMap:    cordonConfigMap,
		RuleSetPriorityMap: ruleSetPriorityMap,
	}
}

// precomputeTaintInitKeys pre-computes taint keys from rulesets for efficient map initialization
func (r *Reconciler) precomputeTaintInitKeys(
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
) {
	r.taintInitKeys = make([]keyValTaint, 0, len(ruleSetEvals))

	for _, eval := range ruleSetEvals {
		taintConfig := rulesetsConfig.TaintConfigMap[eval.GetName()]
		if taintConfig != nil {
			keyVal := keyValTaint{
				Key:   taintConfig.Key,
				Value: taintConfig.Value,
			}
			r.taintInitKeys = append(r.taintInitKeys, keyVal)
		}
	}

	slog.Info("Pre-computed taint initialization keys", "count", len(r.taintInitKeys))
}

// initializeQuarantineMetrics initializes metrics for already quarantined nodes
func (r *Reconciler) initializeQuarantineMetrics() {
	totalNodes, quarantinedNodesMap, err := r.k8sClient.NodeInformer.GetNodeCounts()
	if err != nil {
		slog.Error("Failed to get initial node counts", "error", err)
		return
	}

	for nodeName := range quarantinedNodesMap {
		metrics.CurrentQuarantinedNodes.WithLabelValues(nodeName).Set(1)
	}

	slog.Info("Initial state", "totalNodes", totalNodes, "quarantinedNodes", len(quarantinedNodesMap),
		"quarantinedNodesMap", quarantinedNodesMap)
}

// checkCircuitBreakerAtStartup checks if circuit breaker is tripped at startup
// Returns error if retry exhaustion occurs (should restart pod)
// Blocks indefinitely if circuit breaker is tripped (wait for manual intervention)
func (r *Reconciler) checkCircuitBreakerAtStartup(ctx context.Context) error {
	if !r.config.CircuitBreakerEnabled {
		return nil
	}

	tripped, err := r.cb.IsTripped(ctx)
	if err != nil {
		if errors.Is(err, breaker.ErrRetryExhausted) {
			return err
		}

		slog.Error("Error checking if circuit breaker is tripped", "error", err)
		<-ctx.Done()

		return fmt.Errorf("circuit breaker check failed: %w", err)
	}

	if tripped {
		slog.Error("Fault Quarantine circuit breaker is TRIPPED. Halting event dequeuing indefinitely.")
		<-ctx.Done()

		return fmt.Errorf("circuit breaker is TRIPPED at startup")
	}

	slog.Info("Listening for events on the channel...")

	return nil
}

// ProcessEvent processes a single health event
func (r *Reconciler) ProcessEvent(
	ctx context.Context,
	event *model.HealthEventWithStatus,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
) *model.Status {
	if shouldHalt := r.checkCircuitBreakerAndHalt(ctx); shouldHalt {
		return nil
	}

	slog.Debug("Processing event", "checkName", event.HealthEvent.CheckName)

	isNodeQuarantined := r.handleEvent(ctx, event, ruleSetEvals, rulesetsConfig)

	if isNodeQuarantined == nil {
		slog.Debug("Skipped processing event for node, no status update needed", "node", event.HealthEvent.NodeName)
	} else if *isNodeQuarantined == model.Quarantined ||
		*isNodeQuarantined == model.UnQuarantined ||
		*isNodeQuarantined == model.AlreadyQuarantined {
		metrics.TotalEventsSuccessfullyProcessed.Inc()
	}

	return isNodeQuarantined
}

// checkCircuitBreakerAndHalt checks if circuit breaker is tripped and returns true if processing should halt
func (r *Reconciler) checkCircuitBreakerAndHalt(ctx context.Context) bool {
	if !r.config.CircuitBreakerEnabled {
		return false
	}

	tripped, err := r.cb.IsTripped(ctx)
	if err != nil {
		slog.Error("Error checking if circuit breaker is tripped", "error", err)
		<-ctx.Done()

		return true
	}

	if tripped {
		slog.Error("Circuit breaker TRIPPED. Halting event processing until restart and breaker reset.")
		<-ctx.Done()

		return true
	}

	return false
}

func (r *Reconciler) handleEvent(
	ctx context.Context,
	event *model.HealthEventWithStatus,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
) *model.Status {
	annotations, quarantineAnnotationExists := r.hasExistingQuarantine(event.HealthEvent.NodeName)

	if quarantineAnnotationExists {
		return r.handleAlreadyQuarantinedNode(ctx, event.HealthEvent, ruleSetEvals)
	}

	// For healthy events, if there's no existing quarantine annotation,
	// skip processing as there's no transition from unhealthy to healthy
	if event.HealthEvent.IsHealthy {
		slog.Info("Skipping healthy event for node as there's no existing quarantine annotation",
			"node", event.HealthEvent.NodeName, "event", event.HealthEvent)

		return nil
	}

	taintAppliedMap := make(map[keyValTaint]string, len(r.taintInitKeys))
	taintEffectPriorityMap := make(map[keyValTaint]int, len(r.taintInitKeys))

	for _, keyVal := range r.taintInitKeys {
		taintAppliedMap[keyVal] = ""
		taintEffectPriorityMap[keyVal] = -1
	}

	var labelsMap sync.Map

	var isCordoned atomic.Bool

	r.evaluateRulesets(
		event, ruleSetEvals, rulesetsConfig,
		taintAppliedMap, &labelsMap, &isCordoned, taintEffectPriorityMap,
	)

	taintsToBeApplied := r.collectTaintsToApply(taintAppliedMap)

	annotationsMap := r.prepareAnnotations(taintsToBeApplied, &labelsMap, &isCordoned)

	isNodeQuarantined := len(taintsToBeApplied) > 0 || isCordoned.Load()

	// In dry-run mode, always apply annotations for observability even if no actions would be taken
	if !isNodeQuarantined && !r.config.DryRun {
		return nil
	}

	status := r.applyQuarantine(ctx, event, annotations, taintsToBeApplied, annotationsMap, &labelsMap, &isCordoned)

	return status
}

func (r *Reconciler) hasExistingQuarantine(nodeName string) (map[string]string, bool) {
	annotations, err := r.getNodeQuarantineAnnotations(nodeName)
	if err != nil {
		slog.Error("Failed to fetch annotations for node", "node", nodeName, "error", err)
		return make(map[string]string), false
	}

	if annotations == nil {
		return make(map[string]string), false
	}

	annotationVal, exists := annotations[common.QuarantineHealthEventAnnotationKey]

	return annotations, exists && annotationVal != ""
}

// handleAlreadyQuarantinedNode handles events for nodes that are already quarantined
func (r *Reconciler) handleAlreadyQuarantinedNode(
	ctx context.Context,
	event *protos.HealthEvent,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
) *model.Status {
	healthEventsAnnotationMap, _, err := r.getHealthEventsFromAnnotation(event)

	// Only propagate events to ND/FR if they will modify node annotations
	// Returning nil prevents unnecessary MongoDB writes and downstream processing
	switch {
	case err != nil:
		if errors.Is(err, errNoQuarantineAnnotation) {
			return nil
		}

		metrics.ProcessingErrors.WithLabelValues("get_node_annotations_error").Inc()
		// Cannot proceed without valid annotation data
		return nil
	case event.IsHealthy:
		_, hasExistingCheck := healthEventsAnnotationMap.GetEvent(event)
		if !hasExistingCheck {
			return nil
		}
	case !r.isForceQuarantine(event) && !r.eventMatchesAnyRule(event, ruleSetEvals):
		return nil
	}

	// Event will modify FQ annotations, proceed with quarantine handling
	stayQuarantined := r.handleQuarantinedNode(ctx, event, ruleSetEvals)

	// Partial recovery: healthy event that doesn't fully unquarantine the node should
	// not be propagated to ND/FR
	if event.IsHealthy && stayQuarantined {
		return nil
	}

	var status model.Status
	if stayQuarantined {
		// Only for an unhealthy event, set status to AlreadyQuarantined and
		// propagate to ND/FR
		status = model.AlreadyQuarantined
	} else {
		status = model.UnQuarantined
	}

	return &status
}

// evaluateRulesets evaluates all rulesets against the health event in parallel
func (r *Reconciler) evaluateRulesets(
	event *model.HealthEventWithStatus,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
	taintAppliedMap map[keyValTaint]string,
	labelsMap *sync.Map,
	isCordoned *atomic.Bool,
	taintEffectPriorityMap map[keyValTaint]int,
) {
	// Handle quarantine override (force quarantine without rule evaluation)
	if event.HealthEvent.QuarantineOverrides != nil && event.HealthEvent.QuarantineOverrides.Force {
		isCordoned.Store(true)

		creatorID := event.HealthEvent.Metadata["creator_id"]
		labelsMap.LoadOrStore(r.cordonedByLabelKey, event.HealthEvent.Agent+"-"+creatorID)
		labelsMap.Store(r.cordonedReasonLabelKey,
			formatCordonOrUncordonReasonValue(event.HealthEvent.Message, 63))

		return
	}

	var wg sync.WaitGroup

	for _, eval := range ruleSetEvals {
		wg.Add(1)

		go func(eval evaluator.RuleSetEvaluatorIface) {
			defer wg.Done()

			slog.Info("Handling event for ruleset", "event", event, "ruleset", eval.GetName())

			ruleEvaluatedResult, err := eval.Evaluate(event.HealthEvent)

			switch {
			case ruleEvaluatedResult == common.RuleEvaluationSuccess:
				r.handleSuccessfulRuleEvaluation(
					eval, rulesetsConfig, labelsMap, isCordoned, taintAppliedMap, taintEffectPriorityMap)
			case err != nil:
				r.handleRuleEvaluationError(event.HealthEvent, eval.GetName(), err)
			default:
				metrics.RulesetEvaluations.WithLabelValues(eval.GetName(), metrics.StatusFailed).Inc()
			}
		}(eval)
	}

	wg.Wait()
}

// handleSuccessfulRuleEvaluation processes a successful rule evaluation result
func (r *Reconciler) handleSuccessfulRuleEvaluation(
	eval evaluator.RuleSetEvaluatorIface,
	rulesetsConfig rulesetsConfig,
	labelsMap *sync.Map,
	isCordoned *atomic.Bool,
	taintAppliedMap map[keyValTaint]string,
	taintEffectPriorityMap map[keyValTaint]int,
) {
	metrics.RulesetEvaluations.WithLabelValues(eval.GetName(), metrics.StatusPassed).Inc()

	shouldCordon := rulesetsConfig.CordonConfigMap[eval.GetName()]
	if shouldCordon {
		isCordoned.Store(true)

		newCordonReason := eval.GetName()

		if oldReasonVal, exist := labelsMap.Load(r.cordonedReasonLabelKey); exist {
			oldCordonReason := oldReasonVal.(string)
			newCordonReason = oldCordonReason + "-" + newCordonReason
		}

		labelsMap.Store(r.cordonedReasonLabelKey, formatCordonOrUncordonReasonValue(newCordonReason, 63))
	}

	taintConfig := rulesetsConfig.TaintConfigMap[eval.GetName()]
	if taintConfig != nil {
		r.updateTaintMaps(eval.GetName(), taintConfig, rulesetsConfig, taintAppliedMap, taintEffectPriorityMap)
	}
}

// updateTaintMaps updates taint maps with priority-based logic to handle multiple rulesets
// affecting the same taint key-value pair.
func (r *Reconciler) updateTaintMaps(
	evalName string,
	taintConfig *config.Taint,
	rulesetsConfig rulesetsConfig,
	taintAppliedMap map[keyValTaint]string,
	taintEffectPriorityMap map[keyValTaint]int,
) {
	keyVal := keyValTaint{Key: taintConfig.Key, Value: taintConfig.Value}
	newPriority := rulesetsConfig.RuleSetPriorityMap[evalName]

	r.taintUpdateMu.Lock()
	defer r.taintUpdateMu.Unlock()

	currentEffect := taintAppliedMap[keyVal]
	currentPriority := taintEffectPriorityMap[keyVal]

	if currentEffect == "" || newPriority > currentPriority {
		taintEffectPriorityMap[keyVal] = newPriority
		taintAppliedMap[keyVal] = taintConfig.Effect
	}
}

// handleRuleEvaluationError handles errors during rule evaluation
func (r *Reconciler) handleRuleEvaluationError(
	event *protos.HealthEvent,
	evalName string,
	err error,
) {
	slog.Error("Rule evaluation failed", "ruleset", evalName, "node", event.NodeName, "error", err)
	metrics.ProcessingErrors.WithLabelValues("ruleset_evaluation_error").Inc()
	metrics.RulesetEvaluations.WithLabelValues(evalName, metrics.StatusFailed).Inc()
}

// collectTaintsToApply collects all taints that should be applied from the taint map
func (r *Reconciler) collectTaintsToApply(taintAppliedMap map[keyValTaint]string) []config.Taint {
	taintsToBeApplied := make([]config.Taint, 0, len(taintAppliedMap))

	for keyVal, effect := range taintAppliedMap {
		if effect != "" {
			taintsToBeApplied = append(taintsToBeApplied, config.Taint{
				Key:    keyVal.Key,
				Value:  keyVal.Value,
				Effect: effect,
			})
		}
	}

	return taintsToBeApplied
}

// prepareAnnotations prepares annotations and labels to be applied if any
func (r *Reconciler) prepareAnnotations(
	taintsToBeApplied []config.Taint,
	labelsMap *sync.Map,
	isCordoned *atomic.Bool,
) map[string]string {
	annotationsMap := map[string]string{}

	if len(taintsToBeApplied) > 0 {
		taintsJsonStr, err := json.Marshal(taintsToBeApplied)
		if err != nil {
			slog.Error("Failed to marshal taints for annotation", "error", err)
		} else {
			annotationsMap[common.QuarantineHealthEventAppliedTaintsAnnotationKey] = string(taintsJsonStr)
		}
	}

	if isCordoned.Load() {
		annotationsMap[common.QuarantineHealthEventIsCordonedAnnotationKey] =
			common.QuarantineHealthEventIsCordonedAnnotationValueTrue

		labelsMap.LoadOrStore(r.cordonedByLabelKey, common.ServiceName)
		labelsMap.Store(r.cordonedTimestampLabelKey, time.Now().UTC().Format("2006-01-02T15-04-05Z"))
		labelsMap.Store(string(statemanager.NVSentinelStateLabelKey), string(statemanager.QuarantinedLabelValue))
	}

	return annotationsMap
}

// applyQuarantine applies quarantine actions to a node (taints, cordon, annotations)
func (r *Reconciler) applyQuarantine(
	ctx context.Context,
	event *model.HealthEventWithStatus,
	annotations map[string]string,
	taintsToBeApplied []config.Taint,
	annotationsMap map[string]string,
	labelsMap *sync.Map,
	isCordoned *atomic.Bool,
) *model.Status {
	r.recordCordonEventInCircuitBreaker(event)

	healthEvents := healthEventsAnnotation.NewHealthEventsAnnotationMap()
	updated := healthEvents.AddOrUpdateEvent(event.HealthEvent)

	if !updated {
		slog.Info("Health event already exists for node, skipping quarantine", "node", event.HealthEvent.NodeName)

		return nil
	}

	if err := r.addHealthEventAnnotation(healthEvents, annotationsMap); err != nil {
		slog.Error("Failed to add health event annotation", "error", err, "node", event.HealthEvent.NodeName)

		return nil
	}

	slog.Debug("Added health event annotation successfully", "node", event.HealthEvent.NodeName)

	// Remove manual uncordon annotation if present before applying new quarantine
	r.cleanupManualUncordonAnnotation(ctx, event.HealthEvent.NodeName, annotations)

	if !r.config.CircuitBreakerEnabled {
		slog.Info("Circuit breaker is disabled, proceeding with quarantine action without protection",
			"node", event.HealthEvent.NodeName)
	}

	// Convert sync.Map to regular map for K8s API call
	labels := make(map[string]string)

	labelsMap.Range(func(key, value any) bool {
		if strKey, ok := key.(string); ok {
			if strValue, ok := value.(string); ok {
				labels[strKey] = strValue
			}
		}

		return true
	})

	err := r.k8sClient.QuarantineNodeAndSetAnnotations(
		ctx,
		event.HealthEvent.NodeName,
		taintsToBeApplied,
		isCordoned.Load(),
		annotationsMap,
		labels,
	)
	if err != nil {
		slog.Error("Failed to taint and cordon node", "node", event.HealthEvent.NodeName, "error", err)
		metrics.ProcessingErrors.WithLabelValues("taint_and_cordon_error").Inc()

		return nil
	}

	slog.Debug("QuarantineNodeAndSetAnnotations completed successfully", "node", event.HealthEvent.NodeName)

	r.updateQuarantineMetrics(event.HealthEvent.NodeName, taintsToBeApplied, isCordoned)

	status := model.Quarantined

	return &status
}

// recordCordonEventInCircuitBreaker records a cordon event in the circuit breaker if enabled
func (r *Reconciler) recordCordonEventInCircuitBreaker(event *model.HealthEventWithStatus) {
	if r.config.CircuitBreakerEnabled &&
		(event.HealthEvent.QuarantineOverrides == nil || !event.HealthEvent.QuarantineOverrides.Force) {
		r.cb.AddCordonEvent(event.HealthEvent.NodeName)
	}
}

// addHealthEventAnnotation adds health event annotation to the annotations map
func (r *Reconciler) addHealthEventAnnotation(
	healthEvents *healthEventsAnnotation.HealthEventsAnnotationMap,
	annotationsMap map[string]string,
) error {
	eventJsonStr, err := json.Marshal(healthEvents)
	if err != nil {
		return fmt.Errorf("failed to marshal health events: %w", err)
	}

	annotationsMap[common.QuarantineHealthEventAnnotationKey] = string(eventJsonStr)

	return nil
}

// updateQuarantineMetrics updates Prometheus metrics after quarantining a node
func (r *Reconciler) updateQuarantineMetrics(
	nodeName string,
	taintsToBeApplied []config.Taint,
	isCordoned *atomic.Bool,
) {
	metrics.TotalNodesQuarantined.WithLabelValues(nodeName).Inc()
	metrics.CurrentQuarantinedNodes.WithLabelValues(nodeName).Set(1)

	for _, taint := range taintsToBeApplied {
		metrics.TaintsApplied.WithLabelValues(taint.Key, taint.Effect).Inc()
	}

	if isCordoned.Load() {
		metrics.CordonsApplied.Inc()
	}
}

// eventMatchesAnyRule checks if an event matches at least one configured ruleset
func (r *Reconciler) eventMatchesAnyRule(
	event *protos.HealthEvent,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
) bool {
	for _, eval := range ruleSetEvals {
		result, err := eval.Evaluate(event)
		if err != nil {
			continue
		}

		if result == common.RuleEvaluationSuccess {
			return true
		}
	}

	return false
}

// isForceQuarantine checks if the event has the force quarantine override set
func (r *Reconciler) isForceQuarantine(event *protos.HealthEvent) bool {
	return event.QuarantineOverrides != nil && event.QuarantineOverrides.Force
}

// handleUnhealthyEventOnQuarantinedNode handles unhealthy events on already-quarantined nodes
func (r *Reconciler) handleUnhealthyEventOnQuarantinedNode(
	ctx context.Context,
	event *protos.HealthEvent,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
	healthEventsAnnotationMap *healthEventsAnnotation.HealthEventsAnnotationMap,
) bool {
	if !r.isForceQuarantine(event) && !r.eventMatchesAnyRule(event, ruleSetEvals) {
		slog.Info("Unhealthy event on node doesn't match any rules, skipping annotation update",
			"checkName", event.CheckName, "node", event.NodeName)

		return true
	}

	added := healthEventsAnnotationMap.AddOrUpdateEvent(event)

	if added {
		slog.Info("Added entity failures for check on node",
			"checkName", event.CheckName, "node", event.NodeName, "totalTrackedEntities", healthEventsAnnotationMap.Count())

		if err := r.addEventToAnnotation(ctx, event); err != nil {
			slog.Error("Failed to update health events annotation", "error", err)
			return true
		}
	} else {
		slog.Debug("All entities already tracked for check on node",
			"checkName", event.CheckName, "node", event.NodeName)
	}

	return true
}

func (r *Reconciler) handleQuarantinedNode(
	ctx context.Context,
	event *protos.HealthEvent,
	ruleSetEvals []evaluator.RuleSetEvaluatorIface,
) bool {
	healthEventsAnnotationMap, annotations, err := r.getHealthEventsFromAnnotation(event)
	if err != nil {
		metrics.ProcessingErrors.WithLabelValues("get_node_annotations_error").Inc()
		return !errors.Is(err, errNoQuarantineAnnotation)
	}

	_, hasExistingCheck := healthEventsAnnotationMap.GetEvent(event)

	if !event.IsHealthy {
		return r.handleUnhealthyEventOnQuarantinedNode(ctx, event, ruleSetEvals, healthEventsAnnotationMap)
	}

	if !hasExistingCheck {
		slog.Debug("Received healthy event for untracked check (other checks may still be failing)",
			"check", event.CheckName,
			"node", event.NodeName)

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
		slog.Debug("No matching entities to remove for check on node",
			"check", event.CheckName,
			"node", event.NodeName)
	}

	updatedHealthEventsMap, err := r.removeEventFromAnnotation(ctx, event)
	if err != nil {
		slog.Error("Failed to update health events annotation after recovery", "error", err)
		return true
	}

	if updatedHealthEventsMap.IsEmpty() {
		slog.Info("All health checks recovered for node, proceeding with uncordon",
			"node", event.NodeName)

		return r.performUncordon(ctx, event, annotations)
	}

	slog.Info("Node remains quarantined with failing checks",
		"node", event.NodeName,
		"failingChecksCount", updatedHealthEventsMap.Count(),
		"checks", updatedHealthEventsMap.GetAllCheckNames())

	return true
}

func (r *Reconciler) getHealthEventsFromAnnotation(
	event *protos.HealthEvent,
) (*healthEventsAnnotation.HealthEventsAnnotationMap, map[string]string, error) {
	annotations, err := r.getNodeQuarantineAnnotations(event.NodeName)
	if err != nil {
		slog.Error("Failed to get node annotations for node", "node", event.NodeName, "error", err)
		metrics.ProcessingErrors.WithLabelValues("get_node_annotations_error").Inc()

		return nil, nil, fmt.Errorf("failed to get annotations: %w", err)
	}

	quarantineAnnotationStr, exists := annotations[common.QuarantineHealthEventAnnotationKey]
	if !exists || quarantineAnnotationStr == "" {
		slog.Info("No quarantine annotation found for node", "node", event.NodeName)
		return nil, nil, errNoQuarantineAnnotation
	}

	// Try to unmarshal as HealthEventsAnnotationMap first
	var healthEventsMap healthEventsAnnotation.HealthEventsAnnotationMap

	err = json.Unmarshal([]byte(quarantineAnnotationStr), &healthEventsMap)
	if err != nil {
		var singleHealthEvent protos.HealthEvent

		if err2 := json.Unmarshal([]byte(quarantineAnnotationStr), &singleHealthEvent); err2 == nil {
			slog.Info("Found old format annotation for node, converting locally", "node", event.NodeName)

			healthEventsMap = *healthEventsAnnotation.NewHealthEventsAnnotationMap()
			healthEventsMap.AddOrUpdateEvent(&singleHealthEvent)
		} else {
			return nil, nil, fmt.Errorf("failed to unmarshal annotation for node %s: %w", event.NodeName, err)
		}
	}

	return &healthEventsMap, annotations, nil
}

// addEventToAnnotation adds or updates a health event in the node's quarantine annotation
func (r *Reconciler) addEventToAnnotation(
	ctx context.Context,
	event *protos.HealthEvent,
) error {
	updateFn := func(node *corev1.Node) error {
		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		healthEventsMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
		existingAnnotation := node.Annotations[common.QuarantineHealthEventAnnotationKey]

		if existingAnnotation != "" {
			if err := json.Unmarshal([]byte(existingAnnotation), healthEventsMap); err != nil {
				var singleEvent protos.HealthEvent
				if err2 := json.Unmarshal([]byte(existingAnnotation), &singleEvent); err2 == nil {
					healthEventsMap.AddOrUpdateEvent(&singleEvent)
				} else {
					return fmt.Errorf("failed to parse existing annotation (tried both formats): %w", err)
				}
			}
		}

		added := healthEventsMap.AddOrUpdateEvent(event)
		if !added {
			slog.Debug("Event already exists for node, no annotation update needed", "node", event.NodeName)
			return nil
		}

		annotationBytes, err := json.Marshal(healthEventsMap)
		if err != nil {
			return fmt.Errorf("failed to marshal health events: %w", err)
		}

		node.Annotations[common.QuarantineHealthEventAnnotationKey] = string(annotationBytes)

		slog.Debug("Added/updated event for node", "node", event.NodeName, "totalEntityLevelEvents", healthEventsMap.Count())

		return nil
	}

	return r.k8sClient.UpdateNode(ctx, event.NodeName, updateFn)
}

// removeEventFromAnnotation removes entities from a health event in the node's quarantine annotation
// Returns the updated healthEventsAnnotationMap from fresh K8s data and any error
func (r *Reconciler) removeEventFromAnnotation(
	ctx context.Context,
	event *protos.HealthEvent,
) (*healthEventsAnnotation.HealthEventsAnnotationMap, error) {
	// Capture the updated map based on the ACTUAL state after removal
	updatedMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()

	updateFn := func(node *corev1.Node) error {
		if node.Annotations == nil {
			updatedMap = healthEventsAnnotation.NewHealthEventsAnnotationMap()
			return nil
		}

		existingAnnotation, exists := node.Annotations[common.QuarantineHealthEventAnnotationKey]
		if !exists || existingAnnotation == "" {
			updatedMap = healthEventsAnnotation.NewHealthEventsAnnotationMap()
			return nil
		}

		healthEventsMap := healthEventsAnnotation.NewHealthEventsAnnotationMap()
		if err := json.Unmarshal([]byte(existingAnnotation), healthEventsMap); err != nil {
			var singleEvent protos.HealthEvent
			if err2 := json.Unmarshal([]byte(existingAnnotation), &singleEvent); err2 == nil {
				healthEventsMap.AddOrUpdateEvent(&singleEvent)
			} else {
				return fmt.Errorf("failed to parse existing annotation (tried both formats): %w", err)
			}
		}

		removed := healthEventsMap.RemoveEvent(event)
		if removed == 0 {
			slog.Debug("No matching entities to remove for node, no annotation update needed", "node", event.NodeName)

			updatedMap = healthEventsMap

			return nil
		}

		annotationBytes, err := json.Marshal(healthEventsMap)
		if err != nil {
			return fmt.Errorf("failed to marshal health events after removal: %w", err)
		}

		node.Annotations[common.QuarantineHealthEventAnnotationKey] = string(annotationBytes)

		slog.Debug("Removed entities for node", "node", event.NodeName, "remainingEntityLevelEvents", healthEventsMap.Count())

		updatedMap = healthEventsMap

		return nil
	}

	err := r.k8sClient.UpdateNode(ctx, event.NodeName, updateFn)

	return updatedMap, err
}

func (r *Reconciler) performUncordon(
	ctx context.Context,
	event *protos.HealthEvent,
	annotations map[string]string,
) bool {
	slog.Info("All entities recovered for check - proceeding with uncordon",
		"check", event.CheckName,
		"node", event.NodeName)

	// Prepare uncordon parameters
	taintsToBeRemoved, annotationsToBeRemoved, isUnCordon, labelsMap, err := r.prepareUncordonParams(
		event, annotations)
	if err != nil {
		slog.Error("Failed to prepare uncordon params for node", "node", event.NodeName, "error", err)
		return true
	}

	if len(taintsToBeRemoved) == 0 && !isUnCordon {
		return false
	}

	if !isUnCordon {
		slog.Warn("Node is not cordoned but has quarantine taints/annotations, proceeding with cleanup",
			"node", event.NodeName)
	}

	annotationsToBeRemoved = append(annotationsToBeRemoved, common.QuarantineHealthEventAnnotationKey)

	if !r.config.CircuitBreakerEnabled {
		slog.Info("Circuit breaker is disabled, proceeding with unquarantine action for node", "node", event.NodeName)
	}

	labelsToRemove := []string{
		r.cordonedByLabelKey,
		r.cordonedReasonLabelKey,
		r.cordonedTimestampLabelKey,
		statemanager.NVSentinelStateLabelKey,
	}

	if err := r.k8sClient.UnQuarantineNodeAndRemoveAnnotations(
		ctx,
		event.NodeName,
		taintsToBeRemoved,
		annotationsToBeRemoved,
		labelsToRemove,
		labelsMap,
	); err != nil {
		slog.Error("Failed to untaint and uncordon node", "node", event.NodeName, "error", err)
		metrics.ProcessingErrors.WithLabelValues("untaint_and_uncordon_error").Inc()

		return true
	}

	r.updateUncordonMetrics(event.NodeName, taintsToBeRemoved, isUnCordon)

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

	quarantineAnnotationEventTaintsAppliedStr, taintsExists :=
		annotations[common.QuarantineHealthEventAppliedTaintsAnnotationKey]
	if taintsExists && quarantineAnnotationEventTaintsAppliedStr != "" {
		annotationsToBeRemoved = append(annotationsToBeRemoved,
			common.QuarantineHealthEventAppliedTaintsAnnotationKey)

		err := json.Unmarshal([]byte(quarantineAnnotationEventTaintsAppliedStr), &taintsToBeRemoved)
		if err != nil {
			return nil, nil, false, nil, fmt.Errorf("failed to unmarshal taints annotation for node %s: %w", event.NodeName, err)
		}
	}

	quarantineAnnotationEventIsCordonStr, cordonExists :=
		annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]
	if cordonExists && quarantineAnnotationEventIsCordonStr == common.QuarantineHealthEventIsCordonedAnnotationValueTrue {
		isUnCordon = true

		annotationsToBeRemoved = append(annotationsToBeRemoved,
			common.QuarantineHealthEventIsCordonedAnnotationKey)
		labelsMap[r.uncordonedByLabelKey] = common.ServiceName
		labelsMap[r.uncordonedTimestampLabelKey] = time.Now().UTC().Format("2006-01-02T15-04-05Z")
	}

	return taintsToBeRemoved, annotationsToBeRemoved, isUnCordon, labelsMap, nil
}

func (r *Reconciler) updateUncordonMetrics(
	nodeName string,
	taintsToBeRemoved []config.Taint,
	isUnCordon bool,
) {
	metrics.TotalNodesUnquarantined.WithLabelValues(nodeName).Inc()
	metrics.CurrentQuarantinedNodes.WithLabelValues(nodeName).Set(0)
	slog.Info("Set currentQuarantinedNodes to 0 for unquarantined node", "node", nodeName)

	for _, taint := range taintsToBeRemoved {
		metrics.TaintsRemoved.WithLabelValues(taint.Key, taint.Effect).Inc()
	}

	if isUnCordon {
		metrics.CordonsRemoved.Inc()
	}
}

func formatCordonOrUncordonReasonValue(input string, length int) string {
	formatted := labelValueRegex.ReplaceAllString(input, "-")

	if len(formatted) > length {
		formatted = formatted[:length]
	}

	// Ensure it starts and ends with an alphanumeric character
	formatted = strings.Trim(formatted, "-")

	return formatted
}

// getNodeQuarantineAnnotations retrieves quarantine annotations from the informer cache
func (r *Reconciler) getNodeQuarantineAnnotations(nodeName string) (map[string]string, error) {
	node, err := r.k8sClient.NodeInformer.GetNode(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node from cache: %w", err)
	}

	// Extract only quarantine annotations
	quarantineAnnotations := make(map[string]string)
	quarantineKeys := []string{
		common.QuarantineHealthEventAnnotationKey,
		common.QuarantineHealthEventAppliedTaintsAnnotationKey,
		common.QuarantineHealthEventIsCordonedAnnotationKey,
		common.QuarantinedNodeUncordonedManuallyAnnotationKey,
	}

	if node.Annotations != nil {
		for _, key := range quarantineKeys {
			if value, exists := node.Annotations[key]; exists {
				quarantineAnnotations[key] = value
			}
		}
	}

	slog.Debug("Retrieved quarantine annotations for node from informer cache", "node", nodeName)

	return quarantineAnnotations, nil
}

func (r *Reconciler) cleanupManualUncordonAnnotation(ctx context.Context, nodeName string,
	annotations map[string]string) {
	if _, hasManualUncordon := annotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey]; hasManualUncordon {
		slog.Info("Removing manual uncordon annotation from node before applying new quarantine", "node", nodeName)

		updateFn := func(node *corev1.Node) error {
			if node.Annotations == nil {
				slog.Debug("Node has no annotations, manual uncordon annotation already absent", "node", nodeName)
				return nil
			}

			if _, exists := node.Annotations[common.QuarantinedNodeUncordonedManuallyAnnotationKey]; !exists {
				slog.Debug("Manual uncordon annotation already removed from node", "node", nodeName)
				return nil
			}

			delete(node.Annotations, common.QuarantinedNodeUncordonedManuallyAnnotationKey)

			return nil
		}

		if err := r.k8sClient.UpdateNode(ctx, nodeName, updateFn); err != nil {
			slog.Error("Failed to remove manual uncordon annotation from node", "node", nodeName, "error", err)
		}
	}
}

// handleManualUncordon handles the case when a node is manually uncordoned while having FQ annotations
func (r *Reconciler) handleManualUncordon(nodeName string) error {
	annotations, err := r.getNodeQuarantineAnnotations(nodeName)
	if err != nil {
		slog.Error("Failed to get node annotations", "node", nodeName, "error", err)
		return fmt.Errorf("failed to get annotations for manually uncordoned node %s: %w", nodeName, err)
	}

	slog.Debug("Retrieved node annotations for manual uncordon", "node", nodeName, "annotationCount", len(annotations))

	annotationsToRemove := []string{}

	var taintsToRemove []config.Taint

	taintsKey := common.QuarantineHealthEventAppliedTaintsAnnotationKey
	if taintsStr, exists := annotations[taintsKey]; exists && taintsStr != "" {
		annotationsToRemove = append(annotationsToRemove, taintsKey)

		if err := json.Unmarshal([]byte(taintsStr), &taintsToRemove); err != nil {
			slog.Error("Failed to unmarshal taints", "node", nodeName, "error", err)
			return fmt.Errorf("failed to unmarshal taints for manually uncordoned node %s: %w", nodeName, err)
		}

		slog.Debug("Parsed taints to remove", "node", nodeName, "taintCount", len(taintsToRemove))
	}

	if _, exists := annotations[common.QuarantineHealthEventAnnotationKey]; exists {
		annotationsToRemove = append(annotationsToRemove, common.QuarantineHealthEventAnnotationKey)
	}

	if _, exists := annotations[common.QuarantineHealthEventIsCordonedAnnotationKey]; exists {
		annotationsToRemove = append(annotationsToRemove, common.QuarantineHealthEventIsCordonedAnnotationKey)
	}

	slog.Debug("Prepared annotations to remove", "node", nodeName, "count", len(annotationsToRemove))

	newAnnotations := map[string]string{
		common.QuarantinedNodeUncordonedManuallyAnnotationKey: common.QuarantinedNodeUncordonedManuallyAnnotationValue,
	}

	ctx := context.Background()

	if err := r.k8sClient.HandleManualUncordonCleanup(
		ctx,
		nodeName,
		taintsToRemove,
		annotationsToRemove,
		newAnnotations,
		[]string{statemanager.NVSentinelStateLabelKey},
	); err != nil {
		slog.Error("Failed to clean up manually uncordoned node", "node", nodeName, "error", err)
		metrics.ProcessingErrors.WithLabelValues("manual_uncordon_cleanup_error").Inc()

		return fmt.Errorf("failed to clean up manually uncordoned node %s: %w", nodeName, err)
	}

	slog.Debug("Successfully completed K8s cleanup for manual uncordon", "node", nodeName)

	// Cancel latest quarantining events (if eventWatcher is available)
	if r.eventWatcher != nil {
		slog.Debug("Calling CancelLatestQuarantiningEvents for manual uncordon", "node", nodeName)

		if err := r.eventWatcher.CancelLatestQuarantiningEvents(ctx, nodeName); err != nil {
			slog.Error("Failed to cancel latest quarantining events for manually uncordoned node",
				"node", nodeName, "error", err)
			metrics.ProcessingErrors.WithLabelValues("mongodb_cancelled_update_error").Inc()

			return fmt.Errorf("failed to cancel latest quarantining events for node %s: %w", nodeName, err)
		}

		slog.Debug("Successfully cancelled latest quarantining events", "node", nodeName)
	} else {
		slog.Warn("eventWatcher is NIL - cannot cancel quarantining events in database", "node", nodeName)
	}

	metrics.TotalNodesManuallyUncordoned.WithLabelValues(nodeName).Inc()
	metrics.CurrentQuarantinedNodes.WithLabelValues(nodeName).Set(0)
	slog.Info("Set currentQuarantinedNodes to 0 for manually uncordoned node", "node", nodeName)

	slog.Info("Successfully completed manual uncordon handling", "node", nodeName)

	return nil
}
