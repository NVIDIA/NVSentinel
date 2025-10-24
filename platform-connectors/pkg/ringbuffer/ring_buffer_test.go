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

package ringbuffer

import (
	"context"
	"os"
	"testing"
	"time"

	platformconnector "github.com/nvidia/nvsentinel/data-models/pkg/protos"
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
		healthEvents := platformconnector.HealthEvents{Version: 1, Events: make([]*platformconnector.HealthEvent, 0)}
		healthEvents.Events = append(healthEvents.Events, healthEvent.healthEvent)
		ringBuffer.Enqueue(&healthEvents)
	}

	for testCase, healthEvent := range healthEventsList {
		item := ringBuffer.Dequeue()
		for _, healthEventItem := range item.Events {
			if healthEventItem.CheckName != healthEvent.expectedHealthEventOutput {
				t.Errorf("Testcase %d. The expected healthEvent %s is not matching with the currentEvent %s from the queue", testCase, healthEvent.expectedHealthEventOutput, healthEventItem.CheckName)
			}

			queueSize := ringBuffer.healthMetricQueue.Len()
			if queueSize != len(healthEventsList)-testCase-1 {
				t.Errorf("queueSize %d is not matching with expectedQueueSize %d ", queueSize, len(healthEventsList)-testCase)
			}
			ringBuffer.HealthMetricEleProcessingCompleted(item)
		}
	}
}

func TestRingBuffer_DequeueWithCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ringBuffer := NewRingBuffer("testCancelledContext", ctx)

	cancel()

	healthEvent := &platformconnector.HealthEvent{
		CheckName:          "GpuXidError",
		IsHealthy:          false,
		EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
		ErrorCode:          []string{"44"},
		IsFatal:            true,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		ComponentClass:     "gpu",
	}
	healthEvents := platformconnector.HealthEvents{Version: 1, Events: []*platformconnector.HealthEvent{healthEvent}}
	ringBuffer.Enqueue(&healthEvents)

	result := ringBuffer.Dequeue()
	if result != nil {
		t.Errorf("Expected nil when context is cancelled, got %+v", result)
	}
}

func TestRingBuffer_HealthMetricEleProcessingFailed(t *testing.T) {
	ctx := context.Background()
	ringBuffer := NewRingBuffer("testProcessingFailed", ctx)

	healthEvent := &platformconnector.HealthEvent{
		CheckName:          "GpuXidError",
		IsHealthy:          false,
		EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
		ErrorCode:          []string{"44"},
		IsFatal:            true,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		ComponentClass:     "gpu",
	}
	healthEvents := platformconnector.HealthEvents{Version: 1, Events: []*platformconnector.HealthEvent{healthEvent}}

	ringBuffer.Enqueue(&healthEvents)
	item := ringBuffer.Dequeue()

	ringBuffer.HealthMetricEleProcessingFailed(item)

	if ringBuffer.CurrentLength() != 0 {
		t.Errorf("Expected queue length 0 after marking as failed, got %d", ringBuffer.CurrentLength())
	}
}

func TestRingBuffer_ShutDown(t *testing.T) {
	ctx := context.Background()
	ringBuffer := NewRingBuffer("testShutdown", ctx)

	healthEvent := &platformconnector.HealthEvent{
		CheckName:          "GpuXidError",
		IsHealthy:          false,
		EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
		ErrorCode:          []string{"44"},
		IsFatal:            true,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		ComponentClass:     "gpu",
	}
	healthEvents := platformconnector.HealthEvents{Version: 1, Events: []*platformconnector.HealthEvent{healthEvent}}
	ringBuffer.Enqueue(&healthEvents)

	ringBuffer.ShutDownHealthMetricQueue()

	result := ringBuffer.Dequeue()
	if result == nil {
		t.Errorf("Expected to get the enqueued item, got nil")
	}

	result2 := ringBuffer.Dequeue()
	if result2 != nil {
		t.Errorf("Expected nil on second dequeue after shutdown, got %+v", result2)
	}
}

func TestRingBuffer_CurrentLength(t *testing.T) {
	ctx := context.Background()
	ringBuffer := NewRingBuffer("testCurrentLength", ctx)

	if ringBuffer.CurrentLength() != 0 {
		t.Errorf("Expected initial length 0, got %d", ringBuffer.CurrentLength())
	}

	for range 3 {
		healthEvent := &platformconnector.HealthEvent{
			CheckName:          "GpuXidError",
			IsHealthy:          false,
			EntitiesImpacted:   []*platformconnector.Entity{{EntityType: "GPU", EntityValue: "0"}},
			ErrorCode:          []string{"44"},
			IsFatal:            true,
			GeneratedTimestamp: timestamppb.New(time.Now()),
			ComponentClass:     "gpu",
		}
		healthEvents := platformconnector.HealthEvents{Version: 1, Events: []*platformconnector.HealthEvent{healthEvent}}
		ringBuffer.Enqueue(&healthEvents)
	}

	if ringBuffer.CurrentLength() != 3 {
		t.Errorf("Expected length 3 after adding 3 events, got %d", ringBuffer.CurrentLength())
	}
}
