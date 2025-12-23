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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/nvidia/nvsentinel/commons/pkg/statemanager"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/common"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/config"
	"github.com/nvidia/nvsentinel/fault-remediation/pkg/crstatus"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/testutils"
)

// MockChangeStreamWatcher provides a mock implementation of datastore.ChangeStreamWatcher for testing
type MockChangeStreamWatcher struct {
	// Used for concurrency safety between reconciler calling writes and tests reading counts
	mu                 sync.Mutex
	EventsChan         chan datastore.EventWithToken
	markProcessedCount int
}

// NewMockChangeStreamWatcher creates a new mock change stream Watcher
func NewMockChangeStreamWatcher() *MockChangeStreamWatcher {
	return &MockChangeStreamWatcher{
		EventsChan: make(chan datastore.EventWithToken, 10),
	}
}

// Events returns the read-only events channel
func (m *MockChangeStreamWatcher) Events() <-chan datastore.EventWithToken {
	return m.EventsChan
}

// Start starts the change stream Watcher (no-op for tests)
func (m *MockChangeStreamWatcher) Start(ctx context.Context) {
	// No-op for tests
}

// MarkProcessed marks an event as processed (no-op for tests)
func (m *MockChangeStreamWatcher) MarkProcessed(ctx context.Context, token []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markProcessedCount++

	return nil
}

// Close closes the change stream Watcher
func (m *MockChangeStreamWatcher) Close(ctx context.Context) error {
	close(m.EventsChan)
	return nil
}

// GetCallCounts returns the call counts for various methods (for testing purposes)
func (m *MockChangeStreamWatcher) GetCallCounts() (int, int, int, int) {
	// Return counts for: start, markProcessed, close, getUnprocessed
	// For simplicity, only tracking markProcessed
	m.mu.Lock()
	defer m.mu.Unlock()
	return 0, m.markProcessedCount, 0, 0
}

// MockHealthEventStore provides a mock implementation of datastore.HealthEventStore for testing
type MockHealthEventStore struct {
	UpdateHealthEventStatusFn func(ctx context.Context, id string, status datastore.HealthEventStatus) error
	updateCalled              int
}

// UpdateHealthEventStatus updates a health event status (mock implementation)
func (m *MockHealthEventStore) UpdateHealthEventStatus(ctx context.Context, id string, status datastore.HealthEventStatus) error {
	if m.UpdateHealthEventStatusFn != nil {
		return m.UpdateHealthEventStatusFn(ctx, id, status)
	}
	return nil
}

// Implement other required methods with no-op for testing
func (m *MockHealthEventStore) InsertHealthEvents(ctx context.Context, events *datastore.HealthEventWithStatus) error {
	return nil
}

func (m *MockHealthEventStore) UpdateHealthEventStatusByNode(ctx context.Context, nodeName string, status datastore.HealthEventStatus) error {
	return nil
}

func (m *MockHealthEventStore) FindHealthEventsByNode(ctx context.Context, nodeName string) ([]datastore.HealthEventWithStatus, error) {
	return nil, nil
}

func (m *MockHealthEventStore) FindHealthEventsByFilter(ctx context.Context, filter map[string]interface{}) ([]datastore.HealthEventWithStatus, error) {
	return nil, nil
}

func (m *MockHealthEventStore) FindHealthEventsByStatus(ctx context.Context, status datastore.Status) ([]datastore.HealthEventWithStatus, error) {
	return nil, nil
}

func (m *MockHealthEventStore) UpdateNodeQuarantineStatus(ctx context.Context, eventID string, status datastore.Status) error {
	return nil
}

func (m *MockHealthEventStore) UpdatePodEvictionStatus(ctx context.Context, eventID string, status datastore.OperationStatus) error {
	return nil
}

func (m *MockHealthEventStore) UpdateRemediationStatus(ctx context.Context, eventID string, status interface{}) error {
	return nil
}

func (m *MockHealthEventStore) CheckIfNodeAlreadyDrained(ctx context.Context, nodeName string) (bool, error) {
	return false, nil
}

func (m *MockHealthEventStore) FindLatestEventForNode(ctx context.Context, nodeName string) (*datastore.HealthEventWithStatus, error) {
	return nil, nil
}

func (m *MockHealthEventStore) FindHealthEventsByQuery(ctx context.Context, builder datastore.QueryBuilder) ([]datastore.HealthEventWithStatus, error) {
	return nil, nil
}

func (m *MockHealthEventStore) UpdateHealthEventsByQuery(ctx context.Context, queryBuilder datastore.QueryBuilder, updateBuilder datastore.UpdateBuilder) error {
	return nil
}

var (
	testClient     *kubernetes.Clientset
	testDynamic    dynamic.Interface
	testContext    context.Context
	testCancelFunc context.CancelFunc
	testEnv        *envtest.Environment
	testRestConfig *rest.Config
	mockWatcher    *MockChangeStreamWatcher
	mockStore      *MockHealthEventStore
	reconciler     FaultRemediationReconciler
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

	remediationClient, err := createTestRemediationClient(false)
	if err != nil {
		log.Fatalf("Failed to create remediation client: %v", err)
	}

	cfg := ReconcilerConfig{
		RemediationClient: remediationClient,
		StateManager:      statemanager.NewStateManager(testClient),
		UpdateMaxRetries:  3,
		UpdateRetryDelay:  100 * time.Millisecond,
	}

	// Create mock health event store
	mockStore = &MockHealthEventStore{
		updateCalled: 0,
	}
	mockStore.UpdateHealthEventStatusFn = func(ctx context.Context, id string, status datastore.HealthEventStatus) error {
		mockStore.updateCalled++
		return nil
	}

	// Create mock Watcher with event channel
	mockWatcher = NewMockChangeStreamWatcher()

	reconciler = NewFaultRemediationReconciler(nil, mockWatcher, mockStore, cfg, false)
	mgr, err := ctrl.NewManager(testEnv.Config, ctrl.Options{
		Scheme: scheme.Scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
	})
	if err != nil {
		panic(err)
	}
	err = reconciler.SetupWithManager(testContext, mgr)
	if err != nil {
		log.Fatalf("Failed to launch reconciler with mgr %v", err)
	}

	go func() {
		if err := mgr.Start(testContext); err != nil {
			log.Fatalf("Failed to start the test environment manager: %v", err)
		}
	}()

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
func createTestRemediationClient(dryRun bool) (*FaultRemediationClient, error) {

	// Create discovery client for RESTMapper
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(testRestConfig)
	if err != nil {
		return nil, err
	}

	cachedClient := memory.NewMemCacheClient(discoveryClient)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedClient)

	templatePath := filepath.Join("templates", "rebootnode-template.yaml")
	templateContent, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New("maintenance").Parse(string(templateContent))
	if err != nil {
		return nil, err
	}

	// Create remediation config with the test template
	remediationConfig := config.TomlConfig{
		RemediationActions: map[string]config.MaintenanceResource{
			"RESTART_BM": {
				ApiGroup:              "janitor.dgxc.nvidia.com",
				Version:               "v1alpha1",
				Kind:                  "RebootNode",
				TemplateFileName:      "test.yaml",
				CompleteConditionType: "NodeReady",
				EquivalenceGroup:      "restart",
			},
			"COMPONENT_RESET": {
				ApiGroup:              "janitor.dgxc.nvidia.com",
				Version:               "v1alpha1",
				Kind:                  "RebootNode",
				TemplateFileName:      "gpu-reset.yaml",
				CompleteConditionType: "NodeReady",
				EquivalenceGroup:      "restart",
			},
		},
	}

	// Create templates map
	templates := map[string]*template.Template{
		"RESTART_BM":      tmpl,
		"COMPONENT_RESET": tmpl, // Use same template for testing
	}

	client := &FaultRemediationClient{
		clientset:         testDynamic,
		kubeClient:        testClient,
		restMapper:        mapper,
		remediationConfig: remediationConfig,
		templates:         templates,
		templateMountPath: "/tmp",
		annotationManager: NewNodeAnnotationManager(testClient),
		statusChecker:     crstatus.NewCRStatusChecker(testDynamic, mapper, remediationConfig.RemediationActions, dryRun),
	}

	if dryRun {
		client.dryRunMode = []string{metav1.DryRunAll}
	} else {
		client.dryRunMode = []string{}
	}

	return client, nil
}

func TestCRBasedDeduplication_Integration(t *testing.T) {
	ctx := testContext

	nodeName := "test-node-dedup-" + "test-node-123"
	createTestNode(ctx, nodeName, nil, map[string]string{"test": "label"})
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	t.Run("FirstEvent_CreatesAnnotation", func(t *testing.T) {
		cleanupNodeAnnotations(ctx, t, nodeName)

		remediationClient, err := createTestRemediationClient(false)
		assert.NoError(t, err)

		cfg := ReconcilerConfig{
			RemediationClient: remediationClient,
			StateManager:      statemanager.NewStateManager(testClient),
			UpdateMaxRetries:  3,
			UpdateRetryDelay:  100 * time.Millisecond,
		}

		r := FaultRemediationReconciler{
			config:            cfg,
			annotationManager: cfg.RemediationClient.GetAnnotationManager(),
		}

		// Process Event 1
		healthEventDoc := &HealthEventDoc{
			ID: "test-event-id-1",
			HealthEventWithStatus: model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:          nodeName,
					RecommendedAction: protos.RecommendedAction_RESTART_BM,
				},
			},
		}

		success, crName, err := r.performRemediation(ctx, healthEventDoc)
		assert.NoError(t, err)
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

		remediationClient, err := createTestRemediationClient(false)
		assert.NoError(t, err)

		cfg := ReconcilerConfig{
			RemediationClient: remediationClient,
			StateManager:      statemanager.NewStateManager(testClient),
			UpdateMaxRetries:  3,
			UpdateRetryDelay:  100 * time.Millisecond,
		}
		r := FaultRemediationReconciler{
			config:            cfg,
			annotationManager: cfg.RemediationClient.GetAnnotationManager(),
		}

		// Event 1: Create first CR
		event1 := &HealthEventDoc{
			ID: "test-event-id-cr-1",
			HealthEventWithStatus: model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:          nodeName,
					RecommendedAction: protos.RecommendedAction_RESTART_BM,
				},
			},
		}
		_, firstCRName, err := r.performRemediation(ctx, event1)
		assert.NoError(t, err)

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

		remediationClient, err := createTestRemediationClient(false)
		assert.NoError(t, err)

		cfg := ReconcilerConfig{
			RemediationClient: remediationClient,
			StateManager:      statemanager.NewStateManager(testClient),
			UpdateMaxRetries:  3,
			UpdateRetryDelay:  100 * time.Millisecond,
		}
		r := FaultRemediationReconciler{
			config:            cfg,
			annotationManager: cfg.RemediationClient.GetAnnotationManager(),
		}

		// Event 1: Create first CR
		event1 := &HealthEventDoc{
			ID: "test-event-id-cr-1",
			HealthEventWithStatus: model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:          nodeName,
					RecommendedAction: protos.RecommendedAction_RESTART_BM,
				},
			},
		}
		_, firstCRName, err := r.performRemediation(ctx, event1)
		assert.NoError(t, err)

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
			ID: "test-event-id",
			HealthEventWithStatus: model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:          nodeName,
					RecommendedAction: protos.RecommendedAction_RESTART_BM,
				},
			},
		}

		_, secondCRName, err := r.performRemediation(ctx, event2)
		assert.NoError(t, err)

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

		remediationClient, err := createTestRemediationClient(false)
		assert.NoError(t, err)

		cfg := ReconcilerConfig{
			RemediationClient: remediationClient,
			StateManager:      statemanager.NewStateManager(testClient),
			UpdateMaxRetries:  3,
			UpdateRetryDelay:  100 * time.Millisecond,
		}
		r := FaultRemediationReconciler{
			config:            cfg,
			annotationManager: cfg.RemediationClient.GetAnnotationManager(),
		}

		// Event 1: COMPONENT_RESET
		event1 := &HealthEventDoc{
			ID: "test-event-id",
			HealthEventWithStatus: model.HealthEventWithStatus{
				HealthEvent: &protos.HealthEvent{
					NodeName:          nodeName,
					RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
				},
			},
		}
		_, firstCRName, err := r.performRemediation(ctx, event1)
		assert.NoError(t, err)

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

	nodeName := "test-node-sequence-" + "test-node-123"
	createTestNode(ctx, nodeName, nil, map[string]string{"test": "label"})
	defer func() {
		_ = testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	cleanupNodeAnnotations(ctx, t, nodeName)

	remediationClient, err := createTestRemediationClient(false)
	assert.NoError(t, err)

	cfg := ReconcilerConfig{
		RemediationClient: remediationClient,
		StateManager:      statemanager.NewStateManager(testClient),
		UpdateMaxRetries:  3,
		UpdateRetryDelay:  100 * time.Millisecond,
	}
	r := FaultRemediationReconciler{
		config:            cfg,
		annotationManager: cfg.RemediationClient.GetAnnotationManager(),
	}

	gvr := schema.GroupVersionResource{
		Group:    "janitor.dgxc.nvidia.com",
		Version:  "v1alpha1",
		Resource: "rebootnodes",
	}

	// Event 1: RESTART_VM creates CR-1
	event1 := &HealthEventDoc{
		ID: "test-event-id",
		HealthEventWithStatus: model.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName:          nodeName,
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
		},
	}
	success, crName1, err := r.performRemediation(ctx, event1)
	assert.NoError(t, err)
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

	// Event 3: CR succeeds - subsequent event should be created
	updateRebootNodeStatus(ctx, t, crName1, "Succeeded")

	event3 := &protos.HealthEvent{
		NodeName:          nodeName,
		RecommendedAction: protos.RecommendedAction_RESTART_VM,
	}
	shouldCreate, _, err = r.checkExistingCRStatus(ctx, event3)
	assert.NoError(t, err)
	assert.True(t, shouldCreate, "RESTART_VM should be skipped (CR succeeded)")

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
		ID: "test-event-id",
		HealthEventWithStatus: model.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName:          nodeName,
				RecommendedAction: protos.RecommendedAction_RESTART_BM,
			},
		},
	}
	_, crName2, err := r.performRemediation(ctx, event5)
	assert.NoError(t, err)

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

	nodeName := "test-node-full-e2e-" + "test-node-123"
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
		mockStore.updateCalled = 0

		beforeReceived := getCounterValue(t, totalEventsReceived)
		beforeDuration := getHistogramCount(t, eventHandlingDuration)

		// Event 1: Send quarantine event through channel
		eventID1 := "test-event-id-1"
		event1 := createQuarantineEvent(eventID1, nodeName, protos.RecommendedAction_RESTART_BM)
		eventToken1 := datastore.EventWithToken{
			Event:       map[string]interface{}(event1),
			ResumeToken: []byte("test-token-1"),
		}
		mockWatcher.EventsChan <- eventToken1

		// Wait for CR creation
		var crName string
		assert.Eventually(t, func() bool {
			state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
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
		state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
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
		updateCountBefore := mockStore.updateCalled

		eventID2 := "test-event-id-2"
		event2 := createQuarantineEvent(eventID2, nodeName, protos.RecommendedAction_COMPONENT_RESET)
		eventToken2 := datastore.EventWithToken{
			Event:       map[string]interface{}(event2),
			ResumeToken: []byte("test-token-2"),
		}
		mockWatcher.EventsChan <- eventToken2

		// Wait for event to be processed and verify deduplication
		assert.Eventually(t, func() bool {
			state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
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
		state, err = reconciler.annotationManager.GetRemediationState(ctx, nodeName)
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
		assert.Equal(t, updateCountBefore, mockStore.updateCalled, "MongoDB update should not be called for skipped event")

		// Event 3: Send unquarantine event
		unquarantineEvent := createUnquarantineEvent(nodeName)
		unquarantineEventToken := datastore.EventWithToken{
			Event:       map[string]interface{}(unquarantineEvent),
			ResumeToken: []byte("test-token-3"),
		}
		mockWatcher.EventsChan <- unquarantineEventToken

		// Wait for annotation cleanup
		assert.Eventually(t, func() bool {
			state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
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
			state, err = reconciler.annotationManager.GetRemediationState(ctx, nodeName)
			require.NoError(t, err)
			assert.NotContains(t, state.EquivalenceGroups, "restart", "Restart group should be removed from annotation")
		}

		// Verify MarkProcessed was called for processed events
		_, markedCount, _, _ := mockWatcher.GetCallCounts()
		assert.Greater(t, markedCount, 0, "MarkProcessed should be called for processed events")

		afterReceived := getCounterValue(t, totalEventsReceived)
		afterDuration := getHistogramCount(t, eventHandlingDuration)
		createdCount := getCounterVecValue(t, eventsProcessed, CRStatusCreated, nodeName)
		skippedCount := getCounterVecValue(t, eventsProcessed, CRStatusSkipped, nodeName)

		assert.GreaterOrEqual(t, afterReceived, beforeReceived+3, "totalEventsReceived should increment for all events")
		assert.GreaterOrEqual(t, createdCount, float64(1), "eventsProcessed with cr_status=created should increment for CR creation")
		assert.GreaterOrEqual(t, skippedCount, float64(1), "eventsProcessed with cr_status=skipped should increment for duplicate event")
		assert.GreaterOrEqual(t, afterDuration, beforeDuration+3, "eventHandlingDuration should record observations for all events")

		// Cleanup
		_ = testDynamic.Resource(gvr).Delete(ctx, crName, metav1.DeleteOptions{})
	})

	t.Run("UnsupportedAction_TrackedInMetrics", func(t *testing.T) {
		remediationClient, err := createTestRemediationClient(false)
		assert.NoError(t, err)

		cfg := ReconcilerConfig{
			RemediationClient: remediationClient,
			StateManager:      statemanager.NewStateManager(testClient),
			UpdateMaxRetries:  3,
			UpdateRetryDelay:  100 * time.Millisecond,
		}

		reconcilerInstance := FaultRemediationReconciler{
			config:            cfg,
			annotationManager: cfg.RemediationClient.GetAnnotationManager(),
		}

		beforeUnsupported := getCounterVecValue(t, totalUnsupportedRemediationActions, "UNKNOWN", nodeName)

		healthEvent := model.HealthEventWithStatus{
			HealthEvent: &protos.HealthEvent{
				NodeName:          nodeName,
				RecommendedAction: protos.RecommendedAction_UNKNOWN,
			},
		}

		shouldSkip := reconcilerInstance.shouldSkipEvent(ctx, healthEvent)
		assert.True(t, shouldSkip, "Should skip unsupported action")

		afterUnsupported := getCounterVecValue(t, totalUnsupportedRemediationActions, "UNKNOWN", nodeName)
		assert.Equal(t, beforeUnsupported+1, afterUnsupported, "totalUnsupportedRemediationActions should increment")
	})
}

// Helper to create quarantine event
func createQuarantineEvent(eventID string, nodeName string, action protos.RecommendedAction) datastore.Event {
	return datastore.Event{
		"operationType": "update",
		"fullDocument": map[string]interface{}{
			"_id": eventID,
			"healtheventstatus": map[string]interface{}{
				"userpodsevictionstatus": map[string]interface{}{
					"status": model.StatusSucceeded,
				},
				"nodequarantined": model.Quarantined,
			},
			"healthevent": map[string]interface{}{
				"nodename":          nodeName,
				"recommendedaction": int32(action),
			},
		},
	}
}

// Helper to create unquarantine event
func createUnquarantineEvent(nodeName string) datastore.Event {
	return datastore.Event{
		"operationType": "update",
		"fullDocument": map[string]interface{}{
			"_id": "test-doc-id",
			"healtheventstatus": map[string]interface{}{
				"nodequarantined": model.UnQuarantined,
				"userpodsevictionstatus": map[string]interface{}{
					"status": model.StatusSucceeded,
				},
			},
			"healthevent": map[string]interface{}{
				"nodename": nodeName,
			},
		},
	}
}

// Helper to create cancelled event
func createCancelledEvent(eventID string, nodeName string, action protos.RecommendedAction) datastore.Event {
	return datastore.Event{
		"operationType": "update",
		"fullDocument": map[string]interface{}{
			"_id": eventID,
			"healtheventstatus": map[string]interface{}{
				"nodequarantined": model.Cancelled,
			},
			"healthevent": map[string]interface{}{
				"nodename":          nodeName,
				"recommendedaction": int32(action),
			},
		},
	}
}

// TestReconciler_CancelledEventCleansAnnotation tests that a cancelled event removes the equivalence group from the annotation
func TestReconciler_CancelledEventCleansAnnotation(t *testing.T) {
	mockStore.updateCalled = 0
	ctx, cancel := context.WithTimeout(testContext, 30*time.Second)
	defer cancel()

	nodeName := testutils.GenerateTestNodeName("test-cancelled-clean")
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

	t.Log("Send Quarantined event to create CR and annotation")
	eventID1 := "test-event-id-1"
	event1 := createQuarantineEvent(eventID1, nodeName, protos.RecommendedAction_RESTART_BM)
	eventToken1 := datastore.EventWithToken{
		Event:       map[string]interface{}(event1),
		ResumeToken: []byte("test-token-1"),
	}
	mockWatcher.EventsChan <- eventToken1

	// Wait for CR creation and annotation
	var crName string
	require.Eventually(t, func() bool {
		state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
		if err != nil {
			return false
		}
		if grp, ok := state.EquivalenceGroups["restart"]; ok {
			crName = grp.MaintenanceCR
			return crName != ""
		}
		return false
	}, 5*time.Second, 100*time.Millisecond, "CR and annotation should be created")

	t.Log("Verify annotation contains restart group")
	state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	require.Contains(t, state.EquivalenceGroups, "restart", "Annotation should contain restart group")

	t.Log("Send Cancelled event to remove group from annotation")
	cancelledEvent := createCancelledEvent(eventID1, nodeName, protos.RecommendedAction_RESTART_BM)
	cancelledEventToken := datastore.EventWithToken{
		Event:       map[string]interface{}(cancelledEvent),
		ResumeToken: []byte("test-token-2"),
	}
	mockWatcher.EventsChan <- cancelledEventToken

	t.Log("Verify group removed from annotation")
	require.Eventually(t, func() bool {
		state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
		if err != nil {
			return false
		}
		_, hasRestart := state.EquivalenceGroups["restart"]
		return !hasRestart
	}, 5*time.Second, 100*time.Millisecond, "Group should be removed from annotation")

	t.Log("Verify CR still exists (not deleted by cancellation)")
	_, err = testDynamic.Resource(gvr).Get(ctx, crName, metav1.GetOptions{})
	require.NoError(t, err, "CR should still exist")

	// Cleanup
	_ = testDynamic.Resource(gvr).Delete(ctx, crName, metav1.DeleteOptions{})
}

// TestReconciler_CancelledEventClearsAllGroups tests that a cancelled event clears all equivalence groups
func TestReconciler_CancelledEventClearsAllGroups(t *testing.T) {
	mockStore.updateCalled = 0
	ctx, cancel := context.WithTimeout(testContext, 30*time.Second)
	defer cancel()

	nodeName := testutils.GenerateTestNodeName("test-cancelled-all")
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

	t.Log("Send multiple Quarantined events with different recommended actions")
	eventID1 := "test-event-id-1"
	event1 := createQuarantineEvent(eventID1, nodeName, protos.RecommendedAction_RESTART_BM)
	eventToken1 := datastore.EventWithToken{
		Event:       map[string]interface{}(event1),
		ResumeToken: []byte("test-token-1"),
	}
	mockWatcher.EventsChan <- eventToken1

	// Wait for first CR
	var crName1 string
	require.Eventually(t, func() bool {
		state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
		if err != nil {
			return false
		}
		if grp, ok := state.EquivalenceGroups["restart"]; ok {
			crName1 = grp.MaintenanceCR
			return crName1 != ""
		}
		return false
	}, 5*time.Second, 100*time.Millisecond, "First CR should be created")

	t.Log("Send second event with different action (same equivalence group)")
	eventID2 := "test-event-id-2"
	event2 := createQuarantineEvent(eventID2, nodeName, protos.RecommendedAction_COMPONENT_RESET)
	eventToken2 := datastore.EventWithToken{
		Event:       map[string]interface{}(event2),
		ResumeToken: []byte("test-token-2"),
	}
	mockWatcher.EventsChan <- eventToken2

	// Allow time for second event to be processed (should be deduplicated)
	time.Sleep(500 * time.Millisecond)

	t.Log("Verify annotation has restart group")
	state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	require.Contains(t, state.EquivalenceGroups, "restart", "Should have restart group")

	t.Log("Send Cancelled event")
	cancelledEvent := createCancelledEvent(eventID1, nodeName, protos.RecommendedAction_RESTART_BM)
	cancelledEventToken := datastore.EventWithToken{
		Event:       map[string]interface{}(cancelledEvent),
		ResumeToken: []byte("test-token-3"),
	}
	mockWatcher.EventsChan <- cancelledEventToken

	t.Log("Verify all groups cleared from annotation")
	require.Eventually(t, func() bool {
		state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
		if err != nil {
			return false
		}
		return len(state.EquivalenceGroups) == 0
	}, 5*time.Second, 100*time.Millisecond, "All groups should be cleared")

	// Cleanup
	_ = testDynamic.Resource(gvr).Delete(ctx, crName1, metav1.DeleteOptions{})
}

// TestReconciler_CancelledAndUnQuarantinedClearAllState tests that Cancelled followed by UnQuarantined clears all state
func TestReconciler_CancelledAndUnQuarantinedClearAllState(t *testing.T) {
	mockStore.updateCalled = 0
	ctx, cancel := context.WithTimeout(testContext, 30*time.Second)
	defer cancel()

	nodeName := testutils.GenerateTestNodeName("test-cancelled-unquarantine")
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

	t.Log("Send Quarantined event")
	eventID1 := "test-event-id-1"
	event1 := createQuarantineEvent(eventID1, nodeName, protos.RecommendedAction_RESTART_BM)
	eventToken1 := datastore.EventWithToken{
		Event:       map[string]interface{}(event1),
		ResumeToken: []byte("test-token-1"),
	}
	mockWatcher.EventsChan <- eventToken1

	var crName string
	require.Eventually(t, func() bool {
		state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
		if err != nil {
			return false
		}
		if grp, ok := state.EquivalenceGroups["restart"]; ok {
			crName = grp.MaintenanceCR
			return crName != ""
		}
		return false
	}, 5*time.Second, 100*time.Millisecond, "CR should be created")

	t.Log("Send Cancelled event")
	cancelledEvent := createCancelledEvent(eventID1, nodeName, protos.RecommendedAction_RESTART_BM)
	cancelledEventToken := datastore.EventWithToken{
		Event:       map[string]interface{}(cancelledEvent),
		ResumeToken: []byte("test-token-2"),
	}
	mockWatcher.EventsChan <- cancelledEventToken

	require.Eventually(t, func() bool {
		state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
		if err != nil {
			return false
		}
		return len(state.EquivalenceGroups) == 0
	}, 5*time.Second, 100*time.Millisecond, "Groups should be cleared after Cancelled")

	t.Log("Send UnQuarantined event")
	unquarantineEvent := createUnquarantineEvent(nodeName)
	unquarantineEventToken := datastore.EventWithToken{
		Event:       map[string]interface{}(unquarantineEvent),
		ResumeToken: []byte("test-token-3"),
	}
	mockWatcher.EventsChan <- unquarantineEventToken

	// Allow time for processing
	time.Sleep(500 * time.Millisecond)

	t.Log("Verify complete state cleanup")
	state, err := reconciler.annotationManager.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	require.Empty(t, state.EquivalenceGroups, "All state should be cleared")

	// Verify no annotation on node
	node, err := testClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	_, hasAnnotation := node.Annotations[AnnotationKey]
	require.False(t, hasAnnotation, "Annotation should be removed after complete cleanup")

	// Cleanup
	_ = testDynamic.Resource(gvr).Delete(ctx, crName, metav1.DeleteOptions{})
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
		conditions = append(conditions, map[string]interface{}{
			"type":               "NodeReady",
			"status":             "Unknown",
			"reason":             "InProgress",
			"message":            "Waiting for node to become ready",
			"lastTransitionTime": time.Now().Format(time.RFC3339),
		})
		cr.Object["status"] = map[string]interface{}{
			"conditions": conditions,
			"startTime":  time.Now().Add(-1 * time.Minute).Format(time.RFC3339),
		}
	case "Failed":
		conditions = append(conditions, map[string]interface{}{
			"type":               "SignalSent",
			"status":             "True",
			"reason":             "SignalSent",
			"message":            "Reboot signal sent",
			"lastTransitionTime": time.Now().Format(time.RFC3339),
		})
		conditions = append(conditions, map[string]interface{}{
			"type":               "NodeReady",
			"status":             "False",
			"reason":             "Failed",
			"message":            "Node failed to reach ready state",
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

// Metrics E2E Tests

// TestMetrics_ProcessingErrors tests error tracking
func TestMetrics_ProcessingErrors(t *testing.T) {
	beforeError := getCounterVecValue(t, processingErrors, "unmarshal_doc_error", "unknown")

	invalidEventToken := &datastore.EventWithToken{
		Event: map[string]interface{}{
			"fullDocument": "invalid-data",
		},
		ResumeToken: []byte("test-token"),
	}
	watcher := NewMockChangeStreamWatcher()

	r := &FaultRemediationReconciler{
		Watcher: watcher,
	}

	r.Reconcile(testContext, invalidEventToken)

	afterError := getCounterVecValue(t, processingErrors, "unmarshal_doc_error", "unknown")
	assert.Greater(t, afterError, beforeError, "processingErrors should increment for unmarshal error")
}

// Helper functions for reading Prometheus metrics

func getCounterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	err := counter.Write(metric)
	require.NoError(t, err)
	return metric.Counter.GetValue()
}

func getCounterVecValue(t *testing.T, counterVec *prometheus.CounterVec, labelValues ...string) float64 {
	t.Helper()
	counter, err := counterVec.GetMetricWithLabelValues(labelValues...)
	require.NoError(t, err)
	metric := &dto.Metric{}
	err = counter.Write(metric)
	require.NoError(t, err)
	return metric.Counter.GetValue()
}

func getHistogramCount(t *testing.T, histogram prometheus.Histogram) uint64 {
	t.Helper()
	metric := &dto.Metric{}
	err := histogram.Write(metric)
	require.NoError(t, err)
	return metric.Histogram.GetSampleCount()
}
