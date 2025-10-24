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
	"testing"
	"time"

	platformconnector "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

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

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvents := &platformconnector.HealthEvents{
		Version: 1,
		Events: []*platformconnector.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "XID 48 detected",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_CONTACT_SUPPORT,
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

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvents := &platformconnector.HealthEvents{
		Version: 1,
		Events: []*platformconnector.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          true,
				Message:            "No errors",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_NONE,
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

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvents := &platformconnector.HealthEvents{
		Version: 1,
		Events: []*platformconnector.HealthEvent{
			{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				Message:            "GPU temperature warning",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
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
	connector := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvents := []*platformconnector.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "1"}},
			ErrorCode:          []string{"48"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			NodeName:           "test-node",
		},
	}

	err = connector.updateNodeConditions(ctx, healthEvents)
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
	connector := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvents := []*platformconnector.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          true,
			EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
			GeneratedTimestamp: timestamppb.New(time.Now()),
			NodeName:           "test-node",
		},
	}

	err = connector.updateNodeConditions(ctx, healthEvents)
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
	connector := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEventsProto := &platformconnector.HealthEvents{
		Events: []*platformconnector.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				NodeName:           "test-node",
			},
			{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "1"}},
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
	connector := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvents := []*platformconnector.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
			ErrorCode:          []string{"48"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			NodeName:           "test-node",
		},
	}

	err = connector.updateNodeConditions(ctx, healthEvents)
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
	connector := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvent := &platformconnector.HealthEvent{
		CheckName:          "GpuThermalWatch",
		IsHealthy:          false,
		EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
		ErrorCode:          []string{"THERMAL_WARNING"},
		IsFatal:            false,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		NodeName:           "test-node",
	}

	healthEventsProto := &platformconnector.HealthEvents{
		Events: []*platformconnector.HealthEvent{healthEvent},
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

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvents := &platformconnector.HealthEvents{
		Version: 1,
		Events: []*platformconnector.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "XID 48 detected",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_CONTACT_SUPPORT,
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

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvents := &platformconnector.HealthEvents{
		Version: 1,
		Events:  []*platformconnector.HealthEvent{},
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

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvents := &platformconnector.HealthEvents{
		Version: 1,
		Events: []*platformconnector.HealthEvent{
			{
				CheckName: "GpuXidError",
				IsHealthy: false,
				Message:   "Multiple GPUs affected",
				EntitiesImpacted: []*platformconnector.Entity{
					{EntityType: "GPU", EntityValue: "0"},
					{EntityType: "GPU", EntityValue: "1"},
					{EntityType: "GPU", EntityValue: "2"},
					{EntityType: "GPU", EntityValue: "3"},
				},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_CONTACT_SUPPORT,
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

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvents := &platformconnector.HealthEvents{
		Version: 1,
		Events: []*platformconnector.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "Error: GPU failed with status <critical> @10:30AM",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_CONTACT_SUPPORT,
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

	k8sConn := NewK8sConnector(cli, nil, stopCh, ctx)

	healthEvents := &platformconnector.HealthEvents{
		Version: 1,
		Events: []*platformconnector.HealthEvent{
			{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "XID error",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_CONTACT_SUPPORT,
				NodeName:           "test-node",
			},
			{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				Message:            "Temperature warning",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "1"}},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				NodeName:           "test-node",
			},
			{
				CheckName:          "InfinibandLinkFlapping",
				IsHealthy:          false,
				Message:            "Link flapping detected",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "IB", EntityValue: "mlx5_0"}},
				ErrorCode:          []string{},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "network",
				RecommendedAction:  platformconnector.RecommenedAction_RESTART_BM,
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
