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
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-node",
			Labels: map[string]string{},
		},
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
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-node",
			Labels: map[string]string{},
		},
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
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-node",
			Labels: map[string]string{},
		},
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
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
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
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

// TestK8sConnector_WithEnvtest_EventCountIncrement tests that event counts are incremented for duplicate events
func TestK8sConnector_WithEnvtest_EventCountIncrement(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
	}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)
	connector := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	healthEvent := &protos.HealthEvent{
		CheckName:          "GpuThermalWatch",
		IsHealthy:          false,
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
		ErrorCode:          []string{"THERMAL_WARNING"},
		IsFatal:            false,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		NodeName:           "test-node",
	}

	healthEventsProto := &protos.HealthEvents{
		Events: []*protos.HealthEvent{healthEvent},
	}

	err = connector.processHealthEvents(ctx, healthEventsProto)
	require.NoError(t, err)

	events, err := cli.CoreV1().Events(DefaultNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=test-node",
	})
	require.NoError(t, err)
	assert.True(t, len(events.Items) > 0, "event was not created")

	err = connector.processHealthEvents(ctx, healthEventsProto)
	require.NoError(t, err)

	events, err = cli.CoreV1().Events(DefaultNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.name=test-node",
	})
	require.NoError(t, err)

	eventFound := false
	for _, event := range events.Items {
		if event.Type == "GpuThermalWatch" && event.Count >= 2 {
			eventFound = true
			break
		}
	}
	assert.True(t, eventFound, "event count was not incremented")
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
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-node",
			Labels: map[string]string{},
		},
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
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-node",
			Labels: map[string]string{},
		},
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
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-node",
			Labels: map[string]string{},
		},
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

// TestK8sConnector_WithEnvtest_CompactionAndDeduplication verifies the full real-world flow:
// health events are appended one by one to the same node condition. Once the accumulated
// message exceeds 1024 bytes, compaction fires and the result must contain no two entries
// with identical compacted text. A duplicate event (same entity + same Recommended Action,
// different diagnostic text) must collapse to a single entry, not accumulate.
func TestK8sConnector_WithEnvtest_CompactionAndDeduplication(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "test-node"}}
	_, err := cli.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create node")

	stopCh := make(chan struct{})
	defer close(stopCh)
	connector := NewK8sConnector(cli, nil, stopCh, ctx, defaultConnectorConfig)

	// Send 5 health events for 5 distinct GPUs. Each message is ~197 bytes;
	// 5 messages total ~990 bytes < 1024 — no compaction should fire yet.
	// The timestamp is embedded in the diagnostic text and falls within the
	// 72-byte compacted prefix, so it distinguishes entries after compaction.
	for i := 0; i < 5; i++ {
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

	// Send a duplicate event for GPU:0 (same GPU/PCI entities + same Recommended Action,
	// different timestamp in diagnostic text). This pushes the total to ~1188 bytes > 1024,
	// triggering Tier 1 compaction. deduplicateMessagesByIdentity drops the old GPU:0 entry
	// and keeps this fresher one; all 5 surviving entries are then compacted to ~555 bytes.
	dupEvent := []*protos.HealthEvent{
		{
			CheckName: "GpuXidError",
			IsHealthy: false,
			Message:   "kernel: [16450077.000001] NVRM: Xid (PCI:0000:00:00.0): 119, pid=9999999, name=proc, Timeout after 6s waiting for GPU GSP response",
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "0"},
				{EntityType: "PCI", EntityValue: "0000:00:00.0"},
			},
			ErrorCode:          []string{"119"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			RecommendedAction:  protos.RecommendedAction_COMPONENT_RESET,
			NodeName:           "test-node",
		},
	}
	_, err = connector.updateNodeConditions(ctx, dupEvent)
	require.NoError(t, err, "failed to process duplicate event for GPU:0")

	node, err = cli.CoreV1().Nodes().Get(ctx, "test-node", metav1.GetOptions{})
	require.NoError(t, err)
	condMsg = ""
	for _, c := range node.Status.Conditions {
		if c.Type == "GpuXidError" {
			condMsg = c.Message
			break
		}
	}
	require.NotEmpty(t, condMsg, "GpuXidError condition not found after duplicate event")

	// 1. Condition message must be within the 1024-byte Kubernetes limit.
	assert.LessOrEqual(t, len(condMsg), 1024,
		"condition message must not exceed 1024 bytes after compaction")

	// 2. Entries must be in compacted form (free-text truncated at 72 bytes).
	assert.Contains(t, condMsg, truncationSuffix,
		"entries must be in compacted form after limit was exceeded")

	// 3. No two compacted entries in the condition message may have identical text.
	//    If dedup and compaction are working correctly, each entry originates from a
	//    different entity and carries a distinct 72-byte prefix.
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

	// 4. GPU:0 must appear exactly once — the old entry replaced by the fresh one.
	gpu0Count := 0
	for _, e := range entries {
		if strings.Contains(e, "GPU:0") && strings.Contains(e, "PCI:0000:00:00.0") {
			gpu0Count++
		}
	}
	assert.Equal(t, 1, gpu0Count,
		"GPU:0 must appear exactly once after dedup+compaction")

	// 5. Old GPU:0 entry must be gone; the fresh one must survive.
	//    The timestamp "16450076.000001" falls within the 72-byte compacted prefix,
	//    so its absence confirms the old entry was dropped.
	assert.NotContains(t, condMsg, "16450076.000001",
		"old GPU:0 entry (timestamp 16450076.000001) must be dropped")
	assert.Contains(t, condMsg, "16450077.000001",
		"fresh GPU:0 entry (timestamp 16450077.000001) must survive in compacted prefix")

	// 6. All other GPU entries (1–4) must still be present after compaction.
	for i := 1; i < 5; i++ {
		assert.Contains(t, condMsg, fmt.Sprintf("GPU:%d", i),
			"GPU:%d entry must survive compaction", i)
	}
}

// TestK8sConnector_WithEnvtest_MultipleCheckTypes tests multiple different check types on same node
func TestK8sConnector_WithEnvtest_MultipleCheckTypes(t *testing.T) {
	ctx := context.Background()
	testEnv, cli := setupEnvtest(t)
	defer testEnv.Stop()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-node",
			Labels: map[string]string{},
		},
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
