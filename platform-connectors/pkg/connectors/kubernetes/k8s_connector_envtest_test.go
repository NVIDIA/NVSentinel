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

package kubernetes

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

var defaultConnectorConfig = K8sConnectorConfig{
	MaxNodeConditionMessageLength: 1024,
	CompactedHealthEventMsgLen:    72,
}

// go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
// source <(setup-envtest use -p env)
func setupEnvtest(t *testing.T) (*envtest.Environment, *kubernetes.Clientset) {
	t.Helper()

	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err, "failed to setup envtest")

	cli, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err, "failed to create a client")

	return testEnv, cli
}

func TestK8sConnector_WithEnvtest_NodeConditionUpdate(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{
		Name:   "test-node",
		Labels: map[string]string{},
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvents := &protos.HealthEvents{
		Version: 1,
		Events: []*protos.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "XID 48 detected",
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
				NodeName:           "test-node",
			},
		},
	}

	err = k8sConn.processHealthEvents(ctx, healthEvents)
	require.NoError(t, err, "failed to process health events")

	updatedNode, err := cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
	require.NoError(t, err, "failed to get node")

	conditionFound := false
	for _, condition := range updatedNode.Status.Conditions {
		if condition.Type == "GpuXidError" {
			conditionFound = true
			assert.Equal(t, corev1.ConditionTrue, condition.Status)
			assert.Contains(t, condition.Message, "ErrorCode:48")
			assert.Contains(t, condition.Message, "GPU:0")
			assert.Equal(t, "GpuXidErrorIsNotHealthy", condition.Reason)
			break
		}
	}
	assert.True(t, conditionFound, "node condition was not updated")
}

func TestK8sConnector_WithEnvtest_NodeConditionClear(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{
		Name:   "test-node",
		Labels: map[string]string{},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               "GpuXidError",
					Status:             corev1.ConditionTrue,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "GpuXidErrorIsNotHealthy",
					Message:            "ErrorCode:48 GPU:0 Previous error",
				},
			},
		},
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	node.Status.Conditions = []corev1.NodeCondition{
		{
			Type:               "GpuXidError",
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Reason:             "GpuXidErrorIsNotHealthy",
			Message:            "ErrorCode:48 GPU:0 Previous error",
		},
	}
	_, err = cli.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err, "failed to update node status")

	stopCh := make(chan struct{})
	defer close(stopCh)

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvents := &protos.HealthEvents{
		Version: 1,
		Events: []*protos.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          true,
				Message:            "No errors",
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_NONE,
				NodeName:           "test-node",
			},
		},
	}

	err = k8sConn.processHealthEvents(ctx, healthEvents)
	require.NoError(t, err, "failed to process health events")

	updatedNode, err := cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
	require.NoError(t, err, "failed to get node")

	conditionFound := false
	for _, condition := range updatedNode.Status.Conditions {
		if condition.Type == "GpuXidError" {
			conditionFound = true
			assert.Equal(t, corev1.ConditionFalse, condition.Status)
			assert.Equal(t, "No Health Failures", condition.Message)
			assert.Equal(t, "GpuXidErrorIsHealthy", condition.Reason)
			break
		}
	}
	assert.True(t, conditionFound, "node condition was not cleared")
}

func TestK8sConnector_WithEnvtest_NodeEventCreation(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	// Create a test node
	node := &corev1.Node{
		Name:   "test-node",
		Labels: map[string]string{},
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvents := &protos.HealthEvents{
		Version: 1,
		Events: []*protos.HealthEvent{
			{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				Message:            "GPU temperature warning",
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_UNKNOWN,
				NodeName:           "test-node",
			},
		},
	}

	err = k8sConn.processHealthEvents(ctx, healthEvents)
	require.NoError(t, err, "failed to process health events")

	events, err := cli.CoreV1().Events("").List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Node,involvedObject.name=test-node",
	})
	require.NoError(t, err, "failed to list events")

	eventFound := false
	for _, event := range events.Items {
		if event.Type == "GpuThermalWatch" {
			eventFound = true
			assert.Contains(t, event.Message, "ErrorCode:DCGM_FR_CLOCK_THROTTLE_THERMAL")
			assert.Contains(t, event.Message, "GPU:0")
			assert.Equal(t, "GpuThermalWatchIsNotHealthy", event.Reason)
			break
		}
	}
	assert.True(t, eventFound, "kubernetes event was not created")
}

// TestK8sConnector_WithEnvtest_AddMessages tests adding messages to an existing condition
func TestK8sConnector_WithEnvtest_AddMessages(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{
		Name: "test-node",
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               "GpuXidError",
					Status:             corev1.ConditionTrue,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Message:            "GPU:0 error;",
					Reason:             "GpuXidErrorDetected",
				},
			},
		},
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)
	connector := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvents := []*protos.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
			ErrorCode:          []string{"48"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			NodeName:           "test-node",
		},
	}

	_, err = connector.updateNodeConditions(ctx, healthEvents)
	require.NoError(t, err)

	node, err = cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
	require.NoError(t, err)

	conditionFound := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == "GpuXidError" {
			conditionFound = true
			assert.Contains(t, condition.Message, "GPU:0")
			assert.Contains(t, condition.Message, "GPU:1")
			break
		}
	}
	assert.True(t, conditionFound, "node condition message was not updated with both GPUs")
}

// TestK8sConnector_WithEnvtest_RemoveMessages tests removing specific messages from a condition
func TestK8sConnector_WithEnvtest_RemoveMessages(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{
		Name: "test-node",
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               "GpuXidError",
					Status:             corev1.ConditionTrue,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Message:            "GPU:0 error;GPU:1 error;",
					Reason:             "GpuXidErrorDetected",
				},
			},
		},
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)
	connector := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvents := []*protos.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          true,
			EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			GeneratedTimestamp: timestamppb.New(time.Now()),
			NodeName:           "test-node",
		},
	}

	_, err = connector.updateNodeConditions(ctx, healthEvents)
	require.NoError(t, err)

	node, err = cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
	require.NoError(t, err)

	conditionFound := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == "GpuXidError" {
			conditionFound = true
			assert.NotContains(t, condition.Message, "GPU:0")
			assert.Contains(t, condition.Message, "GPU:1")
			break
		}
	}
	assert.True(t, conditionFound, "node condition message was not updated correctly")
}

// TestK8sConnector_WithEnvtest_MultipleEventsForSameNode tests processing multiple events for the same node
func TestK8sConnector_WithEnvtest_MultipleEventsForSameNode(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{
		Name: "test-node",
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)
	connector := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEventsProto := &protos.HealthEvents{
		Events: []*protos.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				NodeName:           "test-node",
			},
			{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
				ErrorCode:          []string{"THERMAL_WARNING"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				NodeName:           "test-node",
			},
		},
	}

	err = connector.processHealthEvents(ctx, healthEventsProto)
	require.NoError(t, err)

	node, err = cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
	require.NoError(t, err)

	conditionFound := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == "GpuXidError" {
			conditionFound = true
			assert.Equal(t, corev1.ConditionTrue, condition.Status)
			break
		}
	}
	assert.True(t, conditionFound, "fatal health event did not create node condition")

	events, err := cli.CoreV1().Events(DefaultNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=test-node",
	})
	require.NoError(t, err)

	eventFound := false
	for _, event := range events.Items {
		if event.Type == "GpuThermalWatch" {
			eventFound = true
			break
		}
	}
	assert.True(t, eventFound, "non-fatal health event did not create Kubernetes event")
}

// TestK8sConnector_WithEnvtest_TransitionTimeUpdates tests that LastTransitionTime is updated when status changes
func TestK8sConnector_WithEnvtest_TransitionTimeUpdates(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	initialTime := time.Now().Add(-1 * time.Hour)
	node := &corev1.Node{
		Name: "test-node",
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               "GpuXidError",
					Status:             corev1.ConditionFalse,
					LastHeartbeatTime:  metav1.NewTime(initialTime),
					LastTransitionTime: metav1.NewTime(initialTime),
					Message:            NoHealthFailureMsg,
					Reason:             "GpuXidErrorResolved",
				},
			},
		},
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)
	connector := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvents := []*protos.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			ErrorCode:          []string{"48"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			NodeName:           "test-node",
		},
	}

	_, err = connector.updateNodeConditions(ctx, healthEvents)
	require.NoError(t, err)

	node, err = cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
	require.NoError(t, err)

	conditionFound := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == "GpuXidError" {
			conditionFound = true
			assert.Equal(t, corev1.ConditionTrue, condition.Status)
			assert.True(t, condition.LastTransitionTime.Time.After(initialTime.Add(30*time.Minute)))
			break
		}
	}
	assert.True(t, conditionFound, "LastTransitionTime was not updated on status change")
}

// TestK8sConnector_WithEnvtest_EventDedupeCacheRecovery tests the cases where the
// connector's memory of a node event does not resolve to a live event: the remembered
// event was deleted (or TTL-expired) out from under the connector, so once the refresh
// interval has passed a fresh one is created; and the connector restarted with an
// empty memory, where the event's name, derived from the fault, lets it find and
// refresh the existing event instead of writing a second one.
func TestK8sConnector_WithEnvtest_EventDedupeCacheRecovery(t *testing.T) {
	tests := []struct {
		name                string
		deleteExistingEvent bool
		restartConnector    bool
		expectedEvents      int
		expectedCount       int32
		description         string
	}{
		{
			name:                "cached event deleted before second write",
			deleteExistingEvent: true,
			restartConnector:    false,
			expectedEvents:      1,
			expectedCount:       1,
			description:         "stale memory should fall back to creating a fresh event",
		},
		{
			name:                "connector restarted with empty cache",
			deleteExistingEvent: false,
			restartConnector:    true,
			expectedEvents:      1,
			expectedCount:       2,
			description:         "restarted connector should refresh the existing event, not write a second one",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			testEnv, cli := setupEnvtest(t)

			defer testEnv.Stop()

			nodeName := "test-node"
			_, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{
				Name: nodeName,
			}, metav1.CreateOptions{})
			require.NoError(t, err, "failed to create node")

			stopCh := make(chan struct{})
			defer close(stopCh)

			connector := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

			healthEventsProto := &protos.HealthEvents{
				Events: []*protos.HealthEvent{
					{
						CheckName:          "GpuThermalWatch",
						IsHealthy:          false,
						EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
						ErrorCode:          []string{"THERMAL_WARNING"},
						IsFatal:            false,
						GeneratedTimestamp: timestamppb.New(time.Now()),
						NodeName:           nodeName,
					},
				},
			}

			listNodeEvents := func() []corev1.Event {
				t.Helper()

				events, err := cli.CoreV1().Events(DefaultNamespace).List(ctx, metav1.ListOptions{
					FieldSelector: fmt.Sprintf("involvedObject.name=%s", nodeName),
				})
				require.NoError(t, err)

				var nodeEvents []corev1.Event

				for _, event := range events.Items {
					if event.Type == "GpuThermalWatch" {
						nodeEvents = append(nodeEvents, event)
					}
				}

				return nodeEvents
			}

			require.NoError(t, connector.processHealthEvents(ctx, healthEventsProto))

			events := listNodeEvents()
			require.Len(t, events, 1, "first health event did not create exactly one Kubernetes event")

			if tt.deleteExistingEvent {
				require.NoError(t,
					cli.CoreV1().Events(DefaultNamespace).Delete(ctx, events[0].Name, metav1.DeleteOptions{}))
				// Inside the refresh interval a repeat writes nothing; past it the
				// memory is used for a refresh, which finds the Event gone.
				ageRememberedEvent(t, connector, nodeName, connector.createK8sEvent(ctx, healthEventsProto.Events[0]))
			}

			// A restarted connector starts with an empty dedupe cache.
			if tt.restartConnector {
				connector = NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)
			}

			// This is a later report of the fault, rather than a replay of the
			// already persisted occurrence after a restart.
			previousTime := healthEventsProto.Events[0].GeneratedTimestamp.AsTime()
			healthEventsProto.Events[0].GeneratedTimestamp = timestamppb.New(previousTime.Add(time.Minute))
			require.NoError(t, connector.processHealthEvents(ctx, healthEventsProto))

			events = listNodeEvents()
			require.Len(t, events, tt.expectedEvents, tt.description)

			for _, event := range events {
				assert.Equal(t, tt.expectedCount, event.Count, tt.description)
			}
		})
	}
}

// TestK8sConnector_WithEnvtest_NodeNotFound tests handling of non-existent nodes
func TestK8sConnector_WithEnvtest_NodeNotFound(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	stopCh := make(chan struct{})
	defer close(stopCh)

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvents := &protos.HealthEvents{
		Version: 1,
		Events: []*protos.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "XID 48 detected",
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
				NodeName:           "non-existent-node",
			},
		},
	}

	err := k8sConn.processHealthEvents(ctx, healthEvents)
	if err != nil {
		assert.Contains(t, err.Error(), "not found")
	}
}

// TestK8sConnector_WithEnvtest_EmptyHealthEvents tests handling of empty health events
func TestK8sConnector_WithEnvtest_EmptyHealthEvents(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{
		Name:   "test-node",
		Labels: map[string]string{},
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvents := &protos.HealthEvents{
		Version: 1,
		Events:  []*protos.HealthEvent{},
	}

	err = k8sConn.processHealthEvents(ctx, healthEvents)
	require.NoError(t, err, "should handle empty events list")
}

// TestK8sConnector_WithEnvtest_MultipleEntities tests health events with multiple impacted entities
func TestK8sConnector_WithEnvtest_MultipleEntities(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{
		Name:   "test-node",
		Labels: map[string]string{},
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvents := &protos.HealthEvents{
		Version: 1,
		Events: []*protos.HealthEvent{
			{
				CheckName: "GpuXidError",
				IsHealthy: false,
				Message:   "Multiple GPUs affected",
				EntitiesImpacted: []*protos.Entity{
					{EntityType: "GPU", EntityValue: "0"},
					{EntityType: "GPU", EntityValue: "1"},
					{EntityType: "GPU", EntityValue: "2"},
					{EntityType: "GPU", EntityValue: "3"},
				},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
				NodeName:           "test-node",
			},
		},
	}

	err = k8sConn.processHealthEvents(ctx, healthEvents)
	require.NoError(t, err, "failed to process health events")

	updatedNode, err := cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
	require.NoError(t, err, "failed to get node")

	conditionFound := false
	for _, condition := range updatedNode.Status.Conditions {
		if condition.Type == "GpuXidError" {
			conditionFound = true
			assert.Equal(t, corev1.ConditionTrue, condition.Status)
			assert.Contains(t, condition.Message, "GPU:0")
			assert.Contains(t, condition.Message, "GPU:1")
			assert.Contains(t, condition.Message, "GPU:2")
			assert.Contains(t, condition.Message, "GPU:3")
			break
		}
	}
	assert.True(t, conditionFound, "node condition was not created")
}

// TestK8sConnector_WithEnvtest_SpecialCharactersInMessage tests handling of special characters
func TestK8sConnector_WithEnvtest_SpecialCharactersInMessage(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{
		Name:   "test-node",
		Labels: map[string]string{},
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvents := &protos.HealthEvents{
		Version: 1,
		Events: []*protos.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "Error: GPU failed with status <critical> @10:30AM",
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
				NodeName:           "test-node",
			},
		},
	}

	err = k8sConn.processHealthEvents(ctx, healthEvents)
	require.NoError(t, err, "failed to process health events with special characters")

	updatedNode, err := cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
	require.NoError(t, err, "failed to get node")

	conditionFound := false
	for _, condition := range updatedNode.Status.Conditions {
		if condition.Type == "GpuXidError" {
			conditionFound = true
			assert.Equal(t, corev1.ConditionTrue, condition.Status)
			assert.Contains(t, condition.Message, "Error: GPU failed with status <critical> @10:30AM")
			break
		}
	}
	assert.True(t, conditionFound, "node condition with special characters was not created")
}

// TestK8sConnector_WithEnvtest_SharedEntityIsNotTheSameFault: two SXID faults
// on the same NVSwitch (same error code, same switch PCI address) that hit
// different GPUs and links are different faults, so both stay in the
// condition; a repeat of either with other diagnostic text still changes
// nothing.
func TestK8sConnector_WithEnvtest_SharedEntityIsNotTheSameFault(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	_, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "sxid-node"}}, metav1.CreateOptions{})
	require.NoError(t, err)

	stopCh := make(chan struct{})
	defer close(stopCh)
	connector := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	sxid := func(gpu, link int, text string) []*protos.HealthEvent {
		return []*protos.HealthEvent{{
			CheckName: "SysLogsSXIDError",
			IsHealthy: false,
			Message:   text,
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "NVSWITCH", EntityValue: "0"},
				{EntityType: "PCI", EntityValue: "0000:c4:00.0"},
				{EntityType: "NVLINK", EntityValue: fmt.Sprintf("%d", link)},
				{EntityType: "GPU", EntityValue: fmt.Sprintf("%d", gpu)},
			},
			ErrorCode:          []string{"12028"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
			NodeName:           "sxid-node",
		}}
	}
	message := func() string {
		t.Helper()

		node, err := cli.CoreV1().Nodes().Get(ctx, "sxid-node", metav1.GetOptions{})
		require.NoError(t, err)

		for _, c := range node.Status.Conditions {
			if c.Type == "SysLogsSXIDError" {
				return c.Message
			}
		}

		return ""
	}

	_, err = connector.updateNodeConditions(ctx, sxid(0, 1, "SXid (PCI:0000:c4:00.0): 12028, link 1, GPU 0"))
	require.NoError(t, err)
	_, err = connector.updateNodeConditions(ctx, sxid(3, 5, "SXid (PCI:0000:c4:00.0): 12028, link 5, GPU 3"))
	require.NoError(t, err)

	msg := message()
	require.Contains(t, msg, "NVLINK:1 GPU:0")
	require.Contains(t, msg, "NVLINK:5 GPU:3", "a fault on another GPU and link is its own entry, even on the same switch")

	written, err := connector.updateNodeConditions(ctx, sxid(3, 5, "SXid (PCI:0000:c4:00.0): 12028, link 5, GPU 3, again"))
	require.NoError(t, err)
	require.False(t, written, "a repeat with other diagnostic text is still the same fault")
	require.Equal(t, msg, message())
}

// TestK8sConnector_WithEnvtest_CompactionAndDeduplication verifies the full real-world flow:
// health events are appended one by one to the same node condition. A repeat of a fault
// the node already shows (same entity + same Recommended Action, different diagnostic
// text) changes nothing and costs no write. Once the accumulated message exceeds 1024
// bytes, compaction fires and the result must contain no two entries with identical
// compacted text.
func TestK8sConnector_WithEnvtest_CompactionAndDeduplication(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{Name: "test-node"}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)
	connector := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	// Send 5 health events for 5 distinct GPUs. Each message is ~197 bytes;
	// 5 messages total ~990 bytes < 1024 — no compaction should fire yet.
	// The timestamp is embedded in the diagnostic text and falls within the
	// 72-byte compacted prefix, so it distinguishes entries after compaction.
	for i := range 5 {
		events := []*protos.HealthEvent{
			{
				CheckName: "GpuXidError",
				IsHealthy: false,
				Message: fmt.Sprintf(
					"kernel: [16450076.00000%d] NVRM: Xid (PCI:0000:0%d:00.0): 119, pid=10000%d, name=proc, Timeout after 6s waiting for GPU GSP response",
					i+1, i, i+1),
				EntitiesImpacted: []*protos.Entity{
					{EntityType: "GPU", EntityValue: fmt.Sprintf("%d", i)},
					{EntityType: "PCI", EntityValue: fmt.Sprintf("0000:0%d:00.0", i)},
				},
				ErrorCode:          []string{"119"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				RecommendedAction:  protos.RecommendedAction_COMPONENT_RESET,
				NodeName:           "test-node",
			},
		}
		_, err = connector.updateNodeConditions(ctx, events)
		require.NoError(t, err, "failed to process event for GPU:%d", i)
	}

	// Verify: 5 entries, no compaction yet.
	node, err = cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
	require.NoError(t, err)
	var condMsg string
	for _, c := range node.Status.Conditions {
		if c.Type == "GpuXidError" {
			condMsg = c.Message
			break
		}
	}
	require.NotEmpty(t, condMsg, "GpuXidError condition not found after 5 events")
	assert.Less(t, len(condMsg), 1024, "5 messages should not yet exceed 1024 bytes")
	assert.NotContains(t, condMsg, truncationSuffix, "compaction must not have fired yet")

	readMessage := func() string {
		t.Helper()

		node, err := cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
		require.NoError(t, err)

		for _, c := range node.Status.Conditions {
			if c.Type == "GpuXidError" {
				return c.Message
			}
		}

		return ""
	}
	xidEventFor := func(gpu int, message string) []*protos.HealthEvent {
		return []*protos.HealthEvent{{
			CheckName: "GpuXidError",
			IsHealthy: false,
			Message:   message,
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: fmt.Sprintf("%d", gpu)},
				{EntityType: "PCI", EntityValue: fmt.Sprintf("0000:0%d:00.0", gpu)},
			},
			ErrorCode:          []string{"119"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			RecommendedAction:  protos.RecommendedAction_COMPONENT_RESET,
			NodeName:           "test-node",
		}}
	}

	// A repeat of GPU:0's fault (same GPU/PCI entities and Recommended Action,
	// another timestamp in the diagnostic text) names a fault the node already
	// shows, so it changes nothing: the message keeps its text and the node is
	// not written.
	versionBefore := nodeVersion(t, cli, "test-node")
	_, err = connector.updateNodeConditions(ctx, xidEventFor(0,
		"kernel: [16450077.000001] NVRM: Xid (PCI:0000:00:00.0): 119, pid=9999999, name=proc, Timeout after 6s waiting for GPU GSP response"))
	require.NoError(t, err, "failed to process the repeat for GPU:0")
	assert.Equal(t, condMsg, readMessage(), "a repeat of a fault the node already shows leaves the message as it was")
	assert.Equal(t, versionBefore, nodeVersion(t, cli, "test-node"), "and costs no status write")

	// A sixth GPU pushes the total to ~1188 bytes > 1024, triggering Tier 1
	// compaction: all six entries are compacted to their 72-byte prefixes.
	_, err = connector.updateNodeConditions(ctx, xidEventFor(5,
		"kernel: [16450076.000006] NVRM: Xid (PCI:0000:05:00.0): 119, pid=100006, name=proc, Timeout after 6s waiting for GPU GSP response"))
	require.NoError(t, err, "failed to process event for GPU:5")

	condMsg = readMessage()
	require.NotEmpty(t, condMsg, "GpuXidError condition not found after the sixth event")

	// 1. Condition message must be within the 1024-byte Kubernetes limit.
	assert.LessOrEqual(t, len(condMsg), 1024,
		"condition message must not exceed 1024 bytes after compaction")

	// 2. Entries must be in compacted form (free-text truncated at 72 bytes).
	assert.Contains(t, condMsg, truncationSuffix,
		"entries must be in compacted form after limit was exceeded")

	// 3. No two compacted entries in the condition message may have identical text.
	//    Each entry originates from a different entity and carries a distinct
	//    72-byte prefix.
	parts := strings.Split(condMsg, ";")
	var entries []string
	for _, p := range parts {
		if p != "" && p != truncationSuffix {
			entries = append(entries, p)
		}
	}
	seen := make(map[string]int)
	for _, e := range entries {
		seen[e]++
	}
	for entry, count := range seen {
		assert.Equal(t, 1, count,
			"compacted entry appears %d times (expected 1): %q", count, entry)
	}

	// 4. GPU:0 must appear exactly once, with its original text: the repeat's
	//    fresher text did not replace it, and compaction did not duplicate it.
	gpu0Count := 0
	for _, e := range entries {
		if strings.Contains(e, "GPU:0") && strings.Contains(e, "PCI:0000:00:00.0") {
			gpu0Count++
		}
	}
	assert.Equal(t, 1, gpu0Count, "GPU:0 must appear exactly once after compaction")
	assert.Contains(t, condMsg, "16450076.000001",
		"the original GPU:0 entry (timestamp 16450076.000001) survives in the compacted prefix")
	assert.NotContains(t, condMsg, "16450077.000001",
		"the repeat's text (timestamp 16450077.000001) never entered the message")

	// 5. All other GPU entries (1 to 5) must be present after compaction.
	for i := 1; i < 6; i++ {
		assert.Contains(t, condMsg, fmt.Sprintf("GPU:%d", i),
			"GPU:%d entry must be present after compaction", i)
	}
}

// TestK8sConnector_WithEnvtest_MultipleCheckTypes tests multiple different check types on same node
func TestK8sConnector_WithEnvtest_MultipleCheckTypes(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{
		Name:   "test-node",
		Labels: map[string]string{},
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvents := &protos.HealthEvents{
		Version: 1,
		Events: []*protos.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "XID error",
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
				NodeName:           "test-node",
			},
			{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				Message:            "Temperature warning",
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_UNKNOWN,
				NodeName:           "test-node",
			},
			{
				CheckName:          "InfinibandLinkFlapping",
				IsHealthy:          false,
				Message:            "Link flapping detected",
				EntitiesImpacted:   []*protos.Entity{{EntityType: "IB", EntityValue: "mlx5_0"}},
				ErrorCode:          []string{},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "network",
				RecommendedAction:  protos.RecommendedAction_RESTART_BM,
				NodeName:           "test-node",
			},
		},
	}

	err = k8sConn.processHealthEvents(ctx, healthEvents)
	require.NoError(t, err, "failed to process multiple health events")

	updatedNode, err := cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
	require.NoError(t, err, "failed to get node")

	conditionsFound := map[string]bool{
		"GpuXidError":            false,
		"InfinibandLinkFlapping": false,
	}

	for _, condition := range updatedNode.Status.Conditions {
		if _, exists := conditionsFound[string(condition.Type)]; exists {
			conditionsFound[string(condition.Type)] = true
			assert.Equal(t, corev1.ConditionTrue, condition.Status)
		}
	}

	for condType, found := range conditionsFound {
		assert.True(t, found, "condition %s was not created", condType)
	}

	events, err := cli.CoreV1().Events("").List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Node,involvedObject.name=test-node",
	})
	require.NoError(t, err, "failed to list events")

	eventFound := false
	for _, event := range events.Items {
		if event.Type == "GpuThermalWatch" {
			eventFound = true
			break
		}
	}
	assert.True(t, eventFound, "non-fatal event was not created")
}

// These tests run against the package's envtest API server (see TestMain), so
// the evidence is what the API server recorded, not what a fake counted: a
// node status write moves the node's resourceVersion and a skipped one does
// not; a Kubernetes Event write shows up as an Event object, and a refresh
// bumps its count.

var nonNodeNameChars = regexp.MustCompile(`[^a-zA-Z0-9.-]+`)

// envtestConnector returns a connector over the shared API server and a node
// of this test's own; the node and its Events are removed when the test ends.
func envtestConnector(t *testing.T, cfg ...K8sConnectorConfig) (*K8sConnector, *kubernetes.Clientset, string) {
	t.Helper()

	cli := envtestClient(t)
	ctx := context.Background()
	// Node names are DNS subdomains: lower case letters, digits, "-" and ".".
	nodeName := "node-" + strings.ToLower(nonNodeNameChars.ReplaceAllString(t.Name(), "-"))

	_, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = cli.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
		_ = cli.CoreV1().Events(DefaultNamespace).DeleteCollection(ctx, metav1.DeleteOptions{},
			metav1.ListOptions{FieldSelector: "involvedObject.name=" + nodeName})
	})

	config := K8sConnectorConfig{
		MaxNodeConditionMessageLength: 1024,
		CompactedHealthEventMsgLen:    72,
	}
	if len(cfg) > 0 {
		config = cfg[0]
	}

	return NewK8sConnector(cli, nil, nil, ctx, config), cli, nodeName
}

// nodeVersion is the node's resourceVersion: it moves on every status write
// the API server accepted and stays put when the connector skipped the write.
func nodeVersion(t *testing.T, cli *kubernetes.Clientset, nodeName string) string {
	t.Helper()

	node, err := cli.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	require.NoError(t, err)

	return node.ResourceVersion
}

// nodeEvents lists the Events written for the node, oldest name first.
func nodeEvents(t *testing.T, cli *kubernetes.Clientset, nodeName string) []corev1.Event {
	t.Helper()

	list, err := cli.CoreV1().Events(DefaultNamespace).List(context.Background(),
		metav1.ListOptions{FieldSelector: "involvedObject.name=" + nodeName})
	require.NoError(t, err)

	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })

	return list.Items
}

// eventCount returns the count of the one Event carrying the message.
func eventCount(t *testing.T, cli *kubernetes.Clientset, nodeName, message string) int32 {
	t.Helper()

	for _, event := range nodeEvents(t, cli, nodeName) {
		if event.Message == message {
			return event.Count
		}
	}

	require.Failf(t, "Event not found", "no Event on %s with message %q", nodeName, message)

	return 0
}

func xidEvent(nodeName string, at time.Time, healthy bool) *protos.HealthEvent {
	return &protos.HealthEvent{
		CheckName:          "GpuXidError",
		IsHealthy:          healthy,
		IsFatal:            !healthy,
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		ErrorCode:          []string{"79"},
		GeneratedTimestamp: timestamppb.New(at),
		ComponentClass:     "GPU",
		RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
		Message:            "XID 79 on GPU 0",
		NodeName:           nodeName,
	}
}

func batch(events ...*protos.HealthEvent) *protos.HealthEvents {
	return &protos.HealthEvents{Version: 1, Events: events}
}

// TestUpdateOnChange_SkipsRepeats: the first fault is a transition and
// updates the node; the same fault again (a repeat, or a resent batch) changes
// nothing the node shows and must not cost a status write; a recovery is a
// transition again.
func TestUpdateOnChange_SkipsRepeats(t *testing.T) {
	connector, cli, node := envtestConnector(t)
	ctx := context.Background()
	now := time.Now()
	created := nodeVersion(t, cli, node)

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now, false))))
	afterFault := nodeVersion(t, cli, node)
	require.NotEqual(t, created, afterFault, "the first fault is a transition")

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now, false))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now.Add(time.Minute), false))))
	require.Equal(t, afterFault, nodeVersion(t, cli, node), "a repeat of the same fault changes nothing and is skipped")

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now.Add(2*time.Minute), true))))
	afterRecovery := nodeVersion(t, cli, node)
	require.NotEqual(t, afterFault, afterRecovery, "the recovery is a transition")

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now.Add(3*time.Minute), true))))
	require.Equal(t, afterRecovery, nodeVersion(t, cli, node), "healthy again is a repeat")
}

// TestUpdateOnChange_SaturatedMessageIsStillARepeat: when a node's
// condition message would exceed its length cap the stored entries are
// compacted, so their text never equals a repeat's full text again; the repeat
// must still count as no change, or exactly the busiest nodes would pay a
// status write on every repeat.
func TestUpdateOnChange_SaturatedMessageIsStillARepeat(t *testing.T) {
	connector, cli, node := envtestConnector(t, K8sConnectorConfig{
		// Tight enough that six full messages do not fit and are compacted,
		// wide enough that the six compacted ones do.
		MaxNodeConditionMessageLength: 700,
		CompactedHealthEventMsgLen:    40,
	})
	ctx := context.Background()
	now := time.Now()

	faults := make([]*protos.HealthEvent, 0, 6)
	for gpu := range 6 {
		fault := xidEvent(node, now.Add(time.Duration(gpu)*time.Second), false)
		fault.EntitiesImpacted = []*protos.Entity{{EntityType: "GPU", EntityValue: fmt.Sprint(gpu)}}
		fault.Message = fmt.Sprintf("XID 79 on GPU %d: %s", gpu, strings.Repeat("diagnostic detail ", 8))
		faults = append(faults, fault)
	}

	require.NoError(t, connector.ProcessBatch(ctx, batch(faults...)))
	afterFirst := nodeVersion(t, cli, node)

	// The same faults again, as one batch and one by one: nothing changed.
	require.NoError(t, connector.ProcessBatch(ctx, batch(faults...)))

	for _, fault := range faults {
		require.NoError(t, connector.ProcessBatch(ctx, batch(fault)))
	}

	require.Equal(t, afterFirst, nodeVersion(t, cli, node), "repeats of compacted faults are no change")
}

// TestUpdateOnChange_NewFaultJoiningIsAChange: a second fault on the same
// check adds a message, which the node does not show yet.
func TestUpdateOnChange_NewFaultJoiningIsAChange(t *testing.T) {
	connector, cli, node := envtestConnector(t)
	ctx := context.Background()
	now := time.Now()

	require.NoError(t, connector.ProcessBatch(ctx, batch(xidEvent(node, now, false))))
	afterFirst := nodeVersion(t, cli, node)

	second := xidEvent(node, now.Add(time.Second), false)
	second.EntitiesImpacted = []*protos.Entity{{EntityType: "GPU", EntityValue: "1"}}

	require.NoError(t, connector.ProcessBatch(ctx, batch(second)))
	afterSecond := nodeVersion(t, cli, node)
	require.NotEqual(t, afterFirst, afterSecond, "a new fault joining an existing one changes the message")

	require.NoError(t, connector.ProcessBatch(ctx, batch(second)))
	require.Equal(t, afterSecond, nodeVersion(t, cli, node))
}

// thermalEvent is a non-fatal fault, the kind that is announced as a
// Kubernetes Event rather than a node condition.
func thermalEvent(nodeName string, at time.Time, healthy bool, gpu string) *protos.HealthEvent {
	return &protos.HealthEvent{
		CheckName:          "GpuThermalWatch",
		IsHealthy:          healthy,
		IsFatal:            false,
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: gpu}},
		ErrorCode:          []string{"THERMAL_WARNING"},
		GeneratedTimestamp: timestamppb.New(at),
		ComponentClass:     "GPU",
		RecommendedAction:  protos.RecommendedAction_NONE,
		Message:            "GPU " + gpu + " is hot",
		NodeName:           nodeName,
	}
}

// TestUpdateOnChange_EventsWrittenOnChange: a fault's first report
// creates its Event; repeats write nothing; a fault on another GPU is a
// change; after the check recovers, the same fault is announced again by
// refreshing the Event that still exists in the cluster.
func TestUpdateOnChange_EventsWrittenOnChange(t *testing.T) {
	connector, cli, node := envtestConnector(t)
	ctx := context.Background()
	now := time.Now()
	hot0 := connector.fetchHealthEventMessage(thermalEvent(node, now, false, "0"))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 1, "the first report of a fault creates its Event")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(time.Minute), false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 1, "a repeat, or a resent batch, writes nothing")
	require.Equal(t, int32(1), eventCount(t, cli, node, hot0))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(time.Minute), false, "1"))))
	require.Len(t, nodeEvents(t, cli, node), 2, "a fault on another GPU is a change")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(2*time.Minute), true, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 2, "a recovery writes no Event")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(3*time.Minute), false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 2, "the fault's return reuses its Event, which still exists in the cluster")
	require.Equal(t, int32(2), eventCount(t, cli, node, hot0), "the return after a recovery is announced by refreshing that Event")
}

// ageRememberedEvent backdates the connector's memory of this fault's Event
// past nodeEventRefreshInterval, so the next repeat is a refresh of the Event
// instead of a skip.
func ageRememberedEvent(t *testing.T, connector *K8sConnector, nodeName string, k8sEvent *corev1.Event) {
	t.Helper()

	connector.nodeEventMu.Lock()
	defer connector.nodeEventMu.Unlock()

	written, ok := connector.nodeEventMemory().Get(nodeCheckKey(nodeName, k8sEvent.Type))
	require.True(t, ok, "the fault's Event should be remembered")

	remembered := written[k8sEvent.Message]
	remembered.writtenAt = time.Now().Add(-nodeEventRefreshInterval - time.Second)
	written[k8sEvent.Message] = remembered
}

// TestUpdateOnChange_EventRefreshedAfterInterval: once the refresh
// interval has passed, the next repeat refreshes the existing Event (count and
// timestamp) instead of creating another one.
func TestUpdateOnChange_EventRefreshedAfterInterval(t *testing.T) {
	connector, cli, node := envtestConnector(t)
	ctx := context.Background()
	now := time.Now()
	hot0 := connector.fetchHealthEventMessage(thermalEvent(node, now, false, "0"))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 1)

	ageRememberedEvent(t, connector, node, connector.createK8sEvent(ctx, thermalEvent(node, now, false, "0")))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(time.Minute), false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 1, "the refresh reuses the existing Event")
	require.Equal(t, int32(2), eventCount(t, cli, node, hot0), "the refresh bumps its count")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(2*time.Minute), false, "0"))))
	require.Equal(t, int32(2), eventCount(t, cli, node, hot0), "the refresh restarts the interval")
}

// TestUpdateOnChange_EventsFollowTimestampOrder: a batch is processed in
// timestamp order, like the condition path, so an older recovery that arrives
// after a newer fault in the same batch does not erase the memory of that
// fault, which would announce it again on its next repeat.
func TestUpdateOnChange_EventsFollowTimestampOrder(t *testing.T) {
	connector, cli, node := envtestConnector(t)
	ctx := context.Background()
	now := time.Now()
	hot0 := connector.fetchHealthEventMessage(thermalEvent(node, now, false, "0"))

	// Wire order: the fault first, then a recovery that is a minute older.
	require.NoError(t, connector.ProcessBatch(ctx, batch(
		thermalEvent(node, now.Add(time.Minute), false, "0"),
		thermalEvent(node, now, true, "0"),
	)))
	require.Len(t, nodeEvents(t, cli, node), 1, "the fault, the latest word on GPU 0, is announced")

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(2*time.Minute), false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 1)
	require.Equal(t, int32(1), eventCount(t, cli, node, hot0), "the fault is still remembered: the older recovery did not erase it")
}

// TestUpdateOnChange_PartialRecoveryKeepsOtherFaults: a recovery names
// the entities that recovered, so only their Events are forgotten; a fault on
// another entity of the same check stays a repeat.
func TestUpdateOnChange_PartialRecoveryKeepsOtherFaults(t *testing.T) {
	connector, cli, node := envtestConnector(t)
	ctx := context.Background()
	now := time.Now()
	hot0 := connector.fetchHealthEventMessage(thermalEvent(node, now, false, "0"))
	hot1 := connector.fetchHealthEventMessage(thermalEvent(node, now, false, "1"))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))
	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "1"))))
	require.Len(t, nodeEvents(t, cli, node), 2)

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(time.Minute), true, "0"))))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(2*time.Minute), false, "1"))))
	require.Len(t, nodeEvents(t, cli, node), 2, "GPU 1 is still the same fault; GPU 0 recovering does not re-announce it")
	require.Equal(t, int32(1), eventCount(t, cli, node, hot1))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(3*time.Minute), false, "0"))))
	require.Len(t, nodeEvents(t, cli, node), 2)
	require.Equal(t, int32(2), eventCount(t, cli, node, hot0), "GPU 0 faulting again after its recovery is announced by refreshing its Event")

	// A recovery naming no entity clears the whole check.
	recoveredAll := thermalEvent(node, now.Add(4*time.Minute), true, "0")
	recoveredAll.EntitiesImpacted = nil
	require.NoError(t, connector.ProcessBatch(ctx, batch(recoveredAll)))

	require.NoError(t, connector.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(5*time.Minute), false, "1"))))
	require.Len(t, nodeEvents(t, cli, node), 2)
	require.Equal(t, int32(2), eventCount(t, cli, node, hot1), "GPU 1 is announced again the same way")
}

// TestNodeEventMemory_PrunesWritesOlderThanTheRefreshInterval: a remembered
// write is useful only inside the refresh interval, after which the next
// repeat refreshes the Event through the API anyway. Remembering a new message
// drops the stale ones, so one check's memory holds only the faults written
// in the last interval, however many distinct messages it produces over time;
// a message still inside the interval is kept.
func TestNodeEventMemory_PrunesWritesOlderThanTheRefreshInterval(t *testing.T) {
	connector := &K8sConnector{}
	remembered := func(message string) bool {
		_, ok := connector.rememberedNodeEvent("node-a", &corev1.Event{Type: "check", Message: message})

		return ok
	}

	connector.rememberNodeEvent("node-a", &corev1.Event{Type: "check", Message: "stale"}, nil)
	connector.rememberNodeEvent("node-a", &corev1.Event{Type: "check", Message: "fresh"}, nil)
	ageRememberedEvent(t, connector, "node-a", &corev1.Event{Type: "check", Message: "stale"})

	connector.rememberNodeEvent("node-a", &corev1.Event{Type: "check", Message: "new"}, nil)

	require.False(t, remembered("stale"), "a write older than the refresh interval is dropped")
	require.True(t, remembered("fresh"), "a write inside the interval is kept")
	require.True(t, remembered("new"))
}

// TestNodeEventName_DerivedFromTheFault: the same fault gets the same name on
// every replica and across restarts; a different message is a different
// Event.
func TestNodeEventName_DerivedFromTheFault(t *testing.T) {
	connector, _, node := envtestConnector(t)
	ctx := context.Background()
	now := time.Now()

	first := connector.createK8sEvent(ctx, thermalEvent(node, now, false, "0"))
	again := connector.createK8sEvent(ctx, thermalEvent(node, now.Add(time.Hour), false, "0"))
	other := connector.createK8sEvent(ctx, thermalEvent(node, now, false, "1"))

	require.Equal(t, first.Name, again.Name, "the time of the report does not change the name")
	require.NotEqual(t, first.Name, other.Name, "another GPU is another Event")
	require.Regexp(t, `^`+node+`\.[0-9a-f]{16}$`, first.Name)
}

// TestNodeEvents_ReplicaWithoutMemoryRefreshesTheExistingEvent: a replica that
// has never seen a fault (or lost its memory of it) finds the Event another
// replica wrote and bumps it instead of writing a second one; the API server
// answers its create with AlreadyExists for real. The same holds for the
// DaemonSet after a restart.
func TestNodeEvents_ReplicaWithoutMemoryRefreshesTheExistingEvent(t *testing.T) {
	first, cli, node := envtestConnector(t)
	ctx := context.Background()
	now := time.Now()
	hot0 := first.fetchHealthEventMessage(thermalEvent(node, now, false, "0"))

	require.NoError(t, first.ProcessBatch(ctx, batch(thermalEvent(node, now, false, "0"))))

	// Another replica, or the same process after a restart: empty memory.
	second := NewK8sConnector(cli, nil, nil, ctx, first.config)
	require.NoError(t, second.ProcessBatch(ctx, batch(thermalEvent(node, now.Add(time.Minute), false, "0"))))

	require.Len(t, nodeEvents(t, cli, node), 1, "one Event per fault, however many replicas saw it")
	require.Equal(t, int32(2), eventCount(t, cli, node, hot0), "the second replica bumped the existing Event")

	_, known := second.rememberedNodeEvent(node, second.createK8sEvent(ctx, thermalEvent(node, now, false, "0")))
	require.True(t, known, "the second replica now remembers the Event it refreshed")
}
