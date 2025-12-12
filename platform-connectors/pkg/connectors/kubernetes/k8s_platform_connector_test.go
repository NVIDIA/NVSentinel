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
	"io"
	"log/slog"
	"math"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	maxNodeConditionMessageLength := int64(1024)
	k8sConnector = NewK8sConnector(clientSet, ringBuffer, stopCh, ctx, maxNodeConditionMessageLength)
	exitVal := m.Run()
	os.Exit(exitVal)
}

type healthConditionList struct {
	healthEvent                 *protos.HealthEvent
	ExpectedOutputReason        string
	ExpectedOutputMessage       string
	ExpectedHealthFailureStatus string
	ExpectedOutputConditionType string
}

func getNode() *corev1.Node {
	// Create a fake node
	fakeNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "testnode",
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeReady,
					Status:             corev1.ConditionTrue,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "KubeletReady",
					Message:            "kubelet is posting ready status",
				},
				{
					Type:               corev1.NodeMemoryPressure,
					Status:             corev1.ConditionFalse,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "KubeletHasSufficientMemory",
					Message:            "kubelet has sufficient memory available",
				},
				{
					Type:               corev1.NodeDiskPressure,
					Status:             corev1.ConditionFalse,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "KubeletHasNoDiskPressure",
					Message:            "kubelet has no disk pressure",
				},
				{
					Type:               corev1.NodeConditionType("GpuThermalWatch"),
					Status:             corev1.ConditionFalse,
					LastHeartbeatTime:  metav1.Now(),
					LastTransitionTime: metav1.Now(),
					Reason:             "GpuThermalWatchIsHealthy",
					Message:            "No Health Failures",
				},
				{
					Type:               corev1.NodeConditionType("GpuPcieWatch"),
					Status:             corev1.ConditionFalse,
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
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuPcieWatch",
				IsHealthy:          true,
				EntitiesImpacted:   []*protos.Entity{},
				ErrorCode:          []string{},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_UNKNOWN,
				Message:            "Pcie watch error on GPU 0",
				NodeName:           "testnode",
			},
			ExpectedOutputMessage:       "No Health Failures",
			ExpectedOutputReason:        "GpuPcieWatchIsHealthy",
			ExpectedOutputConditionType: "GpuPcieWatch",
			ExpectedHealthFailureStatus: "False",
		},
		{
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          true,
				Message:            "",
				EntitiesImpacted:   []*protos.Entity{},
				ErrorCode:          []string{},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_NONE,
				NodeName:           "testnode",
			},
			ExpectedOutputMessage:       "No Health Failures",
			ExpectedOutputReason:        "GpuXidErrorIsHealthy",
			ExpectedOutputConditionType: "GpuXidError",
			ExpectedHealthFailureStatus: "False",
		},
		{
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuPcieWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_PCI_REPLAY_RATE"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_UNKNOWN,
				Message:            "Pcie error on GPU 0",
				NodeName:           "testnode",
			},
			ExpectedOutputMessage:       "ErrorCode:DCGM_FR_PCI_REPLAY_RATE GPU:0 Pcie error on GPU 0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuPcieWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuPcieWatch",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "",
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"44"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
				NodeName:           "testnode",
			},
			ExpectedOutputMessage:       "ErrorCode:44 GPU:0 Recommended Action=CONTACT_SUPPORT;",
			ExpectedOutputReason:        "GpuXidErrorIsNotHealthy",
			ExpectedOutputConditionType: "GpuXidError",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "",
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"45"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_NONE,
				NodeName:           "testnode",
			},
			ExpectedOutputMessage:       "ErrorCode:44 GPU:0 Recommended Action=CONTACT_SUPPORT;ErrorCode:45 GPU:0 Recommended Action=NONE;",
			ExpectedOutputReason:        "GpuXidErrorIsNotHealthy",
			ExpectedOutputConditionType: "GpuXidError",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_UNKNOWN,
				Message:            "Thermal watch error on GPU 0",
				NodeName:           "testnode",
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
		slog.Error("Failed to create node", "error", err)
		os.Exit(1)
	}
	for testCase, healthEvent := range healthEventsList {
		healthEvents := protos.HealthEvents{Version: 1, Events: make([]*protos.HealthEvent, 0)}
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
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuPcieWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_PCI_REPLAY_RATE"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_UNKNOWN,
				Message:            "PCI Replay Rate error on GPU 0",
			},
			ExpectedOutputMessage:       "ErrorCode:DCGM_FR_PCI_REPLAY_RATE GPU:0 PCI Replay Rate error on GPU 0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuPcieWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuPcieWatch",
		},
		{
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_UNKNOWN,
				Message:            "Thermal error on GPU 0",
				NodeName:           "testnode",
			},
			ExpectedOutputMessage:       "ErrorCode:DCGM_FR_CLOCK_THROTTLE_THERMAL GPU:0 Thermal error on GPU 0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuThermalWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuThermalWatch",
		},
		{
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_UNKNOWN,
				Message:            "Thermal error on GPU 0",
				NodeName:           "testnode",
			},
			ExpectedOutputMessage:       "ErrorCode:DCGM_FR_CLOCK_THROTTLE_THERMAL GPU:0 Thermal error on GPU 0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuThermalWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuThermalWatch",
		},
	}
	fakeNode := getNode()
	_, err := clientSet.CoreV1().Nodes().Create(ctx, fakeNode, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Failed to create node", "error", err)
		os.Exit(1)
	}

	healthEvents := protos.HealthEvents{Version: 1, Events: make([]*protos.HealthEvent, 0)}
	for _, event := range healthEventsList {
		healthEvents.Events = append(healthEvents.Events, event.healthEvent)
	}
	err = k8sConnector.processHealthEvents(ctx, &healthEvents)
	if err != nil {
		t.Errorf("Failed to process healthEvents with err %s", err)
	}
	events, _ := clientSet.CoreV1().Events("").List(ctx, metav1.ListOptions{
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
		event    *protos.HealthEvent
		expected []string
	}{
		{
			messages: []string{},
			event: &protos.HealthEvent{
				ErrorCode:         []string{"E001"},
				EntitiesImpacted:  []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				Message:           "msg1",
				RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
				NodeName:          "testnode",
			},
			expected: []string{"ErrorCode:E001 GPU:0 msg1 Recommended Action=COMPONENT_RESET"},
		},
		{
			messages: []string{"ErrorCode:E001 GPU:0 msg1 Recommended Action=COMPONENT_RESET"},
			event: &protos.HealthEvent{
				ErrorCode:         []string{"E002"},
				EntitiesImpacted:  []*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
				Message:           "msg2",
				RecommendedAction: protos.RecommendedAction_RESTART_VM,
				NodeName:          "testnode",
			},
			expected: []string{
				"ErrorCode:E001 GPU:0 msg1 Recommended Action=COMPONENT_RESET",
				"ErrorCode:E002 GPU:1 msg2 Recommended Action=RESTART_VM",
			},
		},
		{
			messages: []string{"ErrorCode:E001 GPU:0 msg1 Recommended Action=COMPONENT_RESET"},
			event: &protos.HealthEvent{
				ErrorCode:         []string{"E001"},
				EntitiesImpacted:  []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				Message:           "msg1",
				RecommendedAction: protos.RecommendedAction_COMPONENT_RESET,
				NodeName:          "testnode",
			},
			expected: []string{"ErrorCode:E001 GPU:0 msg1 Recommended Action=COMPONENT_RESET"},
		},
		{
			messages: []string{
				"ErrorCode:E001 GPU:0 msg1 Recommended Action=COMPONENT_RESET",
				"ErrorCode:E002 GPU:1 msg2 Recommended Action=RESTART_VM",
			},
			event: &protos.HealthEvent{
				ErrorCode:         []string{"E002"},
				EntitiesImpacted:  []*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
				Message:           "msg2",
				RecommendedAction: protos.RecommendedAction_RESTART_VM,
				NodeName:          "testnode",
			},
			expected: []string{
				"ErrorCode:E001 GPU:0 msg1 Recommended Action=COMPONENT_RESET",
				"ErrorCode:E002 GPU:1 msg2 Recommended Action=RESTART_VM",
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

func convertToEntityPointers(entities []protos.Entity) []*protos.Entity {
	entityPointers := make([]*protos.Entity, len(entities))
	for i := range entities {
		entityPointers[i] = &entities[i]
	}
	return entityPointers
}

func TestRemoveImpactedEntitiesMessages(t *testing.T) {
	tests := []struct {
		messages         []string
		EntitiesImpacted []protos.Entity
		checkName        string
		expected         []string
		componentClass   string
		NodeName         string
	}{
		{
			messages:         []string{" GPU:0 error", " GPU:1 error"},
			EntitiesImpacted: []protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			checkName:        "GpuErrorCheck",
			expected:         []string{" GPU:1 error"},
			componentClass:   "GPU",
			NodeName:         "testnode",
		},
		{
			messages:         []string{"NIC:eth0 error", "NIC:eth1 error"},
			EntitiesImpacted: []protos.Entity{{EntityType: "NIC", EntityValue: "eth0"}},
			checkName:        "InfiniBandErrorCheck",
			expected:         []string{"NIC:eth1 error"},
			componentClass:   "NIC",
			NodeName:         "testnode",
		},
		{
			messages:         []string{" NVSWITCH:0 error", " NVSWITCH:1 error"},
			EntitiesImpacted: []protos.Entity{{EntityType: "NVSWITCH", EntityValue: "0"}},
			checkName:        "NvswitchErrorFromKmsgWatch",
			expected:         []string{" NVSWITCH:1 error"},
			componentClass:   "NVSWITCH",
			NodeName:         "testnode",
		},
		{
			messages:         []string{" GPU:0 error", " GPU:1 error"},
			EntitiesImpacted: []protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
			checkName:        "SomeOtherCheck",
			expected:         []string{" GPU:0 error"},
			componentClass:   "GPU",
			NodeName:         "testnode",
		},

		{
			messages:         []string{" GPU:0 error", " GPU:1 error"},
			EntitiesImpacted: []protos.Entity{{EntityType: "GPU", EntityValue: "2"}},
			checkName:        "GpuErrorCheck",
			expected:         []string{" GPU:0 error", " GPU:1 error"},
			componentClass:   "GPU",
			NodeName:         "testnode",
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

	healthEventsList := []protos.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			ErrorCode:          []string{"44"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(fixedTime),
			ComponentClass:     "gpu",
			RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
			Message:            "XID44 error on GPU 0",
			NodeName:           "testnode",
		},
		{
			CheckName:          "InfiniBandErrorCheck",
			IsHealthy:          false,
			EntitiesImpacted:   []*protos.Entity{{EntityType: "NIC", EntityValue: "mlx5_0"}},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(fixedTime),
			ComponentClass:     "network",
			RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
			Message:            "InfiniBand error on mlx5_0",
			NodeName:           "testnode",
		},
		{
			CheckName:          "NvswitchErrorFromKmsgWatch",
			IsHealthy:          false,
			EntitiesImpacted:   []*protos.Entity{{EntityType: "NVSWITCH", EntityValue: "0"}},
			ErrorCode:          []string{"SWITCH_ERROR"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(fixedTime),
			ComponentClass:     "nvswitch",
			RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
			Message:            "Nvswitch error on nvswitch0",
			NodeName:           "testnode",
		},
	}

	for i := range healthEventsList {
		healthEvent := &(healthEventsList)[i]
		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})

		conditionType := corev1.NodeConditionType(healthEvent.CheckName)
		fakeNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "testnode",
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
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

		healthEvents := protos.HealthEvents{Version: 1, Events: make([]*protos.HealthEvent, 0)}
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
	healthEventsList := []*protos.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			ErrorCode:          []string{"44"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "gpu",
			RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
			Message:            "XID44 error on GPU 0",
			NodeName:           "testnode",
		},
		{
			CheckName:          "InfiniBandErrorCheck",
			IsHealthy:          false,
			EntitiesImpacted:   []*protos.Entity{{EntityType: "NIC", EntityValue: "mlx5_0"}},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "network",
			RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
			Message:            "InfiniBand error on mlx5_0",
			NodeName:           "testnode",
		},
		{
			CheckName:          "NvswitchErrorFromKmsgWatch",
			IsHealthy:          false,
			EntitiesImpacted:   []*protos.Entity{{EntityType: "NVSWITCH", EntityValue: "0"}},
			ErrorCode:          []string{"SWITCH_ERROR"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "nvswitch",
			RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
			Message:            "Nvswitch error on nvswitch0",
			NodeName:           "testnode",
		},
	}

	for _, healthEvent := range healthEventsList {
		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})

		fakeNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "testnode",
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{},
			},
		}
		_, err := clientSet.CoreV1().Nodes().Create(ctx, fakeNode, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create node: %v", err)
		}

		conditionType := corev1.NodeConditionType(healthEvent.CheckName)
		healthEvents := protos.HealthEvents{Version: 1, Events: make([]*protos.HealthEvent, 0)}
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
		healthEvent     *protos.HealthEvent
		expectedMessage string
	}{
		{
			conditionType: "GpuXidError",
			existingMsg:   "GPU:0 error",
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "1"}},
				ErrorCode:          []string{"45"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
				Message:            "XID45 error on GPU 1",
				NodeName:           "testnode",
			},
			expectedMessage: "GPU:0 error;ErrorCode:45 GPU:1 XID45 error on GPU 1 Recommended Action=CONTACT_SUPPORT;",
		},
		{
			conditionType: "EthernetErrorCheck",
			existingMsg:   "NIC:eth0 error",
			healthEvent: &protos.HealthEvent{
				CheckName:          "EthernetErrorCheck",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "NIC", EntityValue: "eth1"}},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "network",
				RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
				Message:            "error on eth1",
				NodeName:           "testnode",
			},
			expectedMessage: "NIC:eth0 error;NIC:eth1 error on eth1 Recommended Action=CONTACT_SUPPORT;",
		},
		{
			conditionType: "NvswitchErrorFromKmsgWatch",
			existingMsg:   " nvswitch0 error",
			healthEvent: &protos.HealthEvent{
				CheckName:          "NvswitchErrorFromKmsgWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "NVSWITCH", EntityValue: "1"}},
				ErrorCode:          []string{"SWITCH_ERROR"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "nvswitch",
				RecommendedAction:  protos.RecommendedAction_CONTACT_SUPPORT,
				Message:            "Nvswitch error on nvswitch1",
				NodeName:           "testnode",
			},
			expectedMessage: " nvswitch0 error;ErrorCode:SWITCH_ERROR NVSWITCH:1 Nvswitch error on nvswitch1 Recommended Action=CONTACT_SUPPORT;",
		},
	}

	for _, testCase := range healthEventsList {
		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})

		fakeNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "testnode",
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
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

		healthEvents := protos.HealthEvents{Version: 1, Events: make([]*protos.HealthEvent, 0)}
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
		entitiesImpacted []*protos.Entity
		expectedMessage  string
	}{
		{
			conditionType:    "GpuXidError",
			existingMsg:      "GPU:0 error;GPU:1 error;",
			entitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
			expectedMessage:  "GPU:1 error;",
		},
		{
			conditionType:    "InfiniBandErrorCheck",
			existingMsg:      "NIC:eth0 error;NIC:eth1 error;",
			entitiesImpacted: []*protos.Entity{{EntityType: "NIC", EntityValue: "eth0"}},
			expectedMessage:  "NIC:eth1 error;",
		},
		{
			conditionType:    "NvswitchErrorFromKmsgWatch",
			existingMsg:      "NVSWITCH:0 error;NVSWITCH:1 error;",
			entitiesImpacted: []*protos.Entity{{EntityType: "NVSWITCH", EntityValue: "0"}},
			expectedMessage:  "NVSWITCH:1 error;",
		},
	}

	for index, testCase := range testCases {
		_ = clientSet.CoreV1().Nodes().Delete(ctx, "testnode", metav1.DeleteOptions{})

		fakeNode := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "testnode",
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
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

		healthEvent := &protos.HealthEvent{
			CheckName:          string(testCase.conditionType),
			IsHealthy:          true,
			EntitiesImpacted:   testCase.entitiesImpacted,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			NodeName:           "testnode",
		}

		healthEvents := protos.HealthEvents{Version: 1, Events: make([]*protos.HealthEvent, 0)}
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

func TestIsTemporaryError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		// Nil error
		{
			name:     "nil error should return false",
			err:      nil,
			expected: false,
		},

		// Context errors
		{
			name:     "context.DeadlineExceeded should be retryable",
			err:      context.DeadlineExceeded,
			expected: true,
		},
		{
			name:     "context.Canceled should be retryable",
			err:      context.Canceled,
			expected: true,
		},
		{
			name:     "wrapped context.DeadlineExceeded should be retryable",
			err:      fmt.Errorf("operation failed: %w", context.DeadlineExceeded),
			expected: true,
		},

		// Kubernetes API errors
		{
			name:     "timeout error should be retryable",
			err:      apierrors.NewTimeoutError("operation timed out", 30),
			expected: true,
		},
		{
			name:     "server timeout error should be retryable",
			err:      apierrors.NewServerTimeout(schema.GroupResource{Group: "", Resource: "nodes"}, "update", 30),
			expected: true,
		},
		{
			name:     "service unavailable error should be retryable",
			err:      apierrors.NewServiceUnavailable("service temporarily unavailable"),
			expected: true,
		},
		{
			name:     "too many requests error should be retryable",
			err:      apierrors.NewTooManyRequests("rate limit exceeded", 60),
			expected: true,
		},
		{
			name:     "internal server error should be retryable",
			err:      apierrors.NewInternalError(fmt.Errorf("internal error")),
			expected: true,
		},
		{
			name:     "not found error should not be retryable",
			err:      apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "nodes"}, "test-node"),
			expected: false,
		},
		{
			name:     "bad request error should not be retryable",
			err:      apierrors.NewBadRequest("invalid request"),
			expected: false,
		},

		// Network errors
		{
			name:     "network timeout error should be retryable",
			err:      &timeoutError{timeout: true},
			expected: true,
		},
		{
			name:     "non-timeout network error should not be retryable",
			err:      &timeoutError{timeout: false},
			expected: false,
		},

		// Syscall errors
		{
			name:     "ECONNREFUSED should be retryable",
			err:      syscall.ECONNREFUSED,
			expected: true,
		},
		{
			name:     "ECONNRESET should be retryable",
			err:      syscall.ECONNRESET,
			expected: true,
		},
		{
			name:     "ECONNABORTED should be retryable",
			err:      syscall.ECONNABORTED,
			expected: true,
		},
		{
			name:     "ETIMEDOUT should be retryable",
			err:      syscall.ETIMEDOUT,
			expected: true,
		},
		{
			name:     "EHOSTUNREACH should be retryable",
			err:      syscall.EHOSTUNREACH,
			expected: true,
		},
		{
			name:     "ENETUNREACH should be retryable",
			err:      syscall.ENETUNREACH,
			expected: true,
		},
		{
			name:     "EPIPE should be retryable",
			err:      syscall.EPIPE,
			expected: true,
		},
		{
			name:     "wrapped ECONNRESET should be retryable",
			err:      fmt.Errorf("connection failed: %w", syscall.ECONNRESET),
			expected: true,
		},
		{
			name:     "EACCES should not be retryable",
			err:      syscall.EACCES,
			expected: false,
		},

		// io.EOF errors
		{
			name:     "io.EOF should be retryable",
			err:      io.EOF,
			expected: true,
		},
		{
			name:     "wrapped io.EOF should be retryable",
			err:      fmt.Errorf("read failed: %w", io.EOF),
			expected: true,
		},

		// String-based HTTP/2 and connection errors
		{
			name:     "http2: client connection lost should be retryable",
			err:      fmt.Errorf("http2: client connection lost"),
			expected: true,
		},
		{
			name:     "http2: server connection lost should be retryable",
			err:      fmt.Errorf("http2: server connection lost"),
			expected: true,
		},
		{
			name:     "http2: connection closed should be retryable",
			err:      fmt.Errorf("http2: connection closed"),
			expected: true,
		},
		{
			name:     "connection reset by peer should be retryable",
			err:      fmt.Errorf("read: connection reset by peer"),
			expected: true,
		},
		{
			name:     "broken pipe should be retryable",
			err:      fmt.Errorf("write: broken pipe"),
			expected: true,
		},
		{
			name:     "connection refused should be retryable",
			err:      fmt.Errorf("dial tcp: connection refused"),
			expected: true,
		},
		{
			name:     "connection timed out should be retryable",
			err:      fmt.Errorf("dial tcp: connection timed out"),
			expected: true,
		},
		{
			name:     "i/o timeout should be retryable",
			err:      fmt.Errorf("Post \"https://example.com\": i/o timeout"),
			expected: true,
		},
		{
			name:     "network is unreachable should be retryable",
			err:      fmt.Errorf("dial tcp: network is unreachable"),
			expected: true,
		},
		{
			name:     "host is unreachable should be retryable",
			err:      fmt.Errorf("dial tcp: no route to host: host is unreachable"),
			expected: true,
		},

		// TLS/SSL errors
		{
			name:     "tls: handshake timeout should be retryable",
			err:      fmt.Errorf("tls: handshake timeout"),
			expected: true,
		},
		{
			name:     "tls: oversized record received should be retryable",
			err:      fmt.Errorf("tls: oversized record received with length 65536"),
			expected: true,
		},
		{
			name:     "remote error: tls: should be retryable",
			err:      fmt.Errorf("remote error: tls: bad certificate"),
			expected: true,
		},

		// DNS errors
		{
			name:     "no such host should be retryable",
			err:      fmt.Errorf("dial tcp: lookup example.com: no such host"),
			expected: true,
		},
		{
			name:     "dns: no answer should be retryable",
			err:      fmt.Errorf("dns: no answer from server"),
			expected: true,
		},
		{
			name:     "temporary failure in name resolution should be retryable",
			err:      fmt.Errorf("dial tcp: lookup example.com on 127.0.0.1:53: temporary failure in name resolution"),
			expected: true,
		},

		// Load balancer and proxy errors
		{
			name:     "502 Bad Gateway should be retryable",
			err:      fmt.Errorf("502 Bad Gateway"),
			expected: true,
		},
		{
			name:     "503 Service Unavailable should be retryable",
			err:      fmt.Errorf("503 Service Unavailable"),
			expected: true,
		},
		{
			name:     "504 Gateway Timeout should be retryable",
			err:      fmt.Errorf("504 Gateway Timeout"),
			expected: true,
		},

		// Kubernetes-specific error patterns
		{
			name:     "server unable to handle request should be retryable",
			err:      fmt.Errorf("the server is currently unable to handle the request"),
			expected: true,
		},
		{
			name:     "etcd cluster unavailable should be retryable",
			err:      fmt.Errorf("etcd cluster is unavailable or misconfigured"),
			expected: true,
		},
		{
			name:     "unable to connect to server should be retryable",
			err:      fmt.Errorf("unable to connect to the server: dial tcp 10.0.0.1:6443: connect: connection refused"),
			expected: true,
		},
		{
			name:     "server not ready should be retryable",
			err:      fmt.Errorf("server is not ready to handle requests"),
			expected: true,
		},

		// String EOF errors
		{
			name:     "string EOF should be retryable",
			err:      fmt.Errorf("unexpected EOF"),
			expected: true,
		},

		// Non-retryable errors
		{
			name:     "generic error should not be retryable",
			err:      fmt.Errorf("some random error"),
			expected: false,
		},
		{
			name:     "permission denied should not be retryable",
			err:      fmt.Errorf("permission denied"),
			expected: false,
		},
		{
			name:     "invalid argument should not be retryable",
			err:      fmt.Errorf("invalid argument"),
			expected: false,
		},

		// Complex error scenarios
		{
			name:     "nested http2 error in url.Error should be retryable",
			err:      &url.Error{Op: "Get", URL: "https://example.com", Err: fmt.Errorf("http2: client connection lost")},
			expected: true,
		},
		{
			name:     "deeply nested retryable error should be retryable",
			err:      fmt.Errorf("operation failed: %w", fmt.Errorf("network error: %w", syscall.ECONNRESET)),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isTemporaryError(tt.err)
			if result != tt.expected {
				t.Errorf("isTemporaryError(%v) = %v, expected %v", tt.err, result, tt.expected)
			}
		})
	}
}

// timeoutError is a mock implementation of net.Error for testing
type timeoutError struct {
	timeout bool
}

func (e *timeoutError) Error() string {
	if e.timeout {
		return "operation timed out"
	}
	return "network error"
}

func (e *timeoutError) Timeout() bool {
	return e.timeout
}

func (e *timeoutError) Temporary() bool {
	return false
}

func TestUpdateNodeConditions_ErrorHandling(t *testing.T) {
	tests := []struct {
		name        string
		nodeName    string
		healthEvent *protos.HealthEvent
		expectError bool
		setupNode   bool
	}{
		{
			name:     "node not found",
			nodeName: "nonexistent-node",
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				GeneratedTimestamp: timestamppb.New(time.Now()),
				NodeName:           "nonexistent-node",
			},
			expectError: true,
			setupNode:   false,
		},
		{
			name:     "empty node name",
			nodeName: "",
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				GeneratedTimestamp: timestamppb.New(time.Now()),
				NodeName:           "",
			},
			expectError: true,
			setupNode:   false,
		},
		{
			name:     "successful update with no existing conditions",
			nodeName: "test-node-success",
			healthEvent: &protos.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"48"},
				GeneratedTimestamp: timestamppb.New(time.Now()),
				NodeName:           "test-node-success",
			},
			expectError: false,
			setupNode:   true,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			localCtx := context.Background()
			localClientSet := fake.NewSimpleClientset()
			stopCh := make(chan struct{})
			defer close(stopCh)

			ringBuffer := ringbuffer.NewRingBuffer(fmt.Sprintf("testRingBuffer-%d", i), localCtx)
			maxNodeConditionMessageLength := int64(1024)
			connector := NewK8sConnector(localClientSet, ringBuffer, stopCh, localCtx, maxNodeConditionMessageLength)

			if tt.setupNode {
				node := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: tt.nodeName,
					},
				}
				_, err := localClientSet.CoreV1().Nodes().Create(localCtx, node, metav1.CreateOptions{})
				require.NoError(t, err)
			}

			healthEvents := &protos.HealthEvents{
				Events: []*protos.HealthEvent{tt.healthEvent},
			}

			err := connector.updateNodeConditions(localCtx, healthEvents.Events)

			if tt.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
func TestTruncateConditionMessage(t *testing.T) {
	const truncationSuffix = "..."

	// Generate test messages that would exceed 1KB when combined
	generateLongMessages := func(count int) []string {
		var msgs []string
		for i := 0; i < count; i++ {
			msgs = append(msgs, "ErrorCode:45 PCI:0000:29:00 GPU:GPU-8614c5d9-371d-1d8a-9bab-78d0434427ec ROBUST_CHANNEL Resolution: WORKFLOW_XID_45 Recommended Action=CONTACT_SUPPORT")
		}
		return msgs
	}

	tests := []struct {
		name                          string
		maxNodeConditionMessageLength int64
		messages                      []string
		shouldTruncate                bool
		description                   string
	}{
		{
			name:                          "Empty NodeConditionMessage  with 1KB limit",
			maxNodeConditionMessageLength: 1024,
			messages:                      []string{},
			shouldTruncate:                false,
			description:                   "Empty NodeConditionMessage should return NoHealthFailureMsg",
		},
		{
			name:                          "Single short NodeConditionMessage with 1KB limit",
			maxNodeConditionMessageLength: 1024,
			messages:                      []string{"ErrorCode:45 PCI:0000:29:00 GPU:GPU-xxx Recommended Action=CONTACT_SUPPORT"},
			shouldTruncate:                false,
			description:                   "Single short NodeConditionMessage should not be truncated",
		},
		{
			name:                          "Multiple NodeConditionMessages exceeding 1KB limit - should truncate",
			maxNodeConditionMessageLength: 1024,
			messages:                      generateLongMessages(7), // ~1050 chars total
			shouldTruncate:                true,
			description:                   "NodeConditionMessages exceeding 1KB should be truncated",
		},
		{
			name:                          "Many NodeConditionMessages with INFINITE limit - should NOT truncate",
			maxNodeConditionMessageLength: math.MaxInt64,
			messages:                      generateLongMessages(100), // ~15000 chars total
			shouldTruncate:                false,
			description:                   "Even 100 NodeConditionMessages should not be truncated with infinite limit",
		},
		{
			name:                          "Multiple NodeConditionMessages with small limit (256 bytes) - should truncate",
			maxNodeConditionMessageLength: 256,
			messages:                      generateLongMessages(3),
			shouldTruncate:                true,
			description:                   "With 256 byte limit, even 3 NodeConditionMessages should be truncated",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create connector with configurable maxNodeConditionMessageLength
			connector := &K8sConnector{
				maxNodeConditionMessageLength: tc.maxNodeConditionMessageLength,
			}

			result := connector.truncateNodeConditionMessage(tc.messages)

			t.Logf("Test: %s", tc.description)
			t.Logf("Max limit: %d, Result length: %d", tc.maxNodeConditionMessageLength, len(result))

			if tc.shouldTruncate {
				// When truncation is expected
				assert.LessOrEqual(t, len(result), int(tc.maxNodeConditionMessageLength),
					"NodeConditionMessage length should not exceed configured max")
				assert.Contains(t, result, truncationSuffix,
					"truncated NodeConditionMessage should contain truncation suffix '...'")
			} else {
				// When no truncation is expected
				assert.NotContains(t, result, truncationSuffix,
					"non-truncated NodeConditionMessage should NOT contain truncation suffix '...'")

				// For non-empty messages, verify all content is preserved
				if len(tc.messages) > 0 {
					for _, msg := range tc.messages {
						assert.Contains(t, result, msg,
							"all NodeConditionMessages should be preserved when not truncating")
					}
				}
			}
		})
	}
}
