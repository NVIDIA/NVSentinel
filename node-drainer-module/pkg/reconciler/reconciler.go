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
	"fmt"
	"sync"
	"time"

	multierror "github.com/hashicorp/go-multierror"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/node-drainer-module/pkg/config"
	storeconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/klog"
)

const (
	maxRetries              = 5
	retryDelay              = 10 * time.Second
	NodeDrainStatusLabelKey = "nvsentinel.dgxc.nvidia.com/node-drain-status"
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
}

type Reconciler struct {
	Config              ReconcilerConfig
	NodeEvictionContext sync.Map
	DryRun              bool
}

type EvictionContext struct {
	cancel context.CancelFunc
}

func NewReconciler(cfg ReconcilerConfig, dryRunEnabled bool) *Reconciler {
	return &Reconciler{Config: cfg, NodeEvictionContext: sync.Map{}, DryRun: dryRunEnabled}
}

func (r *Reconciler) Start(ctx context.Context) {

	watcher, err := storewatcher.NewChangeStreamWatcher(ctx, r.Config.MongoConfig, r.Config.TokenConfig, r.Config.MongoPipeline)
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
			r.startEventProcessing(ctx, wrappedEvent, collection)
		}
	}

	watcher.Start(ctx)

	klog.Infoln("Listening for events on the channel...")

	for event := range watcher.Events() {
		totalEventsReceived.Inc()

		r.startEventProcessing(ctx, event, collection)

		if err := watcher.MarkProcessed(ctx); err != nil {
			totalEventProcessingError.WithLabelValues("mark_processed_error").Inc()
			klog.Errorf("Error updating resume token: %+v", err)
		}
	}
}

func (r *Reconciler) startEventProcessing(ctx context.Context, event bson.M, collection *mongo.Collection) {
	healthEventWithStatus := storeconnector.HealthEventWithStatus{}
	if err := storewatcher.UnmarshalFullDocumentFromEvent(
		event,
		&healthEventWithStatus,
	); err != nil {
		totalEventProcessingError.WithLabelValues("unmarshal_doc_error").Inc()
		klog.Errorf("Failed to unmarshal health event: \n%v; \nError:%+v", event, err)
		return
	}

	currentStatus := healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus.Status
	if currentStatus == storeconnector.StatusSucceeded || currentStatus == storeconnector.StatusFailed {
		klog.Infof("Skipping health event as its already in terminal state, \nHealth event: %+v", healthEventWithStatus.HealthEvent)
		return
	}

	klog.Infof("Received event: \n%+v", healthEventWithStatus.HealthEvent)
	// set the user pod eviction status to in-progress
	podsEvictionStatus := &healthEventWithStatus.HealthEventStatus.UserPodsEvictionStatus
	podsEvictionStatus.Status = storeconnector.StatusInProgress

	switch *healthEventWithStatus.HealthEventStatus.NodeQuarantined {
	case storeconnector.Quarantined:
		unhealthyEvent.WithLabelValues(healthEventWithStatus.HealthEvent.NodeName,
			healthEventWithStatus.HealthEvent.CheckName).Inc()
	case storeconnector.UnQuarantined:
		healthyEvent.WithLabelValues(healthEventWithStatus.HealthEvent.NodeName,
			healthEventWithStatus.HealthEvent.CheckName).Inc()
	}

	err := r.updateNodeUserPodsEvictedStatus(ctx, collection, event, podsEvictionStatus)
	if err != nil {
		totalEventProcessingError.WithLabelValues("update_status_error").Inc()
		klog.Errorf("Error in updating health event: \n%+v:, \nerror: %+v", healthEventWithStatus.HealthEvent.NodeName, err)
	}

	go func(event bson.M) {
		r.processEvents(ctx, event, collection, healthEventWithStatus)
	}(event)
}

func (r *Reconciler) processEvents(ctx context.Context, event bson.M, collection *mongo.Collection, healthEventWithStatus storeconnector.HealthEventWithStatus) {
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
		klog.Errorf("Max attempt reached, error in handling health event: \n%+v:, \nerror: %+v", healthEventWithStatus.HealthEvent, err)
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

func (r *Reconciler) handleEvent(ctx context.Context, nodeName string, healthEventWithStatus *storeconnector.HealthEventWithStatus) error {

	namespaceMap := r.getMatchingNamespace(ctx)
	deleteAfterTimeout := r.Config.TomlConfig.DeleteAfterTimeoutMinutes
	getTimeoutNamespaces := r.getTimeoutNamespaces(ctx)

	// If DrainOverrides.Force is true, override all namespaces to use immediate eviction
	if healthEventWithStatus.HealthEvent.DrainOverrides != nil && healthEventWithStatus.HealthEvent.DrainOverrides.Force {
		klog.Infof("DrainOverrides.Force is true, forcing immediate eviction for all namespaces")
		for ns := range namespaceMap {
			namespaceMap[ns] = config.ModeImmediateEvict
		}
	}

	var mu sync.Mutex
	nsWithImmediateMode := []string{}

	var wg sync.WaitGroup
	errChan := make(chan error, len(r.Config.TomlConfig.UserNamespaces))

	// Set metric based on node quarantine status
	if *healthEventWithStatus.HealthEventStatus.NodeQuarantined == storeconnector.UnQuarantined {
		// Node is healthy/unquarantined - set metric to 0
		nodeDrainStatus.WithLabelValues(nodeName).Set(0)
	} else {
		err := r.Config.K8sClient.UpdateNodeLabel(ctx, nodeName, true)
		if err != nil {
			klog.Errorf("Error updating node label: %+v", err)
		}
		// Node is quarantined - set metric to 1 to indicate draining started
		nodeDrainStatus.WithLabelValues(nodeName).Set(1)
	}

	if len(getTimeoutNamespaces) > 0 && *healthEventWithStatus.HealthEventStatus.NodeQuarantined == storeconnector.Quarantined {
		ctxTimeout, cancelTimeout := context.WithCancel(ctx)
		timeoutKey := fmt.Sprintf("%s-timeout", nodeName)
		r.NodeEvictionContext.Store(timeoutKey, &EvictionContext{cancel: cancelTimeout})

		wg.Add(1)
		go func(ctx context.Context, cancelFn context.CancelFunc, timeoutKey string, nodeName string, namespaces []string) {
			defer func() {
				// ensure the derived context is cancelled to release resources
				cancelFn()
				// remove the key so that future healthy events don't try to cancel again
				r.NodeEvictionContext.Delete(timeoutKey)
				nodeDrainTimeout.WithLabelValues(nodeName).Set(0)
				wg.Done()
			}()

			nodeDrainTimeout.WithLabelValues(nodeName).Set(1)
			if err := r.Config.K8sClient.DeletePodsAfterTimeout(ctx, nodeName, namespaces, deleteAfterTimeout, healthEventWithStatus); err != nil {
				klog.Errorf("Error in deleting pod if not finished: %+v", err)
				nodeDrainError.WithLabelValues(nodeName, "delete_pods_after_timeout_error").Inc()
			}
		}(ctxTimeout, cancelTimeout, timeoutKey, nodeName, getTimeoutNamespaces)
	}

	for ns, mode := range namespaceMap {
		nsWithNode := fmt.Sprintf("%s-%s", nodeName, ns)
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

					if err := r.Config.K8sClient.EvictAllPodsInImmediateMode(ctx, ns, nodeName, r.Config.TomlConfig.EvictionTimeoutInSeconds.Duration); err != nil {
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

				default:
					klog.Errorf("Invalid mode of eviction; ignoring pods in namespace %s in %s mode", ns, mode)
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

	var mErr *multierror.Error
	if len(errChan) > 0 {
		for err := range errChan {
			mErr = multierror.Append(mErr, err)
		}
		err := r.Config.K8sClient.UpdateNodeLabel(ctx, nodeName, false)
		if err != nil {
			klog.Errorf("Error updating node label: %+v", err)
		}
		nodeDrainStatus.WithLabelValues(nodeName).Set(0)
		return mErr
	}

	err := r.verifyEvictionCompleted(ctx, healthEventWithStatus, nodeName, nsWithImmediateMode)

	// Set metric to 0 only after successful completion of draining
	if err == nil && *healthEventWithStatus.HealthEventStatus.NodeQuarantined == storeconnector.Quarantined {
		err := r.Config.K8sClient.UpdateNodeLabel(ctx, nodeName, false)
		if err != nil {
			klog.Errorf("Error updating node label: %+v", err)
		}
		nodeDrainStatus.WithLabelValues(nodeName).Set(0)
	}

	return err
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
			//Add only if not present in the map
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
func (r *Reconciler) getInProgressEvents(ctx context.Context, collection *mongo.Collection) ([]bson.M, error) {
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

func (r *Reconciler) verifyEvictionCompleted(ctx context.Context, healthEventWithStatus *storeconnector.HealthEventWithStatus, nodeName string, nsWithImmediateMode []string) error {

	if *healthEventWithStatus.HealthEventStatus.NodeQuarantined == storeconnector.Quarantined && !r.DryRun {
		klog.Infof("Verifying if all pods have been successfully evicted, if not, forcefully deleting them")
		allEvicted := r.Config.K8sClient.CheckIfAllPodsAreEvictedInImmediateMode(ctx, nsWithImmediateMode, nodeName, r.Config.TomlConfig.EvictionTimeoutInSeconds.Duration)
		if !allEvicted {
			return fmt.Errorf("error in evicting all pods in namespace %v on node %s", nsWithImmediateMode, nodeName)
		}
		nodeDrainSuccess.WithLabelValues(nodeName).Inc()
	}
	return nil
}

func (r *Reconciler) updateNodeUserPodsEvictedStatus(ctx context.Context, collection *mongo.Collection, event bson.M, userPodsEvictionStatus *storeconnector.OperationStatus) error {
	var err error
	document, ok := event["fullDocument"].(bson.M)
	if !ok {
		return fmt.Errorf("error extracting fullDocument from event: %+v", event)
	}
	filter := bson.M{"_id": document["_id"]}
	update := bson.M{
		"$set": bson.M{
			"healtheventstatus.userpodsevictionstatus": userPodsEvictionStatus,
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
