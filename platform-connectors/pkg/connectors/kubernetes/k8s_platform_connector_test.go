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
			ExpectedOutputMessage:       "DCGM_FR_PCI_REPLAY_RATE:Pcie error on GPU 0 GPU:0 Recommended Action=UNKNOWN.",
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
			ExpectedOutputMessage:       "XID44 GPU:0 Recommended Action=REPORT_ISSUE.",
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
			ExpectedOutputMessage:       "XID44 GPU:0 Recommended Action=REPORT_ISSUE. XID45 GPU:0 Recommended Action=NONE.",
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
			ExpectedOutputMessage:       "DCGM_FR_CLOCK_THROTTLE_THERMAL:Thermal watch error on GPU 0 GPU:0 Recommended Action=UNKNOWN.",
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
			ExpectedOutputMessage:       "DCGM_FR_PCI_REPLAY_RATE:PCI Replay Rate error on GPU 0 GPU:0 Recommended Action=UNKNOWN.",
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
			ExpectedOutputMessage:       "DCGM_FR_CLOCK_THROTTLE_THERMAL:Thermal error on GPU 0 GPU:0 Recommended Action=UNKNOWN.",
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
			ExpectedOutputMessage:       "DCGM_FR_CLOCK_THROTTLE_THERMAL:Thermal error on GPU 0 GPU:0 Recommended Action=UNKNOWN.",
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
