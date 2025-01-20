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

package kubernetes

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"k8s.io/klog/v2"

	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/ringbuffer"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var (
	k8sConnector *K8sConnector
	clientSet    *fake.Clientset
	ctx          context.Context
)

func TestMain(m *testing.M) {
	clientSet = fake.NewSimpleClientset()
	ctx = context.Background()
	stopCh := make(chan struct{})
	ringBuffer := ringbuffer.NewRingBuffer("k8sRingBuffer", ctx)
	k8sConnector = NewK8sConnector(clientSet, ringBuffer, "testnode", stopCh, ctx)
	exitVal := m.Run()
	os.Exit(exitVal)
}

type healthConditionList struct {
	healthEvent                 *platformconnector.HealthEvent
	ExpectedOutputReason        string
	ExpectedOutputMessage       string
	ExpectedHealthFailureStatus string
	ExpectedOutputConditionType string
}

func getNode() *v1.Node {
	// Create a fake node
	fakeNode := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testnode",
		},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("4"),
				v1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Conditions: []v1.NodeCondition{
				{
					Type:               v1.NodeReady,
					Status:             v1.ConditionTrue,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "KubeletReady",
					Message:            "kubelet is posting ready status",
				},
				{
					Type:               v1.NodeMemoryPressure,
					Status:             v1.ConditionFalse,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "KubeletHasSufficientMemory",
					Message:            "kubelet has sufficient memory available",
				},
				{
					Type:               v1.NodeDiskPressure,
					Status:             v1.ConditionFalse,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "KubeletHasNoDiskPressure",
					Message:            "kubelet has no disk pressure",
				},
				{
					Type:               corev1.NodeConditionType("GpuThermalWatch"),
					Status:             v1.ConditionFalse,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "GpuThermalWatchIsHealthy",
					Message:            "No Health Failures",
				},
				{
					Type:               corev1.NodeConditionType("GpuPcieWatch"),
					Status:             v1.ConditionFalse,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "GpuPcieWatchIsHealthy",
					Message:            "No Health Failures",
				},
			},
		},
	}
	return fakeNode
}

func TestK8sNodeConditions(t *testing.T) {
	healthEventsList := []*healthConditionList{
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuPcieWatch",
				IsHealthy:          true,
				EntitiesImpacted:   []*platformconnector.Entity{},
				ErrorCode:          []string{},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				Message:            "Pcie watch error on GPU 0",
			},
			ExpectedOutputMessage:       "No Health Failures",
			ExpectedOutputReason:        "GpuPcieWatchIsHealthy",
			ExpectedOutputConditionType: "GpuPcieWatch",
			ExpectedHealthFailureStatus: "False",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          true,
				Message:            "",
				EntitiesImpacted:   []*platformconnector.Entity{},
				ErrorCode:          []string{},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_NONE,
			},
			ExpectedOutputMessage:       NoHealthFailureMsg,
			ExpectedOutputReason:        "GpuXidErrorIsHealthy",
			ExpectedOutputConditionType: "GpuXidError",
			ExpectedHealthFailureStatus: "False",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuPcieWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_PCI_REPLAY_RATE"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				Message:            "Pcie error on GPU 0",
			},
			ExpectedOutputMessage:       "ErrorCode:DCGM_FR_PCI_REPLAY_RATE GPU:0 Pcie error on GPU 0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuPcieWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuPcieWatch",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"44"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			},
			ExpectedOutputMessage:       "ErrorCode:44 GPU:0 Recommended Action=REPORT_ISSUE;",
			ExpectedOutputReason:        "GpuXidErrorIsNotHealthy",
			ExpectedOutputConditionType: "GpuXidError",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"45"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_NONE,
			},
			ExpectedOutputMessage:       "ErrorCode:44 GPU:0 Recommended Action=REPORT_ISSUE;ErrorCode:45 GPU:0 Recommended Action=NONE;",
			ExpectedOutputReason:        "GpuXidErrorIsNotHealthy",
			ExpectedOutputConditionType: "GpuXidError",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				Message:            "Thermal watch error on GPU 0",
			},
			ExpectedOutputMessage:       "ErrorCode:DCGM_FR_CLOCK_THROTTLE_THERMAL GPU:0 Thermal watch error on GPU 0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuThermalWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuThermalWatch",
			ExpectedHealthFailureStatus: "True",
		},
	}
	fakeNode := getNode()
	_, err := clientSet.CoreV1().Nodes().Create(ctx, fakeNode, metav1.CreateOptions{})
	if err != nil {
		klog.Errorf("Failed to create  node with err %s", err)
		os.Exit(1)
	}
	for testCase, healthEvent := range healthEventsList {
		healthEvents := platformconnector.HealthEvents{Version: 1, Events: make([]*platformconnector.HealthEvent, 0)}
		healthEvents.Events = append(healthEvents.Events, healthEvent.healthEvent)
		err := k8sConnector.processHealthEvents(ctx, &healthEvents)
		if err != nil {
			t.Errorf("Failed to process healthEvent for testCase %d with err %s", testCase, err)
		}
		node, err := clientSet.CoreV1().Nodes().Get(ctx, fakeNode.Name, metav1.GetOptions{})
		if err != nil {
			t.Errorf("Failed to get node for testCase %d with err %s", testCase, err)
		}

		conditions := node.Status.Conditions
		conditionFound := false
		for _, condition := range conditions {
			if string(condition.Type) == healthEvent.ExpectedOutputConditionType {
				conditionFound = true
				if healthEvent.ExpectedHealthFailureStatus != string(condition.Status) {
					t.Errorf("Testcase %d. Node Condition Status %s is not matching with expectedConditionStatus %s", testCase, string(condition.Status), healthEvent.ExpectedHealthFailureStatus)
				}
				if healthEvent.ExpectedOutputMessage != string(condition.Message) {
					t.Errorf("Testcase %d. Node Condition Message  %s is not matching with expectedConditionMessage %s", testCase, string(condition.Message), healthEvent.ExpectedOutputMessage)
				}
				if healthEvent.ExpectedOutputReason != string(condition.Reason) {
					t.Errorf("Testcase %d. Node Condition Reason %s is not matching with expectedConditionReason %s", testCase, string(condition.Reason), healthEvent.ExpectedOutputReason)
				}
				break
			}
		}
		if conditionFound == false {
			t.Errorf("Testcase %d nodeCondition is missing", testCase)
		}
	}
	err = clientSet.CoreV1().Nodes().Delete(ctx, fakeNode.Name, metav1.DeleteOptions{})
	if err != nil {
		t.Errorf("Failed to delete  node with err %s", err)
	}
}

func TestK8sNodeEvents(t *testing.T) {
	healthEventsList := []*healthConditionList{
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuPcieWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_PCI_REPLAY_RATE"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				Message:            "PCI Replay Rate error on GPU 0",
			},
			ExpectedOutputMessage:       "ErrorCode:DCGM_FR_PCI_REPLAY_RATE GPU:0 PCI Replay Rate error on GPU 0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuPcieWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuPcieWatch",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				Message:            "Thermal error on GPU 0",
			},
			ExpectedOutputMessage:       "ErrorCode:DCGM_FR_CLOCK_THROTTLE_THERMAL GPU:0 Thermal error on GPU 0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuThermalWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuThermalWatch",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				Message:            "Thermal error on GPU 0",
			},
			ExpectedOutputMessage:       "ErrorCode:DCGM_FR_CLOCK_THROTTLE_THERMAL GPU:0 Thermal error on GPU 0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuThermalWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuThermalWatch",
		},
	}
	fakeNode := getNode()
	_, err := clientSet.CoreV1().Nodes().Create(ctx, fakeNode, metav1.CreateOptions{})
	if err != nil {
		klog.Errorf("Failed to create  node with err %s", err)
		os.Exit(1)
	}

	healthEvents := platformconnector.HealthEvents{Version: 1, Events: make([]*platformconnector.HealthEvent, 0)}
	for _, event := range healthEventsList {
		healthEvents.Events = append(healthEvents.Events, event.healthEvent)
	}
	err = k8sConnector.processHealthEvents(ctx, &healthEvents)
	if err != nil {
		t.Errorf("Failed to process healthEvents with err %s", err)
	}
	events, err := clientSet.CoreV1().Events("").List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.kind=Node,involvedObject.name=%s", fakeNode.Name),
	})

	for testCase, healthEvent := range healthEventsList {
		conditionFound := false
		for _, event := range events.Items {
			if event.Type == healthEvent.ExpectedOutputConditionType {
				conditionFound = true

				if healthEvent.ExpectedOutputMessage != string(event.Message) {
					t.Errorf("Testcase %d. Node event Message  %s is not matching with expectedEventMessage %s", testCase, string(event.Message), healthEvent.ExpectedOutputMessage)
				}
				if healthEvent.ExpectedOutputReason != string(event.Reason) {
					t.Errorf("Testcase %d. Node event Reason %s is not matching with expectedEventReason %s", testCase, string(event.Reason), healthEvent.ExpectedOutputReason)
				}
			}
		}
		if conditionFound == false {
			t.Errorf("Testcase %d nodeEvent is missing", testCase)
		}
	}
	err = clientSet.CoreV1().Nodes().Delete(ctx, fakeNode.Name, metav1.DeleteOptions{})
	if err != nil {
		t.Errorf("Failed to delete  node with err %s", err)
	}
}

func TestParseMessages(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"", []string{}},
		{"message1;", []string{"message1"}},
		{"message1;message2;", []string{"message1", "message2"}},
	}

	for i, test := range tests {
		result := k8sConnector.parseMessages(test.input)
		if !equalStringSlices(result, test.expected) {
			t.Errorf("Test %d failed: expected %v, got %v", i, test.expected, result)
		}
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAddMessageIfNotExist(t *testing.T) {
	tests := []struct {
		messages []string
		event    *platformconnector.HealthEvent
		expected []string
	}{
		{
			messages: []string{},
			event: &platformconnector.HealthEvent{
				ErrorCode:         []string{"E001"},
				EntitiesImpacted:  []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				Message:           "msg1",
				RecommendedAction: platformconnector.RecommenedAction_APPLICATION_RESTART,
			},
			expected: []string{"ErrorCode:E001 GPU:0 msg1 Recommended Action=APPLICATION_RESTART"},
		},
		{
			messages: []string{"ErrorCode:E001 GPU:0 msg1 Recommended Action=APPLICATION_RESTART"},
			event: &platformconnector.HealthEvent{
				ErrorCode:         []string{"E002"},
				EntitiesImpacted:  []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "1"}},
				Message:           "msg2",
				RecommendedAction: platformconnector.RecommenedAction_NODE_REBOOT,
			},
			expected: []string{
				"ErrorCode:E001 GPU:0 msg1 Recommended Action=APPLICATION_RESTART",
				"ErrorCode:E002 GPU:1 msg2 Recommended Action=NODE_REBOOT",
			},
		},
		{
			messages: []string{"ErrorCode:E001 GPU:0 msg1 Recommended Action=APPLICATION_RESTART"},
			event: &platformconnector.HealthEvent{
				ErrorCode:         []string{"E001"},
				EntitiesImpacted:  []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				Message:           "msg1",
				RecommendedAction: platformconnector.RecommenedAction_APPLICATION_RESTART,
			},
			expected: []string{"ErrorCode:E001 GPU:0 msg1 Recommended Action=APPLICATION_RESTART"},
		},
		{
			messages: []string{
				"ErrorCode:E001 GPU:0 msg1 Recommended Action=APPLICATION_RESTART",
				"ErrorCode:E002 GPU:1 msg2 Recommended Action=NODE_REBOOT",
			},
			event: &platformconnector.HealthEvent{
				ErrorCode:         []string{"E002"},
				EntitiesImpacted:  []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "1"}},
				Message:           "msg2",
				RecommendedAction: platformconnector.RecommenedAction_NODE_REBOOT,
			},
			expected: []string{
				"ErrorCode:E001 GPU:0 msg1 Recommended Action=APPLICATION_RESTART",
				"ErrorCode:E002 GPU:1 msg2 Recommended Action=NODE_REBOOT",
			},
		},
	}

	for i, test := range tests {
		result := k8sConnector.addMessageIfNotExist(test.messages, test.event)
		if !equalStringSlices(result, test.expected) {
			t.Errorf("Test %d failed: expected %v, got %v", i, test.expected, result)
		}
	}
}

func convertToEntityPointers(entities []platformconnector.Entity) []*platformconnector.Entity {
	entityPointers := make([]*platformconnector.Entity, len(entities))
	for i := range entities {
		entityPointers[i] = &entities[i]
	}
	return entityPointers
}

func TestRemoveImpactedEntitiesMessages(t *testing.T) {
	tests := []struct {
		messages         []string
		EntitiesImpacted []platformconnector.Entity
		checkName        string
		expected         []string
		componentClass   string
	}{
		{
			messages:         []string{" GPU:0 error", " GPU:1 error"},
			EntitiesImpacted: []platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
			checkName:        "GpuErrorCheck",
			expected:         []string{" GPU:1 error"},
			componentClass:   "GPU",
		},
		{
			messages:         []string{"NIC:eth0 error", "NIC:eth1 error"},
			EntitiesImpacted: []platformconnector.Entity{{EntityType: "NIC", EntityValue: "eth0"}},
			checkName:        "InfiniBandErrorCheck",
			expected:         []string{"NIC:eth1 error"},
			componentClass:   "NIC",
		},
		{
			messages:         []string{" NVSWITCH:0 error", " NVSWITCH:1 error"},
			EntitiesImpacted: []platformconnector.Entity{{EntityType: "NVSWITCH", EntityValue: "0"}},
			checkName:        "NvswitchErrorFromKmsgWatch",
			expected:         []string{" NVSWITCH:1 error"},
			componentClass:   "NVSWITCH",
		},
		{
			messages:         []string{" GPU:0 error", " GPU:1 error"},
			EntitiesImpacted: []platformconnector.Entity{{EntityType: "GPU", EntityValue: "1"}},
			checkName:        "SomeOtherCheck",
			expected:         []string{" GPU:0 error"},
			componentClass:   "GPU",
		},

		{
			messages:         []string{" GPU:0 error", " GPU:1 error"},
			EntitiesImpacted: []platformconnector.Entity{{EntityType: "GPU", EntityValue: "2"}},
			checkName:        "GpuErrorCheck",
			expected:         []string{" GPU:0 error", " GPU:1 error"},
			componentClass:   "GPU",
		},
	}

	for i, test := range tests {
		result := k8sConnector.removeImpactedEntitiesMessages(test.messages, convertToEntityPointers(test.EntitiesImpacted))
		if !equalStringSlices(result, test.expected) {
			t.Errorf("Test %d failed: expected %v, got %v", i, test.expected, result)
		}
	}
}

func TestUpdateHealthEventReason(t *testing.T) {
	tests := []struct {
		checkName string
		isHealthy bool
		expected  string
	}{
		{"GpuXidError", true, "GpuXidErrorIsHealthy"},
		{"GpuXidError", false, "GpuXidErrorIsNotHealthy"},
		{"XidBatchError", true, "XidBatchErrorIsHealthy"},
		{"XidBatchError", false, "XidBatchErrorIsNotHealthy"},
		{"GpuPcieWatch", true, "GpuPcieWatchIsHealthy"},
		{"GpuPcieWatch", false, "GpuPcieWatchIsNotHealthy"},
	}

	for i, test := range tests {
		result := k8sConnector.updateHealthEventReason(test.checkName, test.isHealthy)
		if result != test.expected {
			t.Errorf("Test %d failed: expected %s, got %s", i, test.expected, result)
		}
	}
}

func TestUpdateNodeCondition_StatusChange(t *testing.T) {
	fixedTime := time.Date(2025, 1, 16, 5, 13, 23, 0, time.UTC)

	healthEventsList := []platformconnector.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
			ErrorCode:          []string{"44"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(fixedTime),
			ComponentClass:     "gpu",
			RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			Message:            "XID44 error on GPU 0",
		},
		{
			CheckName:          "InfiniBandErrorCheck",
			IsHealthy:          false,
			EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "NIC", EntityValue: "mlx5_0"}},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(fixedTime),
			ComponentClass:     "network",
			RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			Message:            "InfiniBand error on mlx5_0",
		},
		{
			CheckName:          "NvswitchErrorFromKmsgWatch",
			IsHealthy:          false,
			EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "NVSWITCH", EntityValue: "0"}},
			ErrorCode:          []string{"SWITCH_ERROR"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(fixedTime),
			ComponentClass:     "nvswitch",
			RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			Message:            "Nvswitch error on nvswitch0",
		},
	}

	for i := range healthEventsList {
		healthEvent := &(healthEventsList)[i]
		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})

		conditionType := corev1.NodeConditionType(healthEvent.CheckName)
		fakeNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "testnode",
			},
			Status: v1.NodeStatus{
				Conditions: []v1.NodeCondition{
					{
						Type:               conditionType,
						Status:             corev1.ConditionFalse,
						LastHeartbeatTime:  metav1.Time{Time: fixedTime.Add(-10 * time.Minute)},
						LastTransitionTime: metav1.Time{Time: fixedTime.Add(-10 * time.Minute)},
						Message:            NoHealthFailureMsg,
					},
				},
			},
		}
		_, err := clientSet.CoreV1().Nodes().Create(ctx, fakeNode, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create node: %v", err)
		}

		healthEvents := platformconnector.HealthEvents{Version: 1, Events: make([]*platformconnector.HealthEvent, 0)}
		healthEvents.Events = append(healthEvents.Events, healthEvent)
		err = k8sConnector.updateNodeConditions(ctx, healthEvents.Events)
		if err != nil {
			t.Errorf("updateNodeCondition failed: %v", err)
		}

		node, err := clientSet.CoreV1().Nodes().Get(ctx, "testnode", metav1.GetOptions{})
		if err != nil {
			t.Errorf("Failed to get node: %v", err)
		}

		conditionFound := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == conditionType {
				conditionFound = true
				if condition.Status != corev1.ConditionTrue {
					t.Errorf("Expected condition status to be True for %s, got %v", conditionType, condition.Status)
				}
				expectedTime := fixedTime
				actualTime := condition.LastTransitionTime.Time.UTC()
				if !actualTime.Equal(expectedTime) {
					t.Errorf("Expected LastTransitionTime to be updated to %v, got %v", expectedTime, actualTime)
				}
				break
			}
		}
		if !conditionFound {
			t.Errorf("Condition %s not found in node status", conditionType)
		}

		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})
	}
}

func TestUpdateNodeCondition_NewCondition(t *testing.T) {
	healthEventsList := []*platformconnector.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
			ErrorCode:          []string{"44"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "gpu",
			RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			Message:            "XID44 error on GPU 0",
		},
		{
			CheckName:          "InfiniBandErrorCheck",
			IsHealthy:          false,
			EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "NIC", EntityValue: "mlx5_0"}},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "network",
			RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			Message:            "InfiniBand error on mlx5_0",
		},
		{
			CheckName:          "NvswitchErrorFromKmsgWatch",
			IsHealthy:          false,
			EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "NVSWITCH", EntityValue: "0"}},
			ErrorCode:          []string{"SWITCH_ERROR"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "nvswitch",
			RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			Message:            "Nvswitch error on nvswitch0",
		},
	}

	for _, healthEvent := range healthEventsList {
		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})

		fakeNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "testnode",
			},
			Status: v1.NodeStatus{
				Conditions: []v1.NodeCondition{},
			},
		}
		_, err := clientSet.CoreV1().Nodes().Create(ctx, fakeNode, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create node: %v", err)
		}

		conditionType := corev1.NodeConditionType(healthEvent.CheckName)
		healthEvents := platformconnector.HealthEvents{Version: 1, Events: make([]*platformconnector.HealthEvent, 0)}
		healthEvents.Events = append(healthEvents.Events, healthEvent)
		err = k8sConnector.updateNodeConditions(ctx, healthEvents.Events)
		if err != nil {
			t.Errorf("updateNodeCondition failed: %v", err)
		}

		node, err := clientSet.CoreV1().Nodes().Get(ctx, "testnode", metav1.GetOptions{})
		if err != nil {
			t.Errorf("Failed to get node: %v", err)
		}

		conditionFound := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == conditionType {
				conditionFound = true
				if condition.Status != corev1.ConditionTrue {
					t.Errorf("Expected condition status to be True for %s, got %v", conditionType, condition.Status)
				}
				expectedMessage := k8sConnector.fetchHealthEventMessage(healthEvent)
				if condition.Message != expectedMessage {
					t.Errorf("Expected condition message to be %s, got %s", expectedMessage, condition.Message)
				}
				expectedReason := k8sConnector.updateHealthEventReason(healthEvent.CheckName, healthEvent.IsHealthy)
				if condition.Reason != expectedReason {
					t.Errorf("Expected condition reason to be %s, got %s", expectedReason, condition.Reason)
				}
				break
			}
		}
		if !conditionFound {
			t.Errorf("Condition %s not found in node status", conditionType)
		}

		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})
	}
}

func TestUpdateNodeCondition_AddMessage(t *testing.T) {
	healthEventsList := []struct {
		conditionType   corev1.NodeConditionType
		existingMsg     string
		healthEvent     *platformconnector.HealthEvent
		expectedMessage string
	}{
		{
			conditionType: "GpuXidError",
			existingMsg:   "GPU:0 error",
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "1"}},
				ErrorCode:          []string{"45"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
				Message:            "XID45 error on GPU 1",
			},
			expectedMessage: "GPU:0 error;ErrorCode:45 GPU:1 XID45 error on GPU 1 Recommended Action=REPORT_ISSUE;",
		},
		{
			conditionType: "EthernetErrorCheck",
			existingMsg:   "NIC:eth0 error",
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "EthernetErrorCheck",
				IsHealthy:          false,
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "NIC", EntityValue: "eth1"}},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "network",
				RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
				Message:            "error on eth1",
			},
			expectedMessage: "NIC:eth0 error;NIC:eth1 error on eth1 Recommended Action=REPORT_ISSUE;",
		},
		{
			conditionType: "NvswitchErrorFromKmsgWatch",
			existingMsg:   " nvswitch0 error",
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "NvswitchErrorFromKmsgWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "NVSWITCH", EntityValue: "1"}},
				ErrorCode:          []string{"SWITCH_ERROR"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "nvswitch",
				RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
				Message:            "Nvswitch error on nvswitch1",
			},
			expectedMessage: " nvswitch0 error;ErrorCode:SWITCH_ERROR NVSWITCH:1 Nvswitch error on nvswitch1 Recommended Action=REPORT_ISSUE;",
		},
	}

	for _, testCase := range healthEventsList {
		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})

		fakeNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "testnode",
			},
			Status: v1.NodeStatus{
				Conditions: []v1.NodeCondition{
					{
						Type:               testCase.conditionType,
						Status:             corev1.ConditionTrue,
						LastHeartbeatTime:  metav1.Now(),
						LastTransitionTime: metav1.Now(),
						Message:            testCase.existingMsg,
					},
				},
			},
		}
		_, err := clientSet.CoreV1().Nodes().Create(ctx, fakeNode, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create node: %v", err)
		}

		healthEvents := platformconnector.HealthEvents{Version: 1, Events: make([]*platformconnector.HealthEvent, 0)}
		healthEvents.Events = append(healthEvents.Events, testCase.healthEvent)
		err = k8sConnector.updateNodeConditions(ctx, healthEvents.Events)
		if err != nil {
			t.Errorf("updateNodeCondition failed: %v", err)
		}

		node, err := clientSet.CoreV1().Nodes().Get(ctx, "testnode", metav1.GetOptions{})
		if err != nil {
			t.Errorf("Failed to get node: %v", err)
		}

		conditionFound := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == testCase.conditionType {
				conditionFound = true
				if condition.Message != testCase.expectedMessage {
					t.Errorf("Expected condition message to be '%s', got '%s'", testCase.expectedMessage, condition.Message)
				}
				if condition.Status != corev1.ConditionTrue {
					t.Errorf("Expected condition status to be True, got %v", condition.Status)
				}
				break
			}
		}
		if !conditionFound {
			t.Errorf("Condition %s not found in node status", testCase.conditionType)
		}

		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})
	}
}

func TestUpdateNodeCondition_RemoveMessages(t *testing.T) {
	testCases := []struct {
		conditionType    corev1.NodeConditionType
		existingMsg      string
		entitiesImpacted []*platformconnector.Entity
		expectedMessage  string
	}{
		{
			conditionType:    "GpuXidError",
			existingMsg:      "GPU:0 error;GPU:1 error;",
			entitiesImpacted: []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
			expectedMessage:  "GPU:1 error;",
		},
		{
			conditionType:    "InfiniBandErrorCheck",
			existingMsg:      "NIC:eth0 error;NIC:eth1 error;",
			entitiesImpacted: []*platformconnector.Entity{{EntityType: "NIC", EntityValue: "eth0"}},
			expectedMessage:  "NIC:eth1 error;",
		},
		{
			conditionType:    "NvswitchErrorFromKmsgWatch",
			existingMsg:      "NVSWITCH:0 error;NVSWITCH:1 error;",
			entitiesImpacted: []*platformconnector.Entity{{EntityType: "NVSWITCH", EntityValue: "0"}},
			expectedMessage:  "NVSWITCH:1 error;",
		},
	}

	for index, testCase := range testCases {
		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})

		fakeNode := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "testnode",
			},
			Status: v1.NodeStatus{
				Conditions: []v1.NodeCondition{
					{
						Type:               testCase.conditionType,
						Status:             corev1.ConditionTrue,
						LastHeartbeatTime:  metav1.Now(),
						LastTransitionTime: metav1.Now(),
						Message:            testCase.existingMsg,
					},
				},
			},
		}
		_, err := clientSet.CoreV1().Nodes().Create(ctx, fakeNode, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create node: %v", err)
		}

		healthEvent := &platformconnector.HealthEvent{
			CheckName:          string(testCase.conditionType),
			IsHealthy:          true,
			EntitiesImpacted:   testCase.entitiesImpacted,
			GeneratedTimestamp: timestamppb.New(time.Now()),
		}

		healthEvents := platformconnector.HealthEvents{Version: 1, Events: make([]*platformconnector.HealthEvent, 0)}
		healthEvents.Events = append(healthEvents.Events, healthEvent)

		err = k8sConnector.updateNodeConditions(ctx, healthEvents.Events)
		if err != nil {
			t.Errorf("testcase %d updateNodeCondition failed: %v", index+1, err)
		}

		node, err := clientSet.CoreV1().Nodes().Get(ctx, "testnode", metav1.GetOptions{})
		if err != nil {
			t.Errorf("Failed to get node: %v", err)
		}

		conditionFound := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == testCase.conditionType {
				conditionFound = true
				if condition.Message != testCase.expectedMessage {
					t.Errorf("testcase %d Expected condition message to be '%s', got '%s'", index+1, testCase.expectedMessage, condition.Message)
				}

				if condition.Status != corev1.ConditionTrue {
					t.Errorf("testcase %d Expected condition status to be True, got %v", index+1, condition.Status)
				}
				break
			}
		}
		if !conditionFound {
			t.Errorf("testcase %d Condition %s not found in node status", index+1, testCase.conditionType)
		}

		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})
	}
}
