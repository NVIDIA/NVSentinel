package ringbuffer

import (
	"context"
	"os"
	"testing"
	"time"

	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/client-go/kubernetes/fake"
)

var (
	clientSet  *fake.Clientset
	ctx        context.Context
	ringBuffer *RingBuffer
)

type healthEvents struct {
	healthEvent               *platformconnector.HealthEvent
	expectedHealthEventOutput string
}

func TestMain(m *testing.M) {
	clientSet = fake.NewSimpleClientset()
	ctx = context.Background()
	exitVal := m.Run()
	os.Exit(exitVal)
}

func TestNewRingBuffer(t *testing.T) {
	ringBuffer = NewRingBuffer("ringbuffer", ctx)
	if ringBuffer == nil {
		t.Errorf("Not able to initialize ringBuffer")
	}
}

func TestRingBuffer_Queue(t *testing.T) {
	healthEventsList := []healthEvents{
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
			},
			expectedHealthEventOutput: "GpuXidError",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuThermalWatch",
				IsHealthy:          false,
				Message:            "DCGM_FR_EC_HARDWARE_MEMORY: 0",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"ThermalWatchWarn"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
			},
			expectedHealthEventOutput: "GpuThermalWatch",
		},
		{
			healthEvent: &platformconnector.HealthEvent{
				CheckName:          "GpuPcieWatch",
				IsHealthy:          false,
				Message:            "DCGM_FR_PCI_REPLAY_RATE: 0",
				EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
				ErrorCode:          []string{"PcieWatchWarn"},
				IsFatal:            true,
				GeneratedTimestamp: timestamppb.New(time.Now()),
				ComponentClass:     "gpu",
			},
			expectedHealthEventOutput: "GpuPcieWatch",
		},
	}
	for _, healthEvent := range healthEventsList {
		ringBuffer.Enqueue(healthEvent.healthEvent)
	}

	for testCase, healthEvent := range healthEventsList {
		item := ringBuffer.Dequeue()

		if item.CheckName != healthEvent.expectedHealthEventOutput {
			t.Errorf("Testcase %d. The expected healthEvent %s is not matching with the currentEvent %s from the queue", testCase, healthEvent.expectedHealthEventOutput, item.CheckName)
		}

		queueSize := ringBuffer.healthMetricQueue.Len()
		if queueSize != len(healthEventsList)-testCase-1 {
			t.Errorf("queueSize %d is not matching with expectedQueueSize %d ", queueSize, len(healthEventsList)-testCase)
		}
		ringBuffer.HealthMetricEleProcessingCompleted(item)
	}
}
