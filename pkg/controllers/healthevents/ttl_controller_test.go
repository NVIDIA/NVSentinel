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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

func TestTTLController_Reconcile(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = nvsentinelv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	now := metav1.Now()
	oldTime := metav1.NewTime(now.Add(-200 * time.Hour)) // 200 hours ago (past 168h default TTL)
	recentTime := metav1.NewTime(now.Add(-1 * time.Hour)) // 1 hour ago

	tests := []struct {
		name           string
		healthEvent    *nvsentinelv1alpha1.HealthEvent
		ttl            time.Duration
		wantDeleted    bool
		wantRequeue    bool
	}{
		{
			name: "expired resolved event should be deleted",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-ttl-1",
					CreationTimestamp: oldTime,
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:         "test",
					NodeName:       "worker-1",
					ComponentClass: "GPU",
					CheckName:      "test-check",
					DetectedAt:     oldTime,
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase:      nvsentinelv1alpha1.PhaseResolved,
					ResolvedAt: &oldTime,
				},
			},
			ttl:         168 * time.Hour,
			wantDeleted: true,
			wantRequeue: false,
		},
		{
			name: "recent resolved event should be requeued",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-ttl-2",
					CreationTimestamp: recentTime,
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:         "test",
					NodeName:       "worker-1",
					ComponentClass: "GPU",
					CheckName:      "test-check",
					DetectedAt:     recentTime,
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase:      nvsentinelv1alpha1.PhaseResolved,
					ResolvedAt: &recentTime,
				},
			},
			ttl:         168 * time.Hour,
			wantDeleted: false,
			wantRequeue: true,
		},
		{
			name: "non-terminal event should be skipped",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-ttl-3",
					CreationTimestamp: oldTime,
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:         "test",
					NodeName:       "worker-1",
					ComponentClass: "GPU",
					CheckName:      "test-check",
					DetectedAt:     oldTime,
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase: nvsentinelv1alpha1.PhaseQuarantined,
				},
			},
			ttl:         168 * time.Hour,
			wantDeleted: false,
			wantRequeue: false,
		},
		{
			name: "expired cancelled event should be deleted",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-ttl-4",
					CreationTimestamp: oldTime,
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:         "test",
					NodeName:       "worker-1",
					ComponentClass: "GPU",
					CheckName:      "test-check",
					DetectedAt:     oldTime,
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase:      nvsentinelv1alpha1.PhaseCancelled,
					ResolvedAt: &oldTime,
				},
			},
			ttl:         168 * time.Hour,
			wantDeleted: true,
			wantRequeue: false,
		},
		{
			name: "very long TTL should not delete recent event",
			healthEvent: &nvsentinelv1alpha1.HealthEvent{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test-ttl-5",
					CreationTimestamp: recentTime,
				},
				Spec: nvsentinelv1alpha1.HealthEventSpec{
					Source:         "test",
					NodeName:       "worker-1",
					ComponentClass: "GPU",
					CheckName:      "test-check",
					DetectedAt:     recentTime,
				},
				Status: nvsentinelv1alpha1.HealthEventStatus{
					Phase:      nvsentinelv1alpha1.PhaseResolved,
					ResolvedAt: &recentTime,
				},
			},
			ttl:         8760 * time.Hour, // 1 year - won't delete
			wantDeleted: false,
			wantRequeue: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.healthEvent).
				Build()

			recorder := record.NewFakeRecorder(10)

			r := &TTLController{
				Client:     fakeClient,
				Scheme:     scheme,
				Recorder:   recorder,
				DefaultTTL: tt.ttl,
			}

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

			// Check if event was deleted
			var event nvsentinelv1alpha1.HealthEvent
			err = fakeClient.Get(ctx, req.NamespacedName, &event)

			if tt.wantDeleted {
				if !apierrors.IsNotFound(err) {
					t.Error("Expected event to be deleted, but it still exists")
				}
			} else {
				if apierrors.IsNotFound(err) {
					t.Error("Expected event to exist, but it was deleted")
				}
			}

			// Check requeue
			if tt.wantRequeue && result.RequeueAfter == 0 {
				t.Error("Expected requeue, but RequeueAfter is 0")
			}
			if !tt.wantRequeue && result.RequeueAfter > 0 {
				t.Errorf("Did not expect requeue, but RequeueAfter is %v", result.RequeueAfter)
			}
		})
	}
}

func TestTTLController_GetPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	tests := []struct {
		name       string
		configMap  *corev1.ConfigMap
		wantTTL    time.Duration
		wantMode   string
	}{
		{
			name:      "no ConfigMap returns defaults",
			configMap: nil,
			wantTTL:   168 * time.Hour,
			wantMode:  "standard",
		},
		{
			name: "ConfigMap with custom TTL",
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-policy",
					Namespace: "test-ns",
				},
				Data: map[string]string{
					"ttlAfterResolved": "720h",
					"retentionMode":    "standard",
				},
			},
			wantTTL:  720 * time.Hour,
			wantMode: "standard",
		},
		{
			name: "ConfigMap with compliance mode",
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-policy",
					Namespace: "test-ns",
				},
				Data: map[string]string{
					"ttlAfterResolved":    "168h",
					"retentionMode":       "compliance",
					"complianceRetention": "2160h",
				},
			},
			wantTTL:  168 * time.Hour,
			wantMode: "compliance",
		},
		{
			name: "ConfigMap with zero TTL disables cleanup",
			configMap: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-policy",
					Namespace: "test-ns",
				},
				Data: map[string]string{
					"ttlAfterResolved": "0",
					"retentionMode":    "standard",
				},
			},
			wantTTL:  0,
			wantMode: "standard",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.configMap != nil {
				builder = builder.WithObjects(tt.configMap)
			}
			fakeClient := builder.Build()

			r := &TTLController{
				Client:                   fakeClient,
				PolicyConfigMapName:      "test-policy",
				PolicyConfigMapNamespace: "test-ns",
			}

			ctx := context.Background()
			policy := r.getPolicy(ctx)

			if policy.TTLAfterResolved != tt.wantTTL {
				t.Errorf("getPolicy() TTL = %v, want %v", policy.TTLAfterResolved, tt.wantTTL)
			}
			if policy.RetentionMode != tt.wantMode {
				t.Errorf("getPolicy() Mode = %v, want %v", policy.RetentionMode, tt.wantMode)
			}
		})
	}
}

func TestDefaultTTLPolicy(t *testing.T) {
	policy := DefaultTTLPolicy()

	if policy.TTLAfterResolved != 168*time.Hour {
		t.Errorf("DefaultTTLPolicy() TTLAfterResolved = %v, want 168h", policy.TTLAfterResolved)
	}
	if policy.RetentionMode != "standard" {
		t.Errorf("DefaultTTLPolicy() RetentionMode = %v, want standard", policy.RetentionMode)
	}
	if policy.ComplianceRetention != 2160*time.Hour {
		t.Errorf("DefaultTTLPolicy() ComplianceRetention = %v, want 2160h", policy.ComplianceRetention)
	}
	if policy.BatchSize != 100 {
		t.Errorf("DefaultTTLPolicy() BatchSize = %v, want 100", policy.BatchSize)
	}
	if policy.CleanupInterval != 5*time.Minute {
		t.Errorf("DefaultTTLPolicy() CleanupInterval = %v, want 5m", policy.CleanupInterval)
	}
}

func TestTTLController_CleanupExpiredEvents(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = nvsentinelv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	oldTime := metav1.NewTime(time.Now().Add(-200 * time.Hour))
	recentTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))

	// Create mix of events
	events := []client.Object{
		&nvsentinelv1alpha1.HealthEvent{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "expired-1",
				CreationTimestamp: oldTime,
			},
			Spec: nvsentinelv1alpha1.HealthEventSpec{
				Source:         "test",
				NodeName:       "worker-1",
				ComponentClass: "GPU",
				CheckName:      "test",
				DetectedAt:     oldTime,
			},
			Status: nvsentinelv1alpha1.HealthEventStatus{
				Phase:      nvsentinelv1alpha1.PhaseResolved,
				ResolvedAt: &oldTime,
			},
		},
		&nvsentinelv1alpha1.HealthEvent{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "expired-2",
				CreationTimestamp: oldTime,
			},
			Spec: nvsentinelv1alpha1.HealthEventSpec{
				Source:         "test",
				NodeName:       "worker-1",
				ComponentClass: "GPU",
				CheckName:      "test",
				DetectedAt:     oldTime,
			},
			Status: nvsentinelv1alpha1.HealthEventStatus{
				Phase:      nvsentinelv1alpha1.PhaseCancelled,
				ResolvedAt: &oldTime,
			},
		},
		&nvsentinelv1alpha1.HealthEvent{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "recent-1",
				CreationTimestamp: recentTime,
			},
			Spec: nvsentinelv1alpha1.HealthEventSpec{
				Source:         "test",
				NodeName:       "worker-1",
				ComponentClass: "GPU",
				CheckName:      "test",
				DetectedAt:     recentTime,
			},
			Status: nvsentinelv1alpha1.HealthEventStatus{
				Phase:      nvsentinelv1alpha1.PhaseResolved,
				ResolvedAt: &recentTime,
			},
		},
		&nvsentinelv1alpha1.HealthEvent{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "active-1",
				CreationTimestamp: oldTime,
			},
			Spec: nvsentinelv1alpha1.HealthEventSpec{
				Source:         "test",
				NodeName:       "worker-1",
				ComponentClass: "GPU",
				CheckName:      "test",
				DetectedAt:     oldTime,
			},
			Status: nvsentinelv1alpha1.HealthEventStatus{
				Phase: nvsentinelv1alpha1.PhaseQuarantined,
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(events...).
		Build()

	r := &TTLController{
		Client:     fakeClient,
		DefaultTTL: 168 * time.Hour,
	}

	ctx := context.Background()
	log := ctrl.Log.WithName("test")

	r.cleanupExpiredEvents(ctx, log)

	// Verify expired events were deleted
	var remaining nvsentinelv1alpha1.HealthEventList
	if err := fakeClient.List(ctx, &remaining); err != nil {
		t.Fatalf("Failed to list events: %v", err)
	}

	// Should have 2 remaining: recent-1 (not expired) and active-1 (not terminal)
	if len(remaining.Items) != 2 {
		t.Errorf("Expected 2 remaining events, got %d", len(remaining.Items))
		for _, e := range remaining.Items {
			t.Logf("Remaining: %s (phase=%s)", e.Name, e.Status.Phase)
		}
	}
}
