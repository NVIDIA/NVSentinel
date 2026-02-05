// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package crdpublisher

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	devicev1alpha1 "github.com/nvidia/nvsentinel/api/device/v1alpha1"
	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

func TestPublisher_ExtractErrorInfo(t *testing.T) {
	p := &Publisher{
		config: Config{NodeName: "test-node"},
	}

	tests := []struct {
		name           string
		gpu            *devicev1alpha1.GPU
		wantErrorCodes []string
		wantMessage    string
		wantFatal      bool
	}{
		{
			name: "XID error should be fatal",
			gpu: &devicev1alpha1.GPU{
				Status: devicev1alpha1.GPUStatus{
					Conditions: []metav1.Condition{
						{
							Type:    "Healthy",
							Status:  metav1.ConditionFalse,
							Reason:  "XID79",
							Message: "GPU has fallen off the bus",
						},
					},
					RecommendedAction: "RESTART_BM",
				},
			},
			wantErrorCodes: []string{"79"},
			wantMessage:    "GPU has fallen off the bus",
			wantFatal:      true,
		},
		{
			name: "non-fatal error",
			gpu: &devicev1alpha1.GPU{
				Status: devicev1alpha1.GPUStatus{
					Conditions: []metav1.Condition{
						{
							Type:    "Healthy",
							Status:  metav1.ConditionFalse,
							Reason:  "HighTemperature",
							Message: "GPU temperature above threshold",
						},
					},
					RecommendedAction: "COMPONENT_RESET",
				},
			},
			wantErrorCodes: nil,
			wantMessage:    "GPU temperature above threshold",
			wantFatal:      false,
		},
		{
			name: "healthy GPU",
			gpu: &devicev1alpha1.GPU{
				Status: devicev1alpha1.GPUStatus{
					Conditions: []metav1.Condition{
						{
							Type:   "Healthy",
							Status: metav1.ConditionTrue,
						},
					},
				},
			},
			wantErrorCodes: nil,
			wantMessage:    "GPU reported unhealthy",
			wantFatal:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errorCodes, message, isFatal := p.extractErrorInfo(tt.gpu)

			if len(errorCodes) != len(tt.wantErrorCodes) {
				t.Errorf("extractErrorInfo() errorCodes = %v, want %v", errorCodes, tt.wantErrorCodes)
			}
			for i, code := range errorCodes {
				if code != tt.wantErrorCodes[i] {
					t.Errorf("extractErrorInfo() errorCodes[%d] = %v, want %v", i, code, tt.wantErrorCodes[i])
				}
			}

			if message != tt.wantMessage {
				t.Errorf("extractErrorInfo() message = %v, want %v", message, tt.wantMessage)
			}

			if isFatal != tt.wantFatal {
				t.Errorf("extractErrorInfo() isFatal = %v, want %v", isFatal, tt.wantFatal)
			}
		})
	}
}

func TestPublisher_DetermineCheckName(t *testing.T) {
	p := &Publisher{}

	tests := []struct {
		name string
		gpu  *devicev1alpha1.GPU
		want string
	}{
		{
			name: "XID error",
			gpu: &devicev1alpha1.GPU{
				Status: devicev1alpha1.GPUStatus{
					Conditions: []metav1.Condition{
						{Type: "Healthy", Status: metav1.ConditionFalse, Reason: "XID79"},
					},
				},
			},
			want: "xid-error-check",
		},
		{
			name: "ECC error",
			gpu: &devicev1alpha1.GPU{
				Status: devicev1alpha1.GPUStatus{
					Conditions: []metav1.Condition{
						{Type: "Healthy", Status: metav1.ConditionFalse, Reason: "ECCError"},
					},
				},
			},
			want: "memory-ecc-check",
		},
		{
			name: "thermal error",
			gpu: &devicev1alpha1.GPU{
				Status: devicev1alpha1.GPUStatus{
					Conditions: []metav1.Condition{
						{Type: "Healthy", Status: metav1.ConditionFalse, Reason: "ThermalThrottle"},
					},
				},
			},
			want: "thermal-check",
		},
		{
			name: "unknown error",
			gpu: &devicev1alpha1.GPU{
				Status: devicev1alpha1.GPUStatus{
					Conditions: []metav1.Condition{
						{Type: "Healthy", Status: metav1.ConditionFalse, Reason: "Unknown"},
					},
				},
			},
			want: "health-check",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.determineCheckName(tt.gpu); got != tt.want {
				t.Errorf("determineCheckName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPublisher_GenerateEventName(t *testing.T) {
	p := &Publisher{
		config: Config{NodeName: "worker-1"},
	}

	gpu := &devicev1alpha1.GPU{
		Spec: devicev1alpha1.GPUSpec{
			UUID: "GPU-abc123def456",
		},
	}

	name := p.generateEventName(gpu)

	// Should start with node name
	if name[:8] != "worker-1" {
		t.Errorf("generateEventName() should start with node name, got %s", name)
	}

	// Should contain part of UUID
	if len(name) < 20 {
		t.Errorf("generateEventName() too short: %s", name)
	}
}

func TestPublisher_OnGPUUnhealthy_WithFakeClient(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = nvsentinelv1alpha1.AddToScheme(scheme)
	_ = devicev1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	p := &Publisher{
		config: Config{
			NodeName:         "worker-1",
			Enabled:          true,
			DebounceInterval: 100 * time.Millisecond,
			Source:           "test",
		},
		client:      fakeClient,
		logger:      klog.NewKlogr(),
		lastPublish: make(map[string]time.Time),
		pendingGPUs: make(map[string]*devicev1alpha1.GPU),
	}

	gpu := &devicev1alpha1.GPU{
		Spec: devicev1alpha1.GPUSpec{
			UUID: "GPU-test-12345678",
		},
		Status: devicev1alpha1.GPUStatus{
			Conditions: []metav1.Condition{
				{
					Type:    "Healthy",
					Status:  metav1.ConditionFalse,
					Reason:  "XID79",
					Message: "Fatal GPU error",
				},
			},
			RecommendedAction: "RESTART_BM",
		},
	}

	ctx := context.Background()
	p.OnGPUUnhealthy(ctx, gpu)

	// Wait for async publish
	time.Sleep(200 * time.Millisecond)

	// Verify HealthEvent was created
	var events nvsentinelv1alpha1.HealthEventList
	if err := fakeClient.List(ctx, &events); err != nil {
		t.Fatalf("Failed to list HealthEvents: %v", err)
	}

	if len(events.Items) != 1 {
		t.Errorf("Expected 1 HealthEvent, got %d", len(events.Items))
		return
	}

	event := events.Items[0]
	if event.Spec.NodeName != "worker-1" {
		t.Errorf("HealthEvent.Spec.NodeName = %v, want worker-1", event.Spec.NodeName)
	}
	if !event.Spec.IsFatal {
		t.Error("HealthEvent.Spec.IsFatal should be true")
	}
	if event.Status.Phase != nvsentinelv1alpha1.PhaseNew {
		t.Errorf("HealthEvent.Status.Phase = %v, want New", event.Status.Phase)
	}
}

func TestPublisher_Debouncing(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = nvsentinelv1alpha1.AddToScheme(scheme)
	_ = devicev1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	p := &Publisher{
		config: Config{
			NodeName:         "worker-1",
			Enabled:          true,
			DebounceInterval: 500 * time.Millisecond,
			Source:           "test",
		},
		client:       fakeClient,
		logger:       klog.NewKlogr(),
		lastPublish:  make(map[string]time.Time),
		pendingGPUs:  make(map[string]*devicev1alpha1.GPU),
		debounceStop: make(chan struct{}),
	}

	gpu := &devicev1alpha1.GPU{
		Spec: devicev1alpha1.GPUSpec{
			UUID: "GPU-debounce-test",
		},
		Status: devicev1alpha1.GPUStatus{
			Conditions: []metav1.Condition{
				{Type: "Healthy", Status: metav1.ConditionFalse, Reason: "XID79"},
			},
			RecommendedAction: "RESTART_BM",
		},
	}

	ctx := context.Background()

	// First call should publish immediately
	p.OnGPUUnhealthy(ctx, gpu)
	time.Sleep(100 * time.Millisecond)

	// Second call should be debounced
	p.OnGPUUnhealthy(ctx, gpu)
	time.Sleep(100 * time.Millisecond)

	// Third call should still be debounced
	p.OnGPUUnhealthy(ctx, gpu)
	time.Sleep(100 * time.Millisecond)

	// Check only one event was created
	var events nvsentinelv1alpha1.HealthEventList
	if err := fakeClient.List(ctx, &events, &client.ListOptions{}); err != nil {
		t.Fatalf("Failed to list HealthEvents: %v", err)
	}

	if len(events.Items) != 1 {
		t.Errorf("Expected 1 HealthEvent (debounced), got %d", len(events.Items))
	}
}
