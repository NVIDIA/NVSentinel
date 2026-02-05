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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

func TestRemediationController_Reconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = nvsentinelv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)

	now := metav1.Now()

	tests := []struct {
		name             string
		healthEvent      *nvsentinelv1alpha1.HealthEvent
		node             *corev1.Node
		defaultStrategy  RemediationStrategy
		wantPhase        nvsentinelv1alpha1.HealthEventPhase
		wantSkip         bool
		wantRebootJob    bool
		wantNodeDeleted  bool
		wantAnnotation   string
	}{
		{
			name: "drained event with manual strategy should transition to remediated",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-remediation-1",
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:            "test",
					NodeName:          "worker-1",
					ComponentClass:    "GPU",
					CheckName:         "test-check",
					IsFatal:           true,
					DetectedAt:        now,
					RecommendedAction: nvsentinelv1alpha1.ActionContactSupport,
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseDrained,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
			},
			defaultStrategy: StrategyManual,
			wantPhase:       nvsentinelv1alpha1.PhaseRemediated,
		},
		{
			name: "event not in drained phase should be skipped",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-remediation-2",
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:         "test",
					NodeName:       "worker-1",
					ComponentClass: "GPU",
					CheckName:      "test-check",
					IsFatal:        true,
					DetectedAt:     now,
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseQuarantined,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
			},
			defaultStrategy: StrategyManual,
			wantPhase:       nvsentinelv1alpha1.PhaseQuarantined,
			wantSkip:        true,
		},
		{
			name: "drained event with annotation override should use specified strategy",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-remediation-3",
					Annotations: map[string]string{
						"nvsentinel.nvidia.com/remediation-strategy": "none",
					},
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:            "test",
					NodeName:          "worker-1",
					ComponentClass:    "GPU",
					CheckName:         "test-check",
					IsFatal:           true,
					DetectedAt:        now,
					RecommendedAction: nvsentinelv1alpha1.ActionRestartVM, // Would normally be reboot
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseDrained,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
			},
			defaultStrategy: StrategyReboot,
			wantPhase:       nvsentinelv1alpha1.PhaseRemediated,
		},
		{
			name: "drained event with GPU reset action should request reset",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-remediation-4",
					Annotations: map[string]string{},
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:            "test",
					NodeName:          "worker-1",
					ComponentClass:    "GPU",
					CheckName:         "test-check",
					IsFatal:           false,
					DetectedAt:        now,
					RecommendedAction: nvsentinelv1alpha1.ActionComponentReset,
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseDrained,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
			},
			defaultStrategy: StrategyManual,
			wantPhase:       nvsentinelv1alpha1.PhaseRemediated,
			wantAnnotation:  "nvsentinel.nvidia.com/gpu-reset-requested",
		},
		{
			name: "drained event with reboot action should create reboot job",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-remediation-5",
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:            "test",
					NodeName:          "worker-1",
					ComponentClass:    "GPU",
					CheckName:         "test-check",
					IsFatal:           true,
					DetectedAt:        now,
					RecommendedAction: nvsentinelv1alpha1.ActionRestartVM,
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseDrained,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
			},
			defaultStrategy: StrategyManual,
			wantPhase:       nvsentinelv1alpha1.PhaseRemediated,
			wantRebootJob:   true,
		},
		{
			name: "drained event with terminate action and providerID should delete node",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-remediation-6",
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:            "test",
					NodeName:          "worker-1",
					ComponentClass:    "GPU",
					CheckName:         "test-check",
					IsFatal:           true,
					DetectedAt:        now,
					RecommendedAction: nvsentinelv1alpha1.ActionReplaceVM,
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseDrained,
				},
			},
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "worker-1",
				},
				Spec: corev1.NodeSpec{
					ProviderID: "aws:///us-west-2a/i-1234567890abcdef0",
				},
			},
			defaultStrategy: StrategyManual,
			wantPhase:       nvsentinelv1alpha1.PhaseRemediated,
			wantNodeDeleted: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []runtime.Object{tt.healthEvent}
			if tt.node != nil {
				objects = append(objects, tt.node)
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithRuntimeObjects(objects...).
				WithStatusSubresource(&nvsentinelv1alpha1.HealthEvent{}).
				Build()

			recorder := record.NewFakeRecorder(10)

			r := &RemediationController{
				Client:             fakeClient,
				Scheme:             scheme,
				Recorder:           recorder,
				DefaultStrategy:    tt.defaultStrategy,
				RebootJobNamespace: "nvsentinel-system",
				RebootJobImage:     "busybox:latest",
				RebootJobTTL:       time.Hour,
			}

			ctx := context.Background()
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: tt.healthEvent.Name,
				},
			}

			_, err := r.Reconcile(ctx, req)
			if err != nil {
				t.Errorf("Reconcile() error = %v", err)
				return
			}

			// Verify phase
			var event nvsentinelv1alpha1.HealthEvent
			if err := fakeClient.Get(ctx, req.NamespacedName, &event); err != nil {
				t.Fatalf("Failed to get HealthEvent: %v", err)
			}

			if event.Status.Phase != tt.wantPhase {
				t.Errorf("Phase = %v, want %v", event.Status.Phase, tt.wantPhase)
			}

			// Verify annotation if expected
			if tt.wantAnnotation != "" {
				if _, ok := event.Annotations[tt.wantAnnotation]; !ok {
					t.Errorf("Expected annotation %s not found", tt.wantAnnotation)
				}
			}

			// Verify reboot job if expected
			if tt.wantRebootJob {
				var job batchv1.Job
				jobName := "reboot-" + tt.healthEvent.Name
				err := fakeClient.Get(ctx, types.NamespacedName{
					Name:      jobName,
					Namespace: "nvsentinel-system",
				}, &job)
				if err != nil {
					t.Errorf("Expected reboot job %s not found: %v", jobName, err)
				}
			}

			// Verify node deletion if expected
			if tt.wantNodeDeleted && tt.node != nil {
				var node corev1.Node
				err := fakeClient.Get(ctx, types.NamespacedName{Name: tt.node.Name}, &node)
				if err == nil {
					t.Error("Expected node to be deleted, but it still exists")
				}
			}
		})
	}
}

func TestRemediationController_DetermineStrategy(t *testing.T) {
	tests := []struct {
		name            string
		event           *nvsentinelv1alpha1.HealthEvent
		defaultStrategy RemediationStrategy
		wantStrategy    RemediationStrategy
	}{
		{
			name: "annotation override takes precedence",
			event: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"nvsentinel.nvidia.com/remediation-strategy": "reboot",
					},
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					RecommendedAction: nvsentinelv1alpha1.ActionNone,
				},
			},
			defaultStrategy: StrategyManual,
			wantStrategy:    StrategyReboot,
		},
		{
			name: "RestartVM maps to reboot",
			event: &nvsentinelv1alpha1.HealthEvent{
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					RecommendedAction: nvsentinelv1alpha1.ActionRestartVM,
				},
			},
			defaultStrategy: StrategyManual,
			wantStrategy:    StrategyReboot,
		},
		{
			name: "RestartBM maps to reboot",
			event: &nvsentinelv1alpha1.HealthEvent{
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					RecommendedAction: nvsentinelv1alpha1.ActionRestartBM,
				},
			},
			defaultStrategy: StrategyManual,
			wantStrategy:    StrategyReboot,
		},
		{
			name: "ReplaceVM maps to terminate",
			event: &nvsentinelv1alpha1.HealthEvent{
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					RecommendedAction: nvsentinelv1alpha1.ActionReplaceVM,
				},
			},
			defaultStrategy: StrategyManual,
			wantStrategy:    StrategyTerminate,
		},
		{
			name: "ComponentReset maps to gpu-reset",
			event: &nvsentinelv1alpha1.HealthEvent{
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					RecommendedAction: nvsentinelv1alpha1.ActionComponentReset,
				},
			},
			defaultStrategy: StrategyManual,
			wantStrategy:    StrategyGPUReset,
		},
		{
			name: "ActionNone maps to none",
			event: &nvsentinelv1alpha1.HealthEvent{
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					RecommendedAction: nvsentinelv1alpha1.ActionNone,
				},
			},
			defaultStrategy: StrategyManual,
			wantStrategy:    StrategyNone,
		},
		{
			name: "ContactSupport maps to manual",
			event: &nvsentinelv1alpha1.HealthEvent{
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					RecommendedAction: nvsentinelv1alpha1.ActionContactSupport,
				},
			},
			defaultStrategy: StrategyReboot,
			wantStrategy:    StrategyManual,
		},
		{
			name: "Unknown action uses default",
			event: &nvsentinelv1alpha1.HealthEvent{
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					RecommendedAction: nvsentinelv1alpha1.ActionUnknown,
				},
			},
			defaultStrategy: StrategyReboot,
			wantStrategy:    StrategyReboot,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &RemediationController{
				DefaultStrategy: tt.defaultStrategy,
			}

			got := r.determineStrategy(tt.event)
			if got != tt.wantStrategy {
				t.Errorf("determineStrategy() = %v, want %v", got, tt.wantStrategy)
			}
		})
	}
}

func TestRemediationController_TerminateWithoutProviderID(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = nvsentinelv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)

	now := metav1.Now()

	// Event that would trigger terminate, but node has no providerID
	healthEvent := &nvsentinelv1alpha1.HealthEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-no-provider",
		},
		Spec: nvsentinelv1alpha1.HealthEventSpec{
			Source:            "test",
			NodeName:          "worker-1",
			ComponentClass:    "GPU",
			CheckName:         "test-check",
			IsFatal:           true,
			DetectedAt:        now,
			RecommendedAction: nvsentinelv1alpha1.ActionReplaceVM,
		},
		Status: nvsentinelv1alpha1.HealthEventStatus{
			Phase: nvsentinelv1alpha1.PhaseDrained,
		},
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-1",
		},
		// No ProviderID set
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(healthEvent, node).
		WithStatusSubresource(&nvsentinelv1alpha1.HealthEvent{}).
		Build()

	recorder := record.NewFakeRecorder(10)

	r := &RemediationController{
		Client:          fakeClient,
		Scheme:          scheme,
		Recorder:        recorder,
		DefaultStrategy: StrategyManual,
	}

	ctx := context.Background()
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name: healthEvent.Name,
		},
	}

	// Should not error (requeues for retry)
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Errorf("Reconcile() unexpected error = %v", err)
	}

	// Event should NOT be in Remediated phase (remediation failed)
	var event nvsentinelv1alpha1.HealthEvent
	if err := fakeClient.Get(ctx, req.NamespacedName, &event); err != nil {
		t.Fatalf("Failed to get HealthEvent: %v", err)
	}

	// Phase should still be Drained (remediation failed)
	if event.Status.Phase != nvsentinelv1alpha1.PhaseDrained {
		t.Errorf("Phase = %v, want Drained (remediation should fail)", event.Status.Phase)
	}

	// Should have a failed condition
	cond := GetCondition(event.Status.Conditions, nvsentinelv1alpha1.ConditionRemediated)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Error("Expected Remediated condition to be False")
	}
}
