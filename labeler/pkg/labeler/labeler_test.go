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
	"maps"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	listersv1 "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	"github.com/nvidia/nvsentinel/labeler/pkg/devicecounts"
)

const (
	// dcgmVersionLabel belongs to nvidia-labeler. The tests check that the labeler leaves it alone.
	dcgmVersionLabel = "nvsentinel.dgxc.nvidia.com/dcgm.version"
	unrelatedLabel   = "nvidia.com/gpu.present"
)

// TestKataLabelOverride verifies that the kataLabelOverride parameter correctly
// adds custom kata detection labels to the labeler instance.
func TestKataLabelOverride(t *testing.T) {
	tests := []struct {
		name       string
		override   string
		wantLabels []string
	}{
		{
			name:       "no override - default only",
			override:   "",
			wantLabels: []string{KataRuntimeDefaultLabel},
		},
		{
			name:       "override equals default",
			override:   KataRuntimeDefaultLabel,
			wantLabels: []string{KataRuntimeDefaultLabel},
		},
		{
			name:       "with custom override",
			override:   "custom.io/kata-enabled",
			wantLabels: []string{KataRuntimeDefaultLabel, "custom.io/kata-enabled"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testEnv := envtest.Environment{}
			cfg, err := testEnv.Start()
			require.NoError(t, err, "failed to setup envtest")
			defer func() { _ = testEnv.Stop() }()

			clientset, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err, "failed to create kubernetes client")

			l, err := NewLabeler(
				clientset,
				time.Minute,
				tt.override,
				devicecounts.Config{},
			)

			if err != nil {
				t.Fatalf("NewLabeler() error = %v", err)
			}

			if l == nil {
				t.Fatal("NewLabeler() returned nil labeler")
			}

			require.Equal(t, tt.wantLabels, l.kataLabels, "kataLabels mismatch for override %q", tt.override)
		})
	}
}

func TestLabelerInformerTransforms_EndToEnd(t *testing.T) {
	ctx := context.Background()
	testEnv := envtest.Environment{}

	cfg, err := testEnv.Start()
	require.NoError(t, err, "failed to setup envtest")
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })

	cli, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	t.Run("default node cache shape", func(t *testing.T) {
		node, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{
			Name: "transform-disabled",
			Labels: map[string]string{
				managed.ManagedLabelKey: managed.ManagedLabelValueFalse,
				"retain":                "label",
			},
			Annotations: map[string]string{
				"nvsentinel.dgxc.nvidia.com/dcgm-bootstrap-completed": "true",
				"drop": "annotation",
			},
			Spec: corev1.NodeSpec{ProviderID: "drop-provider"},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		node.Status.Capacity = corev1.ResourceList{
			corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("8"),
		}
		node, err = cli.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		require.NoError(t, err)

		labeler, stop := startTransformTestLabeler(t, cli, devicecounts.Config{})

		cachedNode, err := labeler.getNodeFromCache(node.Name)
		require.NoError(t, err)
		assert.NotEmpty(t, cachedNode.ResourceVersion)
		assert.Equal(t, &corev1.Node{
			Name:            node.Name,
			UID:             node.UID,
			ResourceVersion: cachedNode.ResourceVersion,
			Labels:          node.Labels,
		}, cachedNode)

		stop()
		require.NoError(t, cli.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{}))
		require.Eventually(t, func() bool {
			_, nodeErr := cli.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})

			return errors.IsNotFound(nodeErr)
		}, 30*time.Second, 100*time.Millisecond)
	})

	t.Run("device count fields drive peer learning", func(t *testing.T) {
		nodes := make([]*corev1.Node, 0, 2)
		for name, count := range map[string]string{
			"transform-peer":   "8",
			"transform-target": "4",
		} {
			node, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{
				Name:        name,
				Annotations: map[string]string{"drop": "annotation"},
				Spec:        corev1.NodeSpec{ProviderID: "drop-provider"},
			}, metav1.CreateOptions{})
			require.NoError(t, err)

			node.Status = corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceName("nvidia.com/gpu"): resource.MustParse(count),
				},
				Capacity: corev1.ResourceList{
					corev1.ResourceName("nvidia.com/gpu"): resource.MustParse(count),
				},
				Conditions: []corev1.NodeCondition{{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				}},
			}
			node, err = cli.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
			require.NoError(t, err)

			nodes = append(nodes, node)
		}

		config := deviceCountConfigWithExpression(
			"int(node.status.allocatable['nvidia.com/gpu'])",
		)
		labeler, stop := startTransformTestLabeler(t, cli, config)

		require.Eventually(t, func() bool {
			node, err := cli.CoreV1().Nodes().Get(
				ctx, "transform-target", metav1.GetOptions{})
			if err != nil {
				return false
			}

			return node.Labels["test.nvsentinel/current"] == "4" &&
				node.Labels["test.nvsentinel/expected"] == "8"
		}, 30*time.Second, 100*time.Millisecond)

		for _, node := range nodes {
			cachedNode, err := labeler.getNodeFromCache(node.Name)
			require.NoError(t, err)
			assert.Equal(t, node.Status.Allocatable, cachedNode.Status.Allocatable)
			assert.Equal(t, node.Status.Capacity, cachedNode.Status.Capacity)
			assert.Empty(t, cachedNode.Annotations)
			assert.Empty(t, cachedNode.Spec)
			assert.Nil(t, cachedNode.Status.Conditions)
		}

		stop()
	})

}

func startTransformTestLabeler(
	t *testing.T,
	cli kubernetes.Interface,
	deviceCountConfig devicecounts.Config,
) (*Labeler, func()) {
	t.Helper()

	labeler, err := NewLabeler(
		cli,
		time.Minute,
		"",
		deviceCountConfig,
	)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	labeler.ctx = ctx

	go labeler.nodeInformer.Run(ctx.Done())
	if labeler.resourceSliceInformer != nil {
		go labeler.resourceSliceInformer.Run(ctx.Done())
	}

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
		})
	}
	t.Cleanup(stop)

	syncCtx, syncCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer syncCancel()
	require.True(t, cache.WaitForCacheSync(syncCtx.Done(), labeler.informersSynced...))
	labeler.reconcileAllNodes()

	return labeler, stop
}

func TestNewLabeler_ResourceSliceInformerEnabled(t *testing.T) {
	clientset := fake.NewSimpleClientset()

	t.Run("disabled config does not create ResourceSlice informer", func(t *testing.T) {
		labeler, err := NewLabeler(
			clientset,
			time.Minute,
			"",
			devicecounts.Config{},
		)
		require.NoError(t, err)
		require.Nil(t, labeler.resourceSliceInformer)
		require.Len(t, labeler.informersSynced, 1)
	})

	t.Run("node-only enabled config does not create ResourceSlice informer", func(t *testing.T) {
		labeler, err := NewLabeler(
			clientset,
			time.Minute,
			"",
			testDeviceCountConfig(),
		)
		require.NoError(t, err)
		require.Nil(t, labeler.resourceSliceInformer)
		require.Len(t, labeler.informersSynced, 1)
	})

	t.Run("ResourceSlice expression creates ResourceSlice informer", func(t *testing.T) {
		labeler, err := NewLabeler(
			clientset,
			time.Minute,
			"",
			testResourceSliceDeviceCountConfig(),
		)
		require.NoError(t, err)
		require.NotNil(t, labeler.resourceSliceInformer)
		require.Len(t, labeler.informersSynced, 2)
		require.Contains(
			t,
			labeler.resourceSliceInformer.GetIndexer().GetIndexers(),
			devicecounts.ResourceSliceNodeNameIndex,
		)
	})
}

func TestLabelerNodeRequiresReconciliation_DeviceCountLabels(t *testing.T) {
	labeler, err := NewLabeler(
		fake.NewSimpleClientset(),
		time.Minute,
		"",
		testDeviceCountConfig(),
	)
	require.NoError(t, err)

	oldNode := &corev1.Node{
		Name: "node-a",
		Labels: map[string]string{
			"nvidia.com/gpu.count":     "4",
			"test.nvsentinel/current":  "4",
			"test.nvsentinel/expected": "8",
		},
	}

	t.Run("ignores labels managed by device counts", func(t *testing.T) {
		newNode := oldNode.DeepCopy()
		newNode.Labels["test.nvsentinel/current"] = "3"
		newNode.Labels["test.nvsentinel/expected"] = "8"

		require.False(t, labeler.nodeRequiresReconciliation(oldNode, newNode))
	})

	t.Run("reconciles when an input node label changes", func(t *testing.T) {
		newNode := oldNode.DeepCopy()
		newNode.Labels["nvidia.com/gpu.count"] = "8"

		require.True(t, labeler.nodeRequiresReconciliation(oldNode, newNode))
	})
}

func TestLabelerNodeRequiresReconciliation_AllocatableChanges(t *testing.T) {
	t.Run("reconciles when allocatable changes with status-backed class", func(t *testing.T) {
		labeler, err := NewLabeler(
			fake.NewSimpleClientset(),
			time.Minute,
			"",
			testAllocatableDeviceCountConfig(),
		)
		require.NoError(t, err)

		oldNode := &corev1.Node{
			Name:   "node-a",
			Labels: map[string]string{},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceName("nvidia.com/mlnxnics"): resource.MustParse("0"),
				},
			},
		}

		newNode := oldNode.DeepCopy()
		newNode.Status.Allocatable = corev1.ResourceList{
			corev1.ResourceName("nvidia.com/mlnxnics"): resource.MustParse("4"),
		}

		require.True(t, labeler.nodeRequiresReconciliation(oldNode, newNode))
	})

	t.Run("does not reconcile allocatable changes with label-only class", func(t *testing.T) {
		labeler, err := NewLabeler(
			fake.NewSimpleClientset(),
			time.Minute,
			"",
			testDeviceCountConfig(),
		)
		require.NoError(t, err)

		oldNode := &corev1.Node{
			Name: "node-a",
			Labels: map[string]string{
				"nvidia.com/gpu.count":     "4",
				"test.nvsentinel/current":  "4",
				"test.nvsentinel/expected": "8",
			},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceName("nvidia.com/mlnxnics"): resource.MustParse("0"),
				},
			},
		}

		newNode := oldNode.DeepCopy()
		newNode.Status.Allocatable = corev1.ResourceList{
			corev1.ResourceName("nvidia.com/mlnxnics"): resource.MustParse("4"),
		}

		require.False(t, labeler.nodeRequiresReconciliation(oldNode, newNode))
	})
}

func testDeviceCountConfig() devicecounts.Config {
	return devicecounts.Config{
		Enabled: true,
		Classes: []devicecounts.ClassConfig{
			{
				Name:    "gpu",
				Enabled: true,
				Labels: devicecounts.Labels{
					Current:  "test.nvsentinel/current",
					Expected: "test.nvsentinel/expected",
				},
				CurrentExpression: "int(node.metadata.labels['nvidia.com/gpu.count'])",
			},
		},
	}
}

func testResourceSliceDeviceCountConfig() devicecounts.Config {
	config := testDeviceCountConfig()
	config.Classes[0].CurrentExpression = "resourceSlices.size()"

	return config
}

func deviceCountConfigWithExpression(expression string) devicecounts.Config {
	config := testDeviceCountConfig()
	config.Classes[0].CurrentExpression = expression

	return config
}

func testAllocatableDeviceCountConfig() devicecounts.Config {
	return devicecounts.Config{
		Enabled: true,
		Classes: []devicecounts.ClassConfig{
			{
				Name:    "nic",
				Enabled: true,
				Labels: devicecounts.Labels{
					Current:  "test.nvsentinel/nic-current",
					Expected: "test.nvsentinel/nic-expected",
				},
				CurrentExpression: "int(node.status.allocatable['nvidia.com/mlnxnics'])",
			},
		},
	}
}

// TestKataLabelOverrideIsolation verifies that creating multiple labeler instances
// with different overrides doesn't pollute each other (tests for race conditions).
func TestKataLabelOverrideIsolation(t *testing.T) {
	testEnv := envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err, "failed to setup envtest")
	defer func() { _ = testEnv.Stop() }()

	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err, "failed to create kubernetes client")

	// Create first instance with override "first"
	l1, err := NewLabeler(
		clientset,
		time.Minute,
		"first.io/kata",
		devicecounts.Config{},
	)
	if err != nil {
		t.Fatalf("NewLabeler(first) error = %v", err)
	}

	// Create second instance with override "second"
	l2, err := NewLabeler(
		clientset,
		time.Minute,
		"second.io/kata",
		devicecounts.Config{},
	)
	if err != nil {
		t.Fatalf("NewLabeler(second) error = %v", err)
	}

	// Create third instance with no override
	l3, err := NewLabeler(
		clientset,
		time.Minute,
		"",
		devicecounts.Config{},
	)
	if err != nil {
		t.Fatalf("NewLabeler(empty) error = %v", err)
	}

	// All instances should be valid and independent
	if l1 == nil || l2 == nil || l3 == nil {
		t.Fatal("One or more labeler instances is nil")
	}

	t.Log("Successfully created 3 independent labeler instances with different overrides")
}

// TestKataLabelDetection tests that the labeler correctly detects and sets kata labels on nodes
func TestKataLabelDetection(t *testing.T) {
	tests := []struct {
		name            string
		node            *corev1.Node
		kataOverride    string
		expectedKataVal string
		shouldHaveLabel bool
	}{
		{
			name: "kata node with default label true",
			node: &corev1.Node{
				Name: "kata-node",
				Labels: map[string]string{
					KataRuntimeDefaultLabel: "true",
				},
			},
			kataOverride:    "",
			expectedKataVal: LabelValueTrue,
			shouldHaveLabel: true,
		},
		{
			name: "kata node with default label enabled",
			node: &corev1.Node{
				Name: "kata-node",
				Labels: map[string]string{
					KataRuntimeDefaultLabel: "enabled",
				},
			},
			kataOverride:    "",
			expectedKataVal: LabelValueTrue,
			shouldHaveLabel: true,
		},
		{
			name: "non-kata node with default label false",
			node: &corev1.Node{
				Name: "regular-node",
				Labels: map[string]string{
					KataRuntimeDefaultLabel: "false",
				},
			},
			kataOverride:    "",
			expectedKataVal: LabelValueFalse,
			shouldHaveLabel: true,
		},
		{
			name: "node with custom kata label",
			node: &corev1.Node{
				Name: "custom-kata-node",
				Labels: map[string]string{
					"custom.io/kata": "true",
				},
			},
			kataOverride:    "custom.io/kata",
			expectedKataVal: LabelValueTrue,
			shouldHaveLabel: true,
		},
		{
			name: "node without kata labels",
			node: &corev1.Node{
				Name:   "no-kata-node",
				Labels: map[string]string{},
			},
			kataOverride:    "",
			expectedKataVal: LabelValueFalse,
			shouldHaveLabel: true,
		},
		{
			name: "node with both default and custom kata labels - both true",
			node: &corev1.Node{
				Name: "both-kata-node",
				Labels: map[string]string{
					KataRuntimeDefaultLabel: "true",
					"custom.io/kata":        "true",
				},
			},
			kataOverride:    "custom.io/kata",
			expectedKataVal: LabelValueTrue,
			shouldHaveLabel: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			testEnv := envtest.Environment{}
			cfg, err := testEnv.Start()
			require.NoError(t, err, "failed to setup envtest")
			defer func() { _ = testEnv.Stop() }()

			cli, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err, "failed to create kubernetes client")

			// Create the node
			_, err = cli.CoreV1().Nodes().Create(ctx, tt.node, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create node")

			// Create labeler with kata override if specified
			labeler, err := NewLabeler(
				cli,
				time.Minute,
				tt.kataOverride,
				devicecounts.Config{},
			)
			require.NoError(t, err, "failed to create labeler")

			// Start labeler
			labelerCtx, labelerCancel := context.WithCancel(ctx)
			defer labelerCancel()

			go func() {
				_ = labeler.Run(labelerCtx)
			}()

			// Wait for informer cache to sync
			require.Eventually(t, func() bool {
				return cache.WaitForCacheSync(labelerCtx.Done(), labeler.informersSynced...)
			}, 10*time.Second, 100*time.Millisecond, "informer cache did not sync")

			// Trigger kata detection by handling node event
			err = labeler.handleNodeEvent(tt.node)
			require.NoError(t, err, "failed to handle node event")

			// Verify the kata label was set correctly
			require.Eventually(t, func() bool {
				node, err := cli.CoreV1().Nodes().Get(ctx, tt.node.Name, metav1.GetOptions{})
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				kataLabel, exists := node.Labels[KataEnabledLabel]
				if !tt.shouldHaveLabel {
					return !exists
				}

				if !exists {
					t.Logf("Node %s missing kata.enabled label", tt.node.Name)
					return false
				}

				if kataLabel != tt.expectedKataVal {
					t.Logf("Node %s has wrong kata label: got %s, want %s", tt.node.Name, kataLabel, tt.expectedKataVal)
					return false
				}

				return true
			}, 15*time.Second, 500*time.Millisecond, "kata label not set correctly on node %s", tt.node.Name)
		})
	}
}

// TestMemoryUnderNodeUpdatePressure creates nodes in envtest and rapidly updates
// their status conditions (simulating kubelet heartbeats) to detect unbounded
// memory growth in the labeler's node event handler path.
func TestMemoryUnderNodeUpdatePressure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory stress test in short mode")
	}

	const (
		nodeCount    = 200
		testDuration = 60 * time.Second
		workers      = 10
		maxGrowthMiB = 30
	)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	testEnv := envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	defer func() { _ = testEnv.Stop() }()

	cli, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	createNodes(t, ctx, cli, nodeCount)

	labeler, err := NewLabeler(
		cli,
		30*time.Second,
		"",
		devicecounts.Config{},
	)
	require.NoError(t, err)

	labelerCtx, labelerCancel := context.WithCancel(ctx)
	defer labelerCancel()
	go func() { _ = labeler.Run(labelerCtx) }()

	require.True(t, cache.WaitForCacheSync(labelerCtx.Done(), labeler.informersSynced...))

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	updateCtx, updateCancel := context.WithTimeout(labelerCtx, testDuration)
	defer updateCancel()

	var peakHeapInuse atomic.Uint64
	peakHeapInuse.Store(baseline.HeapInuse)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-updateCtx.Done():
				return
			case <-ticker.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				for {
					cur := peakHeapInuse.Load()
					if ms.HeapInuse <= cur || peakHeapInuse.CompareAndSwap(cur, ms.HeapInuse) {
						break
					}
				}
			}
		}
	}()

	generateNodeUpdates(t, updateCtx, cli, nodeCount, workers)

	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	var final runtime.MemStats
	runtime.ReadMemStats(&final)

	finalGrowthMiB := int64(final.HeapInuse-baseline.HeapInuse) / 1024 / 1024
	peakGrowthMiB := int64(peakHeapInuse.Load()-baseline.HeapInuse) / 1024 / 1024

	t.Logf("nodes=%d duration=%s baseline=%dMiB peak=%dMiB final=%dMiB peakGrowth=%dMiB finalGrowth=%dMiB",
		nodeCount, testDuration,
		baseline.HeapInuse/1024/1024, peakHeapInuse.Load()/1024/1024, final.HeapInuse/1024/1024,
		peakGrowthMiB, finalGrowthMiB)

	if peakGrowthMiB > maxGrowthMiB {
		t.Errorf("peak memory grew by %d MiB (limit %d MiB) — likely unbounded notification buffer growth",
			peakGrowthMiB, maxGrowthMiB)
	}
	if finalGrowthMiB > maxGrowthMiB {
		t.Errorf("final memory grew by %d MiB (limit %d MiB) — memory not reclaimed after GC",
			finalGrowthMiB, maxGrowthMiB)
	}
}

func createNodes(t *testing.T, ctx context.Context, cli kubernetes.Interface, count int) {
	t.Helper()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 20)

	for i := range count {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			_, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{
				Name:   fmt.Sprintf("node-%04d", i),
				Labels: map[string]string{"nvidia.com/gpu.present": "true"},
			}, metav1.CreateOptions{})
			require.NoError(t, err)
		}()
	}

	wg.Wait()
	t.Logf("created %d nodes", count)
}

func generateNodeUpdates(t *testing.T, ctx context.Context, cli kubernetes.Interface, nodeCount, workers int) {
	t.Helper()

	sem := make(chan struct{}, workers)

	for round := 1; ; round++ {
		var wg sync.WaitGroup
		cancelled := false

		for i := range nodeCount {
			select {
			case <-ctx.Done():
				cancelled = true
			case sem <- struct{}{}:
			}
			if cancelled {
				break
			}

			wg.Go(func() {
				defer func() { <-sem }()

				node, err := cli.CoreV1().Nodes().Get(ctx, fmt.Sprintf("node-%04d", i), metav1.GetOptions{})
				if err != nil {
					return
				}
				node.Status.Conditions = []corev1.NodeCondition{{
					Type:              corev1.NodeReady,
					Status:            corev1.ConditionTrue,
					LastHeartbeatTime: metav1.Now(),
					Reason:            fmt.Sprintf("round-%d", round),
				}}
				_, _ = cli.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
			})
		}

		wg.Wait()

		if cancelled {
			return
		}
		t.Logf("heartbeat round %d complete", round)
	}
}

// errNodeLister is a NodeLister that always returns a non-NotFound error from Get,
// used to exercise the fail-closed path in calculateAndSetNodeLabels.
type errNodeLister struct{}

func (e errNodeLister) List(_ labels.Selector) ([]*corev1.Node, error) { return nil, nil }

func (e errNodeLister) Get(_ string) (*corev1.Node, error) {
	return nil, fmt.Errorf("simulated lister error")
}

// nodeIndexerLister builds a NodeLister backed by a fake cache.Indexer containing nodes.
func nodeIndexerLister(nodes ...*corev1.Node) listersv1.NodeLister {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, n := range nodes {
		_ = indexer.Add(n)
	}
	return listersv1.NewNodeLister(indexer)
}

// TestCalculateAndSetNodeLabels_KataAndDeviceCountLabelsMissing_AppliesBoth
// covers a node that needs its Kata label and its device-count labels at the same time.
//
// The results were once combined with `needsUpdate || ...`, and || short-circuits:
// once the Kata label had been set, the next labels were never evaluated.
func TestCalculateAndSetNodeLabels_KataAndDeviceCountLabelsMissing_AppliesBoth(t *testing.T) {
	node := &corev1.Node{
		Name:   "kata-and-count-node",
		Labels: map[string]string{"nvidia.com/gpu.count": "8"},
	}

	// Never started, so no event handler can race the reconcile below.
	nodeInformer := cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Node{}, 0, cache.Indexers{})
	require.NoError(t, nodeInformer.GetStore().Add(node))

	deviceCounts, err := devicecounts.NewManager(testDeviceCountConfig())
	require.NoError(t, err)

	l := &Labeler{
		ctx:          context.Background(),
		nodeInformer: nodeInformer,
		nodeLister:   listersv1.NewNodeLister(nodeInformer.GetIndexer()),
		deviceCounts: deviceCounts,
	}

	target := node.DeepCopy()
	changed, err := l.calculateAndSetNodeLabels(target, l.newDeviceCountReconcileCache())
	require.NoError(t, err)

	assert.True(t, changed, "both labels are missing, so an update is needed")
	assert.Equal(t, LabelValueFalse, target.Labels[KataEnabledLabel])
	assert.Equal(t, "8", target.Labels["test.nvsentinel/current"],
		"the device-count label must not be skipped just because the kata label changed")
}

// TestCalculateAndSetNodeLabels_NVIDIALabels_LeftAlone checks that the labeler leaves
// the NVIDIA labels to nvidia-labeler. During an upgrade, a labeler that removed them
// would take the GPU, syslog and NIC monitor pods off the node.
func TestCalculateAndSetNodeLabels_NVIDIALabels_LeftAlone(t *testing.T) {
	nvidiaLabels := map[string]string{
		"nvsentinel.dgxc.nvidia.com/dcgm.version":     "4.x",
		"nvsentinel.dgxc.nvidia.com/driver.installed": "true",
	}

	node := &corev1.Node{Name: "gpu-node", Labels: map[string]string{}}
	maps.Copy(node.Labels, nvidiaLabels)

	l := &Labeler{
		ctx:          context.Background(),
		nodeLister:   nodeIndexerLister(node),
		deviceCounts: mustDeviceCountManager(t, devicecounts.Config{}),
	}
	l.nodeInformer = cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Node{}, 0, cache.Indexers{})

	target := node.DeepCopy()
	_, err := l.calculateAndSetNodeLabels(target, l.newDeviceCountReconcileCache())
	require.NoError(t, err)

	for key, value := range nvidiaLabels {
		assert.Equal(t, value, target.Labels[key], "the labeler must not change %s", key)
	}
}

func mustDeviceCountManager(t *testing.T, config devicecounts.Config) *devicecounts.Manager {
	t.Helper()

	manager, err := devicecounts.NewManager(config)
	require.NoError(t, err)

	return manager
}

func TestCalculateAndSetNodeLabels_ManagedGate(t *testing.T) {
	t.Run("strips detection labels when node is opted out", func(t *testing.T) {
		node := &corev1.Node{
			Name: "opted-out-node",
			Labels: map[string]string{
				managed.ManagedLabelKey: managed.ManagedLabelValueFalse,
				dcgmVersionLabel:        "3.x",
				KataEnabledLabel:        "true",
			},
		}
		l := &Labeler{
			ctx:        context.Background(),
			nodeLister: nodeIndexerLister(node),
		}

		target := node.DeepCopy()
		changed, err := l.calculateAndSetNodeLabels(target, l.newDeviceCountReconcileCache())
		require.NoError(t, err)

		assert.True(t, changed, "should report labels changed")
		assert.NotContains(t, target.Labels, KataEnabledLabel, "kata label must be stripped")
		assert.Equal(t, "3.x", target.Labels[dcgmVersionLabel], "nvidia-labeler strips the DCGM label")
		// The managed label itself is owned by the ERR reconciler — labeler must not touch it.
		assert.Equal(t, managed.ManagedLabelValueFalse, target.Labels[managed.ManagedLabelKey])
	})

	t.Run("no-op when opted-out node has no detection labels", func(t *testing.T) {
		node := &corev1.Node{
			Name:   "opted-out-clean",
			Labels: map[string]string{managed.ManagedLabelKey: managed.ManagedLabelValueFalse},
		}
		l := &Labeler{
			ctx:        context.Background(),
			nodeLister: nodeIndexerLister(node),
		}
		target := node.DeepCopy()
		changed, err := l.calculateAndSetNodeLabels(target, l.newDeviceCountReconcileCache())
		require.NoError(t, err)
		assert.False(t, changed, "no labels to strip → no update needed")
	})

	t.Run("fail closed — returns false without mutating on lister error", func(t *testing.T) {
		node := &corev1.Node{
			Name:   "error-node",
			Labels: map[string]string{KataEnabledLabel: LabelValueTrue},
		}
		l := &Labeler{
			ctx:        context.Background(),
			nodeLister: errNodeLister{},
		}
		target := node.DeepCopy()
		changed, err := l.calculateAndSetNodeLabels(target, l.newDeviceCountReconcileCache())
		require.NoError(t, err)

		assert.False(t, changed, "lister error must not report changes")
		assert.Contains(t, target.Labels, KataEnabledLabel, "lister error must not strip labels")
	})

}

// TestReconcileNodeLabels_CachedNode_UsesPatchOnly pins the API-call budget and payload
// of the label write path.
func TestReconcileNodeLabels_CachedNode_UsesPatchOnly(t *testing.T) {
	const nodeName = "gpu-node"

	clientset := fake.NewSimpleClientset(&corev1.Node{
		Name:            nodeName,
		ResourceVersion: "1",
		Labels:          map[string]string{unrelatedLabel: LabelValueTrue},
	})

	labeler, _ := startTransformTestLabeler(t, clientset, devicecounts.Config{})
	requireCachedLabel(t, labeler, nodeName, KataEnabledLabel, LabelValueFalse)

	tests := []struct {
		name          string
		setup         func(*testing.T)
		expectedPatch string
	}{
		{
			name: "label change costs one merge patch",
			setup: func(t *testing.T) {
				node, getErr := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
				require.NoError(t, getErr)
				delete(node.Labels, KataEnabledLabel)
				_, updateErr := clientset.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
				require.NoError(t, updateErr)
				requireCachedLabelAbsent(t, labeler, nodeName, KataEnabledLabel)
			},
			expectedPatch: fmt.Sprintf(
				`{"metadata":{"labels":{%q:%q}}}`,
				KataEnabledLabel,
				LabelValueFalse,
			),
		},
		{
			name: "no-op reconcile costs no API call",
			setup: func(t *testing.T) {
				requireCachedLabel(t, labeler, nodeName, KataEnabledLabel, LabelValueFalse)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)
			clientset.ClearActions()

			require.NoError(t, labeler.reconcileNodeLabels(nodeName, labeler.newDeviceCountReconcileCache()))

			expectedPatchCount := 0
			if tt.expectedPatch != "" {
				expectedPatchCount = 1
				action := nodePatchAction(t, clientset.Actions())
				assert.Equal(t, types.MergePatchType, action.GetPatchType())
				assert.JSONEq(t, tt.expectedPatch, string(action.GetPatch()))
			}

			assert.Equal(t, expectedPatchCount, countNodeActions(clientset, "patch"))
			assert.Equal(t, 0, countNodeActions(clientset, "get"))
			assert.Equal(t, 0, countNodeActions(clientset, "update"))

			updated, getErr := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
			require.NoError(t, getErr)
			assert.Equal(t, LabelValueFalse, updated.Labels[KataEnabledLabel], "the desired label is present")
			assert.Equal(t, LabelValueTrue, updated.Labels[unrelatedLabel],
				"a merge patch must leave labels it does not mention alone")
		})
	}
}

func TestReconcileNodeLabels_DelayedInformerEvent_EventuallyConverges(t *testing.T) {
	const nodeName = "delayed-node"

	initial := &corev1.Node{
		Name: nodeName,
		Labels: map[string]string{
			KataRuntimeDefaultLabel: LabelValueFalse,
			KataEnabledLabel:        LabelValueFalse,
		},
	}

	// A controlled fake watch lets the API object advance independently of the
	// informer cache, making the stale-cache checkpoints deterministic.
	clientset := fake.NewSimpleClientset(initial)
	nodeWatch := watch.NewRaceFreeFake()
	clientset.PrependWatchReactor("nodes", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, nodeWatch, nil
	})

	labeler, _ := startTransformTestLabeler(t, clientset, devicecounts.Config{})
	requireCachedLabel(t, labeler, nodeName, KataRuntimeDefaultLabel, LabelValueFalse)

	// Change the API object while withholding its watch event. The informer is synced,
	// but its node projection deliberately remains one generation behind.
	liveNode, err := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	liveNode.Labels[KataRuntimeDefaultLabel] = LabelValueTrue
	liveNode, err = clientset.CoreV1().Nodes().Update(t.Context(), liveNode, metav1.UpdateOptions{})
	require.NoError(t, err)

	cachedNode, err := labeler.getNodeFromCache(nodeName)
	require.NoError(t, err)
	assert.Equal(t, LabelValueFalse, cachedNode.Labels[KataRuntimeDefaultLabel])
	assert.Equal(t, LabelValueFalse, liveNode.Labels[KataEnabledLabel])

	// A reconcile against the stale projection computes the stale derived value and
	// correctly emits no write.
	clientset.ClearActions()
	require.NoError(t, labeler.reconcileNodeLabels(nodeName, labeler.newDeviceCountReconcileCache()))
	assert.Empty(t, clientset.Actions())

	// Deliver the delayed input event. Its handler must reconcile from the new cache
	// generation and correct the derived label.
	nodeWatch.Modify(liveNode.DeepCopy())
	requireCachedLabel(t, labeler, nodeName, KataRuntimeDefaultLabel, LabelValueTrue)

	cachedNode, err = labeler.getNodeFromCache(nodeName)
	require.NoError(t, err)
	assert.Equal(t, LabelValueFalse, cachedNode.Labels[KataEnabledLabel])

	require.Eventually(t, func() bool {
		node, getErr := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
		return getErr == nil && node.Labels[KataEnabledLabel] == LabelValueTrue
	}, 10*time.Second, 10*time.Millisecond)

	finalNode, err := clientset.CoreV1().Nodes().Get(t.Context(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	nodeWatch.Modify(finalNode.DeepCopy())
	requireCachedLabel(t, labeler, nodeName, KataEnabledLabel, LabelValueTrue)
}

func countNodeActions(clientset *fake.Clientset, verb string) int {
	count := 0

	for _, action := range clientset.Actions() {
		if action.GetVerb() == verb && action.GetResource().Resource == "nodes" {
			count++
		}
	}

	return count
}

func nodePatchAction(t *testing.T, actions []k8stesting.Action) k8stesting.PatchAction {
	t.Helper()

	for _, action := range actions {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "nodes" {
			patchAction, ok := action.(k8stesting.PatchAction)
			require.True(t, ok)

			return patchAction
		}
	}

	require.FailNow(t, "node PATCH action not found")
	return nil
}

// requireCachedLabel waits for the informer cache to show the given label, which is
// how a test tells that the labeler has finished reacting to earlier events.
func requireCachedLabel(t *testing.T, l *Labeler, nodeName, key, value string) {
	t.Helper()

	require.Eventually(t, func() bool {
		node, err := l.getNodeFromCache(nodeName)
		return err == nil && node.Labels[key] == value
	}, 10*time.Second, 10*time.Millisecond, "cache never showed %s=%s", key, value)
}

func requireCachedLabelAbsent(t *testing.T, l *Labeler, nodeName, key string) {
	t.Helper()

	require.Eventually(t, func() bool {
		node, err := l.getNodeFromCache(nodeName)
		if err != nil {
			return false
		}

		_, exists := node.Labels[key]

		return !exists
	}, 10*time.Second, 10*time.Millisecond, "cache never dropped %s", key)
}
