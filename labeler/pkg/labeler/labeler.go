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

package labeler

import (
	"context"
	"fmt"
	"hash/maphash"
	"log/slog"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	resourceinformers "k8s.io/client-go/informers/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	listersv1 "k8s.io/client-go/listers/core/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	"github.com/nvidia/nvsentinel/commons/pkg/stringutil"
	"github.com/nvidia/nvsentinel/labeler/pkg/devicecounts"
	"github.com/nvidia/nvsentinel/labeler/pkg/metrics"
)

// The NVIDIA labels (dcgm.version, driver.installed) belong to the nvidia-labeler
// module (ADR-077). This labeler never writes or removes them.
const (
	KataEnabledLabel        = "nvsentinel.dgxc.nvidia.com/kata.enabled"
	KataRuntimeDefaultLabel = "katacontainers.io/kata-runtime"

	// Label values
	LabelValueTrue  = "true"
	LabelValueFalse = "false"
)

var nodeLockHashSeed = maphash.MakeSeed()

// Labeler writes the Kata and device-count labels of each node.
type Labeler struct {
	clientset             kubernetes.Interface
	nodeInformer          cache.SharedIndexInformer
	nodeLister            listersv1.NodeLister
	resourceSliceInformer cache.SharedIndexInformer
	informersSynced       []cache.InformerSynced
	ctx                   context.Context
	kataLabels            []string
	deviceCounts          *devicecounts.Manager
	nodeLocks             [256]sync.Mutex
	nodePatcher           kubeclient.NodePatcher
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
	kataLabelOverride string, expectedDeviceCounts devicecounts.Config) (*Labeler, error) {
	deviceCounts, err := devicecounts.NewManager(expectedDeviceCounts)
	if err != nil {
		return nil, fmt.Errorf("create expected device count manager: %w", err)
	}

	nodeInformer, err := createNodeInformer(
		clientset,
		resyncPeriod,
		deviceCounts.Enabled(),
	)
	if err != nil {
		return nil, fmt.Errorf("create node informer: %w", err)
	}

	informersSynced := []cache.InformerSynced{nodeInformer.HasSynced}

	var resourceSliceInformer cache.SharedIndexInformer
	if deviceCounts.RequiresResourceSlices() {
		resourceSliceInformer = createResourceSliceInformer(clientset, resyncPeriod)
		informersSynced = append(informersSynced, resourceSliceInformer.HasSynced)
	}

	l := &Labeler{
		clientset:             clientset,
		nodeInformer:          nodeInformer,
		nodeLister:            listersv1.NewNodeLister(nodeInformer.GetIndexer()),
		resourceSliceInformer: resourceSliceInformer,
		informersSynced:       informersSynced,
		ctx:                   context.Background(),
		kataLabels:            buildKataLabels(kataLabelOverride),
		deviceCounts:          deviceCounts,
	}

	if err := l.registerEventHandlers(); err != nil {
		return nil, err
	}

	l.logConfiguration()

	return l, nil
}

// registerEventHandlers wires up the informer event handlers.
func (l *Labeler) registerEventHandlers() error {
	if err := l.registerNodeEventHandlers(); err != nil {
		return fmt.Errorf("register node event handlers: %w", err)
	}

	if err := l.registerResourceSliceEventHandlers(); err != nil {
		return fmt.Errorf("register ResourceSlice event handlers: %w", err)
	}

	return nil
}

// logConfiguration emits the labeler's effective configuration at startup.
func (l *Labeler) logConfiguration() {
	if l.deviceCounts.Enabled() {
		slog.Info("Labeler configured to reconcile expected device count labels", "classes", l.deviceCounts.ClassCount())
	}

	slog.Info("Labeler created, watching nodes for kata detection")
}

func buildKataLabels(kataLabelOverride string) []string {
	kataLabels := []string{KataRuntimeDefaultLabel}
	if kataLabelOverride != "" && kataLabelOverride != KataRuntimeDefaultLabel {
		kataLabels = append(kataLabels, kataLabelOverride)
	}

	return kataLabels
}

// transformNodeForCache returns a fixed Node projection for the Labeler.
//
// Identity and labels are always retained.
// When expected device counts are enabled, capacity and allocatable resources
// are also retained for current-count CEL expressions. All other Node fields
// are discarded before the object enters the informer cache.
func transformNodeForCache(deviceCountsEnabled bool) cache.TransformFunc {
	return func(obj any) (any, error) {
		node, ok := obj.(*v1.Node)
		if !ok {
			return nil, fmt.Errorf("node transform: expected Node object, got %T", obj)
		}

		objectMeta := node.ObjectMeta
		status := node.Status

		node.TypeMeta = metav1.TypeMeta{}
		node.ObjectMeta = metav1.ObjectMeta{
			Name:            objectMeta.Name,
			UID:             objectMeta.UID,
			ResourceVersion: objectMeta.ResourceVersion,
			Labels:          objectMeta.Labels,
		}
		node.Spec = v1.NodeSpec{}
		node.Status = v1.NodeStatus{}

		if deviceCountsEnabled {
			node.Status.Allocatable = status.Allocatable
			node.Status.Capacity = status.Capacity
		}

		return node, nil
	}
}

// createNodeInformer applies the Labeler's fixed field projection before Nodes
// enter the shared informer cache.
func createNodeInformer(
	clientset kubernetes.Interface,
	resyncPeriod time.Duration,
	deviceCountsEnabled bool,
) (cache.SharedIndexInformer, error) {
	factory := informers.NewSharedInformerFactory(clientset, resyncPeriod)
	informer := factory.Core().V1().Nodes().Informer()

	if err := informer.SetTransform(transformNodeForCache(deviceCountsEnabled)); err != nil {
		return nil, fmt.Errorf("failed to set node transform: %w", err)
	}

	return informer, nil
}

func createResourceSliceInformer(clientset kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	// Device-count reconciliation queries slices by node for every target and
	// peer. Indexing spec.nodeName avoids a full ResourceSlice store scan.
	return resourceinformers.NewResourceSliceInformer(clientset, resyncPeriod, cache.Indexers{
		devicecounts.ResourceSliceNodeNameIndex: devicecounts.ResourceSliceNodeNameIndexFunc,
	})
}

// registerNodeEventHandlers sets up event handlers for node informer
func (l *Labeler) registerNodeEventHandlers() error {
	_, err := l.nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if err := l.handleNodeEvent(obj); err != nil {
				slog.Error("Failed to handle node add event", "error", err)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			if !l.nodeRequiresReconciliation(oldObj, newObj) {
				return
			}

			if err := l.handleNodeEvent(newObj); err != nil {
				slog.Error("Failed to handle node update event", "error", err)
			}
		},
		DeleteFunc: func(obj any) {
			// A deleted node's count must stop raising its partition's expected count.
			nodeName, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err != nil {
				slog.Warn("Skipping node delete event with unexpected object", "error", err)
				return
			}

			l.deviceCounts.ForgetNode(nodeName)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to add node event handler: %w", err)
	}

	return nil
}

func (l *Labeler) registerResourceSliceEventHandlers() error {
	if l.resourceSliceInformer == nil {
		return nil
	}

	_, err := l.resourceSliceInformer.AddEventHandler(l.newResourceSliceEventHandlers())
	if err != nil {
		return fmt.Errorf("failed to add ResourceSlice event handler: %w", err)
	}

	return nil
}

// Run starts the labeler and waits for cache sync
func (l *Labeler) Run(ctx context.Context) error {
	l.ctx = ctx

	go l.nodeInformer.Run(ctx.Done())

	if l.resourceSliceInformer != nil {
		go l.resourceSliceInformer.Run(ctx.Done())
	}

	slog.Info("Waiting for Labeler caches to sync...")

	if synced := cache.WaitForCacheSync(ctx.Done(), l.informersSynced...); !synced {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	slog.Info("Labeler caches synced")

	// Reconcile all nodes now that all informers have synced. The device counts
	// of nodes that were processed during the initial sync are set here.
	l.reconcileAllNodes()

	<-ctx.Done()
	slog.Info("Labeler stopped")

	return nil
}

func (l *Labeler) reconcileAllNodes() {
	objects := l.nodeInformer.GetStore().List()

	nodes := make([]*v1.Node, 0, len(objects))
	for _, obj := range objects {
		node, ok := obj.(*v1.Node)
		if !ok {
			continue
		}

		nodes = append(nodes, node)
	}

	// Share one cache across the fleet sweep. Peer CEL results and partition
	// maxima are therefore computed once and reused by every target node, while
	// indexed ResourceSlices are loaded at most once per node. The nodes are
	// listed again when a partition is read in full, so that read sees the
	// informer as it is at that moment rather than at the start of the sweep.
	deviceCountCache := l.deviceCounts.NewLazyReconcileCache(
		l.deviceCountCachedNodes,
		l.loadResourceSlicesForNode,
	)
	for _, node := range nodes {
		if err := l.reconcileNodeLabels(node.Name, deviceCountCache); err != nil {
			slog.Error("Failed to reconcile node labels", "node", node.Name, "error", err)
		}
	}

	slog.Info("Completed initial node label reconciliation", "nodeCount", len(objects))
}

// nodeRequiresReconciliation returns true when a node update changed an input
// that the labeler reads from nodes: the managed opt-out label, the kata
// detection labels, and the labels and resources that device-count classes read.
func (l *Labeler) nodeRequiresReconciliation(oldObj, newObj any) bool {
	oldNode, ok1 := oldObj.(*v1.Node)
	newNode, ok2 := newObj.(*v1.Node)

	if !ok1 || !ok2 {
		return true
	}

	if oldNode.Labels[managed.ManagedLabelKey] != newNode.Labels[managed.ManagedLabelKey] {
		return true
	}

	for _, key := range l.kataLabels {
		if oldNode.Labels[key] != newNode.Labels[key] {
			return true
		}
	}

	return l.deviceCounts.NodeResourcesAffectDeviceCounts(oldNode, newNode) ||
		l.deviceCounts.NodeLabelsAffectDeviceCounts(oldNode.Labels, newNode.Labels)
}

// getKataLabelForNode detects if Kata is enabled on the specified node by checking node metadata.
// Returns "true" if Kata is enabled, "false" if not.
func (l *Labeler) getKataLabelForNode(node *v1.Node) string {
	// Check if Kata is enabled using multiple detection methods
	if isKataEnabled(node, l.kataLabels) {
		return LabelValueTrue
	}

	return LabelValueFalse
}

// isKataEnabled checks if a node has Kata Containers enabled by examining node labels.
// Checks the configured kata labels (either custom override or default) for truthy values.
// Returns true if ANY of the configured labels has a truthy value (OR logic).
// Truthy values are: "true", "enabled", "1", "yes" (case-insensitive).
func isKataEnabled(node *v1.Node, kataLabels []string) bool {
	for _, label := range kataLabels {
		if value, exists := node.Labels[label]; exists && stringutil.IsTruthyValue(value) {
			slog.Debug("Kata detected",
				"source", "label",
				"node", node.Name,
				"label", label,
				"value", value,
			)

			return true
		}
	}

	return false
}

func (l *Labeler) handleNodeEvent(obj any) error {
	node, ok := obj.(*v1.Node)
	if !ok {
		return fmt.Errorf("node event: expected Node object, got %T", obj)
	}

	return l.reconcileNodeLabels(node.Name, l.newDeviceCountReconcileCache())
}

func (l *Labeler) reconcileNodeLabels(
	nodeName string,
	deviceCountCache *devicecounts.ReconcileCache,
) error {
	return l.withNodeLock(nodeName, func() error {
		_, err := l.nodePatcher.Patch(
			l.ctx,
			l.clientset.CoreV1().Nodes(),
			nodeName,
			l.cachedNodeForPatch(nodeName),
			func(desired *v1.Node) error {
				if desired.Labels == nil {
					desired.Labels = make(map[string]string)
				}

				_, err := l.calculateAndSetNodeLabels(desired, deviceCountCache)

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

func (l *Labeler) newDeviceCountReconcileCache() *devicecounts.ReconcileCache {
	// Event-driven reconciliations get a fresh cache so observations cannot
	// outlive the informer state that triggered the event. The nodes are listed
	// only if a partition has to be read in full, which most events do not need.
	return l.deviceCounts.NewLazyReconcileCache(
		l.deviceCountCachedNodes,
		l.loadResourceSlicesForNode,
	)
}

func (l *Labeler) withNodeLock(nodeName string, fn func() error) error {
	hash := maphash.String(nodeLockHashSeed, nodeName)
	mutex := &l.nodeLocks[hash%uint64(len(l.nodeLocks))]

	mutex.Lock()
	defer mutex.Unlock()

	return fn()
}

func (l *Labeler) cachedNodeForPatch(nodeName string) *v1.Node {
	if !l.allInformersSynced() {
		return nil
	}

	node, err := l.getNodeFromCache(nodeName)
	if err != nil {
		return nil
	}

	return node
}

// stripKataLabel removes the kata label, which the syslog monitor DaemonSets select
// on. Called when a node carries managed=false so monitors evict without any
// per-monitor chart change. The nvidia-labeler module strips the NVIDIA labels.
func stripKataLabel(node *v1.Node) bool {
	if _, exists := node.Labels[KataEnabledLabel]; !exists {
		return false
	}

	delete(node.Labels, KataEnabledLabel)
	slog.Info("Removing detection label from opted-out node", "node", node.Name, "label", KataEnabledLabel)

	return true
}

func (l *Labeler) calculateAndSetNodeLabels(
	node *v1.Node,
	deviceCountCache *devicecounts.ReconcileCache,
) (bool, error) {
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

		// Labeler leaves this node's labels alone, but the node still has the same
		// hardware as the rest of its partition, so its counts still matter to
		// the other nodes' expected counts.
		deviceCountCache.RememberPeerCounts(l.ctx, node)

		return stripKataLabel(node), nil
	}

	needsUpdate := false

	expectedKataLabel := l.getKataLabelForNode(node)
	if node.Labels[KataEnabledLabel] != expectedKataLabel {
		needsUpdate = true
		node.Labels[KataEnabledLabel] = expectedKataLabel
		slog.Info("Setting Kata enabled label on node", "node", node.Name, "kata", expectedKataLabel)
	}

	// Device counts are calculated only after all informers have synced. During
	// startup, a partition is not yet complete in the node cache.
	if !l.allInformersSynced() {
		return needsUpdate, nil
	}

	if deviceCountCache.CalculateAndSetDeviceCountLabels(l.ctx, node) {
		needsUpdate = true
	}

	return needsUpdate, nil
}

func (l *Labeler) deviceCountCachedNodes() []*v1.Node {
	nodes := []*v1.Node{}

	for _, obj := range l.nodeInformer.GetStore().List() {
		node, ok := obj.(*v1.Node)
		if !ok {
			continue
		}

		nodes = append(nodes, node)
	}

	return nodes
}

func (l *Labeler) loadResourceSlicesForNode(node *v1.Node) []*resourcev1.ResourceSlice {
	if l.resourceSliceInformer == nil {
		return nil
	}

	return devicecounts.ResourceSlicesForNode(l.resourceSliceInformer.GetIndexer(), node)
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
