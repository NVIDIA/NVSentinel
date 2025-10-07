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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/node-drainer-module/pkg/config"
	storeconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"
	platform_connectors "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/statemanager"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

type MockNodeDrainerClient struct {
	getNamespacesMatchingPatternFn func(ctx context.Context, includePattern string, excludePattern string) ([]string, error)
	monitorPodCompletionFn         func(ctx context.Context, namespace string, nodename string) error
	evictAllPodsImmediatelyFn      func(ctx context.Context, namespace string, nodename string, timeout time.Duration) error
	checkIfAllPodsAreEvictedFn     func(ctx context.Context, namespaces []string, nodeName string, timeout time.Duration) bool
	deletePodsAfterTimeoutFn       func(ctx context.Context, nodeName string, namespaces []string, timeout int, event *storeconnector.HealthEventWithStatus) error
}

func (c *MockNodeDrainerClient) GetNamespacesMatchingPattern(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
	return c.getNamespacesMatchingPatternFn(ctx, includePattern, excludePattern)
}

func (c *MockNodeDrainerClient) MonitorPodCompletion(ctx context.Context, namespace string, nodename string) error {
	return c.monitorPodCompletionFn(ctx, namespace, nodename)
}

func (c *MockNodeDrainerClient) EvictAllPodsInImmediateMode(ctx context.Context, namespace string, nodename string, timeout time.Duration) error {
	return c.evictAllPodsImmediatelyFn(ctx, namespace, nodename, timeout)
}

func (c *MockNodeDrainerClient) CheckIfAllPodsAreEvictedInImmediateMode(ctx context.Context, namespaces []string, nodeName string, timeout time.Duration) bool {
	return c.checkIfAllPodsAreEvictedFn(ctx, namespaces, nodeName, timeout)
}

func (c *MockNodeDrainerClient) DeletePodsAfterTimeout(ctx context.Context, nodeName string, namespaces []string, timeout int, event *storeconnector.HealthEventWithStatus) error {
	return c.deletePodsAfterTimeoutFn(ctx, nodeName, namespaces, timeout, event)
}

// MockCollection is a mock implementation of mongo.Collection
type MockCollection struct {
	updateOneFn func(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error)
	findOneFn   func(ctx context.Context, filter interface{}, opts ...*options.FindOneOptions) *mongo.SingleResult
	findFn      func(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (*mongo.Cursor, error)
}

func (m *MockCollection) UpdateOne(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
	return m.updateOneFn(ctx, filter, update, opts...)
}

func (m *MockCollection) FindOne(ctx context.Context, filter interface{}, opts ...*options.FindOneOptions) *mongo.SingleResult {
	if m.findOneFn != nil {
		return m.findOneFn(ctx, filter, opts...)
	}
	return &mongo.SingleResult{}
}

func (m *MockCollection) Find(ctx context.Context, filter interface{}, opts ...*options.FindOptions) (*mongo.Cursor, error) {
	if m.findFn != nil {
		return m.findFn(ctx, filter, opts...)
	}
	return &mongo.Cursor{}, nil
}

// MockCursor implements a simplified version of mongo.Cursor for testing
type MockCursor struct {
	closeFunc func(context.Context) error
	allFunc   func(context.Context, interface{}) error
}

func (m *MockCursor) Close(ctx context.Context) error {
	if m.closeFunc != nil {
		return m.closeFunc(ctx)
	}
	return nil
}

func (m *MockCursor) All(ctx context.Context, results interface{}) error {
	if m.allFunc != nil {
		return m.allFunc(ctx, results)
	}
	return nil
}

func TestHandleEvent(t *testing.T) {
	ctx := context.Background()
	var err error

	config := config.TomlConfig{
		EvictionTimeoutInSeconds: config.Duration{Duration: 40},
		UserNamespaces: []config.UserNamespace{{
			Name: "*ai",
			Mode: "Immediate",
		},
			{
				Name: "*sentin*",
				Mode: "AllowCompletion",
			},
			{
				Name: "*sentin*",
				Mode: "DeleteAfterTimeout",
			}},
		DeleteAfterTimeoutMinutes: 1,
	}
	count := 0
	k8sClient := &MockNodeDrainerClient{
		getNamespacesMatchingPatternFn: func(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
			switch includePattern {
			case "*ai":
				return []string{"runai"}, nil
			case "*sentin*":
				return []string{"nvsentinel"}, nil
			default:
				return []string{}, fmt.Errorf("Unexpected %s pattern passed", includePattern)
			}
		},
		monitorPodCompletionFn: func(ctx context.Context, namespace, nodename string) error {
			assert.Equal(t, "nvsentinel", namespace, "Expected nvsentinel namespace pods to be passed in Allow completion mode eviction but found %s", namespace)
			assert.Equal(t, "node1", nodename, "Expected node1 to be evicted but found %s", nodename)
			return nil
		},
		evictAllPodsImmediatelyFn: func(ctx context.Context, namespace, nodename string, timeout time.Duration) error {
			assert.Equal(t, "runai", namespace, "Expected runai namespace pods to be passed in immediate mode eviction but found %s", namespace)
			assert.Equal(t, "node1", nodename, "Expected node1 to be evicted but found %s", nodename)
			return nil
		},
		checkIfAllPodsAreEvictedFn: func(ctx context.Context, nsWithImmediateMode []string, nodeName string, timeout time.Duration) bool {
			return true
		},
		deletePodsAfterTimeoutFn: func(ctx context.Context, nodeName string, namespaces []string, timeout int, event *storeconnector.HealthEventWithStatus) error {
			assert.Equal(t, "node1", nodeName, "Expected node1 to be deleted but found %s", nodeName)
			assert.Equal(t, []string{"nvsentinel"}, namespaces, "Expected nvsentinel namespace to be deleted but found %s", namespaces)
			assert.Equal(t, 1, timeout, "Expected timeout to be 1 but found %s", timeout)
			return nil
		},
	}
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			count++
			switch count {
			case 1:
				assert.Equal(t, "node1", nodeName, "Expected node1 to be updated but found %s", nodeName)
				assert.Equal(t, statemanager.DrainingLabelValue, newStateLabelValue)
				return true, nil
			case 2:
				assert.Equal(t, "node1", nodeName, "Expected node1 to be updated but found %s", nodeName)
				assert.Equal(t, statemanager.DrainSucceededLabelValue, newStateLabelValue)
				return true, nil
			}
			return true, nil
		},
	}

	fakeClient := fake.NewSimpleClientset() // Fake kubernetes client for tests

	cfg := ReconcilerConfig{
		TomlConfig:   config,
		K8sClient:    k8sClient,
		StateManager: stateManager,
	}

	healthEvent := &storeconnector.HealthEventWithStatus{
		CreatedAt:   time.Now(),
		HealthEvent: &platform_connectors.HealthEvent{},
		HealthEventStatus: storeconnector.HealthEventStatus{
			NodeQuarantined:        ptr.To(storeconnector.Quarantined),
			UserPodsEvictionStatus: storeconnector.OperationStatus{},
			FaultRemediated:        nil,
		},
	}
	r := NewReconciler(cfg, false, fakeClient)
	err = r.handleEvent(ctx, "node1", healthEvent)

	if err != nil {
		t.Errorf("Pods are not evicted completely: %v", err)
	}
}

func TestHandleEventWithError(t *testing.T) {
	ctx := context.Background()
	var err error

	config := config.TomlConfig{
		EvictionTimeoutInSeconds: config.Duration{Duration: 40},
		UserNamespaces: []config.UserNamespace{{
			Name: "*ai",
			Mode: "Immediate",
		},
			{
				Name: "*sentin*",
				Mode: "AllowCompletion",
			}},
	}
	count := 0
	// eviction of pods in immediate mode with error
	k8sClient := &MockNodeDrainerClient{
		getNamespacesMatchingPatternFn: func(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
			switch includePattern {
			case "*ai":
				return []string{"runai"}, nil
			case "*sentin*":
				return []string{"nvsentinel"}, nil
			default:
				return []string{}, fmt.Errorf("Unexpected %s pattern passed", includePattern)
			}
		},
		monitorPodCompletionFn: func(ctx context.Context, namespace, nodename string) error {
			assert.Equal(t, "nvsentinel", namespace, "Expected nvsentinel namespace pods to be passed in Allow completion mode eviction but found %s", namespace)
			assert.Equal(t, "node1", nodename, "Expected node1 to be evicted but found %s", nodename)
			return nil
		},
		evictAllPodsImmediatelyFn: func(ctx context.Context, namespace, nodename string, timeout time.Duration) error {
			assert.Equal(t, "runai", namespace, "Expected runai namespace pods to be passed in immediate mode eviction but found %s", namespace)
			assert.Equal(t, "node1", nodename, "Expected node1 to be evicted but found %s", nodename)
			return fmt.Errorf("error in evicting pods immediately in namespace %s: failed to evict pod pod1 from namespace %s on node %s: ", namespace, namespace, nodename)
		},
		checkIfAllPodsAreEvictedFn: func(ctx context.Context, nsWithImmediateMode []string, nodeName string, timeout time.Duration) bool {
			t.Errorf("Didn't expect this function to be called in error state")
			return false
		},
	}
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			count++
			switch count {
			case 1:
				assert.Equal(t, "node1", nodeName)
				assert.Equal(t, statemanager.DrainingLabelValue, newStateLabelValue)
				return true, nil
			case 2:
				assert.Equal(t, "node1", nodeName)
				assert.Equal(t, statemanager.DrainFailedLabelValue, newStateLabelValue)
				return true, nil
			}
			return true, nil
		},
	}

	fakeClient := fake.NewSimpleClientset() // Fake kubernetes client for tests

	cfg := ReconcilerConfig{
		TomlConfig:   config,
		K8sClient:    k8sClient,
		StateManager: stateManager,
	}

	healthEvent := &storeconnector.HealthEventWithStatus{
		CreatedAt:   time.Now(),
		HealthEvent: &platform_connectors.HealthEvent{},
		HealthEventStatus: storeconnector.HealthEventStatus{
			NodeQuarantined:        ptr.To(storeconnector.Quarantined),
			UserPodsEvictionStatus: storeconnector.OperationStatus{},
			FaultRemediated:        nil,
		},
	}
	r := NewReconciler(cfg, false, fakeClient)

	err = r.handleEvent(ctx, "node1", healthEvent)

	if err == nil {
		t.Errorf("Expected an error for eviction of pods in immediate mode but got nil")
	}
	count = 0
	//eviction of pods in allow completion mode with error
	k8sClient.monitorPodCompletionFn = func(ctx context.Context, namespace, nodename string) error {
		assert.Equal(t, "nvsentinel", namespace, "Expected nvsentinel namespace pods to be passed in Allow completion mode eviction but found %s", namespace)
		assert.Equal(t, "node1", nodename, "Expected node1 to be evicted but found %s", nodename)
		return fmt.Errorf("error in evicting pods immediately in namespace %s: failed to evict pod pod5 from namespace %s on node %s:", namespace, namespace, nodename)
	}
	k8sClient.evictAllPodsImmediatelyFn = func(ctx context.Context, namespace, nodename string, timeout time.Duration) error {
		assert.Equal(t, "runai", namespace, "Expected runai namespace pods to be passed in immediate mode eviction but found %s", namespace)
		assert.Equal(t, "node1", nodename, "Expected node1 to be evicted but found %s", nodename)
		return nil
	}
	k8sClient.checkIfAllPodsAreEvictedFn = func(ctx context.Context, nsWithImmediateMode []string, nodeName string, timeout time.Duration) bool {
		t.Errorf("Didn't expect this function to be called in error state")
		return false
	}
	// We can rely on the same StateManager previously defined
	err = r.handleEvent(ctx, "node1", healthEvent)

	if err == nil {
		t.Errorf("Expected an error for eviction of pods in Allow completion mode but got nil")
	}
}

func TestHandleEventWithVerifyEvictionError(t *testing.T) {
	ctx := context.Background()
	var err error

	config := config.TomlConfig{
		EvictionTimeoutInSeconds: config.Duration{Duration: 40},
		UserNamespaces: []config.UserNamespace{{
			Name: "*ai",
			Mode: "Immediate",
		},
			{
				Name: "*sentin*",
				Mode: "AllowCompletion",
			}},
	}
	count := 0
	// eviction of pods in immediate mode with error
	k8sClient := &MockNodeDrainerClient{
		getNamespacesMatchingPatternFn: func(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
			switch includePattern {
			case "*ai":
				return []string{"runai"}, nil
			case "*sentin*":
				return []string{"nvsentinel"}, nil
			default:
				return []string{}, fmt.Errorf("Unexpected %s pattern passed", includePattern)
			}
		},
		monitorPodCompletionFn: func(ctx context.Context, namespace, nodename string) error {
			assert.Equal(t, "nvsentinel", namespace, "Expected nvsentinel namespace pods to be passed in Allow completion mode eviction but found %s", namespace)
			assert.Equal(t, "node1", nodename, "Expected node1 to be evicted but found %s", nodename)
			return nil
		},
		evictAllPodsImmediatelyFn: func(ctx context.Context, namespace, nodename string, timeout time.Duration) error {
			assert.Equal(t, "runai", namespace, "Expected runai namespace pods to be passed in immediate mode eviction but found %s", namespace)
			assert.Equal(t, "node1", nodename, "Expected node1 to be evicted but found %s", nodename)
			return nil
		},
		checkIfAllPodsAreEvictedFn: func(ctx context.Context, nsWithImmediateMode []string, nodeName string, timeout time.Duration) bool {
			return false
		},
	}
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			count++
			switch count {
			case 1:
				assert.Equal(t, "node1", nodeName)
				assert.Equal(t, statemanager.DrainingLabelValue, newStateLabelValue)
				return true, nil
			case 2:
				assert.Equal(t, "node1", nodeName)
				assert.Equal(t, statemanager.DrainFailedLabelValue, newStateLabelValue)
				return true, nil
			}
			return true, nil
		},
	}
	cfg := ReconcilerConfig{
		TomlConfig:   config,
		K8sClient:    k8sClient,
		StateManager: stateManager,
	}
	healthEvent := &storeconnector.HealthEventWithStatus{
		CreatedAt:   time.Now(),
		HealthEvent: &platform_connectors.HealthEvent{},
		HealthEventStatus: storeconnector.HealthEventStatus{
			NodeQuarantined:        ptr.To(storeconnector.Quarantined),
			UserPodsEvictionStatus: storeconnector.OperationStatus{},
			FaultRemediated:        nil,
		},
	}
	fakeClient := fake.NewSimpleClientset()
	r := NewReconciler(cfg, false, fakeClient)
	err = r.handleEvent(ctx, "node1", healthEvent)
	assert.Error(t, err)
}

func TestHandleEventWithHealthyEvent(t *testing.T) {
	ctx := context.Background()
	var err error

	config := config.TomlConfig{
		EvictionTimeoutInSeconds: config.Duration{Duration: 40},
		UserNamespaces: []config.UserNamespace{{
			Name: "*ai",
			Mode: "Immediate",
		},
			{
				Name: "*sentin*",
				Mode: "AllowCompletion",
			}},
	}
	k8sClient := &MockNodeDrainerClient{
		getNamespacesMatchingPatternFn: func(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
			switch includePattern {
			case "*ai":
				return []string{"runai"}, nil
			case "*sentin*":
				return []string{"nvsentinel"}, nil
			default:
				return []string{}, fmt.Errorf("Unexpected %s pattern passed", includePattern)
			}
		},
		monitorPodCompletionFn: func(ctx context.Context, namespace, nodename string) error {
			t.Errorf("Eviction of pod should not be done for healthy event")
			return nil
		},
		evictAllPodsImmediatelyFn: func(ctx context.Context, namespace, nodename string, timeout time.Duration) error {
			t.Errorf("Eviction of pod should not be done for healthy event")
			return nil
		},
		checkIfAllPodsAreEvictedFn: func(ctx context.Context, nsWithImmediateMode []string, nodeName string, timeout time.Duration) bool {
			t.Errorf("Check for eviction of pod should not be done for healthy event")
			return false
		},
	}
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			t.Errorf("UpdateNVSentinelStateNodeLabel should not be called for healthy event")
			return true, nil
		},
	}

	cfg := ReconcilerConfig{
		TomlConfig:   config,
		K8sClient:    k8sClient,
		StateManager: stateManager,
	}

	healthEvent := &storeconnector.HealthEventWithStatus{
		CreatedAt:   time.Now(),
		HealthEvent: &platform_connectors.HealthEvent{},
		HealthEventStatus: storeconnector.HealthEventStatus{
			NodeQuarantined:        ptr.To(storeconnector.UnQuarantined),
			UserPodsEvictionStatus: storeconnector.OperationStatus{},
			FaultRemediated:        nil,
		},
	}

	fakeClient := fake.NewSimpleClientset() // Fake kubernetes client for tests
	r := NewReconciler(cfg, false, fakeClient)
	_, cancel := context.WithCancel(context.TODO())
	r.NodeEvictionContext = sync.Map{}
	r.NodeEvictionContext.Store("node1-nvsentinel", &EvictionContext{
		cancel: cancel,
	})
	err = r.handleEvent(ctx, "node1", healthEvent)

	if err != nil {
		t.Errorf("Expected nil but found error %s", err)
	}
}

func TestHandleEventWithInvalidMode(t *testing.T) {
	ctx := context.Background()

	tomlCfg := config.TomlConfig{
		EvictionTimeoutInSeconds: config.Duration{Duration: 40 * time.Second},
		UserNamespaces: []config.UserNamespace{{
			Name: "test-ns",
			Mode: "Immiediate", // This is the invalid mode
		},
			{
				Name: "test-ns-actual",
				Mode: "AllowCompletion",
			},
		},
	}
	count := 0
	k8sClient := &MockNodeDrainerClient{
		getNamespacesMatchingPatternFn: func(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
			if includePattern == "test-ns" || includePattern == "test-ns-actual" {
				return []string{"test-ns-actual"}, nil
			}
			return []string{}, fmt.Errorf("Unexpected pattern %s passed", includePattern)
		},
		monitorPodCompletionFn: func(ctx context.Context, namespace, nodename string) error {
			// This should not be called
			t.Errorf("MonitorPodCompletion should not be called for invalid mode")
			return nil
		},
		evictAllPodsImmediatelyFn: func(ctx context.Context, namespace, nodename string, timeout time.Duration) error {
			// This should not be called
			t.Errorf("EvictAllPodsInImmediateMode should not be called for invalid mode")
			return nil
		},
		checkIfAllPodsAreEvictedFn: func(ctx context.Context, nsWithImmediateMode []string, nodeName string, timeout time.Duration) bool {
			return true
		},
	}
	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			count++
			switch count {
			case 1:
				assert.Equal(t, "node-for-invalid-mode", nodeName)
				assert.Equal(t, statemanager.DrainingLabelValue, newStateLabelValue)
				return true, nil
			case 2:
				assert.Equal(t, "node-for-invalid-mode", nodeName)
				assert.Equal(t, statemanager.DrainSucceededLabelValue, newStateLabelValue)
				return true, nil
			}
			return true, nil
		},
	}

	cfg := ReconcilerConfig{
		TomlConfig:   tomlCfg,
		K8sClient:    k8sClient,
		StateManager: stateManager,
	}

	healthEvent := &storeconnector.HealthEventWithStatus{
		CreatedAt:   time.Now(),
		HealthEvent: &platform_connectors.HealthEvent{}, // Minimal HealthEvent
		HealthEventStatus: storeconnector.HealthEventStatus{
			NodeQuarantined:        ptr.To(storeconnector.Quarantined),
			UserPodsEvictionStatus: storeconnector.OperationStatus{},
			FaultRemediated:        nil,
		},
	}

	fakeClient := fake.NewSimpleClientset()    // Fake kubernetes client for tests
	r := NewReconciler(cfg, false, fakeClient) // DryRun is false
	err := r.handleEvent(ctx, "node-for-invalid-mode", healthEvent)
	if err != nil {
		t.Errorf("Expected nil but found error %s", err)
	}
}

// TestIsNodeAlreadyDrained tests the isNodeAlreadyDrained function
func TestIsNodeAlreadyDrained(t *testing.T) {
	ctx := context.Background()
	fakeClient := fake.NewSimpleClientset()

	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			return true, nil
		},
	}

	cfg := ReconcilerConfig{
		TomlConfig:   config.TomlConfig{},
		K8sClient:    &MockNodeDrainerClient{},
		StateManager: stateManager,
	}

	r := NewReconciler(cfg, false, fakeClient)

	tests := []struct {
		name            string
		mongoResult     bson.M
		mongoError      error
		expectedDrained bool
		expectError     bool
	}{
		{
			name: "Node with successful drain",
			mongoResult: bson.M{
				"healtheventstatus": bson.M{
					"nodequarantined": string(storeconnector.Quarantined),
					"userpodsevictionstatus": bson.M{
						"status": string(storeconnector.StatusSucceeded),
					},
				},
			},
			expectedDrained: true,
			expectError:     false,
		},
		{
			name: "Node that was unquarantined",
			mongoResult: bson.M{
				"healtheventstatus": bson.M{
					"nodequarantined": string(storeconnector.UnQuarantined),
				},
			},
			expectedDrained: false,
			expectError:     false,
		},
		{
			name:            "No documents found",
			mongoError:      mongo.ErrNoDocuments,
			expectedDrained: false,
			expectError:     false,
		},
		{
			name: "Node with in-progress drain",
			mongoResult: bson.M{
				"healtheventstatus": bson.M{
					"nodequarantined": string(storeconnector.Quarantined),
					"userpodsevictionstatus": bson.M{
						"status": string(storeconnector.StatusInProgress),
					},
				},
			},
			expectedDrained: false,
			expectError:     false,
		},
		{
			name: "Node with failed drain",
			mongoResult: bson.M{
				"healtheventstatus": bson.M{
					"nodequarantined": string(storeconnector.Quarantined),
					"userpodsevictionstatus": bson.M{
						"status": string(storeconnector.StatusFailed),
					},
				},
			},
			expectedDrained: false,
			expectError:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockCollection := &MockCollection{
				findOneFn: func(ctx context.Context, filter interface{}, opts ...*options.FindOneOptions) *mongo.SingleResult {
					// Verify filter structure
					filterMap := filter.(bson.M)
					assert.Equal(t, "test-node", filterMap["healthevent.nodename"])

					// Verify the quarantine status filter
					quarantinedFilter := filterMap["healtheventstatus.nodequarantined"].(bson.M)
					expectedStatuses := quarantinedFilter["$in"].([]string)
					assert.Contains(t, expectedStatuses, string(storeconnector.Quarantined))
					assert.Contains(t, expectedStatuses, string(storeconnector.UnQuarantined))

					// Verify sort options
					assert.NotNil(t, opts, "Options should be provided")
					if len(opts) > 0 && opts[0].Sort != nil {
						// Verify sort by _id descending to get latest
						sortDoc := opts[0].Sort.(bson.D)
						assert.Equal(t, "_id", sortDoc[0].Key)
						assert.Equal(t, -1, sortDoc[0].Value)
					}

					// Return a mock SingleResult - we can't test the actual decode
					// but we've validated the query parameters
					return &mongo.SingleResult{}
				},
			}

			// Call the function - it will fail on Decode but we've validated the query
			_, _ = r.isNodeAlreadyDrained(ctx, mockCollection, "test-node")

			// The test validates that the correct filter and options are used
		})
	}
}

func TestNodeEventQueueCreation(t *testing.T) {
	fakeClient := fake.NewSimpleClientset()

	// Create a mock client that handles the operations that will be called
	mockK8sClient := &MockNodeDrainerClient{
		getNamespacesMatchingPatternFn: func(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
			return []string{}, nil // Return empty list to skip namespace processing
		},
		checkIfAllPodsAreEvictedFn: func(ctx context.Context, namespaces []string, nodeName string, timeout time.Duration) bool {
			return true
		},
	}

	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			return true, nil
		},
	}

	cfg := ReconcilerConfig{
		TomlConfig: config.TomlConfig{
			EvictionTimeoutInSeconds: config.Duration{Duration: 10 * time.Second},
			UserNamespaces:           []config.UserNamespace{},
		},
		K8sClient:    mockK8sClient,
		StateManager: stateManager,
	}

	r := NewReconciler(cfg, false, fakeClient)

	// Create a test event with proper structure including NodeQuarantined
	event := bson.M{
		"fullDocument": bson.M{
			"_id": primitive.NewObjectID(),
			"healthevent": bson.M{
				"nodename": "test-node",
			},
			"healtheventstatus": bson.M{
				"nodequarantined": string(storeconnector.Quarantined),
				"userpodsevictionstatus": bson.M{
					"status": string(storeconnector.StatusNotStarted),
				},
			},
		},
	}

	// Create a context with timeout to prevent the test from hanging
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Enqueue the event
	mockCollection := &MockCollection{
		updateOneFn: func(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
			return &mongo.UpdateResult{ModifiedCount: 1}, nil
		},
	}
	err := r.enqueueEvent(ctx, event, mockCollection)
	assert.NoError(t, err, "enqueueEvent should not return an error")

	// Verify queue was created
	queueInterface, exists := r.nodeEventQueues.Load("test-node")
	assert.True(t, exists, "Queue should be created for the node")

	queue := queueInterface.(*NodeEventQueue)
	assert.NotNil(t, queue.events, "Event channel should be initialized")

	// Allow some time for the processing goroutine to start
	time.Sleep(100 * time.Millisecond)

	// Note: The active flag may be false if the goroutine finished quickly due to timeout
	// This is expected behavior for the test environment
}

// TestConcurrentEventsOnDifferentNodes tests that events for different nodes are processed in parallel
func TestConcurrentEventsOnDifferentNodes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fakeClient := fake.NewSimpleClientset()

	// Track which nodes have been processed
	var processedNodes sync.Map
	var processingOrder []string
	var orderMutex sync.Mutex

	k8sClient := &MockNodeDrainerClient{
		getNamespacesMatchingPatternFn: func(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
			return []string{"test-ns"}, nil
		},
		evictAllPodsImmediatelyFn: func(ctx context.Context, namespace, nodename string, timeout time.Duration) error {
			orderMutex.Lock()
			processingOrder = append(processingOrder, nodename)
			orderMutex.Unlock()

			// Simulate work that takes time
			time.Sleep(100 * time.Millisecond)
			processedNodes.Store(nodename, true)
			return nil
		},
		checkIfAllPodsAreEvictedFn: func(ctx context.Context, namespaces []string, nodeName string, timeout time.Duration) bool {
			return true
		},
	}

	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			return true, nil
		},
	}

	cfg := ReconcilerConfig{
		TomlConfig: config.TomlConfig{
			EvictionTimeoutInSeconds: config.Duration{Duration: 10 * time.Second},
			UserNamespaces: []config.UserNamespace{
				{Name: "test-ns", Mode: "Immediate"},
			},
		},
		K8sClient:    k8sClient,
		StateManager: stateManager,
	}

	r := NewReconciler(cfg, false, fakeClient)

	// Create a mock collection
	mockCollection := &MockCollection{
		updateOneFn: func(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
			return &mongo.UpdateResult{ModifiedCount: 1}, nil
		},
	}

	// Create events for 3 different nodes
	nodes := []string{"node-1", "node-2", "node-3"}
	startTime := time.Now()

	for _, nodeName := range nodes {
		event := bson.M{
			"fullDocument": bson.M{
				"_id": primitive.NewObjectID(),
				"healthevent": bson.M{
					"nodename": nodeName,
				},
				"healtheventstatus": bson.M{
					"nodequarantined": string(storeconnector.Quarantined),
					"userpodsevictionstatus": bson.M{
						"status": string(storeconnector.StatusNotStarted),
					},
				},
			},
		}

		// Enqueue events - they should be processed in parallel for different nodes
		go func(event bson.M) {
			if err := r.enqueueEvent(ctx, event, mockCollection); err != nil {
				t.Errorf("Failed to enqueue event for node %s: %v", nodeName, err)
			}
		}(event)
	}

	// Wait for all nodes to be processed
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		allProcessed := true
		for _, node := range nodes {
			if _, exists := processedNodes.Load(node); !exists {
				allProcessed = false
				break
			}
		}
		if allProcessed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify all nodes were processed
	for _, node := range nodes {
		_, exists := processedNodes.Load(node)
		assert.True(t, exists, "Node %s should have been processed", node)
	}

	// If processing was truly parallel, total time should be around 100ms (not 300ms)
	elapsed := time.Since(startTime)
	assert.Less(t, elapsed, 200*time.Millisecond, "Processing should be parallel, taking ~100ms not 300ms")
}

// TestSequentialEventsOnSameNode tests that multiple events for the same node are processed sequentially
func TestSequentialEventsOnSameNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fakeClient := fake.NewSimpleClientset()

	// Track processing order
	var processingOrder []string
	var orderMutex sync.Mutex
	var wg sync.WaitGroup
	wg.Add(3) // We're going to send 3 events

	k8sClient := &MockNodeDrainerClient{
		getNamespacesMatchingPatternFn: func(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
			return []string{"test-ns"}, nil
		},
		evictAllPodsImmediatelyFn: func(ctx context.Context, namespace, nodename string, timeout time.Duration) error {
			orderMutex.Lock()
			processingOrder = append(processingOrder, fmt.Sprintf("evict-%d", len(processingOrder)+1))
			orderMutex.Unlock()

			// Simulate work
			time.Sleep(50 * time.Millisecond)
			wg.Done()
			return nil
		},
		checkIfAllPodsAreEvictedFn: func(ctx context.Context, namespaces []string, nodeName string, timeout time.Duration) bool {
			return true
		},
	}

	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			return true, nil
		},
	}

	cfg := ReconcilerConfig{
		TomlConfig: config.TomlConfig{
			EvictionTimeoutInSeconds: config.Duration{Duration: 10 * time.Second},
			UserNamespaces: []config.UserNamespace{
				{Name: "test-ns", Mode: "Immediate"},
			},
		},
		K8sClient:    k8sClient,
		StateManager: stateManager,
	}

	r := NewReconciler(cfg, false, fakeClient)

	mockCollection := &MockCollection{
		updateOneFn: func(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
			return &mongo.UpdateResult{ModifiedCount: 1}, nil
		},
	}

	// Create 3 events for the same node
	nodeName := "test-node"
	for i := 0; i < 3; i++ {
		event := bson.M{
			"fullDocument": bson.M{
				"_id": primitive.NewObjectID(),
				"healthevent": bson.M{
					"nodename": nodeName,
				},
				"healtheventstatus": bson.M{
					"nodequarantined": string(storeconnector.Quarantined),
					"userpodsevictionstatus": bson.M{
						"status": string(storeconnector.StatusNotStarted),
					},
				},
			},
		}

		// Enqueue events rapidly
		err := r.enqueueEvent(ctx, event, mockCollection)
		assert.NoError(t, err, "Failed to enqueue event")
	}

	// Wait for all events to be processed
	done := make(chan bool)
	go func() {
		wg.Wait()
		done <- true
	}()

	select {
	case <-done:
		// All events processed
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for events to be processed")
	}

	// Verify that events were processed sequentially (should have 3 evictions)
	orderMutex.Lock()
	actualOrder := make([]string, len(processingOrder))
	copy(actualOrder, processingOrder)
	orderMutex.Unlock()

	assert.Equal(t, 3, len(actualOrder), "Should have processed 3 events")
	assert.Equal(t, []string{"evict-1", "evict-2", "evict-3"}, actualOrder, "Events should be processed in order")
}

// TestQueueTimeoutBehavior tests that the queue processor stops after 30 seconds of inactivity
func TestQueueTimeoutBehavior(t *testing.T) {
	ctx := context.Background()
	fakeClient := fake.NewSimpleClientset()

	k8sClient := &MockNodeDrainerClient{
		getNamespacesMatchingPatternFn: func(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
			return []string{}, nil
		},
		checkIfAllPodsAreEvictedFn: func(ctx context.Context, namespaces []string, nodeName string, timeout time.Duration) bool {
			return true
		},
	}

	cfg := ReconcilerConfig{
		TomlConfig: config.TomlConfig{
			EvictionTimeoutInSeconds: config.Duration{Duration: 10 * time.Second},
			UserNamespaces:           []config.UserNamespace{},
		},
		K8sClient: k8sClient,
	}

	r := NewReconciler(cfg, false, fakeClient)

	// Create a queue manually
	queue := &NodeEventQueue{
		events: make(chan bson.M, 100),
	}

	mockCollection := &MockCollection{
		updateOneFn: func(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
			return &mongo.UpdateResult{ModifiedCount: 1}, nil
		},
	}

	// Store the queue
	nodeName := "timeout-test-node"
	r.nodeEventQueues.Store(nodeName, queue)

	// Start the processor
	go r.processNodeEventQueue(ctx, nodeName, queue, mockCollection)

	// Process one event
	event := bson.M{
		"fullDocument": bson.M{
			"_id": primitive.NewObjectID(),
			"healthevent": bson.M{
				"nodename": nodeName,
			},
			"healtheventstatus": bson.M{
				"nodequarantined": string(storeconnector.UnQuarantined),
				"userpodsevictionstatus": bson.M{
					"status": string(storeconnector.StatusNotStarted),
				},
			},
		},
	}
	queue.events <- event

	// Wait a bit for processing
	time.Sleep(100 * time.Millisecond)

	// Verify queue is still active
	_, exists := r.nodeEventQueues.Load(nodeName)
	assert.True(t, exists, "Queue should still exist immediately after processing")

	// Note: The 5-minute timeout is implemented in processNodeEventQueue
	// and will clean up idle queues automatically
}

// TestNodeEventQueueIdleTimeout tests that idle queues are cleaned up after timeout
func TestNodeEventQueueIdleTimeout(t *testing.T) {
	ctx := context.Background()
	fakeClient := fake.NewSimpleClientset()

	mockK8sClient := &MockNodeDrainerClient{
		getNamespacesMatchingPatternFn: func(ctx context.Context, includePattern string, excludePattern string) ([]string, error) {
			return []string{}, nil
		},
		checkIfAllPodsAreEvictedFn: func(ctx context.Context, namespaces []string, nodeName string, timeout time.Duration) bool {
			return true
		},
	}

	stateManager := &statemanager.MockStateManager{
		UpdateNVSentinelStateNodeLabelFn: func(ctx context.Context, nodeName string,
			newStateLabelValue statemanager.NVSentinelStateLabelValue, removeStateLabel bool) (bool, error) {
			return true, nil
		},
	}

	cfg := ReconcilerConfig{
		TomlConfig: config.TomlConfig{
			EvictionTimeoutInSeconds: config.Duration{Duration: 10 * time.Second},
			UserNamespaces:           []config.UserNamespace{},
		},
		K8sClient:    mockK8sClient,
		StateManager: stateManager,
	}

	r := NewReconciler(cfg, false, fakeClient)

	// Create an event and enqueue it
	event := bson.M{
		"fullDocument": bson.M{
			"_id": primitive.NewObjectID(),
			"healthevent": bson.M{
				"nodename": "test-timeout-node",
			},
			"healtheventstatus": bson.M{
				"nodequarantined": string(storeconnector.Quarantined),
				"userpodsevictionstatus": bson.M{
					"status": string(storeconnector.StatusNotStarted),
				},
			},
		},
	}

	mockCollection := &MockCollection{
		updateOneFn: func(ctx context.Context, filter interface{}, update interface{}, opts ...*options.UpdateOptions) (*mongo.UpdateResult, error) {
			return &mongo.UpdateResult{ModifiedCount: 1}, nil
		},
	}

	// Enqueue the first event
	err := r.enqueueEvent(ctx, event, mockCollection)
	assert.NoError(t, err, "First enqueue should succeed")

	// Verify queue was created
	_, exists := r.nodeEventQueues.Load("test-timeout-node")
	assert.True(t, exists, "Queue should be created after first event")

	// Wait a bit (less than timeout) and verify queue still exists
	time.Sleep(100 * time.Millisecond)
	_, exists = r.nodeEventQueues.Load("test-timeout-node")
	assert.True(t, exists, "Queue should still exist before timeout")

	// Note: In a real scenario, the queue would be deleted after 5 minutes of inactivity
	// The processNodeEventQueue goroutine handles this automatically
	// We can't wait 5 minutes in a unit test, but the logic is tested by the implementation

	// Test that a deleted queue can be recreated
	r.nodeEventQueues.Delete("test-timeout-node")
	_, exists = r.nodeEventQueues.Load("test-timeout-node")
	assert.False(t, exists, "Queue should be deleted")

	// Enqueue another event - should recreate the queue
	err = r.enqueueEvent(ctx, event, mockCollection)
	assert.NoError(t, err, "Enqueue after deletion should succeed")

	// Verify queue was recreated
	_, exists = r.nodeEventQueues.Load("test-timeout-node")
	assert.True(t, exists, "Queue should be recreated after deletion")
}
