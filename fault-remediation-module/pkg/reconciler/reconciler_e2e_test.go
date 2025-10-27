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
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation-module/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation-module/pkg/crstatus"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	testClient     *kubernetes.Clientset
	testDynamic    dynamic.Interface
	testContext    context.Context
	testCancelFunc context.CancelFunc
	testEnv        *envtest.Environment
	testRestConfig *rest.Config
)

func TestMain(m *testing.M) {
	var err error
	testContext, testCancelFunc = context.WithCancel(context.Background())

	// Get the path to CRD files
	crdPath := filepath.Join("testdata", "janitor.dgxc.nvidia.com_rebootnodes.yaml")

	// Setup envtest environment with CRDs
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Dir(crdPath)},
	}

	testRestConfig, err = testEnv.Start()
	if err != nil {
		log.Fatalf("Failed to start test environment: %v", err)
	}

	testClient, err = kubernetes.NewForConfig(testRestConfig)
	if err != nil {
		log.Fatalf("Failed to create kubernetes client: %v", err)
	}

	testDynamic, err = dynamic.NewForConfig(testRestConfig)
	if err != nil {
		log.Fatalf("Failed to create dynamic client: %v", err)
	}

	// Create test nodes with dummy labels to avoid nil map panic
	dummyLabels := map[string]string{"test": "label"}
	createTestNode(testContext, "test-node-1", nil, dummyLabels)
	createTestNode(testContext, "test-node-2", nil, dummyLabels)
	createTestNode(testContext, "test-node-3", nil, dummyLabels)
	createTestNode(testContext, "node-with-annotation", nil, dummyLabels)

	exitCode := m.Run()

	tearDownTestEnvironment()
	os.Exit(exitCode)
}

func createTestNode(ctx context.Context, name string, annotations map[string]string, labels map[string]string) {
	if labels == nil {
		labels = make(map[string]string)
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
			Labels:      labels,
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	_, err := testClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create test node %s: %v", name, err)
	}
}

func tearDownTestEnvironment() {
	testCancelFunc()
	if err := testEnv.Stop(); err != nil {
		log.Fatalf("Failed to stop test environment: %v", err)
	}
}

// createTestRemediationClient creates a real FaultRemediationClient for e2e tests
func createTestRemediationClient(t *testing.T, dryRun bool) *FaultRemediationClient {
	t.Helper()

	// Create discovery client for RESTMapper
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(testRestConfig)
	require.NoError(t, err)

	cachedClient := memory.NewMemCacheClient(discoveryClient)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)

	templatePath := filepath.Join("templates", "rebootnode-template.yaml")
	templateContent, err := os.ReadFile(templatePath)
	require.NoError(t, err, "Failed to read production template at %s", templatePath)

	tmpl, err := template.New("maintenance").Parse(string(templateContent))
	require.NoError(t, err)

	templateData := TemplateData{
		ApiGroup: "janitor.dgxc.nvidia.com",
		Version:  "v1alpha1",
	}

	client := &FaultRemediationClient{
		clientset:         testDynamic,
		kubeClient:        testClient,
		restMapper:        mapper,
		template:          tmpl,
		templateData:      templateData,
		annotationManager: NewNodeAnnotationManager(testClient),
	}

	if dryRun {
		client.dryRunMode = []string{metav1.DryRunAll}
	} else {
		client.dryRunMode = []string{}
	}

	// Initialize status checker factory
	client.statusCheckerFactory = crstatus.NewCRStatusCheckerFactory(
		testDynamic, mapper, dryRun)

	return client
}

func TestCRBasedDeduplication_Integration(t *testing.T) {
	ctx := testContext

	nodeName := "test-node-dedup-" + primitive.NewObjectID().Hex()[:8]
	createTestNode(ctx, nodeName, nil, map[string]string{"test": "label"})
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	t.Run("FirstEvent_CreatesAnnotation", func(t *testing.T) {
		cleanupNodeAnnotations(ctx, t, nodeName)

		remediationClient := createTestRemediationClient(t, false)

		cfg := ReconcilerConfig{
			RemediationClient: remediationClient,
			StateManager:      statemanager.NewStateManager(testClient),
			UpdateMaxRetries:  3,
			UpdateRetryDelay:  100 * time.Millisecond,
		}
		r := NewReconciler(cfg, false)

		// Process Event 1
		healthEventDoc := &HealthEventDoc{
			ID: primitive.NewObjectID(),
			HealthEventWithStatus: model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:          nodeName,
					RecommendedAction: protos.RecommendedAction_RESTART_BM,
				},
			},
		}

		success, crName := r.performRemediation(ctx, healthEventDoc)
		assert.True(t, success, "First event should create CR")
		assert.NotEmpty(t, crName)

		// Verify annotation exists on node
		node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		require.NoError(t, err)
		require.Contains(t, node.Annotations, AnnotationKey, "Annotation should be created")

		// Verify annotation content
		state, err := r.annotationManager.GetRemediationState(ctx, nodeName)
		require.NoError(t, err)
		assert.Contains(t, state.EquivalenceGroups, "restart")
		assert.NotEmpty(t, state.EquivalenceGroups["restart"].MaintenanceCR)
		assert.WithinDuration(t, time.Now(), state.EquivalenceGroups["restart"].CreatedAt, 5*time.Second)

		// Verify CR was actually created
		gvr := schema.GroupVersionResource{
			Group:    "janitor.dgxc.nvidia.com",
			Version:  "v1alpha1",
			Resource: "rebootnodes",
		}
		cr, err := testDynamic.Resource(gvr).Get(ctx, crName, metav1.GetOptions{})
		require.NoError(t, err)
		assert.Equal(t, nodeName, cr.Object["spec"].(map[string]interface{})["nodeName"])

		// Cleanup
		_ = testDynamic.Resource(gvr).Delete(ctx, crName, metav1.DeleteOptions{})
	})

	t.Run("SecondEvent_SkippedWhenCRInProgress", func(t *testing.T) {
		cleanupNodeAnnotations(ctx, t, nodeName)

		remediationClient := createTestRemediationClient(t, false)

		cfg := ReconcilerConfig{
			RemediationClient: remediationClient,
			StateManager:      statemanager.NewStateManager(testClient),
			UpdateMaxRetries:  3,
			UpdateRetryDelay:  100 * time.Millisecond,
		}
		r := NewReconciler(cfg, false)

		// Event 1: Create first CR
		event1 := &HealthEventDoc{
			ID: primitive.NewObjectID(),
			HealthEventWithStatus: model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:          nodeName,
					RecommendedAction: protos.RecommendedAction_RESTART_BM,
				},
			},
		}
		_, firstCRName := r.performRemediation(ctx, event1)

		// Update CR status to InProgress
		updateRebootNodeStatus(ctx, t, firstCRName, "InProgress")

		// Event 2: Should be skipped
		shouldCreateCR, existingCR, err := r.checkExistingCRStatus(ctx, event1.HealthEventWithStatus.HealthEvent)
		assert.NoError(t, err)
		assert.False(t, shouldCreateCR, "Second event should be skipped")
		assert.Equal(t, firstCRName, existingCR)

		// Verify annotation still exists and unchanged
		state, err := r.annotationManager.GetRemediationState(ctx, nodeName)
		require.NoError(t, err)
		assert.Equal(t, firstCRName, state.EquivalenceGroups["restart"].MaintenanceCR)

		// Cleanup
		gvr := schema.GroupVersionResource{
			Group:    "janitor.dgxc.nvidia.com",
			Version:  "v1alpha1",
			Resource: "rebootnodes",
		}
		_ = testDynamic.Resource(gvr).Delete(ctx, firstCRName, metav1.DeleteOptions{})
	})

	t.Run("FailedCR_CleansAnnotationAndAllowsRetry", func(t *testing.T) {
		cleanupNodeAnnotations(ctx, t, nodeName)

		remediationClient := createTestRemediationClient(t, false)

		cfg := ReconcilerConfig{
			RemediationClient: remediationClient,
			StateManager:      statemanager.NewStateManager(testClient),
			UpdateMaxRetries:  3,
			UpdateRetryDelay:  100 * time.Millisecond,
		}
		r := NewReconciler(cfg, false)

		// Event 1: Create first CR
		event1 := &HealthEventDoc{
			ID: primitive.NewObjectID(),
			HealthEventWithStatus: model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:          nodeName,
					RecommendedAction: protos.RecommendedAction_RESTART_BM,
				},
			},
		}
		_, firstCRName := r.performRemediation(ctx, event1)

		// Simulate CR failure
		updateRebootNodeStatus(ctx, t, firstCRName, "Failed")

		// Event 2: Should create new CR after cleanup
		shouldCreateCR, _, err := r.checkExistingCRStatus(ctx, event1.HealthEventWithStatus.HealthEvent)
		assert.NoError(t, err)
		assert.True(t, shouldCreateCR, "Should allow retry after CR failed")

		// Verify annotation was cleaned up
		state, err := r.annotationManager.GetRemediationState(ctx, nodeName)
		require.NoError(t, err)
		assert.NotContains(t, state.EquivalenceGroups, "restart", "Failed CR should be removed from annotation")

		// Event 2: Create retry CR
		event2 := &HealthEventDoc{
			ID: primitive.NewObjectID(),
			HealthEventWithStatus: model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:          nodeName,
					RecommendedAction: protos.RecommendedAction_RESTART_BM,
				},
			},
		}

		_, secondCRName := r.performRemediation(ctx, event2)

		// Verify new annotation
		state, err = r.annotationManager.GetRemediationState(ctx, nodeName)
		require.NoError(t, err)
		assert.Contains(t, state.EquivalenceGroups, "restart")
		assert.Equal(t, secondCRName, state.EquivalenceGroups["restart"].MaintenanceCR)
		assert.NotEqual(t, firstCRName, secondCRName, "Second CR should have different name")

		// Cleanup
		gvr := schema.GroupVersionResource{
			Group:    "janitor.dgxc.nvidia.com",
			Version:  "v1alpha1",
			Resource: "rebootnodes",
		}
		_ = testDynamic.Resource(gvr).Delete(ctx, firstCRName, metav1.DeleteOptions{})
		_ = testDynamic.Resource(gvr).Delete(ctx, secondCRName, metav1.DeleteOptions{})
	})

	t.Run("CrossAction_SameGroupDeduplication", func(t *testing.T) {
		cleanupNodeAnnotations(ctx, t, nodeName)

		remediationClient := createTestRemediationClient(t, false)

		cfg := ReconcilerConfig{
			RemediationClient: remediationClient,
			StateManager:      statemanager.NewStateManager(testClient),
			UpdateMaxRetries:  3,
			UpdateRetryDelay:  100 * time.Millisecond,
		}
		r := NewReconciler(cfg, false)

		// Event 1: COMPONENT_RESET
		event1 := &HealthEventDoc{
			ID: primitive.NewObjectID(),
			HealthEventWithStatus: model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:          nodeName,
					RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
				},
			},
		}
		_, firstCRName := r.performRemediation(ctx, event1)

		// Set InProgress status
		updateRebootNodeStatus(ctx, t, firstCRName, "InProgress")

		// Event 2: RESTART_VM (same group)
		event2Health := &protos.HealthEvent{
			NodeName:          nodeName,
			RecommendedAction: protos.RecommendedAction_RESTART_BM,
		}

		shouldCreateCR, existingCR, err := r.checkExistingCRStatus(ctx, event2Health)
		assert.NoError(t, err)
		assert.False(t, shouldCreateCR, "RESTART_VM should be deduplicated with COMPONENT_RESET (same group)")
		assert.Equal(t, firstCRName, existingCR)

		// Verify both actions map to same group
		group1 := common.GetRemediationGroupForAction(protos.RecommendedAction_COMPONENT_RESET)
		group2 := common.GetRemediationGroupForAction(protos.RecommendedAction_RESTART_BM)
		assert.Equal(t, group1, group2, "Both actions should be in same equivalence group")
		assert.Equal(t, "restart", group1)

		// Cleanup
		gvr := schema.GroupVersionResource{
			Group:    "janitor.dgxc.nvidia.com",
			Version:  "v1alpha1",
			Resource: "rebootnodes",
		}
		_ = testDynamic.Resource(gvr).Delete(ctx, firstCRName, metav1.DeleteOptions{})
	})
}

func TestEventSequenceWithAnnotations_Integration(t *testing.T) {
	ctx := testContext

	nodeName := "test-node-sequence-" + primitive.NewObjectID().Hex()[:8]
	createTestNode(ctx, nodeName, nil, map[string]string{"test": "label"})
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	cleanupNodeAnnotations(ctx, t, nodeName)

	remediationClient := createTestRemediationClient(t, false)

	cfg := ReconcilerConfig{
		RemediationClient: remediationClient,
		StateManager:      statemanager.NewStateManager(testClient),
		UpdateMaxRetries:  3,
		UpdateRetryDelay:  100 * time.Millisecond,
	}
	r := NewReconciler(cfg, false)

	gvr := schema.GroupVersionResource{
		Group:    "janitor.dgxc.nvidia.com",
		Version:  "v1alpha1",
		Resource: "rebootnodes",
	}

	// Event 1: RESTART_VM creates CR-1
	event1 := &HealthEventDoc{
		ID: primitive.NewObjectID(),
		HealthEventWithStatus: model.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName:          nodeName,
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
		},
	}
	success, crName1 := r.performRemediation(ctx, event1)
	assert.True(t, success)

	// Verify annotation on actual node
	node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, node.Annotations, AnnotationKey, "Node should have annotation after first CR")

	// Event 2: COMPONENT_RESET (same group, CR in progress) - should be skipped
	updateRebootNodeStatus(ctx, t, crName1, "InProgress")

	event2 := &protos.HealthEvent{
		NodeName:          nodeName,
		RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
	}
	shouldCreate, existingCR, err := r.checkExistingCRStatus(ctx, event2)
	assert.NoError(t, err)
	assert.False(t, shouldCreate, "COMPONENT_RESET should be skipped (same group as RESTART_VM)")
	assert.Equal(t, crName1, existingCR)

	// Verify annotation unchanged
	state, err := r.annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Equal(t, crName1, state.EquivalenceGroups["restart"].MaintenanceCR)

	// Event 3: CR succeeds - subsequent event still skipped
	updateRebootNodeStatus(ctx, t, crName1, "Succeeded")

	event3 := &protos.HealthEvent{
		NodeName:          nodeName,
		RecommendedAction: protos.RecommendedAction_RESTART_VM,
	}
	shouldCreate, _, err = r.checkExistingCRStatus(ctx, event3)
	assert.NoError(t, err)
	assert.False(t, shouldCreate, "RESTART_VM should be skipped (CR succeeded)")

	// Event 4: CR fails - annotation cleaned, retry allowed
	updateRebootNodeStatus(ctx, t, crName1, "Failed")

	event4 := &protos.HealthEvent{
		NodeName:          nodeName,
		RecommendedAction: protos.RecommendedAction_RESTART_BM,
	}
	shouldCreate, _, err = r.checkExistingCRStatus(ctx, event4)
	assert.NoError(t, err)
	assert.True(t, shouldCreate, "Should allow retry after failure")

	// Verify annotation cleaned
	state, err = r.annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.NotContains(t, state.EquivalenceGroups, "restart", "Failed CR should clean annotation")

	// Event 5: Create retry CR
	event5 := &HealthEventDoc{
		ID: primitive.NewObjectID(),
		HealthEventWithStatus: model.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName:          nodeName,
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
		},
	}
	_, crName2 := r.performRemediation(ctx, event5)

	// Verify new annotation
	state, err = r.annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Contains(t, state.EquivalenceGroups, "restart")
	assert.Equal(t, crName2, state.EquivalenceGroups["restart"].MaintenanceCR)

	// Cleanup
	_ = testDynamic.Resource(gvr).Delete(ctx, crName1, metav1.DeleteOptions{})
	_ = testDynamic.Resource(gvr).Delete(ctx, crName2, metav1.DeleteOptions{})
}

// TestFullReconcilerWithMockedMongoDB tests the entire reconciler flow
func TestFullReconcilerWithMockedMongoDB_E2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext, 30*time.Second)
	defer cancel()

	nodeName := "test-node-full-e2e-" + primitive.NewObjectID().Hex()[:8]
	createTestNode(ctx, nodeName, nil, map[string]string{"test": "label"})
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	cleanupNodeAnnotations(ctx, t, nodeName)

	gvr := schema.GroupVersionResource{
		Group:    "janitor.dgxc.nvidia.com",
		Version:  "v1alpha1",
		Resource: "rebootnodes",
	}

	t.Run("CompleteFlow_WithEventLoop", func(t *testing.T) {
		remediationClient := createTestRemediationClient(t, false)

		// Create mock MongoDB collection
		updateCalled := 0
		mockColl := &MockCollection{
			updateOneFn: func(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
				updateCalled++
				return &mongo.UpdateResult{ModifiedCount: 1}, nil
			},
		}

		// Create mock watcher with event channel
		eventsChan := make(chan bson.M, 10)
		mockWatcher := &MockChangeStreamWatcher{
			eventsChan:          eventsChan,
			markProcessedCalled: 0,
		}

		cfg := ReconcilerConfig{
			RemediationClient: remediationClient,
			StateManager:      statemanager.NewStateManager(testClient),
			UpdateMaxRetries:  3,
			UpdateRetryDelay:  100 * time.Millisecond,
		}

		reconcilerInstance := NewReconciler(cfg, false)

		// Start event processing loop (simulating Start() event loop)
		reconcilerDone := make(chan struct{})
		go func() {
			defer close(reconcilerDone)
			mockWatcher.Start(ctx)
			slog.Info("Test: Listening for events on the channel...")
			for event := range mockWatcher.Events() {
				slog.Info("Test: Event received", "eventData", event)
				reconcilerInstance.processEvent(ctx, event, mockWatcher, mockColl)
			}
		}()

		// Event 1: Send quarantine event through channel
		eventID1 := primitive.NewObjectID()
		event1 := createQuarantineEvent(eventID1, nodeName, protos.RecommendedAction_RESTART_BM)
		eventsChan <- event1

		// Wait for CR creation
		var crName string
		assert.Eventually(t, func() bool {
			state, err := reconcilerInstance.annotationManager.GetRemediationState(ctx, nodeName)
			if err != nil {
				return false
			}
			if grp, ok := state.EquivalenceGroups["restart"]; ok {
				crName = grp.MaintenanceCR
				return crName != ""
			}
			return false
		}, 5*time.Second, 100*time.Millisecond, "CR should be created")

		// Verify annotation is actually on the node object in Kubernetes
		node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		require.NoError(t, err)
		require.Contains(t, node.Annotations, AnnotationKey, "Node should have remediation annotation")

		// Verify annotation content
		state, err := reconcilerInstance.annotationManager.GetRemediationState(ctx, nodeName)
		require.NoError(t, err)
		assert.Contains(t, state.EquivalenceGroups, "restart", "Should have restart equivalence group")
		assert.Equal(t, crName, state.EquivalenceGroups["restart"].MaintenanceCR, "Annotation should contain correct CR name")

		// Verify CR exists in Kubernetes
		cr, err := testDynamic.Resource(gvr).Get(ctx, crName, metav1.GetOptions{})
		require.NoError(t, err, "CR should exist in Kubernetes")
		assert.Equal(t, nodeName, cr.Object["spec"].(map[string]interface{})["nodeName"])

		// Verify only one CR exists for this node
		crList, err := testDynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		crCount := 0
		for _, item := range crList.Items {
			if item.Object["spec"].(map[string]interface{})["nodeName"] == nodeName {
				crCount++
			}
		}
		assert.Equal(t, 1, crCount, "Only one CR should exist for the node at this point")

		// Event 2: Set CR to InProgress, send duplicate event (different action, same group)
		updateRebootNodeStatus(ctx, t, crName, "InProgress")

		// Record MongoDB update count before sending duplicate event
		updateCountBefore := updateCalled

		eventID2 := primitive.NewObjectID()
		event2 := createQuarantineEvent(eventID2, nodeName, protos.RecommendedAction_COMPONENT_RESET)
		eventsChan <- event2

		// Wait for event to be processed and verify deduplication
		assert.Eventually(t, func() bool {
			state, err := reconcilerInstance.annotationManager.GetRemediationState(ctx, nodeName)
			if err != nil {
				t.Logf("Failed to get remediation state: %v", err)
				return false
			}

			// Check if the CR name is still the original one (deduplication working)
			if grp, ok := state.EquivalenceGroups["restart"]; ok {
				return grp.MaintenanceCR == crName
			}

			return false
		}, 5*time.Second, 100*time.Millisecond, "Event should be processed and deduplicated")

		// Verify annotation is still on the node and unchanged
		node, err = testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		require.NoError(t, err)
		require.Contains(t, node.Annotations, AnnotationKey, "Node should still have remediation annotation")

		// Verify no new CR created (deduplication) - annotation should be unchanged
		state, err = reconcilerInstance.annotationManager.GetRemediationState(ctx, nodeName)
		require.NoError(t, err)
		assert.Equal(t, crName, state.EquivalenceGroups["restart"].MaintenanceCR, "Should still be same CR (deduplicated)")

		// Verify only ONE CR exists (no duplicate CR was created)
		crList2, err := testDynamic.Resource(gvr).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		crCount2 := 0
		for _, item := range crList2.Items {
			if item.Object["spec"].(map[string]interface{})["nodeName"] == nodeName {
				crCount2++
			}
		}
		assert.Equal(t, 1, crCount2, "Should still be only one CR (duplicate event was skipped)")

		// Verify MongoDB update was NOT called (event was skipped due to deduplication)
		assert.Equal(t, updateCountBefore, updateCalled, "MongoDB update should not be called for skipped event")

		// Event 3: Send unquarantine event
		unquarantineEvent := createUnquarantineEvent(nodeName)
		eventsChan <- unquarantineEvent

		// Wait for annotation cleanup
		assert.Eventually(t, func() bool {
			state, err := reconcilerInstance.annotationManager.GetRemediationState(ctx, nodeName)
			if err != nil {
				return false
			}
			_, hasRestart := state.EquivalenceGroups["restart"]
			return !hasRestart
		}, 5*time.Second, 100*time.Millisecond, "Annotation should be cleaned after unquarantine")

		// Verify annotation was actually removed from the node object in Kubernetes
		node, err = testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		require.NoError(t, err)
		_, hasAnnotation := node.Annotations[AnnotationKey]
		if hasAnnotation {
			// Annotation might exist but should be empty or not contain "restart" group
			state, err = reconcilerInstance.annotationManager.GetRemediationState(ctx, nodeName)
			require.NoError(t, err)
			assert.NotContains(t, state.EquivalenceGroups, "restart", "Restart group should be removed from annotation")
		}

		// Stop the reconciler by closing the events channel
		close(eventsChan)

		select {
		case <-reconcilerDone:
			// Reconciler stopped cleanly
		case <-time.After(5 * time.Second):
			t.Fatal("Reconciler did not stop in time")
		}

		// Verify MarkProcessed was called for processed events
		mockWatcher.mu.Lock()
		markedCount := mockWatcher.markProcessedCalled
		mockWatcher.mu.Unlock()
		assert.Greater(t, markedCount, 0, "MarkProcessed should be called for processed events")

		// Cleanup
		_ = testDynamic.Resource(gvr).Delete(ctx, crName, metav1.DeleteOptions{})
	})
}

// Helper to create quarantine event
func createQuarantineEvent(eventID primitive.ObjectID, nodeName string, action protos.RecommendedAction) bson.M {
	return bson.M{
		"operationType": "update",
		"fullDocument": bson.M{
			"_id": eventID,
			"healtheventstatus": bson.M{
				"userpodsevictionstatus": bson.M{
					"status": model.StatusSucceeded,
				},
				"nodequarantined": model.Quarantined,
			},
			"healthevent": bson.M{
				"nodename":          nodeName,
				"recommendedaction": int32(action),
			},
		},
	}
}

// Helper to create unquarantine event
func createUnquarantineEvent(nodeName string) bson.M {
	return bson.M{
		"operationType": "update",
		"fullDocument": bson.M{
			"_id": primitive.NewObjectID(),
			"healtheventstatus": bson.M{
				"nodequarantined": model.UnQuarantined,
				"userpodsevictionstatus": bson.M{
					"status": model.StatusSucceeded,
				},
			},
			"healthevent": bson.M{
				"nodename": nodeName,
			},
		},
	}
}

// MockChangeStreamWatcher mocks the change stream watcher for testing
// Implements WatcherInterface from reconciler package
type MockChangeStreamWatcher struct {
	eventsChan          chan bson.M
	markProcessedCalled int
	mu                  sync.Mutex
}

func (m *MockChangeStreamWatcher) Events() <-chan bson.M {
	return m.eventsChan
}

func (m *MockChangeStreamWatcher) Start(ctx context.Context) {
	// No-op for mock - events are sent directly to channel in tests
}

func (m *MockChangeStreamWatcher) Close(ctx context.Context) error {
	// No-op for mock - channel is closed in test
	return nil
}

func (m *MockChangeStreamWatcher) MarkProcessed(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markProcessedCalled++
	return nil
}

// Helper functions

// updateRebootNodeStatus updates the status of a RebootNode CR for testing
func updateRebootNodeStatus(ctx context.Context, t *testing.T, crName, status string) {
	t.Helper()

	gvr := schema.GroupVersionResource{
		Group:    "janitor.dgxc.nvidia.com",
		Version:  "v1alpha1",
		Resource: "rebootnodes",
	}

	// Get the CR
	cr, err := testDynamic.Resource(gvr).Get(ctx, crName, metav1.GetOptions{})
	if err != nil {
		t.Logf("Warning: Failed to get RebootNode CR %s: %v", crName, err)
		return
	}

	// Update status based on the provided status string
	conditions := []interface{}{}
	switch status {
	case "Succeeded":
		conditions = append(conditions, map[string]interface{}{
			"type":               "SignalSent",
			"status":             "True",
			"reason":             "SignalSent",
			"message":            "Reboot signal sent successfully",
			"lastTransitionTime": time.Now().Format(time.RFC3339),
		})
		conditions = append(conditions, map[string]interface{}{
			"type":               "NodeReady",
			"status":             "True",
			"reason":             "NodeReady",
			"message":            "Node is ready after reboot",
			"lastTransitionTime": time.Now().Format(time.RFC3339),
		})
		cr.Object["status"] = map[string]interface{}{
			"conditions":     conditions,
			"startTime":      time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
			"completionTime": time.Now().Format(time.RFC3339),
		}
	case "InProgress":
		conditions = append(conditions, map[string]interface{}{
			"type":               "SignalSent",
			"status":             "True",
			"reason":             "SignalSent",
			"message":            "Reboot signal sent",
			"lastTransitionTime": time.Now().Format(time.RFC3339),
		})
		cr.Object["status"] = map[string]interface{}{
			"conditions": conditions,
			"startTime":  time.Now().Add(-1 * time.Minute).Format(time.RFC3339),
		}
	case "Failed":
		conditions = append(conditions, map[string]interface{}{
			"type":               "SignalSent",
			"status":             "False",
			"reason":             "Failed",
			"message":            "Failed to send reboot signal",
			"lastTransitionTime": time.Now().Format(time.RFC3339),
		})
		cr.Object["status"] = map[string]interface{}{
			"conditions":     conditions,
			"startTime":      time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
			"completionTime": time.Now().Format(time.RFC3339),
		}
	}

	// Update the CR status using UpdateStatus
	_, err = testDynamic.Resource(gvr).UpdateStatus(ctx, cr, metav1.UpdateOptions{})
	if err != nil {
		t.Logf("Warning: Failed to update RebootNode CR status: %v", err)
	}
}

func cleanupNodeAnnotations(ctx context.Context, t *testing.T, nodeName string) {
	node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		t.Logf("Warning: Failed to get node for cleanup: %v", err)
		return
	}

	if node.Annotations == nil {
		return
	}

	delete(node.Annotations, AnnotationKey)
	_, err = testClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		t.Logf("Warning: Failed to clean node annotations: %v", err)
	}
}
