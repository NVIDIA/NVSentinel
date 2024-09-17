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
	healthEvents := []*healthConditionList{
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuPcieWatch",
				IsHealthy:          true,
				EntitiesImpacted:   []string{},
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
				EntitiesImpacted:   []string{},
				ErrorCode:          []string{},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_NONE,
			},
			ExpectedOutputMessage:       NoHealthFailureMsg,
			ExpectedOutputReason:        "NoXidErrorDetected",
			ExpectedOutputConditionType: "GpuXidError",
			ExpectedHealthFailureStatus: "False",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuPcieWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []string{"0"},
				ErrorCode:          []string{"DCGM_FR_PCI_REPLAY_RATE"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				Message:            "Pcie error on GPU 0",
			},
			ExpectedOutputMessage:       "DCGM_FR_PCI_REPLAY_RATE:Pcie error on GPU 0 GPU:0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuPcieWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuPcieWatch",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "",
				EntitiesImpacted:   []string{"0"},
				ErrorCode:          []string{"44"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			},
			ExpectedOutputMessage:       "XID44 GPU:0 Recommended Action=REPORT_ISSUE;",
			ExpectedOutputReason:        "XidErrorDetected",
			ExpectedOutputConditionType: "GpuXidError",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuXidError",
				IsHealthy:          false,
				Message:            "",
				EntitiesImpacted:   []string{"0"},
				ErrorCode:          []string{"45"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_NONE,
			},
			ExpectedOutputMessage:       "XID44 GPU:0 Recommended Action=REPORT_ISSUE; XID45 GPU:0 Recommended Action=NONE;",
			ExpectedOutputReason:        "XidErrorDetected",
			ExpectedOutputConditionType: "GpuXidError",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []string{"0"},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				Message:            "Thermal watch error on GPU 0",
			},
			ExpectedOutputMessage:       "DCGM_FR_CLOCK_THROTTLE_THERMAL:Thermal watch error on GPU 0 GPU:0 Recommended Action=UNKNOWN;",
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
	for testCase, healthEvent := range healthEvents {
		err := k8sConnector.processHealthEvents(ctx, healthEvent.healthEvent)
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
	healthEvents := []*healthConditionList{
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuPcieWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []string{"0"},
				ErrorCode:          []string{"DCGM_FR_PCI_REPLAY_RATE"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				Message:            "PCI Replay Rate error on GPU 0",
			},
			ExpectedOutputMessage:       "DCGM_FR_PCI_REPLAY_RATE:PCI Replay Rate error on GPU 0 GPU:0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuPcieWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuPcieWatch",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []string{"0"},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				Message:            "Thermal error on GPU 0",
			},
			ExpectedOutputMessage:       "DCGM_FR_CLOCK_THROTTLE_THERMAL:Thermal error on GPU 0 GPU:0 Recommended Action=UNKNOWN;",
			ExpectedOutputReason:        "GpuThermalWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuThermalWatch",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []string{"0"},
				ErrorCode:          []string{"DCGM_FR_CLOCK_THROTTLE_THERMAL"},
				IsFatal:            false,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_UNKNOWN,
				Message:            "Thermal error on GPU 0",
			},
			ExpectedOutputMessage:       "DCGM_FR_CLOCK_THROTTLE_THERMAL:Thermal error on GPU 0 GPU:0 Recommended Action=UNKNOWN;",
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
	for testCase, healthEvent := range healthEvents {
		err := k8sConnector.processHealthEvents(ctx, healthEvent.healthEvent)
		if err != nil {
			t.Errorf("Failed to process healthEvent for testCase %d with err %s", testCase, err)
		}
		events, err := clientSet.CoreV1().Events("").List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.kind=Node,involvedObject.name=%s", fakeNode.Name),
		})
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
		{"message1", []string{"message1"}},
		{"message1;", []string{"message1"}},
		{"message1;message2", []string{"message1", "message2"}},
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
		messages   []string
		newMessage string
		expected   []string
	}{
		{[]string{}, "msg1", []string{"msg1"}},
		{[]string{"msg1"}, "msg2", []string{"msg1", "msg2"}},
		{[]string{"msg1"}, "msg1", []string{"msg1"}},
		{[]string{"msg1", "msg2"}, "msg2", []string{"msg1", "msg2"}},
	}

	for i, test := range tests {
		result := k8sConnector.addMessageIfNotExist(test.messages, test.newMessage)
		if !equalStringSlices(result, test.expected) {
			t.Errorf("Test %d failed: expected %v, got %v", i, test.expected, result)
		}
	}
}

func TestRemoveImpactedEntitiesMessages(t *testing.T) {
	tests := []struct {
		messages  []string
		entities  []string
		checkName string
		expected  []string
	}{
		{
			messages:  []string{" GPU:0 error", " GPU:1 error"},
			entities:  []string{"0"},
			checkName: "GpuErrorCheck",
			expected:  []string{" GPU:1 error"},
		},
		{
			messages:  []string{"NIC:eth0 error", "NIC:eth1 error"},
			entities:  []string{"eth0"},
			checkName: "InfiniBandErrorCheck",
			expected:  []string{"NIC:eth1 error"},
		},
		{
			messages:  []string{" nvswitch0 error", " nvswitch1 error"},
			entities:  []string{"nvswitch0"},
			checkName: "NvswitchErrorFromKmsgWatch",
			expected:  []string{" nvswitch1 error"},
		},
		{
			messages:  []string{" GPU:0 error", " GPU:1 error"},
			entities:  []string{"1"},
			checkName: "SomeOtherCheck",
			expected:  []string{" GPU:0 error"},
		},

		{
			messages:  []string{" GPU:0 error", " GPU:1 error"},
			entities:  []string{"2"},
			checkName: "GpuErrorCheck",
			expected:  []string{" GPU:0 error", " GPU:1 error"},
		},
	}

	for i, test := range tests {
		result := k8sConnector.removeImpactedEntitiesMessages(test.messages, test.entities, test.checkName)
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
		{XidErrorCheck, true, NoXidErrorDetectedReason},
		{XidErrorCheck, false, XidErrorDetectedReason},
		{XidBatchErrorCheck, true, NoXidErrorDetectedReason},
		{XidBatchErrorCheck, false, XidErrorDetectedReason},
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
	healthEvents := []*platformconnector.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			EntitiesImpacted:   []string{"0"},
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
			EntitiesImpacted:   []string{"mlx5_0"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "network",
			RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			Message:            "InfiniBand error on mlx5_0",
		},
		{
			CheckName:          "NvswitchErrorFromKmsgWatch",
			IsHealthy:          false,
			EntitiesImpacted:   []string{"nvswitch0"},
			ErrorCode:          []string{"SWITCH_ERROR"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "nvswitch",
			RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			Message:            "Nvswitch error on nvswitch0",
		},
	}

	for _, healthEvent := range healthEvents {
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
						LastHeartbeatTime:  metav1.Time{Time: time.Now().Add(-10 * time.Minute)},
						LastTransitionTime: metav1.Time{Time: time.Now().Add(-10 * time.Minute)},
						Message:            NoHealthFailureMsg,
					},
				},
			},
		}
		_, err := clientSet.CoreV1().Nodes().Create(ctx, fakeNode, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create node: %v", err)
		}

		newCondition := corev1.NodeCondition{
			Type:               conditionType,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Message:            k8sConnector.fetchHealthEventMessage(healthEvent),
		}

		err = k8sConnector.updateNodeCondition(ctx, newCondition, healthEvent)
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
				if condition.LastTransitionTime.Time.Before(time.Now().Add(-5 * time.Minute)) {
					t.Errorf("Expected LastTransitionTime to be updated for %s", conditionType)
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
	healthEvents := []*platformconnector.HealthEvent{
		{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			EntitiesImpacted:   []string{"0"},
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
			EntitiesImpacted:   []string{"mlx5_0"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "network",
			RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			Message:            "InfiniBand error on mlx5_0",
		},
		{
			CheckName:          "NvswitchErrorFromKmsgWatch",
			IsHealthy:          false,
			EntitiesImpacted:   []string{"nvswitch0"},
			ErrorCode:          []string{"SWITCH_ERROR"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "nvswitch",
			RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
			Message:            "Nvswitch error on nvswitch0",
		},
	}

	for _, healthEvent := range healthEvents {
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
		newCondition := corev1.NodeCondition{
			Type:               conditionType,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Message:            k8sConnector.fetchHealthEventMessage(healthEvent),
		}

		err = k8sConnector.updateNodeCondition(ctx, newCondition, healthEvent)
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
	healthEvents := []struct {
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
				EntitiesImpacted:   []string{"1"},
				ErrorCode:          []string{"45"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
				RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
				Message:            "XID45 error on GPU 1",
			},
			expectedMessage: "GPU:0 error; XID45 GPU:1 Recommended Action=REPORT_ISSUE;",
		},
		{
			conditionType: "EthernetErrorCheck",
			existingMsg:   "NIC:eth0 error",
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "EthernetErrorCheck",
				IsHealthy:          false,
				EntitiesImpacted:   []string{"eth1"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "network",
				RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
				Message:            "error on eth1",
			},
			expectedMessage: "NIC:eth0 error; NIC:eth1, error on eth1. Recommended Action=REPORT_ISSUE;",
		},
		{
			conditionType: "NvswitchErrorFromKmsgWatch",
			existingMsg:   " nvswitch0 error",
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "NvswitchErrorFromKmsgWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []string{"nvswitch1"},
				ErrorCode:          []string{"SWITCH_ERROR"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "nvswitch",
				RecommendedAction:  platformconnector.RecommenedAction_REPORT_ISSUE,
				Message:            "Nvswitch error on nvswitch1",
			},
			expectedMessage: " nvswitch0 error; SWITCH_ERROR:Nvswitch error on nvswitch1 nvswitch1, Recommended Action=REPORT_ISSUE;",
		},
	}

	for _, testCase := range healthEvents {
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

		newCondition := corev1.NodeCondition{
			Type:               testCase.conditionType,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
			Message:            k8sConnector.fetchHealthEventMessage(testCase.healthEvent),
		}

		err = k8sConnector.updateNodeCondition(ctx, newCondition, testCase.healthEvent)
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
		entitiesImpacted []string
		expectedMessage  string
	}{
		{
			conditionType:    "GpuXidError",
			existingMsg:      " GPU:0 error; GPU:1 error",
			entitiesImpacted: []string{"0"},
			expectedMessage:  " GPU:1 error",
		},
		{
			conditionType:    "InfiniBandErrorCheck",
			existingMsg:      "NIC:eth0 error;NIC:eth1 error",
			entitiesImpacted: []string{"eth0"},
			expectedMessage:  "NIC:eth1 error",
		},
		{
			conditionType:    "NvswitchErrorFromKmsgWatch",
			existingMsg:      " nvswitch0 error; nvswitch1 error",
			entitiesImpacted: []string{"nvswitch0"},
			expectedMessage:  " nvswitch1 error",
		},
	}

	for _, testCase := range testCases {
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

		newCondition := corev1.NodeCondition{
			Type:               testCase.conditionType,
			LastHeartbeatTime:  metav1.Now(),
			LastTransitionTime: metav1.Now(),
		}

		err = k8sConnector.updateNodeCondition(ctx, newCondition, healthEvent)
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
