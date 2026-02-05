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

package healthevents

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

func TestQuarantineController_Reconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = nvsentinelv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	tests := []struct {
		name              string
		healthEvent       *nvsentinelv1alpha1.HealthEvent
		node              *corev1.Node
		wantPhase         nvsentinelv1alpha1.HealthEventPhase
		wantNodeCordoned  bool
		wantRequeue       bool
	}{
		{
			name: "fatal event should quarantine node",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-event-1",
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:         "nvml-health-monitor",
					NodeName:       "worker-1",
					ComponentClass: "GPU",
					CheckName:      "xid-error-check",
					IsFatal:        true,
					IsHealthy:      false,
					DetectedAt:     metav1.Now(),
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseNew,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
				Spec: corev1.NodeSpec{
					Unschedulable: false,
				},
			},
			wantPhase:        nvsentinelv1alpha1.PhaseQuarantined,
			wantNodeCordoned: true,
			wantRequeue:      false,
		},
		{
			name: "non-fatal event should be resolved",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-event-2",
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:         "nvml-health-monitor",
					NodeName:       "worker-1",
					ComponentClass: "GPU",
					CheckName:      "memory-ecc-check",
					IsFatal:        false,
					IsHealthy:      false,
					DetectedAt:     metav1.Now(),
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseNew,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
			},
			wantPhase:        nvsentinelv1alpha1.PhaseResolved,
			wantNodeCordoned: false,
			wantRequeue:      false,
		},
		{
			name: "already quarantined phase should be skipped",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-event-3",
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:         "nvml-health-monitor",
					NodeName:       "worker-1",
					ComponentClass: "GPU",
					CheckName:      "xid-error-check",
					IsFatal:        true,
					IsHealthy:      false,
					DetectedAt:     metav1.Now(),
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseQuarantined,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
				Spec: corev1.NodeSpec{
					Unschedulable: true,
				},
			},
			wantPhase:        nvsentinelv1alpha1.PhaseQuarantined,
			wantNodeCordoned: true,
			wantRequeue:      false,
		},
		{
			name: "event with skip override should be cancelled",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-event-4",
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:         "nvml-health-monitor",
					NodeName:       "worker-1",
					ComponentClass: "GPU",
					CheckName:      "xid-error-check",
					IsFatal:        true,
					IsHealthy:      false,
					DetectedAt:     metav1.Now(),
					Overrides: &nvsentinelv1alpha1.BehaviourOverrides{
						Quarantine: &nvsentinelv1alpha1.ActionOverride{
							Skip: true,
						},
					},
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseNew,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
			},
			wantPhase:        nvsentinelv1alpha1.PhaseCancelled,
			wantNodeCordoned: false,
			wantRequeue:      false,
		},
		{
			name: "already cordoned node should transition to quarantined",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-event-5",
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:         "nvml-health-monitor",
					NodeName:       "worker-1",
					ComponentClass: "GPU",
					CheckName:      "xid-error-check",
					IsFatal:        true,
					IsHealthy:      false,
					DetectedAt:     metav1.Now(),
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseNew,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
				Spec: corev1.NodeSpec{
					Unschedulable: true, // Already cordoned
				},
			},
			wantPhase:        nvsentinelv1alpha1.PhaseQuarantined,
			wantNodeCordoned: true,
			wantRequeue:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake client with objects
			objs := []client.Object{tt.healthEvent}
			if tt.node != nil {
				objs = append(objs, tt.node)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				WithStatusSubresource(&nvsentinelv1alpha1.HealthEvent{}).
				Build()

			recorder := record.NewFakeRecorder(10)

			r := &QuarantineController{
				Client:   fakeClient,
				Scheme:   scheme,
				Recorder: recorder,
			}

			// Reconcile
			ctx := context.Background()
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: tt.healthEvent.Name,
				},
			}

			result, err := r.Reconcile(ctx, req)
			if err != nil {
				t.Errorf("Reconcile() error = %v", err)
				return
			}

			if result.Requeue != tt.wantRequeue {
				t.Errorf("Reconcile() requeue = %v, want %v", result.Requeue, tt.wantRequeue)
			}

			// Check HealthEvent phase
			var updatedEvent nvsentinelv1alpha1.HealthEvent
			if err := fakeClient.Get(ctx, req.NamespacedName, &updatedEvent); err != nil {
				t.Errorf("Failed to get updated HealthEvent: %v", err)
				return
			}

			if updatedEvent.Status.Phase != tt.wantPhase {
				t.Errorf("HealthEvent phase = %v, want %v", updatedEvent.Status.Phase, tt.wantPhase)
			}

			// Check node cordon status
			if tt.node != nil {
				var updatedNode corev1.Node
				if err := fakeClient.Get(ctx, types.NamespacedName{Name: tt.node.Name}, &updatedNode); err != nil {
					t.Errorf("Failed to get updated Node: %v", err)
					return
				}

				if updatedNode.Spec.Unschedulable != tt.wantNodeCordoned {
					t.Errorf("Node.Spec.Unschedulable = %v, want %v",
						updatedNode.Spec.Unschedulable, tt.wantNodeCordoned)
				}
			}
		})
	}
}

func TestQuarantineController_Reconcile_NodeNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = nvsentinelv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	healthEvent := &nvsentinelv1alpha1.HealthEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-event-missing-node",
		},
		Spec: nvsentinelv1alpha1.HealthEventSpec{
			Source:         "nvml-health-monitor",
			NodeName:       "nonexistent-node",
			ComponentClass: "GPU",
			CheckName:      "xid-error-check",
			IsFatal:        true,
			IsHealthy:      false,
			DetectedAt:     metav1.Now(),
		},
		Status: nvsentinelv1alpha1.HealthEventStatus{
			Phase: nvsentinelv1alpha1.PhaseNew,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(healthEvent).
		WithStatusSubresource(&nvsentinelv1alpha1.HealthEvent{}).
		Build()

	recorder := record.NewFakeRecorder(10)

	r := &QuarantineController{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: recorder,
	}

	ctx := context.Background()
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: healthEvent.Name,
		},
	}

	// Should not error - just records warning event
	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Errorf("Reconcile() error = %v, want nil", err)
	}
	if result.Requeue {
		t.Errorf("Reconcile() requeue = %v, want false", result.Requeue)
	}

	// Check that a warning event was recorded
	select {
	case event := <-recorder.Events:
		if event == "" {
			t.Error("Expected warning event to be recorded")
		}
	case <-time.After(time.Second):
		t.Error("Timeout waiting for event")
	}
}

func TestConditionHelpers(t *testing.T) {
	now := metav1.Now()

	t.Run("SetCondition adds new condition", func(t *testing.T) {
		var conditions []nvsentinelv1alpha1.HealthEventCondition

		SetCondition(&conditions, nvsentinelv1alpha1.HealthEventCondition{
			Type:               nvsentinelv1alpha1.ConditionDetected,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "Test",
		})

		if len(conditions) != 1 {
			t.Errorf("Expected 1 condition, got %d", len(conditions))
		}
		if conditions[0].Type != nvsentinelv1alpha1.ConditionDetected {
			t.Errorf("Expected Detected condition, got %s", conditions[0].Type)
		}
	})

	t.Run("SetCondition updates existing condition", func(t *testing.T) {
		conditions := []nvsentinelv1alpha1.HealthEventCondition{
			{
				Type:   nvsentinelv1alpha1.ConditionDetected,
				Status: metav1.ConditionFalse,
				Reason: "Old",
			},
		}

		SetCondition(&conditions, nvsentinelv1alpha1.HealthEventCondition{
			Type:               nvsentinelv1alpha1.ConditionDetected,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "New",
		})

		if len(conditions) != 1 {
			t.Errorf("Expected 1 condition, got %d", len(conditions))
		}
		if conditions[0].Status != metav1.ConditionTrue {
			t.Errorf("Expected True status, got %s", conditions[0].Status)
		}
		if conditions[0].Reason != "New" {
			t.Errorf("Expected New reason, got %s", conditions[0].Reason)
		}
	})

	t.Run("GetCondition returns nil for missing", func(t *testing.T) {
		conditions := []nvsentinelv1alpha1.HealthEventCondition{}
		c := GetCondition(conditions, nvsentinelv1alpha1.ConditionDetected)
		if c != nil {
			t.Error("Expected nil for missing condition")
		}
	})

	t.Run("IsConditionTrue", func(t *testing.T) {
		conditions := []nvsentinelv1alpha1.HealthEventCondition{
			{
				Type:   nvsentinelv1alpha1.ConditionDetected,
				Status: metav1.ConditionTrue,
			},
		}
		if !IsConditionTrue(conditions, nvsentinelv1alpha1.ConditionDetected) {
			t.Error("Expected IsConditionTrue to return true")
		}
		if IsConditionTrue(conditions, nvsentinelv1alpha1.ConditionRemediated) {
			t.Error("Expected IsConditionTrue to return false for missing condition")
		}
	})

	t.Run("RemoveCondition", func(t *testing.T) {
		conditions := []nvsentinelv1alpha1.HealthEventCondition{
			{Type: nvsentinelv1alpha1.ConditionDetected, Status: metav1.ConditionTrue},
			{Type: nvsentinelv1alpha1.ConditionRemediated, Status: metav1.ConditionFalse},
		}

		RemoveCondition(&conditions, nvsentinelv1alpha1.ConditionDetected)

		if len(conditions) != 1 {
			t.Errorf("Expected 1 condition after removal, got %d", len(conditions))
		}
		if conditions[0].Type != nvsentinelv1alpha1.ConditionRemediated {
			t.Errorf("Expected Remediated condition to remain, got %s", conditions[0].Type)
		}
	})
}
