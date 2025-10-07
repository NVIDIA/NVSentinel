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
	"errors"
	"fmt"
	"sync"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/node-drainer-module/pkg/config"
	storeconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/statemanager"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
)

const (
	maxRetries              = 5
	retryDelay              = 10 * time.Second
	NodeDrainStatusLabelKey = "nvsentinel.dgxc.nvidia.com/node-drain-status"
	queueIdleTimeout        = 5 * time.Minute // Timeout for idle node event queues
)

type NodeDrainLabelValue string

const (
	InProgress NodeDrainLabelValue = "IN_PROGRESS"
)

type ReconcilerConfig struct {
	TomlConfig    config.TomlConfig
	MongoConfig   storewatcher.MongoDBConfig
	TokenConfig   storewatcher.TokenConfig
	MongoPipeline mongo.Pipeline
	K8sClient     NodeDrainerClientInterface
	StateManager  statemanager.StateManager
}

type Reconciler struct {
	Config              ReconcilerConfig
	NodeEvictionContext sync.Map
	DryRun              bool
	// Per-node event queues for sequential processing
	nodeEventQueues  sync.Map // map[nodeName]*NodeEventQueue
	kubernetesClient kubernetes.Interface
}

type EvictionContext struct {
	cancel context.CancelFunc
}

// NodeEventQueue represents a queue of events for a specific node
type NodeEventQueue struct {
	events chan bson.M
	done   chan struct{} // Signal to stop processing
}

func NewReconciler(cfg ReconcilerConfig, dryRunEnabled bool, kubeClient kubernetes.Interface) *Reconciler {
	return &Reconciler{
		Config:              cfg,
		NodeEvictionContext: sync.Map{},
		DryRun:              dryRunEnabled,
		nodeEventQueues:     sync.Map{},
		kubernetesClient:    kubeClient,
	}
}

func (r *Reconciler) Start(ctx context.Context) {
	watcher, err := storewatcher.NewChangeStreamWatcher(ctx, r.Config.MongoConfig,
		r.Config.TokenConfig, r.Config.MongoPipeline)
	if err != nil {
		klog.Fatalf("failed to create change stream watcher: %+v", err)
	}
	defer watcher.Close(ctx)

	collection, err := storewatcher.GetCollectionClient(ctx, r.Config.MongoConfig)
	if err != nil {
		klog.Fatalf("error initializing collection client with config %+v for mongodb: %+v", r.Config.MongoConfig, err)
	}

	oldEvents, err := r.getInProgressEvents(ctx, collection)
	if err != nil {
		klog.Errorf("Failed to get in-progress events: %v", err)
	} else {
		for _, event := range oldEvents {
			wrappedEvent := bson.M{
				"fullDocument": event,
			}
			// Reprocess old in-progress events
			totalEventsReplayed.Inc()

			if err := r.preprocessAndEnqueueEvent(ctx, wrappedEvent, collection); err != nil {
				totalEventProcessingError.WithLabelValues("preprocess_error").Inc()
				klog.Errorf("Failed to start processing for in-progress event: %+v", err)
			}
		}
	}

	watcher.Start(ctx)

	klog.Infoln("Listening for events on the channel...")

	for event := range watcher.Events() {
		totalEventsReceived.Inc()

		// Validate, set InProgress, and enqueue for per-node processing, then ack
		if err := r.preprocessAndEnqueueEvent(ctx, event, collection); err != nil {
			totalEventProcessingError.WithLabelValues("preprocess_error").Inc()
			klog.Errorf("Error preparing event for processing: %+v", err)

			continue
		}

		if err := watcher.MarkProcessed(ctx); err != nil {
			totalEventProcessingError.WithLabelValues("mark_processed_error").Inc()
			klog.Errorf("Error updating resume token: %+v", err)
		}
	}
}

// isTerminalStatus checks if the eviction status is in a terminal state
func isTerminalStatus(status storeconnector.Status) bool {
	return status == storeconnector.StatusSucceeded ||
		status == storeconnector.StatusFailed ||
		status == storeconnector.AlreadyDrained
}

// updateQuarantineMetrics updates metrics based on node quarantine status
func (r *Reconciler) updateQuarantineMetrics(healthEventWithStatus *storeconnector.HealthEventWithStatus) {
	// Guard against nil NodeQuarantined to prevent crash on malformed/old documents
	if healthEventWithStatus.HealthEventStatus.NodeQuarantined == nil {
		klog.Warningf("NodeQuarantined is nil for node %s, skipping metrics update",
			healthEventWithStatus.HealthEvent.NodeName)
		return
	}

	//nolint:exhaustive // NodeQuarantined uses a subset of Status constants
	switch *healthEventWithStatus.HealthEventStatus.NodeQuarantined {
	case storeconnector.Quarantined:
		unhealthyEvent.WithLabelValues(healthEventWithStatus.HealthEvent.NodeName,
			healthEventWithStatus.HealthEvent.CheckName).Inc()
	case storeconnector.UnQuarantined:
		healthyEvent.WithLabelValues(healthEventWithStatus.HealthEvent.NodeName,
			healthEventWithStatus.HealthEvent.CheckName).Inc()
	case storeconnector.AlreadyQuarantined:
		// AlreadyQuarantined means the node was already in quarantine state
		// We still process this event as it may need draining
		klog.Infof("Node %s is already quarantined", healthEventWithStatus.HealthEvent.NodeName)
		unhealthyEvent.WithLabelValues(healthEventWithStatus.HealthEvent.NodeName,
			healthEventWithStatus.HealthEvent.CheckName).Inc()
	default:
		klog.Warningf("Unknown NodeQuarantined status: %v for node %s",
			*healthEventWithStatus.HealthEventStatus.NodeQuarantined,
			healthEventWithStatus.HealthEvent.NodeName)
	}
}

// preprocessAndEnqueueEvent performs initial event validation and queuing:
// - unmarshals the event
// - skips terminal states
// - marks eviction status as InProgress in the DB
// - enqueues the event to the per-node queue for sequential processing
func (r *Reconciler) preprocessAndEnqueueEvent(ctx context.Context, event bson.M, collection MongoCollectionAPI) error {
	healthEventWithStatus := storeconnector.HealthEventWithStatus{}
	if err := storewatcher.UnmarshalFullDocumentFromEvent(
		event,
		&healthEventWithStatus,
	); err != nil {
		totalEventProcessingError.WithLabelValues("unmarshal_doc_error").Inc()
		klog.Errorf("Failed to unmarshal health event: \n%v; \nError:%+v", event, err)

		return err
	}

	// Check if event is already in terminal state
	if isTerminalStatus(healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status) {
		klog.Infof("Skipping health event as its already in terminal state, \nHealth event: %+v",
			healthEventWithStatus.HealthEvent)
		return nil
	}

	klog.Infof("Received event: \n%+v", healthEventWithStatus.HealthEvent)
	klog.Infof("Current UserPodsEvictionStatus: %+v", healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus)

	// set the user pod eviction status to in-progress
	podsEvictionStatus := &healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = storeconnector.StatusInProgress
	klog.Infof("Setting initial eviction status to InProgress for node %s, status value: %s",
		healthEventWithStatus.HealthEvent.NodeName, string(podsEvictionStatus.Status))

	// Update metrics based on quarantine status
	r.updateQuarantineMetrics(&healthEventWithStatus)

	err := r.updateNodeUserPodsEvictedStatus(ctx, collection, event, podsEvictionStatus)
	if err != nil {
		totalEventProcessingError.WithLabelValues("update_status_error").Inc()
		klog.Errorf("Error in updating health event: \n%+v:, \nerror: %+v", healthEventWithStatus.HealthEvent.NodeName, err)

		return err
	}

	// Enqueue for per-node sequential processing. This will block if the queue is full,
	// ensuring we don't ack before safely accepting the work.
	if err := r.enqueueEvent(ctx, event, collection); err != nil {
		// Failed to enqueue, return error to prevent acknowledging
		return fmt.Errorf("failed to enqueue event: %w", err)
	}

	return nil
}

func (r *Reconciler) processEvent(ctx context.Context, event bson.M, collection MongoCollectionAPI,
	healthEventWithStatus storeconnector.HealthEventWithStatus) {
	startTime := time.Now()

	var err error

	podsEvictionStatus := &healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus

	for i := 1; i <= maxRetries; i++ {
		klog.Infof("Attempt %d, Processing health event: %+v", i, healthEventWithStatus)

		err = r.handleEvent(ctx, healthEventWithStatus.HealthEvent.NodeName, &healthEventWithStatus)
		if err == nil {
			totalEventsSuccessfullyProcessed.Inc()

			podsEvictionStatus.Status = storeconnector.StatusSucceeded

			break
		}

		klog.Errorf("Error in processing the event:\n%+v, error is : \n%+v", healthEventWithStatus.HealthEvent, err)
		totalEventProcessingError.WithLabelValues("handle_event_error").Inc()
		time.Sleep(retryDelay)
	}

	if err != nil {
		klog.Errorf("Max attempt reached, error in handling health event: "+
			"%+v:, \nerror: %+v", healthEventWithStatus.HealthEvent, err)

		podsEvictionStatus.Status = storeconnector.StatusFailed
		podsEvictionStatus.Message = err.Error()
	}

	if err := r.updateNodeUserPodsEvictedStatus(ctx, collection, event, podsEvictionStatus); err != nil {
		totalEventProcessingError.WithLabelValues("update_status_error").Inc()
		klog.Errorf("Error in updating the user pods eviction status for node: %+v", err)
	}

	duration := time.Since(startTime).Seconds()

	eventHandlingDuration.Observe(duration)
}

//nolint:cyclop,gocognit //todo
func (r *Reconciler) handleEvent(ctx context.Context, nodeName string,
	healthEventWithStatus *storeconnector.HealthEventWithStatus) error {
	namespaceMap := r.getMatchingNamespace(ctx)
	deleteAfterTimeout := r.Config.TomlConfig.DeleteAfterTimeoutMinutes
	getTimeoutNamespaces := r.getTimeoutNamespaces(ctx)
	// If DrainOverrides.Force is true, override all namespaces to use immediate eviction
	if healthEventWithStatus.HealthEvent.DrainOverrides != nil &&
		healthEventWithStatus.HealthEvent.DrainOverrides.Force {
		klog.Infof("DrainOverrides.Force is true, forcing immediate eviction for all namespaces")

		for ns := range namespaceMap {
			namespaceMap[ns] = config.ModeImmediateEvict
		}
	}

	var mu sync.Mutex

	nsWithImmediateMode := []string{}

	var wg sync.WaitGroup
	// Buffer error channel to account for all potential goroutines
	maxErrors := len(namespaceMap) + 1
	errChan := make(chan error, maxErrors)

	// Set metric based on node quarantine status
	//nolint:nestif // TODO
	if *healthEventWithStatus.HealthEventStatus.NodeQuarantined == storeconnector.UnQuarantined {
		nodeDrainStatus.WithLabelValues(nodeName).Set(0)
	} else {
		_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx, nodeName, statemanager.DrainingLabelValue, false)
		if err != nil {
			klog.Errorf("Error updating node label: %+v", err)
			totalEventProcessingError.WithLabelValues("label_update_error").Inc()
		}

		nodeDrainStatus.WithLabelValues(nodeName).Set(1)
	}

	if len(getTimeoutNamespaces) > 0 &&
		*healthEventWithStatus.HealthEventStatus.NodeQuarantined == storeconnector.Quarantined {
		ctxTimeout, cancelTimeout := context.WithCancel(ctx)
		timeoutKey := fmt.Sprintf("%s-timeout", nodeName)

		r.NodeEvictionContext.Store(timeoutKey, &EvictionContext{cancel: cancelTimeout})

		wg.Add(1)

		f := func(ctx context.Context, cancelFn context.CancelFunc, timeoutKey string, nodeName string, namespaces []string) {
			defer func() {
				// ensure the derived context is cancelled to release resources
				cancelFn()
				// remove the key so that future healthy events don't try to cancel again
				r.NodeEvictionContext.Delete(timeoutKey)
				nodeDrainTimeout.WithLabelValues(nodeName).Set(0)
				wg.Done()
			}()

			nodeDrainTimeout.WithLabelValues(nodeName).Set(1)

			if err := r.Config.K8sClient.DeletePodsAfterTimeout(ctx, nodeName, namespaces,
				deleteAfterTimeout, healthEventWithStatus); err != nil {
				klog.Errorf("Error in deleting pod if not finished: %+v", err)
				nodeDrainError.WithLabelValues("delete_pods_after_timeout_error", nodeName).Inc()
				errChan <- err
			}
		}
		go f(ctxTimeout, cancelTimeout, timeoutKey, nodeName, getTimeoutNamespaces)
	}

	for ns, mode := range namespaceMap {
		nsWithNode := fmt.Sprintf("%s-%s", nodeName, ns)
		//nolint:nestif // TODO
		if *healthEventWithStatus.HealthEventStatus.NodeQuarantined == storeconnector.UnQuarantined {
			if _, ok := r.NodeEvictionContext.Load(nsWithNode); ok {
				if mode == config.ModeAllowCompletion {
					healthyEventWithContextCancellation.Inc()
					klog.Infof("Cancelling the eviction of pods in namespace %s on node %s", ns, nodeName)

					context, _ := r.NodeEvictionContext.Load(nsWithNode)
					evictionContext := context.(*EvictionContext)

					evictionContext.cancel()
				}
			}

			if context, ok := r.NodeEvictionContext.Load(fmt.Sprintf("%s-timeout", nodeName)); ok {
				klog.Infof("Cancelling the eviction of pods on node %s", nodeName)

				evictionContext := context.(*EvictionContext)

				evictionContext.cancel()
			}
		} else {
			wg.Add(1)

			go func(ctx context.Context, mode config.EvictMode, nodeName string, ns string, nsWithNode string) {
				ctx1, cancel := context.WithCancel(ctx)
				defer func() {
					cancel()
					wg.Done()
				}()

				switch mode {
				case config.ModeImmediateEvict:
					klog.Infof("Evicting pods from namespace %s in %s mode", ns, mode)
					mu.Lock()
					nsWithImmediateMode = append(nsWithImmediateMode, ns)
					mu.Unlock()

					if err := r.Config.K8sClient.EvictAllPodsInImmediateMode(ctx, ns, nodeName,
						r.Config.TomlConfig.EvictionTimeoutInSeconds.Duration); err != nil {
						klog.Infof("error while evicting pods in namespace %s on node %s: %+v\n", ns, nodeName, err)
						errChan <- err
					}
				case config.ModeAllowCompletion:
					r.NodeEvictionContext.Store(nsWithNode, &EvictionContext{cancel: cancel})
					klog.Infof("Monitoring pods for completion in namespace %s in %s mode", ns, mode)

					if err := r.Config.K8sClient.MonitorPodCompletion(ctx1, ns, nodeName); err != nil {
						klog.Infof("error while monitoring pods to complete in namespace %s on node %s: %+v\n", ns, nodeName, err)
						errChan <- err
					}
				case config.ModeDeleteAfterTimeout:
					// ModeDeleteAfterTimeout is handled separately in getTimeoutNamespaces
					// These namespaces are processed with DeletePodsAfterTimeout
					klog.Infof("Namespace %s is configured for deletion after timeout (handled separately)", ns)
				default:
					klog.Errorf("Invalid mode of eviction: %v; ignoring pods in namespace %s", mode, ns)
				}

				r.NodeEvictionContext.Delete(nsWithNode)

				select {
				case <-ctx1.Done():
					klog.Infof("Context cancelled for health event: %+v", healthEventWithStatus)
				default:
				}
			}(ctx, mode, nodeName, ns, nsWithNode)
		}
	}

	wg.Wait()
	close(errChan)

	var drainError error
	// errChan is only written to when draining is occurring for Quarantined HealthEvents
	if len(errChan) > 0 {
		var mErr *multierror.Error
		for err := range errChan {
			mErr = multierror.Append(mErr, err)
		}

		drainError = mErr
	} else {
		// verifyEvictionCompleted ensures that eviction verification only occurs on Quarantined HealthEvents
		drainError = r.verifyEvictionCompleted(ctx, healthEventWithStatus, nodeName, nsWithImmediateMode)
	}

	// We will update the state label from draining to drain-succeeded or drain-failed on Quarantined HealthEvents
	if *healthEventWithStatus.HealthEventStatus.NodeQuarantined == storeconnector.Quarantined {
		drainLabelValue := statemanager.DrainSucceededLabelValue

		if drainError != nil {
			nodeDrainStatus.WithLabelValues(nodeName).Set(0)

			drainLabelValue = statemanager.DrainFailedLabelValue
		}

		_, err := r.Config.StateManager.UpdateNVSentinelStateNodeLabel(ctx, nodeName, drainLabelValue, false)
		if err != nil {
			klog.Errorf("Error updating node label: %+v", err)
			totalEventProcessingError.WithLabelValues("label_update_error").Inc()
		}
	}
	// drainError can only be non-nil on Quarantined HealthEvents
	return drainError
}

func (r *Reconciler) getMatchingNamespace(ctx context.Context) map[string]config.EvictMode {
	namespaceMap := make(map[string]config.EvictMode)
	systemNamespaces := r.Config.TomlConfig.SystemNamespaces

	for _, userNamespace := range r.Config.TomlConfig.UserNamespaces {
		if userNamespace.Mode == config.ModeDeleteAfterTimeout {
			continue
		}

		matchedNamespaces, err := r.Config.K8sClient.GetNamespacesMatchingPattern(ctx, userNamespace.Name, systemNamespaces)

		if err != nil {
			klog.Errorf("Error while matching namespaces with pattern %s: %+v", userNamespace.Name, err)
			continue
		}

		for _, ns := range matchedNamespaces {
			// Add only if not present in the map
			if _, ok := namespaceMap[ns]; !ok {
				namespaceMap[ns] = userNamespace.Mode
			}
		}
	}

	return namespaceMap
}

func (r *Reconciler) getTimeoutNamespaces(ctx context.Context) []string {
	timeoutNamespaces := []string{}
	systemNamespaces := r.Config.TomlConfig.SystemNamespaces

	for _, userNamespace := range r.Config.TomlConfig.UserNamespaces {
		if userNamespace.Mode == config.ModeDeleteAfterTimeout {
			matchedNamespaces, err := r.Config.K8sClient.GetNamespacesMatchingPattern(ctx, userNamespace.Name, systemNamespaces)
			if err != nil {
				klog.Errorf("Error while matching namespaces with pattern %s: %+v", userNamespace.Name, err)
				continue
			}

			timeoutNamespaces = append(timeoutNamespaces, matchedNamespaces...)
		}
	}

	return timeoutNamespaces
}

// getInProgressEvents to get the events for which draining was already started
func (r *Reconciler) getInProgressEvents(ctx context.Context, collection MongoCollectionAPI) ([]bson.M, error) {
	filter := bson.M{
		"healtheventstatus.userpodsevictionstatus.status": storeconnector.StatusInProgress,
	}

	cursor, err := collection.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("error finding in-progress events: %w", err)
	}
	defer cursor.Close(ctx)

	var events []bson.M
	if err := cursor.All(ctx, &events); err != nil {
		return nil, fmt.Errorf("error decoding in-progress events: %w", err)
	}

	klog.Infof("Found %d in-progress events to process", len(events))

	return events, nil
}

func (r *Reconciler) verifyEvictionCompleted(ctx context.Context,
	healthEventWithStatus *storeconnector.HealthEventWithStatus, nodeName string, nsWithImmediateMode []string) error {
	if *healthEventWithStatus.HealthEventStatus.NodeQuarantined == storeconnector.Quarantined && !r.DryRun {
		klog.Infof("Verifying if all pods have been successfully evicted, if not, forcefully deleting them")

		allEvicted := r.Config.K8sClient.CheckIfAllPodsAreEvictedInImmediateMode(ctx, nsWithImmediateMode, nodeName,
			r.Config.TomlConfig.EvictionTimeoutInSeconds.Duration)
		if !allEvicted {
			return fmt.Errorf("error in evicting all pods in namespace %v on node %s", nsWithImmediateMode, nodeName)
		}

		nodeDrainSuccess.WithLabelValues(nodeName).Inc()
	}

	return nil
}

func (r *Reconciler) updateNodeUserPodsEvictedStatus(ctx context.Context, collection MongoCollectionAPI,
	event bson.M, userPodsEvictionStatus *storeconnector.OperationStatus) error {
	var err error

	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}

	filter := bson.M{"_id": document["_id"]}
	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.userpodsevictionstatus": *userPodsEvictionStatus,
		},
	}

	for i := 1; i <= maxRetries; i++ {
		klog.Infof("Attempt %d, updating health event with ID %v", i, document["_id"])

		_, err = collection.UpdateOne(ctx, filter, update)
		if err == nil {
			break
		}

		time.Sleep(retryDelay)
	}

	if err != nil {
		return fmt.Errorf("error updating document with ID: %v, error: %w", document["_id"], err)
	}

	klog.Infof("Health event status has been updated , health event: %+v, status: %+v", event, userPodsEvictionStatus)

	return nil
}

// enqueueEvent adds an event to the appropriate node's queue for sequential processing
func (r *Reconciler) enqueueEvent(ctx context.Context, event bson.M, collection MongoCollectionAPI) error {
	// Extract node name from event
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		klog.Errorf("Failed to extract fullDocument from event: %+v", event)
		return fmt.Errorf("failed to extract fullDocument from event")
	}

	healthEvent, ok := document["healthevent"].(bson.M)
	if !ok {
		klog.Errorf("Failed to extract healthevent from document: %+v", document)
		return fmt.Errorf("failed to extract healthevent from document")
	}

	nodeName, ok := healthEvent["nodename"].(string)
	if !ok {
		klog.Errorf("Failed to extract nodename from healthevent: %+v", healthEvent)
		return fmt.Errorf("failed to extract nodename from healthevent")
	}

	// Get or create the queue for this node
	// If the queue was deleted due to idle timeout, it will be recreated here
	queueInterface, loaded := r.nodeEventQueues.LoadOrStore(nodeName, &NodeEventQueue{
		events: make(chan bson.M, 100), // Buffer size of 100 events per node
		done:   make(chan struct{}),
	})
	queue := queueInterface.(*NodeEventQueue)

	// Start processing goroutine if this is a new queue (or was recreated after timeout)
	if !loaded {
		go r.processNodeEventQueue(ctx, nodeName, queue, collection)
	}

	// Enqueue the event
	select {
	case queue.events <- event:
		// successfully enqueued
		return nil
	case <-queue.done:
		// Queue is shutting down
		return fmt.Errorf("queue is shutting down for node %s", nodeName)
	case <-ctx.Done():
		return fmt.Errorf("context cancelled while enqueueing event for node %s", nodeName)
	case <-time.After(30 * time.Second):
		// Timeout to prevent indefinite blocking on full queue
		return fmt.Errorf("timeout enqueueing event for node %s (queue full)", nodeName)
	}
}

// processNodeEventQueue processes events for a specific node sequentially
// This goroutine will terminate if no events are received for 5 minutes
func (r *Reconciler) processNodeEventQueue(ctx context.Context, nodeName string,
	queue *NodeEventQueue, collection MongoCollectionAPI) {
	klog.Infof("Started event processor for node %s", nodeName)

	// Cleanup: close done channel and remove from map
	defer func() {
		close(queue.done)
		r.nodeEventQueues.Delete(nodeName)
		klog.Infof("Cleaned up event processor for node %s", nodeName)
	}()

	for {
		select {
		case <-ctx.Done():
			klog.Infof("Context cancelled, stopping event processing for node %s", nodeName)
			// Drain any remaining events
			for {
				select {
				case event := <-queue.events:
					klog.Warningf("Processing remaining event during shutdown for node %s", nodeName)
					r.processNodeEvent(ctx, event, collection, nodeName)
				default:
					return
				}
			}

		case event := <-queue.events:
			r.processNodeEvent(ctx, event, collection, nodeName)

		case <-time.After(queueIdleTimeout):
			// Check once more for any last-second events
			select {
			case event := <-queue.events:
				r.processNodeEvent(ctx, event, collection, nodeName)
				// Got an event, continue processing
			default:
				// Queue truly idle, clean up
				klog.Infof("Queue for node %s idle for %v, cleaning up processor", nodeName, queueIdleTimeout)
				return
			}
		}
	}
}

// processNodeEvent processes a single event for a specific node
func (r *Reconciler) processNodeEvent(ctx context.Context, event bson.M,
	collection MongoCollectionAPI, nodeName string) {
	// Add safe type assertion
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		totalEventProcessingError.WithLabelValues("extract_doc_error").Inc()
		klog.Errorf("Failed to extract fullDocument from event for node %s", nodeName)

		return
	}

	healthEventWithStatus := storeconnector.HealthEventWithStatus{}
	if err := storewatcher.UnmarshalFullDocumentFromEvent(
		event,
		&healthEventWithStatus,
	); err != nil {
		totalEventProcessingError.WithLabelValues("unmarshal_doc_error").Inc()
		klog.Errorf("Failed to unmarshal event with ID %v: %+v", document["_id"], err)

		return
	}

	klog.Infof("Processing event for node %s: %+v", nodeName, healthEventWithStatus.HealthEvent)

	// Check if this is an AlreadyQuarantined event that needs checking
	statusPtr := healthEventWithStatus.HealthEventStatus.NodeQuarantined
	//nolint:nestif // TODO
	if statusPtr != nil && *statusPtr == storeconnector.AlreadyQuarantined {
		// Query MongoDB to check if node was already successfully drained
		if isDrained, err := r.isNodeAlreadyDrained(ctx, collection, nodeName); err != nil {
			klog.Errorf("Failed to check if node %s is already drained: %v", nodeName, err)
		} else if isDrained {
			// Node already successfully drained, mark as AlreadyDrained
			klog.Infof("Node %s already drained, updating event status to AlreadyDrained", nodeName)

			podsEvictionStatus := &healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus
			podsEvictionStatus.Status = storeconnector.AlreadyDrained

			if err := r.updateNodeUserPodsEvictedStatus(ctx, collection, event, podsEvictionStatus); err != nil {
				totalEventProcessingError.WithLabelValues("update_status_error").Inc()
				klog.Errorf("Error updating status for already drained node %s: %v", nodeName, err)
			} else {
				totalEventsSuccessfullyProcessed.Inc()
			}

			return
		}
	}

	// Process the event normally
	r.processEvent(ctx, event, collection, healthEventWithStatus)
}

// isNodeAlreadyDrained checks if a node has already been successfully drained
func (r *Reconciler) isNodeAlreadyDrained(ctx context.Context, collection MongoCollectionAPI,
	nodeName string) (bool, error) {
	// Query for the latest event for this node with quarantine status
	filter := bson.M{
		"healthevent.nodename": nodeName,
		"healtheventstatus.nodequarantined": bson.M{
			"$in": []string{string(storeconnector.Quarantined), string(storeconnector.UnQuarantined)},
		},
	}

	// Sort by _id (ObjectID contains timestamp) descending to get latest
	opts := options.FindOne().SetSort(bson.D{bson.E{Key: "_id", Value: -1}})

	var result bson.M
	if err := collection.FindOne(ctx, filter, opts).Decode(&result); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, nil
		}

		return false, fmt.Errorf("failed to query latest event for node %s: %w", nodeName, err)
	}

	// Extract the relevant information
	healthEventStatus, ok := result["healtheventstatus"].(bson.M)
	if !ok {
		return false, fmt.Errorf("invalid healtheventstatus format for node %s", nodeName)
	}

	nodeQuarantined, ok := healthEventStatus["nodequarantined"].(string)
	if !ok {
		return false, fmt.Errorf("invalid nodequarantined format for node %s", nodeName)
	}

	// If the latest event shows the node as UnQuarantined, it's not drained
	if nodeQuarantined == string(storeconnector.UnQuarantined) {
		return false, nil
	}

	// Node is quarantined, check the drain status
	userPodsEvictionStatus, ok := healthEventStatus["userpodsevictionstatus"].(bson.M)
	if !ok {
		return false, nil
	}

	drainStatus, ok := userPodsEvictionStatus["status"].(string)
	if !ok {
		return false, nil
	}

	// Only consider it already drained if it was successfully completed
	return drainStatus == string(storeconnector.StatusSucceeded), nil
}
