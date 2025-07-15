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

	"github.com/hashicorp/go-multierror"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/node-drainer-module/pkg/config"
	storeconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/store-client-sdk/pkg/storewatcher"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"k8s.io/klog"
)

const (
	maxRetries = 5
	retryDelay = 10 * time.Second
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

	watcher.Start(ctx)

	klog.Infoln("Listening for events on the channel...")

	for event := range watcher.Events() {
		totalEventsReceived.Inc()

		go func(event bson.M) {
			startTime := time.Now()
			document := event["fullDocument"].(bson.M)
			healthEventWithStatus := storeconnector.HealthEventWithStatus{}
			if err := storewatcher.UnmarshalFullDocumentFromEvent(
				event,
				&healthEventWithStatus,
			); err != nil {
				totalEventProcessingError.WithLabelValues("unmarshal_doc_error").Inc()
				klog.Errorf("Failed to unmarshal event: %+v", err)
				if err := watcher.MarkProcessed(ctx); err != nil {
					totalEventProcessingError.WithLabelValues("mark_processed_error").Inc()
					klog.Errorf("Error updating resume token: %+v", err)
				}
				return
			}

			klog.Infof("Received an event with ID %v", document["_id"])
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
				klog.Errorf("Error in updating the health event with ID %v:, error: %+v", document["_id"], err)
			} else {

				for i := 1; i <= maxRetries; i++ {
					klog.Infof("Attempt %d, Processing health event: %+v", i, healthEventWithStatus)
					err = r.handleEvent(ctx, document["_id"], healthEventWithStatus.HealthEvent.NodeName, &healthEventWithStatus)
					if err == nil {
						totalEventsSuccessfullyProcessed.Inc()

						podsEvictionStatus.Status = storeconnector.StatusSucceeded
						break
					}
					klog.Errorf("Error in processing the event with ID %v:, error: %+v", document["_id"], err)
					totalEventProcessingError.WithLabelValues("handle_event_error").Inc()
					time.Sleep(retryDelay)
				}

				if err != nil {
					klog.Errorf("Max attempt reached, error in handling the health event with ID %v:, error: %+v", document["_id"], err)
					podsEvictionStatus.Status = storeconnector.StatusFailed
					podsEvictionStatus.Message = err.Error()
				}

				if err := r.updateNodeUserPodsEvictedStatus(ctx, collection, event, podsEvictionStatus); err != nil {
					totalEventProcessingError.WithLabelValues("update_status_error").Inc()
					klog.Errorf("Error in updating the user pods eviction status for node: %+v", err)
				}
			}
			if err := watcher.MarkProcessed(ctx); err != nil {
				totalEventProcessingError.WithLabelValues("mark_processed_error").Inc()
				klog.Errorf("Error updating resume token: %+v", err)
			}
			duration := time.Since(startTime).Seconds()

			eventHandlingDuration.Observe(duration)
		}(event)
	}
}

func (r *Reconciler) handleEvent(ctx context.Context, eventId interface{}, nodeName string, healthEventWithStatus *storeconnector.HealthEventWithStatus) error {

	namespaceMap := r.getMatchingNamespace(ctx)
	var mu sync.Mutex
	nsWithImmediateMode := []string{}

	var wg sync.WaitGroup
	errChan := make(chan error, len(r.Config.TomlConfig.UserNamespaces))

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
		} else {
			wg.Add(1)
			ctx1, cancel := context.WithCancel(ctx)
			r.NodeEvictionContext.Store(nsWithNode, &EvictionContext{cancel: cancel})
			go func(ctx1 context.Context, mode config.EvictMode, nodeName string, ns string, nsWithNode string) {
				defer wg.Done()
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
					klog.Infof("Monitoring pods for completion in namespace %s in %s mode", ns, mode)
					if err := r.Config.K8sClient.MonitorPodCompletion(ctx1, ns, nodeName); err != nil {
						klog.Infof("error while monitoring pods to complete in namespace %s on node %s: %+v\n", ns, nodeName, err)
						errChan <- err
					}

					r.NodeEvictionContext.Delete(nsWithNode)

				default:
					klog.Errorf("Invalid mode of eviction: %s", mode)
					errChan <- fmt.Errorf("invalid mode of eviction: %s", mode)
				}

				select {
				case <-ctx1.Done():
					klog.Infof("Context cancelled for health event: %+v", healthEventWithStatus)
				default:
				}

			}(ctx1, mode, nodeName, ns, nsWithNode)
		}
	}

	wg.Wait()
	close(errChan)

	var mErr *multierror.Error
	if len(errChan) > 0 {
		for err := range errChan {
			mErr = multierror.Append(mErr, err)
		}
		return mErr
	}

	return r.verifyEvictionCompleted(ctx, healthEventWithStatus, nodeName, nsWithImmediateMode)
}

func (r *Reconciler) getMatchingNamespace(ctx context.Context) map[string]config.EvictMode {
	namespaceMap := make(map[string]config.EvictMode)
	for _, userNamespace := range r.Config.TomlConfig.UserNamespaces {
		matchedNamespaces, err := r.Config.K8sClient.GetNamespacesMatchingPattern(ctx, userNamespace.Name)
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
