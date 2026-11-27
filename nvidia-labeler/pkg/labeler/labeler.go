// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package labeler writes the NVIDIA node labels: dcgm.version, driver.installed and
// the dcgm-bootstrap-completed annotation. It reads them from the DCGM and driver pods
// on each node. The labeler module writes the vendor-neutral labels (ADR-077).
package labeler

import (
	"context"
	"fmt"
	"hash/maphash"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	"github.com/nvidia/nvsentinel/nvidia-labeler/pkg/metrics"
)

const (
	DCGMVersionLabel                 = "nvsentinel.dgxc.nvidia.com/dcgm.version"
	DriverInstalledLabel             = "nvsentinel.dgxc.nvidia.com/driver.installed"
	DCGMBootstrapCompletedAnnotation = "nvsentinel.dgxc.nvidia.com/dcgm-bootstrap-completed"

	NodeDCGMIndex               = "nodeDCGM"
	NodeDriverIndex             = "nodeDriver"
	NodeGKEDriverInstallerIndex = "nodeGKEDriverInstaller"
	driverComponentLabel        = "app.kubernetes.io/component"
	driverComponentValue        = "nvidia-driver"
	gpuPresentLabel             = "nvidia.com/gpu.present"
	// DCGM major versions used for the dcgm.version label.
	dcgmVersion3 = "3.x"
	dcgmVersion4 = "4.x"

	LabelValueTrue = "true"
)

var (
	dcgm4Regex       = regexp.MustCompile(`.*dcgm:4\..*`)
	dcgm3Regex       = regexp.MustCompile(`.*dcgm:3\..*`)
	nodeLockHashSeed = maphash.MakeSeed()
)

// Labeler sets the NVIDIA labels of each node from the DCGM and driver pods on it.
type Labeler struct {
	clientset                    kubernetes.Interface
	podInformer                  cache.SharedIndexInformer
	crdDriverInformer            cache.SharedIndexInformer
	gkeInstallerInformer         cache.SharedIndexInformer
	nodeInformer                 cache.SharedIndexInformer
	nodeLister                   listersv1.NodeLister
	informersSynced              []cache.InformerSynced
	ctx                          context.Context
	assumeDriverInstalled        bool
	requireDCGMReadyForBootstrap bool
	assumeDCGMAvailable          bool
	nodeLocks                    [256]sync.Mutex
	nodePatcher                  kubeclient.NodePatcher
}

func (l *Labeler) allInformersSynced() bool {
	for _, synced := range l.informersSynced {
		if !synced() {
			return false
		}
	}

	return true
}

// NewLabeler creates a new Labeler instance
func NewLabeler(clientset kubernetes.Interface, resyncPeriod time.Duration,
	dcgmApp, driverApp, gkeInstallerApp string, assumeDriverInstalled bool,
	requireDCGMReadyForBootstrap bool, assumeDCGMAvailable bool) (*Labeler, error) {
	podInformer, err := createPodInformer(clientset, resyncPeriod, dcgmApp, driverApp)
	if err != nil {
		return nil, fmt.Errorf("create pod informer: %w", err)
	}

	crdDriverInformer, err := createCRDDriverInformer(clientset, resyncPeriod, driverApp)
	if err != nil {
		return nil, fmt.Errorf("create CRD driver pod informer: %w", err)
	}

	gkeInstallerInformer, err := createGKEInstallerInformer(clientset, resyncPeriod, gkeInstallerApp)
	if err != nil {
		return nil, fmt.Errorf("create GKE installer informer: %w", err)
	}

	nodeInformer, err := createNodeInformer(clientset, resyncPeriod)
	if err != nil {
		return nil, fmt.Errorf("create node informer: %w", err)
	}

	l := &Labeler{
		clientset:            clientset,
		podInformer:          podInformer,
		crdDriverInformer:    crdDriverInformer,
		gkeInstallerInformer: gkeInstallerInformer,
		nodeInformer:         nodeInformer,
		nodeLister:           listersv1.NewNodeLister(nodeInformer.GetIndexer()),
		informersSynced: []cache.InformerSynced{
			podInformer.HasSynced,
			crdDriverInformer.HasSynced,
			gkeInstallerInformer.HasSynced,
			nodeInformer.HasSynced,
		},
		ctx:                          context.Background(),
		assumeDriverInstalled:        assumeDriverInstalled,
		requireDCGMReadyForBootstrap: requireDCGMReadyForBootstrap,
		assumeDCGMAvailable:          assumeDCGMAvailable,
	}

	if err := l.registerPodEventHandlers(); err != nil {
		return nil, fmt.Errorf("register pod event handlers: %w", err)
	}

	if err := l.registerNodeEventHandlers(); err != nil {
		return nil, fmt.Errorf("register node event handlers: %w", err)
	}

	slog.Info("NVIDIA labeler created, watching DCGM and driver pods",
		"assumeDriverInstalled", l.assumeDriverInstalled,
		"requireDCGMReadyForBootstrap", l.requireDCGMReadyForBootstrap,
		"assumeDCGMAvailable", l.assumeDCGMAvailable)

	return l, nil
}

func createPodInformer(clientset kubernetes.Interface, resyncPeriod time.Duration,
	dcgmApp, driverApp string) (cache.SharedIndexInformer, error) {
	return createIndexedPodInformer(clientset, resyncPeriod,
		fmt.Sprintf("app in (%s,%s)", dcgmApp, driverApp),
		cache.Indexers{
			NodeDCGMIndex:   podNodeIndexerByLabel("app", dcgmApp),
			NodeDriverIndex: podNodeIndexerByLabel("app", driverApp),
		},
	)
}

func createCRDDriverInformer(clientset kubernetes.Interface, resyncPeriod time.Duration,
	driverApp string) (cache.SharedIndexInformer, error) {
	return createIndexedPodInformer(clientset, resyncPeriod,
		fmt.Sprintf("%s=%s,app!=%s", driverComponentLabel, driverComponentValue, driverApp),
		cache.Indexers{
			NodeDriverIndex: podNodeIndexerByLabel(driverComponentLabel, driverComponentValue),
		},
	)
}

func createGKEInstallerInformer(clientset kubernetes.Interface, resyncPeriod time.Duration,
	gkeInstallerApp string) (cache.SharedIndexInformer, error) {
	return createIndexedPodInformer(clientset, resyncPeriod,
		fmt.Sprintf("k8s-app=%s", gkeInstallerApp),
		cache.Indexers{
			NodeGKEDriverInstallerIndex: podNodeIndexerByLabel("k8s-app", gkeInstallerApp),
		},
	)
}

func createIndexedPodInformer(clientset kubernetes.Interface, resyncPeriod time.Duration,
	selectorStr string, indexers cache.Indexers) (cache.SharedIndexInformer, error) {
	selector, err := labels.Parse(selectorStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse label selector %q: %w", selectorStr, err)
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		resyncPeriod,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = selector.String()
		}),
	)

	informer := factory.Core().V1().Pods().Informer()

	if err := informer.SetTransform(transformPodForCache); err != nil {
		return nil, fmt.Errorf("failed to set pod transform: %w", err)
	}

	if err := informer.GetIndexer().AddIndexers(indexers); err != nil {
		return nil, fmt.Errorf("failed to add indexers: %w", err)
	}

	return informer, nil
}

// transformPodForCache retains only fields used for pod indexing, readiness,
// delete-event identity, and DCGM image version detection.
func transformPodForCache(obj any) (any, error) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return nil, fmt.Errorf("pod transform: expected Pod object, got %T", obj)
	}

	containers := make([]v1.Container, len(pod.Spec.Containers))
	for i, container := range pod.Spec.Containers {
		containers[i] = v1.Container{Image: container.Image}
	}

	conditions := make([]v1.PodCondition, len(pod.Status.Conditions))
	for i, condition := range pod.Status.Conditions {
		conditions[i] = v1.PodCondition{
			Type:   condition.Type,
			Status: condition.Status,
		}
	}

	pod.TypeMeta = metav1.TypeMeta{}
	pod.ObjectMeta = metav1.ObjectMeta{
		Name:            pod.Name,
		Namespace:       pod.Namespace,
		UID:             pod.UID,
		ResourceVersion: pod.ResourceVersion,
		Labels:          pod.Labels,
	}
	pod.Spec = v1.PodSpec{
		NodeName:   pod.Spec.NodeName,
		Containers: containers,
	}
	pod.Status = v1.PodStatus{Conditions: conditions}

	return pod, nil
}

// transformNodeForCache keeps the node's identity, its labels and the DCGM
// bootstrap annotation. All other Node fields are discarded before the object
// enters the informer cache.
func transformNodeForCache(obj any) (any, error) {
	node, ok := obj.(*v1.Node)
	if !ok {
		return nil, fmt.Errorf("node transform: expected Node object, got %T", obj)
	}

	objectMeta := node.ObjectMeta

	var annotations map[string]string
	if value, exists := objectMeta.Annotations[DCGMBootstrapCompletedAnnotation]; exists {
		annotations = map[string]string{DCGMBootstrapCompletedAnnotation: value}
	}

	node.TypeMeta = metav1.TypeMeta{}
	node.ObjectMeta = metav1.ObjectMeta{
		Name:            objectMeta.Name,
		UID:             objectMeta.UID,
		ResourceVersion: objectMeta.ResourceVersion,
		Labels:          objectMeta.Labels,
		Annotations:     annotations,
	}
	node.Spec = v1.NodeSpec{}
	node.Status = v1.NodeStatus{}

	return node, nil
}

func podNodeIndexerByLabel(labelKey, labelValues string) cache.IndexFunc {
	values := strings.Fields(strings.ReplaceAll(labelValues, ",", " "))

	return func(obj any) ([]string, error) {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			return nil, fmt.Errorf("object is not a pod")
		}

		if val, exists := pod.Labels[labelKey]; exists && slices.Contains(values, val) {
			if pod.Spec.NodeName == "" {
				return []string{}, nil
			}

			return []string{pod.Spec.NodeName}, nil
		}

		return []string{}, nil
	}
}

func createNodeInformer(clientset kubernetes.Interface, resyncPeriod time.Duration) (cache.SharedIndexInformer, error) {
	factory := informers.NewSharedInformerFactory(clientset, resyncPeriod)
	informer := factory.Core().V1().Nodes().Informer()

	if err := informer.SetTransform(transformNodeForCache); err != nil {
		return nil, fmt.Errorf("failed to set node transform: %w", err)
	}

	return informer, nil
}

func (l *Labeler) getEventHandlers() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if err := l.handlePodEvent(obj); err != nil {
				metrics.EventsProcessed.WithLabelValues(metrics.StatusFailed).Inc()
				slog.Error("Failed to handle pod add event", "error", err)
			} else {
				metrics.EventsProcessed.WithLabelValues(metrics.StatusSuccess).Inc()
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPod, oldOk := oldObj.(*v1.Pod)

			newPod, newOk := newObj.(*v1.Pod)
			if !oldOk || !newOk {
				slog.Error("Failed to cast objects to pods in UpdateFunc")
				return
			}

			// The labels depend only on readiness and, for DCGM pods, the image.
			if isPodReady(oldPod) == isPodReady(newPod) && slices.Equal(podImages(oldPod), podImages(newPod)) {
				slog.Debug("Pod readiness and images unchanged", "pod", newPod.Name)
				return
			}

			if err := l.handlePodEvent(newPod); err != nil {
				metrics.EventsProcessed.WithLabelValues(metrics.StatusFailed).Inc()
				slog.Error("Failed to handle pod update event", "error", err)
			} else {
				metrics.EventsProcessed.WithLabelValues(metrics.StatusSuccess).Inc()
			}
		},
		DeleteFunc: func(obj any) {
			if err := l.handlePodDeleteEvent(obj); err != nil {
				metrics.EventsProcessed.WithLabelValues(metrics.StatusFailed).Inc()
				slog.Error("Failed to handle pod delete event", "error", err)
			} else {
				metrics.EventsProcessed.WithLabelValues(metrics.StatusSuccess).Inc()
			}
		},
	}
}

// podFromEventObject returns the pod of an event. A delete that the watch missed
// arrives as a tombstone that holds the last known pod.
func podFromEventObject(obj any) (*v1.Pod, bool) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}

	pod, ok := obj.(*v1.Pod)

	return pod, ok
}

// isScheduledPod reports whether an event is about a pod that has a node.
func isScheduledPod(obj any) bool {
	pod, ok := podFromEventObject(obj)
	return ok && pod.Spec.NodeName != ""
}

func podImages(pod *v1.Pod) []string {
	images := make([]string, len(pod.Spec.Containers))
	for i, container := range pod.Spec.Containers {
		images[i] = container.Image
	}

	return images
}

// registerPodEventHandlers sets up event handlers for the pod informers.
func (l *Labeler) registerPodEventHandlers() error {
	scheduledPods := cache.FilteringResourceEventHandler{
		FilterFunc: isScheduledPod,
		Handler:    l.getEventHandlers(),
	}

	for name, informer := range map[string]cache.SharedIndexInformer{
		"pod":           l.podInformer,
		"CRD driver":    l.crdDriverInformer,
		"GKE installer": l.gkeInstallerInformer,
	} {
		if _, err := informer.AddEventHandler(scheduledPods); err != nil {
			return fmt.Errorf("failed to add %s event handler: %w", name, err)
		}
	}

	return nil
}

// registerNodeEventHandlers sets up event handlers for the node informer.
func (l *Labeler) registerNodeEventHandlers() error {
	_, err := l.nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if err := l.handleNodeEvent(obj); err != nil {
				slog.Error("Failed to handle node add event", "error", err)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			if !nodeRequiresReconciliation(oldObj, newObj) {
				return
			}

			if err := l.handleNodeEvent(newObj); err != nil {
				slog.Error("Failed to handle node update event", "error", err)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add node event handler: %w", err)
	}

	return nil
}

// Run starts the labeler and waits for cache sync
func (l *Labeler) Run(ctx context.Context) error {
	l.ctx = ctx

	go l.podInformer.Run(ctx.Done())
	go l.crdDriverInformer.Run(ctx.Done())
	go l.gkeInstallerInformer.Run(ctx.Done())
	go l.nodeInformer.Run(ctx.Done())

	slog.Info("Waiting for NVIDIA labeler caches to sync...")

	if synced := cache.WaitForCacheSync(ctx.Done(), l.informersSynced...); !synced {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	slog.Info("NVIDIA labeler caches synced")

	// Events that arrived before the sync were skipped, so reconcile every node now.
	l.reconcileAllNodes()

	<-ctx.Done()
	slog.Info("NVIDIA labeler stopped")

	return nil
}

func (l *Labeler) reconcileAllNodes() {
	nodeNames := l.nodeInformer.GetStore().ListKeys()

	for _, nodeName := range nodeNames {
		if err := l.reconcileNodeLabels(nodeName, nil); err != nil {
			slog.Error("Failed to reconcile node labels", "node", nodeName, "error", err)
		}
	}

	slog.Info("Completed initial node label reconciliation", "nodeCount", len(nodeNames))
}

// resolveDCGMVersionLabelValue returns the DCGM version label value for a node.
// excludePod, when non-nil, is skipped during pod-based detection (used for
// delete events where the deleted pod may still be in the informer cache).
func (l *Labeler) resolveDCGMVersionLabelValue(node *v1.Node, excludePod *v1.Pod) (string, error) {
	objs, err := l.podInformer.GetIndexer().ByIndex(NodeDCGMIndex, node.Name)
	if err != nil {
		return "", fmt.Errorf("failed to get DCGM pods by node index for node %s: %w", node.Name, err)
	}

	requireDCGMReady := l.requireDCGMReadyForBootstrap
	if node.Annotations != nil && len(node.Annotations[DCGMBootstrapCompletedAnnotation]) > 0 {
		requireDCGMReady = false
	}

	// The index returns pods in no fixed order. During a rolling update a node can
	// have a 3.x and a 4.x pod at once, so prefer 4.x: the result must not depend on
	// the order, or each write of the label would flip it again.
	version := ""

	for _, obj := range objs {
		isTarget, pod := isTargetPod(obj, requireDCGMReady, excludePod)
		if !isTarget {
			continue
		}

		for _, container := range pod.Spec.Containers {
			if dcgm4Regex.MatchString(container.Image) {
				return dcgmVersion4, nil
			} else if dcgm3Regex.MatchString(container.Image) {
				version = dcgmVersion3
			}
		}
	}

	return version, nil
}

// normalizeDCGMVersion validates the dcgm.version label. Anything else is rejected.
func normalizeDCGMVersion(version string) string {
	switch strings.TrimSpace(version) {
	case dcgmVersion3:
		return dcgmVersion3
	case dcgmVersion4:
		return dcgmVersion4
	default:
		return ""
	}
}

// hasReadyDriverPod checks if any pod in the list is ready, optionally excluding a specific pod
func hasReadyDriverPod(objs []any, excludePod *v1.Pod) bool {
	for _, obj := range objs {
		isTarget, _ := isTargetPod(obj, true, excludePod)
		if isTarget {
			return true
		}
	}

	return false
}

// nodeRequiresReconciliation returns true when a node update changed an input that
// the NVIDIA labeler reads from nodes, or one of its own labels:
//   - the managed opt-out label;
//   - the gpu-present label that assumeDriverInstalled reads;
//   - dcgm.version or driver.installed, so that a value that someone else set or
//     removed is corrected, for example a label set on an opted-out node.
//
// After its own patch, the reconcile finds nothing to change and makes no API call.
func nodeRequiresReconciliation(oldObj, newObj any) bool {
	oldNode, ok1 := oldObj.(*v1.Node)
	newNode, ok2 := newObj.(*v1.Node)

	if !ok1 || !ok2 {
		return true
	}

	for _, key := range []string{managed.ManagedLabelKey, gpuPresentLabel, DCGMVersionLabel, DriverInstalledLabel} {
		// Compare presence too: a label can have an empty value.
		oldValue, oldExists := oldNode.Labels[key]
		newValue, newExists := newNode.Labels[key]

		if oldExists != newExists || oldValue != newValue {
			return true
		}
	}

	return false
}

func isTargetPod(podObj any, requireReady bool, excludePod *v1.Pod) (bool, *v1.Pod) {
	pod, ok := podObj.(*v1.Pod)
	if !ok {
		return false, nil
	}

	// Skip the pod we're excluding (the one being deleted)
	if excludePod != nil && pod.UID == excludePod.UID {
		return false, nil
	}

	if requireReady && !isPodReady(pod) {
		return false, nil
	}

	return true, pod
}

// isPodReady reports whether the pod's Ready condition is True. It matches
// IsPodReady from k8s.io/kubernetes/pkg/api/v1/pod. That package is not
// imported, because it replaces the client-go feature gates when it loads.
func isPodReady(pod *v1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return condition.Status == v1.ConditionTrue
		}
	}

	return false
}

// resolveDriverInstalledLabelValue returns the driver-installed label value for
// a node, excluding a specific pod from consideration for delete events.
func (l *Labeler) resolveDriverInstalledLabelValue(node *v1.Node, excludePod *v1.Pod) (string, error) {
	if l.assumeDriverInstalled && node.Labels[gpuPresentLabel] == LabelValueTrue {
		return LabelValueTrue, nil
	}

	objs, err := l.podInformer.GetIndexer().ByIndex(NodeDriverIndex, node.Name)
	if err != nil {
		return "", fmt.Errorf("failed to get driver pods by node index for node %s: %w", node.Name, err)
	}

	if hasReadyDriverPod(objs, excludePod) {
		return LabelValueTrue, nil
	}

	objs, err = l.crdDriverInformer.GetIndexer().ByIndex(NodeDriverIndex, node.Name)
	if err != nil {
		return "", fmt.Errorf("failed to get CRD driver pods by node index for node %s: %w", node.Name, err)
	}

	if hasReadyDriverPod(objs, excludePod) {
		return LabelValueTrue, nil
	}

	// fallback mechanism for GKE pre-installed driver where nvidia-driver-daemonset is not present
	objs, err = l.gkeInstallerInformer.GetIndexer().ByIndex(NodeGKEDriverInstallerIndex, node.Name)
	if err != nil {
		return "", fmt.Errorf("failed to get GKE driver installer pods by node index for node %s: %w", node.Name, err)
	}

	if hasReadyDriverPod(objs, excludePod) {
		return LabelValueTrue, nil
	}

	return "", nil
}

func (l *Labeler) handleNodeEvent(obj any) error {
	node, ok := obj.(*v1.Node)
	if !ok {
		return fmt.Errorf("node event: expected Node object, got %T", obj)
	}

	return l.reconcileNodeLabels(node.Name, nil)
}

// reconcileNodeLabels writes the NVIDIA labels of a node. excludePod, when non-nil,
// is a deleted pod that the pod cache may still hold. Pod and node events take the
// same per-node lock, so the startup sweep cannot overwrite a newer result.
//
// Until every informer has synced, a pod missing from the cache would look like a
// removed driver or DCGM pod, so events are skipped. Run reconciles every node
// after the sync.
func (l *Labeler) reconcileNodeLabels(nodeName string, excludePod *v1.Pod) error {
	if !l.allInformersSynced() {
		return nil
	}

	return l.withNodeLock(nodeName, func() error {
		_, err := l.nodePatcher.Patch(
			l.ctx,
			l.clientset.CoreV1().Nodes(),
			nodeName,
			l.cachedNode(nodeName),
			func(desired *v1.Node) error {
				if desired.Labels == nil {
					desired.Labels = make(map[string]string)
				}

				_, err := l.calculateAndSetNodeLabels(desired, excludePod)

				return err
			},
		)
		if err != nil {
			metrics.NodeUpdateFailures.Inc()

			return fmt.Errorf("failed to update labels for node %s: %w", nodeName, err)
		}

		return nil
	})
}

func (l *Labeler) withNodeLock(nodeName string, fn func() error) error {
	hash := maphash.String(nodeLockHashSeed, nodeName)
	mutex := &l.nodeLocks[hash%uint64(len(l.nodeLocks))]

	mutex.Lock()
	defer mutex.Unlock()

	return fn()
}

// cachedNode returns the cached node, or nil so that the patcher reads it from the
// API server.
func (l *Labeler) cachedNode(nodeName string) *v1.Node {
	node, err := l.getNodeFromCache(nodeName)
	if err != nil {
		return nil
	}

	return node
}

// stripDetectionLabels removes the DCGM and driver labels that DaemonSet monitors
// select on. Called when a node carries managed=false so monitors evict without
// any per-monitor chart change.
func stripDetectionLabels(node *v1.Node) bool {
	needsUpdate := false

	for _, key := range []string{DCGMVersionLabel, DriverInstalledLabel} {
		if _, exists := node.Labels[key]; exists {
			delete(node.Labels, key)

			needsUpdate = true

			slog.Info("Removing detection label from opted-out node", "node", node.Name, "label", key)
		}
	}

	return needsUpdate
}

func (l *Labeler) calculateAndSetNodeLabels(node *v1.Node, excludePod *v1.Pod) (bool, error) {
	// When the node is opted out of NVSentinel management, strip detection labels
	// so DaemonSet monitors evict via their existing nodeSelectors (ADR-040).
	optedOut, err := managed.IsNodeOptedOut(l.ctx, l.nodeLister, node.Name)
	if err != nil {
		// Fail closed: if we cannot determine opt-out state we must not proceed
		// with label mutations — stamping detection labels on a node under active
		// ERR would restore health-monitor scheduling on a released node.
		slog.Warn("Failed to check managed label, skipping label reconciliation", "node", node.Name, "error", err)
		return false, nil
	}

	if optedOut {
		// Count every reconciliation attempt on an opted-out node, not only those
		// that actually removed labels, so the metric reflects how often the gate
		// fired (ADR-040).
		metrics.NodeLabelsSkippedManaged.Inc()

		return stripDetectionLabels(node), nil
	}

	driverInstalledValue, err := l.resolveDriverInstalledLabelValue(node, excludePod)
	if err != nil {
		return false, fmt.Errorf("resolve driver-installed label value: %w", err)
	}

	dcgmVersionValue, err := l.resolveDCGMVersionLabelValue(node, excludePod)
	if err != nil {
		return false, fmt.Errorf("resolve DCGM version label value: %w", err)
	}

	return l.setDriverAndDCGMLabelValues(node, driverInstalledValue, dcgmVersionValue), nil
}

func (l *Labeler) setDriverAndDCGMLabelValues(
	node *v1.Node,
	driverInstalledValue string,
	dcgmVersionValue string,
) bool {
	needsUpdate := setDriverInstalledLabelValue(node, driverInstalledValue)

	if l.setDCGMVersionLabelValue(node, dcgmVersionValue) {
		needsUpdate = true
	}

	if len(node.Labels[DCGMVersionLabel]) > 0 {
		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		if len(node.Annotations[DCGMBootstrapCompletedAnnotation]) == 0 {
			needsUpdate = true
			node.Annotations[DCGMBootstrapCompletedAnnotation] = "true"
			slog.Info("Setting DCGM bootstrap annotation on node", "node", node.Name,
				DCGMBootstrapCompletedAnnotation, "true")
		}
	}

	return needsUpdate
}

// labelUpToDate reports whether the node already has the desired value. An empty
// value means that the label must be absent, so an empty label is not up to date.
func labelUpToDate(node *v1.Node, key, value string) bool {
	current, exists := node.Labels[key]
	if value == "" {
		return !exists
	}

	return current == value
}

func setDriverInstalledLabelValue(node *v1.Node, value string) bool {
	if labelUpToDate(node, DriverInstalledLabel, value) {
		return false
	}

	if value == "" {
		delete(node.Labels, DriverInstalledLabel)
		slog.Info("Removing stale driver installed label from node", "node", node.Name)
	} else {
		node.Labels[DriverInstalledLabel] = value
		slog.Info("Setting driver installed label on node", "node", node.Name)
	}

	return true
}

func (l *Labeler) setDCGMVersionLabelValue(node *v1.Node, value string) bool {
	if labelUpToDate(node, DCGMVersionLabel, value) {
		return false
	}

	if value == "" && l.assumeDCGMAvailable && normalizeDCGMVersion(node.Labels[DCGMVersionLabel]) != "" {
		slog.Debug("Preserving existing DCGM version label", "node", node.Name, "version", node.Labels[DCGMVersionLabel])
		return false
	}

	if value == "" {
		delete(node.Labels, DCGMVersionLabel)
		slog.Info("Removing stale DCGM version label from node", "node", node.Name)
	} else {
		node.Labels[DCGMVersionLabel] = value
		slog.Info("Setting DCGM version label on node", "node", node.Name, "version", value)
	}

	return true
}

// handlePodDeleteEvent processes pod delete events by recalculating node labels
// after excluding the deleted pod from consideration
func (l *Labeler) handlePodDeleteEvent(obj any) error {
	return l.handlePodLabelEvent(obj, true)
}

// handlePodEvent processes all pod events (add, update) idempotently
func (l *Labeler) handlePodEvent(obj any) error {
	return l.handlePodLabelEvent(obj, false)
}

func (l *Labeler) handlePodLabelEvent(obj any, deleting bool) error {
	startTime := time.Now()

	defer func() {
		metrics.EventHandlingDuration.Observe(time.Since(startTime).Seconds())
	}()

	pod, ok := podFromEventObject(obj)
	if !ok {
		return fmt.Errorf("pod event: expected Pod object, got %T", obj)
	}

	var excludePod *v1.Pod
	if deleting {
		excludePod = pod
	}

	return l.reconcileNodeLabels(pod.Spec.NodeName, excludePod)
}

func (l *Labeler) getNodeFromCache(nodeName string) (*v1.Node, error) {
	obj, exists, err := l.nodeInformer.GetStore().GetByKey(nodeName)
	if err != nil {
		return nil, fmt.Errorf("failed to get node from informer %s: %w", nodeName, err)
	}

	if !exists {
		return nil, fmt.Errorf("node %s does not exist in informer cache", nodeName)
	}

	node, ok := obj.(*v1.Node)
	if !ok {
		return nil, fmt.Errorf("unable to cast object to node %s", nodeName)
	}

	return node, nil
}
