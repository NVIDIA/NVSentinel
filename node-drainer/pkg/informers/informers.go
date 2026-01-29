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

package informers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/metrics"
)

const (
	NodeIndex            = "node"
	NamespaceNodeIndex   = "namespace-node"
	NodeEventReasonIndex = "node-event-reason"
)

type Informers struct {
	podInformer            cache.SharedIndexInformer
	eventInformer          cache.SharedIndexInformer
	nodeInformer           cache.SharedIndexInformer
	clientset              kubernetes.Interface
	notReadyTimeoutMinutes *int
	dryRunMode             []string
	namespace              string
}

func NewInformers(clientset kubernetes.Interface, resyncPeriod time.Duration,
	notReadyTimeoutMinutes *int, dryRun bool) (*Informers, error) {
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
	)

	podInformer := informerFactory.Core().V1().Pods().Informer()

	err := podInformer.GetIndexer().AddIndexers(
		cache.Indexers{
			NodeIndex:          NodeIndexFunc,
			NamespaceNodeIndex: NamespaceNodeIndexFunc,
		})
	if err != nil {
		return nil, fmt.Errorf("failed to add indexer: %w", err)
	}

	eventInformerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
		informers.WithNamespace(metav1.NamespaceDefault),
	)

	eventInformer := eventInformerFactory.Core().V1().Events().Informer()

	err = eventInformer.GetIndexer().AddIndexers(
		cache.Indexers{
			NodeEventReasonIndex: NodeEventReasonIndexFunc,
		})
	if err != nil {
		return nil, fmt.Errorf("failed to add event indexer: %w", err)
	}

	nodeInformer := informerFactory.Core().V1().Nodes().Informer()

	dryRunMode := []string{}
	if dryRun {
		dryRunMode = []string{metav1.DryRunAll}
	}

	return &Informers{
		clientset:              clientset,
		podInformer:            podInformer,
		eventInformer:          eventInformer,
		nodeInformer:           nodeInformer,
		notReadyTimeoutMinutes: notReadyTimeoutMinutes,
		dryRunMode:             dryRunMode,
		namespace:              metav1.NamespaceDefault,
	}, nil
}

func (i *Informers) HasSynced() bool {
	return i.podInformer.HasSynced() && i.eventInformer.HasSynced() && i.nodeInformer.HasSynced()
}

func NodeIndexFunc(obj any) ([]string, error) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return []string{}, nil
	}

	if pod.Spec.NodeName == "" {
		return []string{}, nil
	}

	return []string{pod.Spec.NodeName}, nil
}

func NamespaceNodeIndexFunc(obj any) ([]string, error) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return []string{}, nil
	}

	if pod.Spec.NodeName == "" {
		return []string{}, nil
	}

	compositeKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Spec.NodeName)

	return []string{compositeKey}, nil
}

func NodeEventReasonIndexFunc(obj any) ([]string, error) {
	event, ok := obj.(*v1.Event)
	if !ok {
		return []string{}, nil
	}

	if event.InvolvedObject.Kind != "Node" {
		return []string{}, nil
	}

	compositeKey := fmt.Sprintf("%s/%s", event.InvolvedObject.Name, event.Reason)

	return []string{compositeKey}, nil
}

func (i *Informers) Run(ctx context.Context) error {
	go i.podInformer.Run(ctx.Done())
	go i.eventInformer.Run(ctx.Done())
	go i.nodeInformer.Run(ctx.Done())

	if ok := cache.WaitForCacheSync(ctx.Done(),
		i.HasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	return nil
}

func (i *Informers) FindEvictablePodsInNamespaceAndNode(namespace, nodeName string,
	partialDrainEntity *protos.Entity) ([]*v1.Pod, error) {
	compositeKey := fmt.Sprintf("%s/%s", namespace, nodeName)

	objs, err := i.podInformer.GetIndexer().ByIndex(NamespaceNodeIndex, compositeKey)
	if err != nil {
		slog.Error("Failed to get pods by index", "error", err)
		return nil, fmt.Errorf("failed to get pods by index: %w", err)
	}

	pods := make([]*v1.Pod, 0, len(objs))

	for _, obj := range objs {
		if pod, ok := obj.(*v1.Pod); ok {
			pods = append(pods, pod)
		}
	}

	pods = i.filterEvictablePods(pods)

	pods, err = i.filterPodsUsingEntity(pods, partialDrainEntity, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to filter pods using entity: %w", err)
	}

	return pods, nil
}

/*
This function will filter pods which are using the provided partialDrainEntity. If the partialDrainEntity is nil, all
pods will be returned which corresponds to a full drain. The partialDrainEntity is initialized by calling
shouldExecutePartialDrain in evaluator.go. Currently, if the recommended action is COMPONENT_RESET, the
partialDrainEntity will be the GPU_UUID provided in the HealthEvent.

The metadata-collector adds devices with resource names for entities that support partial drains as a pod annotation to
allow the node-drainer to look up the set of pods which are leveraging that entity. This is configured in
pod_device_annotation.go which provides a map from impacted entities supporting partial drain to a list of device
resource names corresponding to that entity type. For example:

	"GPU_UUID": {
		"nvidia.com/gpu",
		"nvidia.com/pgpu",
	},

The metadata-collector will add the following device annotation to any pod that is assigned GPU devices:

	annotations:
	  dgxc.nvidia.com/devices: '{"devices":{"nvidia.com/gpu":["GPU-123"]}}'

When the node-drainer receives an unhealthy HealthEvent for COMPONENT_RESET with GPU_UUID = GPU-123, we will only drain
pods with a device annotation that includes GPU-123:

	{
	  healthevent: {
	    version: Long('1'),
	    agent: 'syslog-health-monitor',
	    componentclass: 'GPU',
	    checkname: 'SysLogsXIDError',
	    isfatal: true,
	    ishealthy: false,
	    message: 'ROBUST_CHANNEL_CTXSW_TIMEOUT_ERROR',
	    recommendedaction: 2,
	    errorcode: [
	      '48'
	    ],
	    entitiesimpacted: [
	      {
	        entitytype: 'PCI',
	        entityvalue: '0000:03:00'
	      },
	      {
	        entitytype: 'GPU_UUID',
	        entityvalue: 'GPU-123'
	      }
	    ],

...
*/
func (i *Informers) filterPodsUsingEntity(pods []*v1.Pod, partialDrainEntity *protos.Entity,
	nodeName string) ([]*v1.Pod, error) {
	if partialDrainEntity == nil {
		return pods, nil
	}

	resourceNames, supportedEntity := model.EntityTypeToResourceNames[partialDrainEntity.EntityType]
	if !supportedEntity {
		return nil, fmt.Errorf("unsupported impacted entity for partial drains: %s",
			partialDrainEntity.EntityType)
	}

	slog.Info("Filtering pods using entity for a partial node drain",
		"nodeName", nodeName, "entityType", partialDrainEntity.EntityType,
		"entityValue", partialDrainEntity.EntityValue)

	var filteredPods []*v1.Pod

	for _, pod := range pods {
		deviceAnnotationJSON, podHasDeviceAnnotation := pod.Annotations[model.PodDeviceAnnotationName]
		// While we can't detect a stale annotation for devices, we can detect pods that are requesting GPUs that do
		// not have the device annotation at all which would indicate an issue with the metadata-collector.
		isPodRequestingDevices := areContainersRequestingDevice(pod.Spec.Containers, resourceNames) ||
			areContainersRequestingDevice(pod.Spec.InitContainers, resourceNames)

		if isPodRequestingDevices && !podHasDeviceAnnotation {
			return nil, fmt.Errorf("pod %s is requesting devices but is missing device annotation", pod.Name)
		}

		if podHasDeviceAnnotation {
			var deviceAnnotation model.DeviceAnnotation
			if err := json.Unmarshal([]byte(deviceAnnotationJSON), &deviceAnnotation); err != nil {
				return nil, fmt.Errorf("error unmarshalling device annotation for pod %s: %w", pod.Name, err)
			}

			if isPodUsingPartialDrainEntity(deviceAnnotation, resourceNames, partialDrainEntity) {
				slog.Info("Pod is eligible to be drained since it's using impacted entity",
					"podName", pod.Name, "nodeName", nodeName, "entityType", partialDrainEntity.EntityType,
					"entityValue", partialDrainEntity.EntityValue)
				filteredPods = append(filteredPods, pod)
			}
		}
	}

	return filteredPods, nil
}

func isPodUsingPartialDrainEntity(deviceAnnotation model.DeviceAnnotation, resourceNames []string,
	partialDrainEntity *protos.Entity) bool {
	for _, resourceName := range resourceNames {
		if devicesForResource, ok := deviceAnnotation.Devices[resourceName]; ok {
			for _, device := range devicesForResource {
				if device == partialDrainEntity.EntityValue {
					return true
				}
			}
		}
	}

	return false
}

func areContainersRequestingDevice(containers []v1.Container, resourceNames []string) bool {
	for _, resourceName := range resourceNames {
		for _, container := range containers {
			if qty, ok := container.Resources.Limits[v1.ResourceName(resourceName)]; ok {
				if qty.Cmp(resource.MustParse("0")) == 1 {
					return true
				}
			}
		}
	}

	return false
}

func (i *Informers) filterEvictablePods(pods []*v1.Pod) []*v1.Pod {
	filteredPods := []*v1.Pod{}

	for _, pod := range pods {
		if i.isDaemonSetPod(pod) {
			continue
		}

		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			slog.Info("Ignoring completed pod %s in namespace %s on node %s (status: %s) during eviction check",
				pod.Name, pod.Namespace, pod.Spec.NodeName, pod.Status.Phase)

			continue
		}

		if i.isPodStuckInTerminating(pod) || i.isPodNotReady(pod) {
			slog.Info("Ignoring pod in namespace on node",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"node", pod.Spec.NodeName)

			continue
		}

		filteredPods = append(filteredPods, pod)
	}

	return filteredPods
}

func (i *Informers) isDaemonSetPod(pod *v1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			slog.Info("Ignoring DaemonSet pod in namespace on node during eviction check",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"node", pod.Spec.NodeName)

			return true
		}
	}

	return false
}

func (i *Informers) isPodStuckInTerminating(pod *v1.Pod) bool {
	if pod.DeletionTimestamp == nil {
		return false
	}

	gracePeriod := 30
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		gracePeriod = int(*pod.Spec.TerminationGracePeriodSeconds)
	}

	timeoutThreshold := pod.DeletionTimestamp.Add(time.Duration(gracePeriod) * time.Second)

	// If current time is beyond the timeout threshold, pod is considered stuck
	if time.Now().After(timeoutThreshold) {
		slog.Info("Pod in namespace is stuck in terminating state",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"deletionTimestamp", pod.DeletionTimestamp,
			"gracePeriod", gracePeriod,
			"timeoutThreshold", timeoutThreshold)

		return true
	}

	return false
}

func (i *Informers) isPodNotReady(pod *v1.Pod) bool {
	notReadyTimeout := 5 * time.Minute

	if i.notReadyTimeoutMinutes != nil {
		notReadyTimeout = time.Duration(*i.notReadyTimeoutMinutes) * time.Minute
	}

	// Check if pod has any NotReady conditions
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady && condition.Status == v1.ConditionFalse {
			// Additional check: if the pod has been in NotReady state for the configured time
			if condition.LastTransitionTime.Add(notReadyTimeout).Before(time.Now()) {
				slog.Info("Pod in namespace is in NotReady state",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"lastTransitionTime", condition.LastTransitionTime,
					"timeout", notReadyTimeout)

				return true
			}
		}
	}

	return false
}

func (i *Informers) EvictAllPodsInImmediateMode(ctx context.Context,
	namespace, nodeName string, timeout time.Duration, partialDrainEntity *protos.Entity) error {
	pods, err := i.FindEvictablePodsInNamespaceAndNode(namespace, nodeName, partialDrainEntity)
	if err != nil {
		slog.Error("Failed to find evictable pods in namespace on node",
			"namespace", namespace,
			"node", nodeName,
			"error", err)

		return fmt.Errorf("failed to find evictable pods in namespace %s on node %s: %w", namespace, nodeName, err)
	}

	if len(pods) == 0 {
		return nil
	}

	err = i.evictPodsInNamespaceAndNode(ctx, namespace, timeout, pods)
	if err != nil {
		slog.Error("Failed to evict pods in namespace on node",
			"namespace", namespace,
			"node", nodeName,
			"error", err)

		return fmt.Errorf("failed to evict pods in namespace %s on node %s: %w", namespace, nodeName, err)
	}

	return nil
}

func (i *Informers) evictPodsInNamespaceAndNode(ctx context.Context,
	namespace string, timeout time.Duration, pods []*v1.Pod) error {
	var wg sync.WaitGroup

	var mu sync.Mutex

	var result *multierror.Error

	for _, pod := range pods {
		wg.Add(1)

		go func(ctx context.Context, pod *v1.Pod, timeout time.Duration) {
			defer wg.Done()

			err := i.sendEvictionRequestForPod(ctx, namespace, timeout, pod)
			if err != nil {
				if errors.IsNotFound(err) {
					slog.Info("Pod already evicted from namespace on node",
						"pod", pod.Name,
						"namespace", pod.Namespace,
						"node", pod.Spec.NodeName)
				} else {
					slog.Error("Failed to evict pod from namespace on node",
						"pod", pod.Name,
						"namespace", pod.Namespace,
						"node", pod.Spec.NodeName,
						"error", err)
					mu.Lock()

					result = multierror.Append(result, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err))

					mu.Unlock()
				}
			} else {
				slog.Info("Pod eviction initiated for namespace on node",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"node", pod.Spec.NodeName)
			}
		}(ctx, pod, timeout)
	}

	wg.Wait()

	return result.ErrorOrNil()
}

func (i *Informers) sendEvictionRequestForPod(ctx context.Context, namespace string,
	timeout time.Duration, pod *v1.Pod) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: namespace,
		},
		DeleteOptions: &metav1.DeleteOptions{
			GracePeriodSeconds: ptr.To(int64(timeout.Seconds())),
			DryRun:             i.dryRunMode,
		},
	}

	err := i.clientset.PolicyV1().Evictions(pod.Namespace).Evict(ctx, eviction)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}

		if errors.IsTooManyRequests(err) {
			metrics.ProcessingErrors.WithLabelValues("PDB_blocking_eviction_error", pod.Spec.NodeName).Inc()
		}

		return fmt.Errorf("error evicting pod %s from namespace %s: %w", pod.Name, pod.Namespace, err)
	}

	return nil
}

func (i *Informers) UpdateNodeEvent(ctx context.Context, nodeName string, reason string, message string) error {
	compositeKey := fmt.Sprintf("%s/%s", nodeName, reason)

	cachedEvents, err := i.eventInformer.GetIndexer().ByIndex(NodeEventReasonIndex, compositeKey)
	if err != nil {
		slog.Error("Failed to query event cache", "error", err)
		return fmt.Errorf("error querying event cache: %w", err)
	}

	now := metav1.NewTime(time.Now())
	eventsClient := i.clientset.CoreV1().Events(i.namespace)

	for _, obj := range cachedEvents {
		existingEvent, ok := obj.(*v1.Event)
		if !ok {
			continue
		}

		if existingEvent.Message == message {
			eventCopy := existingEvent.DeepCopy()
			eventCopy.Count++
			eventCopy.LastTimestamp = now

			_, err = eventsClient.Update(ctx, eventCopy, metav1.UpdateOptions{})
			if err != nil {
				slog.Error("Failed to update event occurrence count", "error", err)
				return fmt.Errorf("error in updating event occurrence count: %w", err)
			}

			return nil
		}
	}

	// Get node from informer cache to retrieve its UID for proper event association
	node, err := i.GetNode(nodeName)
	if err != nil {
		return fmt.Errorf("error getting node from cache %s: %w", nodeName, err)
	}

	newEvent := &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: nodeName + "-",
			Namespace:    i.namespace,
		},
		InvolvedObject: v1.ObjectReference{
			Kind:       "Node",
			Name:       nodeName,
			UID:        node.UID,
			APIVersion: "v1",
		},
		Reason:         reason,
		Message:        message,
		Type:           "NodeDraining",
		Source:         v1.EventSource{Component: "nvsentinel-node-drainer"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}

	_, err = eventsClient.Create(ctx, newEvent, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Failed to create event", "error", err, "node", nodeName, "reason", reason)
		return fmt.Errorf("error in creating event: %w", err)
	}

	return nil
}

func (i *Informers) GetNode(nodeName string) (*v1.Node, error) {
	nodeObj, exists, err := i.nodeInformer.GetIndexer().GetByKey(nodeName)
	if err != nil {
		slog.Error("Failed to get node from cache",
			"node", nodeName,
			"error", err)

		return nil, fmt.Errorf("error getting node %s from cache: %w", nodeName, err)
	}

	if !exists {
		slog.Error("Node not found in cache", "node", nodeName)

		return nil, fmt.Errorf("node %s not found in cache", nodeName)
	}

	node, ok := nodeObj.(*v1.Node)
	if !ok {
		return nil, fmt.Errorf("failed to cast node object for %s", nodeName)
	}

	return node, nil
}

func (i *Informers) DeletePodsAfterTimeout(ctx context.Context, nodeName string, namespaces []string,
	timeout int, event *model.HealthEventWithStatus, partialDrainEntity *protos.Entity) error {
	drainTimeout, err := i.getNodeDrainTimeout(timeout, event)
	if err != nil {
		slog.Error("Failed to get node drain timeout", "error", err)
		return fmt.Errorf("failed to get node drain timeout: %w", err)
	}

	timeoutDeadline := event.CreatedAt.Add(time.Duration(timeout) * time.Minute)
	deleteDateTimeUTC := timeoutDeadline.UTC().Format(time.RFC3339)
	timeoutReached := drainTimeout <= 0

	evicted, remainingPods := i.checkIfPodsPresentInNamespaceAndNode(namespaces, nodeName, partialDrainEntity)
	if evicted {
		slog.Info("All pods on node have been deleted", "node", nodeName)
		metrics.NodeDrainTimeout.WithLabelValues(nodeName).Set(0)

		return nil
	}

	if timeoutReached {
		slog.Info("Timeout reached for node, force deleting remaining pods",
			"node", nodeName,
			"count", len(remainingPods))

		// Track timeout reached for each namespace
		for _, ns := range namespaces {
			metrics.NodeDrainTimeoutReached.WithLabelValues(nodeName, ns).Inc()
		}

		metrics.NodeDrainTimeout.WithLabelValues(nodeName).Set(0)

		if err := i.forceDeletePods(ctx, remainingPods); err != nil {
			slog.Error("Failed to force delete pods on node",
				"node", nodeName,
				"error", err)

			return fmt.Errorf("failed to force delete pods on node %s: %w", nodeName, err)
		}

		// After force deleting, requeue to verify pods are gone
		return fmt.Errorf("force deleted %d pods, requeueing to verify deletion on node %s", len(remainingPods), nodeName)
	}

	metrics.NodeDrainTimeout.WithLabelValues(nodeName).Set(1)

	podNames := make([]string, 0, len(remainingPods))
	for _, pod := range remainingPods {
		podNames = append(podNames, pod.Name)
	}

	sort.Strings(podNames)

	message := fmt.Sprintf(
		"Waiting for following pods to finish: %v in namespace: %v or they will be force deleted on: %s",
		podNames, namespaces, deleteDateTimeUTC,
	)

	reason := "WaitingBeforeForceDelete"

	if err := i.UpdateNodeEvent(ctx, nodeName, reason, message); err != nil {
		slog.Error("Failed to update node event",
			"node", nodeName,
			"error", err)
	}

	slog.Info("Still waiting for pods to finish",
		"pods", podNames,
		"namespaces", namespaces,
		"node", nodeName,
		"timeout", drainTimeout)

	return fmt.Errorf("waiting for %d pods to complete or timeout (%v remaining) on node %s",
		len(remainingPods), drainTimeout, nodeName)
}

func (i *Informers) getNodeDrainTimeout(timeout int,
	event *model.HealthEventWithStatus) (time.Duration, error) {
	elapsed := time.Since(event.CreatedAt)
	drainTimeout := time.Duration(timeout) * time.Minute

	return drainTimeout - elapsed, nil
}

func (i *Informers) forceDeletePods(ctx context.Context, pods []*v1.Pod) error {
	gracePeriod := int64(0)

	var wg sync.WaitGroup

	var mu sync.Mutex

	var result *multierror.Error

	for _, pod := range pods {
		wg.Add(1)

		go func(p *v1.Pod) {
			defer wg.Done()

			err := i.clientset.CoreV1().Pods(p.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
				DryRun:             i.dryRunMode,
			})
			if err != nil {
				if !errors.IsNotFound(err) {
					slog.Error("Failed to force delete pod in namespace",
						"pod", p.Name,
						"namespace", p.Namespace,
						"error", err)
					mu.Lock()

					result = multierror.Append(result, fmt.Errorf("pod %s/%s: %w", p.Namespace, p.Name, err))

					mu.Unlock()
				}
			} else {
				slog.Info("Force deleted pod in namespace",
					"pod", p.Name,
					"namespace", p.Namespace)
			}
		}(pod)
	}

	wg.Wait()

	return result.ErrorOrNil()
}

func (i *Informers) GetNamespacesMatchingPattern(ctx context.Context,
	includePattern string, excludePattern string, nodeName string) ([]string, error) {
	objs, err := i.podInformer.GetIndexer().ByIndex(NodeIndex, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get pods on node %s: %w", nodeName, err)
	}

	excludeRegex, err := i.compileExcludePattern(excludePattern)
	if err != nil {
		slog.Error("Failed to compile exclude pattern", "error", err)
		return nil, fmt.Errorf("failed to compile exclude pattern: %w", err)
	}

	namespaceSet, err := i.extractNamespacesFromPods(objs, includePattern, excludeRegex)
	if err != nil {
		slog.Error("Failed to extract namespaces from pods", "error", err)
		return nil, fmt.Errorf("failed to extract namespaces from pods: %w", err)
	}

	return i.convertSetToSlice(namespaceSet), nil
}

func (i *Informers) compileExcludePattern(excludePattern string) (*regexp.Regexp, error) {
	if excludePattern == "" {
		return nil, nil
	}

	excludeRegex, err := regexp.Compile(excludePattern)
	if err != nil {
		slog.Error("Failed to compile exclude pattern", "error", err)
		return nil, fmt.Errorf("invalid exclude regex %s: %w", excludePattern, err)
	}

	return excludeRegex, nil
}

func (i *Informers) extractNamespacesFromPods(objs []any,
	includePattern string, excludeRegex *regexp.Regexp) (map[string]struct{}, error) {
	namespaceSet := make(map[string]struct{})

	for _, obj := range objs {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			continue
		}

		shouldIncludeNamespace, err := i.shouldIncludeNamespace(pod.Namespace, includePattern, excludeRegex)
		if err != nil {
			slog.Error("Failed to check if namespace should be included",
				"namespace", pod.Namespace,
				"error", err)

			return nil, fmt.Errorf("failed to check if namespace %s should be included: %w", pod.Namespace, err)
		}

		if shouldIncludeNamespace {
			namespaceSet[pod.Namespace] = struct{}{}
		}
	}

	return namespaceSet, nil
}

func (i *Informers) shouldIncludeNamespace(namespace string,
	includePattern string, excludeRegex *regexp.Regexp) (bool, error) {
	if excludeRegex != nil && excludeRegex.MatchString(namespace) {
		return false, nil
	}

	includeMatches, err := filepath.Match(includePattern, namespace)
	if err != nil {
		slog.Error("Failed to match include pattern", "error", err)
		return false, fmt.Errorf("failed to match include pattern: %w", err)
	}

	return includeMatches, nil
}

func (i *Informers) convertSetToSlice(namespaceSet map[string]struct{}) []string {
	namespaceNames := make([]string, 0, len(namespaceSet))
	for namespace := range namespaceSet {
		namespaceNames = append(namespaceNames, namespace)
	}

	return namespaceNames
}

func (i *Informers) checkIfPodsPresentInNamespaceAndNode(namespaces []string, nodeName string,
	partialDrainEntity *protos.Entity) (bool, []*v1.Pod) {
	allEvicted := true

	var remainingPods []*v1.Pod

	for _, namespace := range namespaces {
		pods, err := i.FindEvictablePodsInNamespaceAndNode(namespace, nodeName, partialDrainEntity)
		if err != nil {
			slog.Error("Failed to check namespace on node",
				"namespace", namespace,
				"node", nodeName,
				"error", err)

			allEvicted = false

			continue
		}

		if len(pods) > 0 {
			remainingPods = append(remainingPods, pods...)
			allEvicted = false
		}
	}

	return allEvicted, remainingPods
}

func (i *Informers) CheckIfAllPodsAreEvictedInImmediateMode(ctx context.Context,
	namespaces []string, nodeName string, timeout time.Duration, partialDrainEntity *protos.Entity) bool {
	allEvicted, remainingPods := i.checkIfPodsPresentInNamespaceAndNode(namespaces, nodeName, partialDrainEntity)

	if allEvicted {
		slog.Info("All pods evicted in namespace from node",
			"namespaces", namespaces,
			"node", nodeName)

		return true
	}

	now := time.Now()
	shouldForceDelete := false

	for _, pod := range remainingPods {
		if pod.DeletionTimestamp != nil {
			gracePeriod := 30 * time.Second
			if pod.Spec.TerminationGracePeriodSeconds != nil {
				gracePeriod = time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second
			}

			deletionDeadline := pod.DeletionTimestamp.Add(gracePeriod).Add(timeout)
			if now.After(deletionDeadline) {
				shouldForceDelete = true
				break
			}
		}
	}

	if shouldForceDelete {
		slog.Info("Pods on node exceeded timeout, attempting force deletion",
			"node", nodeName)

		err := i.forceDeletePods(ctx, remainingPods)
		if err != nil {
			metrics.ProcessingErrors.WithLabelValues("pods_force_deletion_error", nodeName).Inc()
			slog.Error("Failed to force delete pods on node",
				"node", nodeName,
				"error", err)

			return false
		}

		allEvicted, _ = i.checkIfPodsPresentInNamespaceAndNode(namespaces, nodeName, partialDrainEntity)
		if allEvicted {
			slog.Info("All pods evicted after force deletion on node",
				"node", nodeName)
		}

		return allEvicted
	}

	remainingPodNames := make([]string, 0, len(remainingPods))
	for _, pod := range remainingPods {
		remainingPodNames = append(remainingPodNames, fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
	}

	slog.Info("Pods still present on node, will retry",
		"node", nodeName,
		"pods", remainingPodNames)

	return false
}
