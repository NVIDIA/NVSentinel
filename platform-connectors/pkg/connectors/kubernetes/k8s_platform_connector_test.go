package kubernetes

import (
	"context"
	"os"
	"testing"
	"time"

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
	k8sConnector = NewK8sConnector(clientSet, ringBuffer, "testnode", stopCh)
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
				IsHealthy:          false,
				EntitiesImpacted:   []string{"0"},
				ErrorCode:          "DCGM_FR_PCI_REPLAY_RATE",
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
			},
			ExpectedOutputMessage:       "DCGM_FR_PCI_REPLAY_RATE:0.",
			ExpectedOutputReason:        "GpuPcieWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuPcieWatch",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "XidError",
				IsHealthy:          false,
				Message:            "",
				EntitiesImpacted:   []string{"0"},
				ErrorCode:          "44",
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
			},
			ExpectedOutputMessage:       "XID44:0.",
			ExpectedOutputReason:        "XidErrorDetected",
			ExpectedOutputConditionType: "XidError",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "XidError",
				IsHealthy:          false,
				Message:            "",
				EntitiesImpacted:   []string{"0"},
				ErrorCode:          "45",
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
			},
			ExpectedOutputMessage:       "XID44:0. XID45:0.",
			ExpectedOutputReason:        "XidErrorDetected",
			ExpectedOutputConditionType: "XidError",
			ExpectedHealthFailureStatus: "True",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				EntitiesImpacted:   []string{"0"},
				ErrorCode:          "DCGM_FR_EC_HARDWARE_MEMORY",
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
			},
			ExpectedOutputMessage:       "DCGM_FR_EC_HARDWARE_MEMORY:0.",
			ExpectedOutputReason:        "GpuThermalWatchIsNotHealthy",
			ExpectedOutputConditionType: "GpuThermalWatch",
			ExpectedHealthFailureStatus: "True",
		},
	}
	fakeNode := getNode()
	_, err := clientSet.CoreV1().Nodes().Create(ctx, fakeNode, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Failed to create  node with err %s", err)
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
}
