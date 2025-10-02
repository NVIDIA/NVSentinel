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
	"k8s.io/utils/ptr"
)

type MockNodeDrainerClient struct {
	getNamespacesMatchingPatternFn func(ctx context.Context, includePattern string, excludePattern string) ([]string, error)
	monitorPodCompletionFn         func(ctx context.Context, namespace string, nodename string) error
	evictAllPodsImmediatelyFn      func(ctx context.Context, namespace string, nodename string, timeout time.Duration) error
	checkIfAllPodsAreEvictedFn     func(ctx context.Context, namespaces []string, nodeName string, timeout time.Duration) bool
	updateNodeLabelFn              func(ctx context.Context, nodeName string, isDraining bool) error
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
	r := NewReconciler(cfg, false)
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
	r := NewReconciler(cfg, false)

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
	r := NewReconciler(cfg, false)
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

	r := NewReconciler(cfg, false)
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

	r := NewReconciler(cfg, false) // DryRun is false
	err := r.handleEvent(ctx, "node-for-invalid-mode", healthEvent)
	if err != nil {
		t.Errorf("Expected nil but found error %s", err)
	}
}
