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
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	resourcev1listers "k8s.io/client-go/listers/resource/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/nvidia/nvsentinel/commons/pkg/nodecache"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/node-drainer/pkg/metrics"
)

const (
	NodeIndex          = "node"
	NamespaceNodeIndex = "namespace-node"
)

type Informers struct {
	podInformer            cache.SharedIndexInformer
	nodeInformer           cache.SharedIndexInformer
	claimInformer          cache.SharedIndexInformer
	claimLister            resourcev1listers.ResourceClaimLister
	eventBroadcaster       record.EventBroadcaster
	eventRecorder          record.EventRecorder
	clientset              kubernetes.Interface
	notReadyTimeoutMinutes *int
	drainGPUPods           bool
	entityTypes            model.PartialRemediationEntityTypes
	dryRunMode             []string
}

// PodFilter narrows an action to pods assigned to its drain mode.
type PodFilter func(*v1.Pod) bool

// NewInformers creates the pod, node and ResourceClaim informers used to observe drainable workloads.
// The pod cache retains only the labels named by podLabelKeys. The node cache retains
// the fields in nodeFields. Build it with NodeFields, and add each Node rule with
// AddNodeExpression. entityTypes names the devices that partial drains and drainGPUPods
// look for.
func NewInformers(clientset kubernetes.Interface, resyncPeriod time.Duration,
	notReadyTimeoutMinutes *int, drainGPUPods bool, entityTypes model.PartialRemediationEntityTypes,
	dryRun bool, systemNamespaces string, nodeFields nodecache.Keys, podLabelKeys ...string) (*Informers, error) {
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
	)

	podInformer := informerFactory.Core().V1().Pods().Informer()

	systemNamespacesRegex, err := compileExcludePattern(systemNamespaces)
	if err != nil {
		return nil, fmt.Errorf("failed to compile system namespaces regex: %w", err)
	}

	if err := podInformer.SetTransform(excludedPodTransform(systemNamespacesRegex, podLabelKeys...)); err != nil {
		return nil, fmt.Errorf("failed to set pod informer transform: %w", err)
	}

	err = podInformer.GetIndexer().AddIndexers(
		cache.Indexers{
			NodeIndex:          NodeIndexFunc,
			NamespaceNodeIndex: NamespaceNodeIndexFunc,
		})
	if err != nil {
		return nil, fmt.Errorf("failed to add indexer: %w", err)
	}

	nodeInformer := informerFactory.Core().V1().Nodes().Informer()
	if err := nodeInformer.SetTransform(nodeTransform(nodeFields)); err != nil {
		return nil, fmt.Errorf("failed to set node informer transform: %w", err)
	}

	// Partial drains read the DRA claims of pods to find their devices, so they read them from a cache instead of
	// calling the API server for each pod on each pass.
	claims := informerFactory.Resource().V1().ResourceClaims()
	if err := claims.Informer().SetTransform(resourceClaimTransform); err != nil {
		return nil, fmt.Errorf("failed to set ResourceClaim informer transform: %w", err)
	}

	eventBroadcaster := record.NewBroadcaster()

	dryRunMode := []string{}
	if dryRun {
		dryRunMode = []string{metav1.DryRunAll}
	}

	return &Informers{
		clientset:        clientset,
		podInformer:      podInformer,
		nodeInformer:     nodeInformer,
		claimInformer:    claims.Informer(),
		claimLister:      claims.Lister(),
		eventBroadcaster: eventBroadcaster,
		eventRecorder: eventBroadcaster.NewRecorder(
			scheme.Scheme,
			v1.EventSource{Component: "nvsentinel-node-drainer"},
		),
		notReadyTimeoutMinutes: notReadyTimeoutMinutes,
		drainGPUPods:           drainGPUPods,
		entityTypes:            entityTypes,
		dryRunMode:             dryRunMode,
	}, nil
}

// excludedPodTransform keeps only identity for system and DaemonSet pods so they
// stay out of node indexes, and retains drain fields and requested labels for other pods.
func excludedPodTransform(systemNamespacesRegex *regexp.Regexp, podLabelKeys ...string) cache.TransformFunc {
	return func(obj any) (any, error) {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			return obj, nil
		}

		isSystemNamespace := systemNamespacesRegex != nil && systemNamespacesRegex.MatchString(pod.Namespace)
		if !isSystemNamespace && !isDaemonSetOwned(pod.OwnerReferences) {
			return drainEligiblePodCacheObject(pod, podLabelKeys...), nil
		}

		return &v1.Pod{
			ObjectMeta: identityObjectMeta(
				pod.Name,
				pod.Namespace,
				pod.UID,
				pod.ResourceVersion,
				nil,
			),
		}, nil
	}
}

// drainEligiblePodCacheObject retains only fields used by pod indexes and drain decisions.
// Keep this contract in sync with the cached Pod reads in this package.
func drainEligiblePodCacheObject(pod *v1.Pod, podLabelKeys ...string) *v1.Pod {
	var podLabels map[string]string

	for _, key := range podLabelKeys {
		if value, exists := pod.Labels[key]; exists {
			if podLabels == nil {
				podLabels = make(map[string]string)
			}

			podLabels[key] = value
		}
	}

	var annotations map[string]string
	if devices, exists := pod.Annotations[model.PodDeviceAnnotationName]; exists {
		annotations = map[string]string{model.PodDeviceAnnotationName: devices}
	}

	ownerReferences := make([]metav1.OwnerReference, len(pod.OwnerReferences))
	for idx, owner := range pod.OwnerReferences {
		ownerReferences[idx] = metav1.OwnerReference{Kind: owner.Kind}

		// Pod drain policies match ownerKinds against the controller reference only.
		if owner.Controller != nil {
			ownerReferences[idx].Controller = new(*owner.Controller)
		}
	}

	var deletionTimestamp *metav1.Time
	if pod.DeletionTimestamp != nil {
		deletionTimestamp = pod.DeletionTimestamp.DeepCopy()
	}

	var terminationGracePeriodSeconds *int64
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		terminationGracePeriodSeconds = new(*pod.Spec.TerminationGracePeriodSeconds)
	}

	return &v1.Pod{
		TypeMeta:          pod.TypeMeta,
		Name:              pod.Name,
		Namespace:         pod.Namespace,
		UID:               pod.UID,
		ResourceVersion:   pod.ResourceVersion,
		Labels:            podLabels,
		Annotations:       annotations,
		OwnerReferences:   ownerReferences,
		DeletionTimestamp: deletionTimestamp,
		Spec: v1.PodSpec{
			NodeName:                      pod.Spec.NodeName,
			TerminationGracePeriodSeconds: terminationGracePeriodSeconds,
			Containers:                    trimContainers(pod.Spec.Containers),
			InitContainers:                trimContainers(pod.Spec.InitContainers),
			ResourceClaims:                pod.Spec.ResourceClaims,
		},
		Status: v1.PodStatus{
			Phase:                       pod.Status.Phase,
			Conditions:                  trimPodReadyConditions(pod.Status.Conditions),
			ResourceClaimStatuses:       pod.Status.ResourceClaimStatuses,
			ExtendedResourceClaimStatus: pod.Status.ExtendedResourceClaimStatus,
		},
	}
}

// resourceClaimTransform retains only the fields that drains read from a ResourceClaim.
func resourceClaimTransform(obj any) (any, error) {
	claim, ok := obj.(*resourcev1.ResourceClaim)
	if !ok {
		return obj, nil
	}

	return drainResourceClaimCacheObject(claim), nil
}

// drainResourceClaimCacheObject retains the identity of a claim, the DeviceClasses of its requests, and the drivers
// of its allocation. Keep this contract in sync with the cached claim reads in this package (claimDeviceClasses and
// claimDrivers). Cached claims are pruned, so never update one.
func drainResourceClaimCacheObject(claim *resourcev1.ResourceClaim) *resourcev1.ResourceClaim {
	return &resourcev1.ResourceClaim{
		TypeMeta: claim.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:            claim.Name,
			Namespace:       claim.Namespace,
			UID:             claim.UID,
			ResourceVersion: claim.ResourceVersion,
		},
		Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{
			Requests: trimClaimRequests(claim.Spec.Devices.Requests),
		}},
		Status: resourcev1.ResourceClaimStatus{Allocation: trimClaimAllocation(claim.Status.Allocation)},
	}
}

func trimClaimRequests(requests []resourcev1.DeviceRequest) []resourcev1.DeviceRequest {
	cached := make([]resourcev1.DeviceRequest, len(requests))

	for idx, request := range requests {
		if request.Exactly != nil {
			cached[idx].Exactly = &resourcev1.ExactDeviceRequest{DeviceClassName: request.Exactly.DeviceClassName}
		}

		if len(request.FirstAvailable) > 0 {
			cached[idx].FirstAvailable = make([]resourcev1.DeviceSubRequest, len(request.FirstAvailable))
			for subIdx, subRequest := range request.FirstAvailable {
				cached[idx].FirstAvailable[subIdx].DeviceClassName = subRequest.DeviceClassName
			}
		}
	}

	return cached
}

// trimClaimAllocation keeps a nil allocation nil, because an allocated claim and a claim that is not allocated yet
// are read differently.
func trimClaimAllocation(allocation *resourcev1.AllocationResult) *resourcev1.AllocationResult {
	if allocation == nil {
		return nil
	}

	results := make([]resourcev1.DeviceRequestAllocationResult, len(allocation.Devices.Results))
	for idx, result := range allocation.Devices.Results {
		results[idx].Driver = result.Driver
	}

	return &resourcev1.AllocationResult{Devices: resourcev1.DeviceAllocationResult{Results: results}}
}

func trimContainers(containers []v1.Container) []v1.Container {
	cached := make([]v1.Container, len(containers))
	for idx, container := range containers {
		limits := make(v1.ResourceList, len(container.Resources.Limits))
		for resourceName, quantity := range container.Resources.Limits {
			limits[resourceName] = quantity.DeepCopy()
		}

		// The name links a container to its entry in status.extendedResourceClaimStatus.
		cached[idx].Name = container.Name
		cached[idx].Resources.Limits = limits
	}

	return cached
}

func trimPodReadyConditions(conditions []v1.PodCondition) []v1.PodCondition {
	var cached []v1.PodCondition

	for _, condition := range conditions {
		if condition.Type != v1.PodReady {
			continue
		}

		cached = append(cached, v1.PodCondition{
			Type:               condition.Type,
			Status:             condition.Status,
			LastTransitionTime: condition.LastTransitionTime,
		})
	}

	return cached
}

// NodeFields is the starting pruning set for the node cache. labelKeys are the
// keys customDrain.nodeSelector reads. A caller adds each Node rule to the
// result with AddNodeExpression. With no keys and no rules, the cache keeps
// only the annotations in NodeAnnotationKeys.
func NodeFields(labelKeys []string) nodecache.Keys {
	fields := nodecache.NewPruningKeys()

	for _, key := range labelKeys {
		fields.AddLabel(key)
	}

	return fields
}

// NodeAnnotationKeys are the annotations node-drainer reads for its own use.
// The node cache keeps them whatever nodeFields holds.
func NodeAnnotationKeys() []string {
	return []string{common.QuarantineHealthEventAnnotationKey}
}

// nodeTransform keeps the identity of the node, NodeAnnotationKeys, and
// the fields in nodeFields. Status is always dropped. Spec and the rest of
// metadata are kept only when a Node rule was added, because that is when a
// rule can read them. With no rules the cache stays as small as it is today.
func nodeTransform(nodeFields nodecache.Keys) cache.TransformFunc {
	annotationKeys := NodeAnnotationKeys()

	return func(obj any) (any, error) {
		node, ok := obj.(*v1.Node)
		if !ok {
			return obj, nil
		}

		objectMeta := cachedNodeObjectMeta(node, nodeFields)
		objectMeta.Labels = cachedNodeLabels(node.Labels, nodeFields)
		objectMeta.Annotations = cachedNodeAnnotations(node.Annotations, nodeFields, annotationKeys)

		return &v1.Node{
			ObjectMeta: objectMeta,
			Spec:       cachedNodeSpec(node, nodeFields),
		}, nil
	}
}

// cachedNodeObjectMeta returns the metadata of node without labels and
// annotations, which the caller prunes. With no Node rule it keeps only the
// identity. With a Node rule it keeps every other metadata field, such as
// generation and creationTimestamp, because a rule can read any of them.
// managedFields is large, so it is kept only when a rule reads it.
func cachedNodeObjectMeta(node *v1.Node, nodeFields nodecache.Keys) metav1.ObjectMeta {
	if !nodeFields.HasNodeRule() {
		return identityObjectMeta(node.Name, "", node.UID, node.ResourceVersion, nil)
	}

	objectMeta := node.ObjectMeta
	objectMeta.Labels = nil
	objectMeta.Annotations = nil

	if nodeFields.PruneManagedFields() {
		objectMeta.ManagedFields = nil
	}

	return *objectMeta.DeepCopy()
}

// cachedNodeSpec keeps spec when a Node rule can read it, and drops it otherwise.
func cachedNodeSpec(node *v1.Node, nodeFields nodecache.Keys) v1.NodeSpec {
	if !nodeFields.HasNodeRule() {
		return v1.NodeSpec{}
	}

	return *node.Spec.DeepCopy()
}

// cachedNodeLabels keeps the named labels, or every label when a rule reads
// the label map as a whole.
func cachedNodeLabels(in map[string]string, nodeFields nodecache.Keys) map[string]string {
	if !nodeFields.PruneLabels() {
		return maps.Clone(in)
	}

	return withoutEmpty(nodeFields.RetainLabels(in, nil), nodeFields)
}

// cachedNodeAnnotations keeps the named annotations and the annotations
// node-drainer reads for its own use. A rule that reads the annotation map as
// a whole keeps every annotation.
func cachedNodeAnnotations(in map[string]string, nodeFields nodecache.Keys, ownKeys []string) map[string]string {
	if !nodeFields.PruneAnnotations() {
		return maps.Clone(in)
	}

	return withoutEmpty(nodeFields.RetainAnnotations(in, ownKeys), nodeFields)
}

// withoutEmpty turns a pruned map with no entries into nil when no Node rule
// exists, which keeps the cache as it was before rules could widen it.
//
// With a Node rule, an empty map stays empty. celrule.NodeToUnstructured gives
// a rule an empty map only for a non-nil source map. For a nil map, a guard such
// as "scheduler" in node.metadata.labels fails with "no such key" instead of
// evaluating to false.
func withoutEmpty(in map[string]string, nodeFields nodecache.Keys) map[string]string {
	if len(in) > 0 || nodeFields.HasNodeRule() {
		return in
	}

	return nil
}

func identityObjectMeta(name, namespace string, uid types.UID, resourceVersion string,
	annotations map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:            name,
		Namespace:       namespace,
		UID:             uid,
		ResourceVersion: resourceVersion,
		Annotations:     annotations,
	}
}

func isDaemonSetOwned(ownerReferences []metav1.OwnerReference) bool {
	for _, owner := range ownerReferences {
		if owner.Kind == "DaemonSet" {
			return true
		}
	}

	return false
}

func (i *Informers) HasSynced() bool {
	return i.podInformer.HasSynced() && i.nodeInformer.HasSynced() && i.claimInformer.HasSynced()
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

func (i *Informers) Run(ctx context.Context) error {
	i.eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: i.clientset.CoreV1().Events(metav1.NamespaceDefault),
	})

	go i.podInformer.Run(ctx.Done())
	go i.nodeInformer.Run(ctx.Done())
	go i.claimInformer.Run(ctx.Done())
	go func() {
		<-ctx.Done()
		i.eventBroadcaster.Shutdown()
	}()

	if ok := cache.WaitForCacheSync(ctx.Done(),
		i.HasSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	return nil
}

// FindEvictablePodsInNamespaceAndNode returns cached pods that pass the drain eligibility,
// GPU, partial-drain, and supplied pod filters. All non-nil pod filters must match.
func (i *Informers) FindEvictablePodsInNamespaceAndNode(ctx context.Context, namespace, nodeName string,
	partialDrainEntity *protos.Entity, podFilters ...PodFilter) ([]*v1.Pod, error) {
	compositeKey := fmt.Sprintf("%s/%s", namespace, nodeName)

	objs, err := i.podInformer.GetIndexer().ByIndex(NamespaceNodeIndex, compositeKey)
	if err != nil {
		slog.Error("Failed to get pods by index", "error", err)
		return nil, fmt.Errorf("failed to get pods by index: %w", err)
	}

	pods := make([]*v1.Pod, 0, len(objs))

	for _, obj := range objs {
		if pod, ok := obj.(*v1.Pod); ok {
			if matchesPodFilters(pod, podFilters) {
				pods = append(pods, pod)
			}
		}
	}

	pods = i.filterEvictablePods(pods, partialDrainEntity)

	if i.drainGPUPods && partialDrainEntity == nil {
		pods, err = i.filterPodsWithGPURequests(ctx, pods)
		if err != nil {
			return nil, fmt.Errorf("failed to filter pods with GPU requests: %w", err)
		}
	}

	pods, err = i.filterPodsUsingEntity(ctx, pods, partialDrainEntity, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to filter pods using entity: %w", err)
	}

	return pods, nil
}

// matchesPodFilters accepts a pod when every non-nil filter accepts it.
func matchesPodFilters(pod *v1.Pod, filters []PodFilter) bool {
	for _, filter := range filters {
		if filter != nil && !filter(pod) {
			return false
		}
	}

	return true
}

/*
filterPodsUsingEntity keeps the pods that use the partialDrainEntity. A nil partialDrainEntity is a full drain, so
every pod is kept. drain.PartialDrainEntity picks the entity from the scopedActions of
partialRemediationEntityTypes, for example GPU_UUID for COMPONENT_RESET.

The metadata-collector pod device mapper records the devices of each pod in a pod annotation, keyed by the
device-plugin resource name or the DRA driver name:

	annotations:
	  dgxc.nvidia.com/devices: '{"devices":{"nvidia.com/gpu":["GPU-123"]}}'

For an event with GPU_UUID = GPU-123, we only drain the pods whose annotation lists GPU-123 under a resource name
or a driver of the GPU_UUID entity type.

The annotation can miss a device: the mapper has not seen the pod yet, or it wrote the annotation before the
operator configured that resource name or driver. Such a pod could use the failed device, so the drain fails and
is retried until the mapper records the device.
*/
func (i *Informers) filterPodsUsingEntity(ctx context.Context, pods []*v1.Pod, partialDrainEntity *protos.Entity,
	nodeName string) ([]*v1.Pod, error) {
	if partialDrainEntity == nil {
		return pods, nil
	}

	entityType, supportedEntity := i.entityTypes[partialDrainEntity.EntityType]
	if !supportedEntity {
		return nil, fmt.Errorf("unsupported impacted entity for partial drains: %s",
			partialDrainEntity.EntityType)
	}

	slog.Info("Filtering pods using entity for a partial node drain",
		"nodeName", nodeName, "entityType", partialDrainEntity.EntityType,
		"entityValue", partialDrainEntity.EntityValue)

	annotationKeys := entityType.AnnotationKeys()

	var filteredPods []*v1.Pod

	for _, pod := range pods {
		deviceAnnotation, hasAnnotation, err := podDeviceAnnotation(pod)
		if err != nil {
			return nil, fmt.Errorf("failed to read the device annotation of pod %s: %w", pod.Name, err)
		}

		if err := i.checkAnnotationRecordsRequests(ctx, pod, entityType, deviceAnnotation, hasAnnotation); err != nil {
			return nil, fmt.Errorf("failed to scope the partial drain to %s %s: %w",
				partialDrainEntity.EntityType, partialDrainEntity.EntityValue, err)
		}

		if isPodUsingPartialDrainEntity(deviceAnnotation, annotationKeys, partialDrainEntity) {
			slog.Info("Pod is eligible to be drained since it's using impacted entity",
				"podName", pod.Name, "nodeName", nodeName, "entityType", partialDrainEntity.EntityType,
				"entityValue", partialDrainEntity.EntityValue)
			filteredPods = append(filteredPods, pod)
		}
	}

	return filteredPods, nil
}

// podDeviceAnnotation parses the device annotation of the pod. The bool is false when the pod has none.
func podDeviceAnnotation(pod *v1.Pod) (model.DeviceAnnotation, bool, error) {
	var deviceAnnotation model.DeviceAnnotation

	deviceAnnotationJSON, ok := pod.Annotations[model.PodDeviceAnnotationName]
	if !ok {
		return deviceAnnotation, false, nil
	}

	if err := json.Unmarshal([]byte(deviceAnnotationJSON), &deviceAnnotation); err != nil {
		return deviceAnnotation, true, fmt.Errorf("error unmarshalling device annotation for pod %s: %w", pod.Name, err)
	}

	return deviceAnnotation, true, nil
}

// checkAnnotationRecordsRequests returns an error when the pod requests a device of the entity type that its
// device annotation has no entry for.
func (i *Informers) checkAnnotationRecordsRequests(ctx context.Context, pod *v1.Pod,
	entityType model.PartialRemediationEntityType, deviceAnnotation model.DeviceAnnotation, hasAnnotation bool) error {
	unrecorded, err := i.unrecordedRequest(ctx, pod, entityType, deviceAnnotation)
	if err != nil {
		return fmt.Errorf("failed to check device requests for pod %s: %w", pod.Name, err)
	}

	switch {
	case unrecorded == "":
		return nil
	case !hasAnnotation:
		return fmt.Errorf("pod %s is requesting devices but is missing device annotation; "+
			"partial drains need the metadata-collector pod device mapper (podMapper.enabled)", pod.Name)
	default:
		// The mapper may not have seen the pod yet, or it may not support the device at all. Either way the pod
		// could use the failed device, so the drain waits.
		return fmt.Errorf("pod %s is requesting %s but its device annotation has no entry for it; "+
			"the partial drain waits until the metadata-collector pod device mapper records the device, "+
			"and a device that the mapper does not support keeps it waiting", pod.Name, unrecorded)
	}
}

// unrecordedRequest describes the first device of the entity type that the pod requests and the device
// annotation has no entry for, or returns "" when the annotation records every request. Resource names come
// from container limits. DRA requests come from the drivers of the pod's claims (see claimDrivers), which are the
// names the annotation uses.
func (i *Informers) unrecordedRequest(ctx context.Context, pod *v1.Pod,
	entityType model.PartialRemediationEntityType, deviceAnnotation model.DeviceAnnotation) (string, error) {
	if unrecorded := unrecordedResourceName(pod, entityType, deviceAnnotation); unrecorded != "" {
		return unrecorded, nil
	}

	// Reading a claim is an API call, so skip it when the annotation has an entry for every driver.
	if len(pod.Spec.ResourceClaims) == 0 || hasAnnotationEntries(deviceAnnotation, entityType.Drivers()) {
		return "", nil
	}

	claimed, err := i.claimedDrivers(ctx, pod, entityType)
	if err != nil {
		return "", fmt.Errorf("failed to read the DRA drivers of pod %s: %w", pod.Name, err)
	}

	for _, device := range claimed {
		if !hasAnnotationEntry(deviceAnnotation, device.driver) {
			return device.description, nil
		}
	}

	return "", nil
}

// unrecordedResourceName describes the first resource name of the entity type that the pod requests in its
// container limits and the device annotation has no entry for, or returns "".
func unrecordedResourceName(pod *v1.Pod, entityType model.PartialRemediationEntityType,
	deviceAnnotation model.DeviceAnnotation) string {
	for _, resourceName := range entityType.ResourceNames() {
		byDevicePlugin, byDRA := resourceRequestSources(pod, resourceName)
		if byDevicePlugin && !hasAnnotationEntry(deviceAnnotation, resourceName) {
			return "resource " + resourceName
		}

		// A DRA driver can serve a resource name through a claim that the scheduler generates
		// (status.extendedResourceClaimStatus). The kubelet reports those devices as DRA devices, so the mapper
		// records them under the driver name, not the resource name.
		if byDRA && !hasAnyAnnotationEntry(deviceAnnotation, entityType.Drivers()) {
			return "resource " + resourceName + " through a DRA driver"
		}
	}

	return ""
}

func hasAnnotationEntry(deviceAnnotation model.DeviceAnnotation, name string) bool {
	_, ok := deviceAnnotation.Devices[name]
	return ok
}

func hasAnnotationEntries(deviceAnnotation model.DeviceAnnotation, names []string) bool {
	for _, name := range names {
		if !hasAnnotationEntry(deviceAnnotation, name) {
			return false
		}
	}

	return true
}

func hasAnyAnnotationEntry(deviceAnnotation model.DeviceAnnotation, names []string) bool {
	return slices.ContainsFunc(names, func(name string) bool { return hasAnnotationEntry(deviceAnnotation, name) })
}

// resourceRequestSources reports whether a container of the pod gets the resource from a device plugin, and
// whether a container gets it from a DRA driver through the claim that the scheduler generates for it.
func resourceRequestSources(pod *v1.Pod, resourceName string) (byDevicePlugin, byDRA bool) {
	resourceNames := []string{resourceName}

	for _, containers := range [][]v1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for _, container := range containers {
			if !areContainersRequestingDevice([]v1.Container{container}, resourceNames) {
				continue
			}

			if isResourceServedByDRA(pod, container.Name, resourceName) {
				byDRA = true
			} else {
				byDevicePlugin = true
			}
		}
	}

	return byDevicePlugin, byDRA
}

// isResourceServedByDRA reports whether status.extendedResourceClaimStatus maps the resource of the container to
// a DRA request. The scheduler writes it only when a DRA driver serves the resource name.
func isResourceServedByDRA(pod *v1.Pod, containerName, resourceName string) bool {
	status := pod.Status.ExtendedResourceClaimStatus
	if status == nil {
		return false
	}

	return slices.ContainsFunc(status.RequestMappings, func(mapping v1.ContainerExtendedResourceRequest) bool {
		return mapping.ContainerName == containerName && mapping.ResourceName == resourceName
	})
}

// isPodRequestingDevices reports whether a pod requests any of the devices through container limits or DRA claims.
func (i *Informers) isPodRequestingDevices(ctx context.Context, pod *v1.Pod,
	devices model.PartialRemediationEntityType) (bool, error) {
	resourceNames := devices.ResourceNames()
	if areContainersRequestingDevice(pod.Spec.Containers, resourceNames) ||
		areContainersRequestingDevice(pod.Spec.InitContainers, resourceNames) {
		return true, nil
	}

	return i.isPodClaimingDeviceClass(ctx, pod, devices.DeviceClasses())
}

// isPodClaimingDeviceClass reports whether any DRA claim of the pod requests one of the device classes.
func (i *Informers) isPodClaimingDeviceClass(ctx context.Context, pod *v1.Pod,
	deviceClassNames []string) (bool, error) {
	if len(deviceClassNames) == 0 {
		return false, nil
	}

	for _, podClaim := range pod.Spec.ResourceClaims {
		claim, err := i.getPodResourceClaim(ctx, pod, podClaim)
		if err != nil {
			return false, fmt.Errorf("failed to resolve claim %s of pod %s/%s: %w",
				podClaim.Name, pod.Namespace, pod.Name, err)
		}

		if claim != nil && isClaimRequestingDeviceClass(&claim.Spec, deviceClassNames) {
			return true, nil
		}
	}

	return false, nil
}

// claimedDriver is a DRA driver of the entity type that a claim of the pod uses, with a description for errors.
type claimedDriver struct {
	driver      string
	description string
}

// claimedDrivers returns the DRA drivers of the entity type that the claims of the pod use.
func (i *Informers) claimedDrivers(ctx context.Context, pod *v1.Pod,
	entityType model.PartialRemediationEntityType) ([]claimedDriver, error) {
	var claimed []claimedDriver

	for _, podClaim := range pod.Spec.ResourceClaims {
		claim, err := i.getPodResourceClaim(ctx, pod, podClaim)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve claim %s of pod %s/%s: %w",
				podClaim.Name, pod.Namespace, pod.Name, err)
		}

		if claim != nil {
			claimed = append(claimed, claimDrivers(claim, entityType)...)
		}
	}

	return claimed, nil
}

// claimDrivers returns the DRA drivers of the entity type that the claim uses. For an allocated claim, they come
// from the allocation, so a FirstAvailable sub-request that was not selected does not count. For a claim that is
// not allocated yet, they come from the DeviceClasses of every request and sub-request.
func claimDrivers(claim *resourcev1.ResourceClaim, entityType model.PartialRemediationEntityType) []claimedDriver {
	var claimed []claimedDriver

	if allocation := claim.Status.Allocation; allocation != nil {
		drivers := entityType.Drivers()

		for _, result := range allocation.Devices.Results {
			if slices.Contains(drivers, result.Driver) {
				claimed = append(claimed, claimedDriver{
					driver:      result.Driver,
					description: "a device of DRA driver " + result.Driver,
				})
			}
		}

		return claimed
	}

	for _, deviceClass := range claimDeviceClasses(&claim.Spec) {
		if driver, ok := entityType.DriverForDeviceClass(deviceClass); ok {
			claimed = append(claimed, claimedDriver{
				driver:      driver,
				description: fmt.Sprintf("DeviceClass %s of DRA driver %s", deviceClass, driver),
			})
		}
	}

	return claimed
}

// getPodResourceClaim returns the ResourceClaim a pod claim uses, or nil when the pod needs no
// claim for it. A claim generated from a ResourceClaimTemplate is bound to the pod in status.resourceClaimStatuses
// before the pod is scheduled, so the template itself is never read and deleting it does not affect the lookup.
func (i *Informers) getPodResourceClaim(_ context.Context, pod *v1.Pod,
	podClaim v1.PodResourceClaim) (*resourcev1.ResourceClaim, error) {
	claimName := podClaim.ResourceClaimName
	if claimName == nil {
		for _, claimStatus := range pod.Status.ResourceClaimStatuses {
			if claimStatus.Name == podClaim.Name {
				// An unset name means generating a claim was not necessary, so the entry holds no device.
				if claimStatus.ResourceClaimName == nil {
					return nil, nil
				}

				claimName = claimStatus.ResourceClaimName

				break
			}
		}
	}

	if claimName == nil {
		return nil, fmt.Errorf("no ResourceClaim is bound to claim %s of pod %s/%s", podClaim.Name, pod.Namespace, pod.Name)
	}

	// A claim is created and allocated before its pod starts, so the cache has it. A claim that is not in the cache
	// fails the drain, which is retried.
	claim, err := i.claimLister.ResourceClaims(pod.Namespace).Get(*claimName)
	if err != nil {
		return nil, fmt.Errorf("failed to get ResourceClaim %s/%s from the cache: %w", pod.Namespace, *claimName, err)
	}

	return claim, nil
}

func isClaimRequestingDeviceClass(claimSpec *resourcev1.ResourceClaimSpec, deviceClassNames []string) bool {
	return slices.ContainsFunc(claimDeviceClasses(claimSpec), func(deviceClass string) bool {
		return slices.Contains(deviceClassNames, deviceClass)
	})
}

// claimDeviceClasses returns the DeviceClasses of every request and sub-request of the claim.
func claimDeviceClasses(claimSpec *resourcev1.ResourceClaimSpec) []string {
	var deviceClasses []string

	for _, request := range claimSpec.Devices.Requests {
		if request.Exactly != nil {
			deviceClasses = append(deviceClasses, request.Exactly.DeviceClassName)
		}

		for _, subRequest := range request.FirstAvailable {
			deviceClasses = append(deviceClasses, subRequest.DeviceClassName)
		}
	}

	return deviceClasses
}

func isPodUsingPartialDrainEntity(deviceAnnotation model.DeviceAnnotation, annotationKeys []string,
	partialDrainEntity *protos.Entity) bool {
	for _, key := range annotationKeys {
		if devicesForKey, ok := deviceAnnotation.Devices[key]; ok {
			if slices.Contains(devicesForKey, partialDrainEntity.EntityValue) {
				return true
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

func (i *Informers) filterEvictablePods(pods []*v1.Pod, partialDrainEntity *protos.Entity) []*v1.Pod {
	filteredPods := []*v1.Pod{}

	for _, pod := range pods {
		if i.isDaemonSetPod(pod) {
			continue
		}

		// Completed pods hold no devices and are never evicted or waited for. Evicted pods can
		// stay on a node by the hundred, and this runs on every scan, so keep it at debug level.
		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			slog.Debug("Ignoring completed pod during eviction check",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"node", pod.Spec.NodeName,
				"phase", pod.Status.Phase)

			continue
		}

		// Only skip draining for pods stuck terminating or not ready if we are executing a full drain.
		// There is no guarantee that a pod in either state does not still have a GPU device handle that
		// would cause a GPU reset to fail.
		if partialDrainEntity == nil && (i.isPodStuckInTerminating(pod) || i.isPodNotReady(pod)) {
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

// filterPodsWithGPURequests keeps pods with a device annotation, and unannotated pods that request a device of any
// entity type in partialRemediationEntityTypes through container limits or DRA claims, so a missed
// metadata-collector update does not leave a device pod running.
func (i *Informers) filterPodsWithGPURequests(ctx context.Context, pods []*v1.Pod) ([]*v1.Pod, error) {
	filteredPods := []*v1.Pod{}

	for _, pod := range pods {
		_, isPodRequestingGPU := pod.Annotations[model.PodDeviceAnnotationName]
		if !isPodRequestingGPU {
			var err error

			isPodRequestingGPU, err = i.isPodRequestingDevices(ctx, pod, i.entityTypes.AllDevices())
			if err != nil {
				return nil, fmt.Errorf("failed to check GPU requests for pod %s: %w", pod.Name, err)
			}
		}

		if isPodRequestingGPU {
			slog.Info("Pod is eligible for draining as it is requesting GPU",
				"pod", pod.Name,
				"namespace", pod.Namespace,
				"node", pod.Spec.NodeName,
			)
			filteredPods = append(filteredPods, pod)
		}
	}

	return filteredPods, nil
}

func (i *Informers) isDaemonSetPod(pod *v1.Pod) bool {
	if isDaemonSetOwned(pod.OwnerReferences) {
		slog.Info("Ignoring DaemonSet pod in namespace on node during eviction check",
			"pod", pod.Name,
			"namespace", pod.Namespace,
			"node", pod.Spec.NodeName)

		return true
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

// EvictAllPodsInImmediateMode requests eviction of eligible pods within the supplied filters.
// A nil error means the requests succeeded, not that the pods have finished terminating.
func (i *Informers) EvictAllPodsInImmediateMode(ctx context.Context,
	namespace, nodeName string, timeout time.Duration, partialDrainEntity *protos.Entity, podFilters ...PodFilter) error {
	pods, err := i.FindEvictablePodsInNamespaceAndNode(ctx, namespace, nodeName, partialDrainEntity, podFilters...)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to find evictable pods in namespace on node",
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
		slog.ErrorContext(ctx, "Failed to evict pods in namespace on node",
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
					slog.InfoContext(ctx, "Pod already evicted from namespace on node",
						"pod", pod.Name,
						"namespace", pod.Namespace,
						"node", pod.Spec.NodeName)
				} else {
					slog.ErrorContext(ctx, "Failed to evict pod from namespace on node",
						"pod", pod.Name,
						"namespace", pod.Namespace,
						"node", pod.Spec.NodeName,
						"error", err)
					mu.Lock()

					result = multierror.Append(result, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err))

					mu.Unlock()
				}
			} else {
				slog.InfoContext(ctx, "Pod eviction initiated for namespace on node",
					"pod", pod.Name,
					"namespace", pod.Namespace,
					"node", pod.Spec.NodeName)
			}
		}(ctx, pod, timeout)
	}

	wg.Wait()

	return result.ErrorOrNil()
}

// sendEvictionRequestForPod sends an eviction with the observed pod's deletion preconditions.
// Missing pods are treated as already evicted; API conflicts are returned for a fresh observation.
func (i *Informers) sendEvictionRequestForPod(ctx context.Context, namespace string,
	timeout time.Duration, pod *v1.Pod) error {
	eviction := &policyv1.Eviction{
		Name:      pod.Name,
		Namespace: namespace,
		DeleteOptions: &metav1.DeleteOptions{
			GracePeriodSeconds: new(int64(timeout.Seconds())),
			DryRun:             i.dryRunMode,
			Preconditions:      podDeletionPreconditions(pod),
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

// UpdateNodeEvent records a normal event for the named node.
func (i *Informers) UpdateNodeEvent(_ context.Context, nodeName string, reason string, message string) error {
	node, err := i.GetNode(nodeName)
	if err != nil {
		return fmt.Errorf("error getting node from cache %s: %w", nodeName, err)
	}

	i.eventRecorder.Event(node, v1.EventTypeNormal, reason, message)

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

// DeletePodsAfterTimeout waits for selected pods until timeout minutes after event creation,
// then requests force deletion. It returns an error until a later observation finds no pods.
func (i *Informers) DeletePodsAfterTimeout(ctx context.Context, nodeName string, namespaces []string,
	timeout int, event *model.HealthEventWithStatus, partialDrainEntity *protos.Entity, podFilters ...PodFilter) error {
	drainTimeout, err := i.getNodeDrainTimeout(timeout, event)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get node drain timeout", "error", err)
		return fmt.Errorf("failed to get node drain timeout: %w", err)
	}

	timeoutDeadline := event.CreatedAt.Add(time.Duration(timeout) * time.Minute)
	deleteDateTimeUTC := timeoutDeadline.UTC().Format(time.RFC3339)
	timeoutReached := drainTimeout <= 0

	evicted, remainingPods := i.checkIfPodsPresentInNamespaceAndNode(
		ctx, namespaces, nodeName, partialDrainEntity, podFilters...)
	if evicted {
		slog.InfoContext(ctx, "All pods on node have been deleted", "node", nodeName)
		metrics.NodeDrainTimeout.WithLabelValues(nodeName).Set(0)

		return nil
	}

	if len(remainingPods) == 0 {
		// A namespace could not be checked (already logged); requeue instead of force deleting nothing.
		return fmt.Errorf("could not check every namespace on node %s, requeuing", nodeName)
	}

	if timeoutReached {
		slog.InfoContext(ctx, "Timeout reached for node, force deleting remaining pods",
			"node", nodeName,
			"count", len(remainingPods))

		recordTimeoutNamespaces(nodeName, remainingPods)

		metrics.NodeDrainTimeout.WithLabelValues(nodeName).Set(0)

		if err := i.forceDeletePods(ctx, remainingPods); err != nil {
			slog.ErrorContext(ctx, "Failed to force delete pods on node",
				"node", nodeName,
				"error", err)

			return fmt.Errorf("failed to force delete pods on node %s: %w", nodeName, err)
		}

		// After force deleting, requeue to verify pods are gone
		return fmt.Errorf("force deleted %d pods, requeuing to verify deletion on node %s", len(remainingPods), nodeName)
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
		slog.ErrorContext(ctx, "Failed to update node event",
			"node", nodeName,
			"error", err)
	}

	slog.InfoContext(ctx, "Still waiting for pods to finish",
		"pods", podNames,
		"namespaces", namespaces,
		"node", nodeName,
		"timeout", drainTimeout)

	return fmt.Errorf("waiting for %d pods to complete or timeout (%v remaining) on node %s",
		len(remainingPods), drainTimeout, nodeName)
}

// recordTimeoutNamespaces increments the timeout metric once per namespace in the selected pods.
func recordTimeoutNamespaces(nodeName string, pods []*v1.Pod) {
	seen := make(map[string]bool)
	for _, pod := range pods {
		if !seen[pod.Namespace] {
			metrics.NodeDrainTimeoutReached.WithLabelValues(nodeName, pod.Namespace).Inc()
			seen[pod.Namespace] = true
		}
	}
}

func (i *Informers) getNodeDrainTimeout(timeout int,
	event *model.HealthEventWithStatus) (time.Duration, error) {
	elapsed := time.Since(event.CreatedAt)
	drainTimeout := time.Duration(timeout) * time.Minute

	return drainTimeout - elapsed, nil
}

// forceDeletePods requests deletion with zero grace and each observed pod's preconditions.
// It ignores missing pods and returns other API errors, including stale-observation conflicts.
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
				Preconditions:      podDeletionPreconditions(p),
			})
			if err != nil {
				if !errors.IsNotFound(err) {
					slog.ErrorContext(ctx, "Failed to force delete pod in namespace",
						"pod", p.Name,
						"namespace", p.Namespace,
						"error", err)
					mu.Lock()

					result = multierror.Append(result, fmt.Errorf("pod %s/%s: %w", p.Namespace, p.Name, err))

					mu.Unlock()
				}
			} else {
				slog.InfoContext(ctx, "Force deleted pod in namespace",
					"pod", p.Name,
					"namespace", p.Namespace)
			}
		}(pod)
	}

	wg.Wait()

	return result.ErrorOrNil()
}

// A pod can be relabelled or replaced between cache selection and deletion.
// Retry against a fresh observation instead of evicting a different policy's pod.
func podDeletionPreconditions(pod *v1.Pod) *metav1.Preconditions {
	preconditions := &metav1.Preconditions{}
	if pod.UID != "" {
		preconditions.UID = new(pod.UID)
	}

	if pod.ResourceVersion != "" {
		preconditions.ResourceVersion = new(pod.ResourceVersion)
	}

	return preconditions
}

func (i *Informers) GetNamespacesMatchingPattern(ctx context.Context,
	includePattern string, excludePattern string, nodeName string) ([]string, error) {
	objs, err := i.podInformer.GetIndexer().ByIndex(NodeIndex, nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get pods on node %s: %w", nodeName, err)
	}

	excludeRegex, err := i.compileExcludePattern(excludePattern)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to compile exclude pattern", "error", err)
		return nil, fmt.Errorf("failed to compile exclude pattern: %w", err)
	}

	namespaceSet, err := i.extractNamespacesFromPods(objs, includePattern, excludeRegex)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to extract namespaces from pods", "error", err)
		return nil, fmt.Errorf("failed to extract namespaces from pods: %w", err)
	}

	return i.convertSetToSlice(namespaceSet), nil
}

func (i *Informers) compileExcludePattern(excludePattern string) (*regexp.Regexp, error) {
	return compileExcludePattern(excludePattern)
}

func compileExcludePattern(excludePattern string) (*regexp.Regexp, error) {
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

// checkIfPodsPresentInNamespaceAndNode returns whether all selected pods are gone and
// any remaining pods. A cache lookup error prevents reporting that all pods are gone.
func (i *Informers) checkIfPodsPresentInNamespaceAndNode(ctx context.Context, namespaces []string, nodeName string,
	partialDrainEntity *protos.Entity, podFilters ...PodFilter) (bool, []*v1.Pod) {
	allEvicted := true

	var remainingPods []*v1.Pod

	for _, namespace := range namespaces {
		pods, err := i.FindEvictablePodsInNamespaceAndNode(ctx, namespace, nodeName, partialDrainEntity, podFilters...)
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

// CheckIfAllPodsAreEvictedInImmediateMode reports whether the selected drain scope is empty.
// If a selected pod exceeds its termination deadline, it requests force deletion of the remaining scope.
func (i *Informers) CheckIfAllPodsAreEvictedInImmediateMode(ctx context.Context,
	namespaces []string, nodeName string, timeout time.Duration, partialDrainEntity *protos.Entity,
	podFilters ...PodFilter) bool {
	allEvicted, remainingPods := i.checkIfPodsPresentInNamespaceAndNode(
		ctx, namespaces, nodeName, partialDrainEntity, podFilters...)

	if allEvicted {
		slog.InfoContext(ctx, "All pods evicted in namespace from node",
			"namespaces", namespaces,
			"node", nodeName)

		return true
	}

	if len(remainingPods) == 0 {
		return false // A cache lookup failed; an empty result does not establish completion.
	}

	return i.CheckIfObservedPodsAreEvictedInImmediateMode(ctx, namespaces, nodeName, timeout,
		partialDrainEntity, remainingPods, podFilters...)
}

// CheckIfObservedPodsAreEvictedInImmediateMode checks a previously listed immediate-mode scope,
// enforcing termination deadlines and refreshing the cache after any force deletion.
func (i *Informers) CheckIfObservedPodsAreEvictedInImmediateMode(ctx context.Context,
	namespaces []string, nodeName string, timeout time.Duration, partialDrainEntity *protos.Entity,
	remainingPods []*v1.Pod, podFilters ...PodFilter) bool {
	if len(remainingPods) == 0 {
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
		slog.InfoContext(ctx, "Pods on node exceeded timeout, attempting force deletion",
			"node", nodeName)

		// Delete using the observed UID and resource version. A relabelled or replaced
		// pod causes an API conflict, so this snapshot cannot delete its current state.
		err := i.forceDeletePods(ctx, remainingPods)
		if err != nil {
			metrics.ProcessingErrors.WithLabelValues("pods_force_deletion_error", nodeName).Inc()
			slog.ErrorContext(ctx, "Failed to force delete pods on node",
				"node", nodeName,
				"error", err)

			return false
		}

		allEvicted, _ := i.checkIfPodsPresentInNamespaceAndNode(ctx, namespaces, nodeName, partialDrainEntity,
			podFilters...)
		if allEvicted {
			slog.InfoContext(ctx, "All pods evicted after force deletion on node",
				"node", nodeName)
		}

		return allEvicted
	}

	remainingPodNames := make([]string, 0, len(remainingPods))
	for _, pod := range remainingPods {
		remainingPodNames = append(remainingPodNames, fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
	}

	slog.InfoContext(ctx, "Pods still present on node, will retry",
		"node", nodeName,
		"pods", remainingPodNames)

	return false
}
