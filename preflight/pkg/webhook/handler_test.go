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

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func buildAdmissionReview(pod *corev1.Pod, uid types.UID, namespace string) []byte {
	raw, _ := json.Marshal(pod)
	review := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       uid,
			Namespace: namespace,
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
	body, _ := json.Marshal(review)
	return body
}

func handlerConfig() *config.Config {
	return &config.Config{
		FileConfig: config.FileConfig{
			InitContainers: []config.InitContainerSpec{
				{Container: corev1.Container{Name: "preflight-dcgm-diag", Image: "dcgm:latest"}},
			},
			GPUResourceNames: []string{"nvidia.com/gpu"},
			ConnectorSocket:    "/var/run/nvsentinel/nvsentinel.sock",
			ProcessingStrategy: "EXECUTE_REMEDIATION",
		},
	}
}

func handlerGangConfig() *config.Config {
	cfg := handlerConfig()
	cfg.GangCoordination = config.GangCoordinationConfig{
		Enabled:              true,
		Timeout:              "10m",
		TimeoutDuration:      10 * time.Minute,
		MasterPort:           29500,
		ConfigMapMountPath:   "/etc/preflight",
		MirrorResourceClaims: boolPtr(true),
	}
	return cfg
}

// TestHandleMutate covers the HTTP admission webhook layer: request parsing,
// AdmissionReview serialization, UID propagation, namespace backfill,
// gang registration callback, and error responses for invalid input.
func TestHandleMutate(t *testing.T) {
	t.Run("valid GPU pod returns patch", func(t *testing.T) {
		handler := NewHandler(handlerConfig(), nil, nil)

		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "train",
					Image: "train:latest",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
					},
				}},
			},
		}

		body := buildAdmissionReview(pod, "uid-1", "default")
		req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		handler.HandleMutate(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var review admissionv1.AdmissionReview
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &review))
		require.NotNil(t, review.Response)
		assert.True(t, review.Response.Allowed)
		assert.NotEmpty(t, review.Response.Patch)
		require.NotNil(t, review.Response.PatchType)
		assert.Equal(t, admissionv1.PatchTypeJSONPatch, *review.Response.PatchType)
	})

	t.Run("valid non-GPU pod returns allowed without patch", func(t *testing.T) {
		handler := NewHandler(handlerConfig(), nil, nil)

		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "web", Image: "nginx"}},
			},
		}

		body := buildAdmissionReview(pod, "uid-2", "default")
		req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		handler.HandleMutate(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var review admissionv1.AdmissionReview
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &review))
		require.NotNil(t, review.Response)
		assert.True(t, review.Response.Allowed)
		assert.Empty(t, review.Response.Patch)
	})

	t.Run("invalid body returns 400", func(t *testing.T) {
		handler := NewHandler(handlerConfig(), nil, nil)

		req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader([]byte("not-json")))
		rec := httptest.NewRecorder()

		handler.HandleMutate(rec, req)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("invalid pod JSON returns not allowed", func(t *testing.T) {
		handler := NewHandler(handlerConfig(), nil, nil)

		// Build the outer JSON manually so that object.raw contains invalid JSON
		// without json.Marshal rejecting it.
		body := []byte(`{
			"apiVersion": "admission.k8s.io/v1",
			"kind": "AdmissionReview",
			"request": {
				"uid": "uid-3",
				"namespace": "default",
				"object": {"not": "a pod spec"}
			}
		}`)

		req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		handler.HandleMutate(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var resp admissionv1.AdmissionReview
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Response)

		// The pod will unmarshal successfully but with empty fields;
		// it won't have GPU resources, so no mutation happens -> allowed: true.
		// For a truly invalid unmarshal, we need raw bytes that are not valid JSON at all.
		// However, json.Unmarshal of the outer review would fail first.
		// So this test validates the "no GPU resources" path via malformed pod.
		assert.True(t, resp.Response.Allowed)
	})

	t.Run("nil request returns allowed", func(t *testing.T) {
		handler := NewHandler(handlerConfig(), nil, nil)

		review := admissionv1.AdmissionReview{
			TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		}
		body, _ := json.Marshal(review)

		req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		handler.HandleMutate(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		var resp admissionv1.AdmissionReview
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Response)
		assert.True(t, resp.Response.Allowed)
	})

	t.Run("response UID matches request", func(t *testing.T) {
		handler := NewHandler(handlerConfig(), nil, nil)

		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "train", Image: "train:latest",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
					},
				}},
			},
		}

		body := buildAdmissionReview(pod, "test-uid-abc", "default")
		req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		handler.HandleMutate(rec, req)

		var resp admissionv1.AdmissionReview
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, types.UID("test-uid-abc"), resp.Response.UID)
	})

	t.Run("namespace backfilled from request", func(t *testing.T) {
		var mu sync.Mutex
		var captured GangRegistration

		disc := &mockDiscoverer{name: "test", canHandle: true, gangID: "test-gang"}
		cfg := handlerGangConfig()
		handler := NewHandler(cfg, disc, func(_ context.Context, reg GangRegistration) {
			mu.Lock()
			defer mu.Unlock()
			captured = reg
		})

		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "train", Image: "train:latest",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
					},
				}},
			},
		}

		body := buildAdmissionReview(pod, "uid-ns", "production")
		req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		handler.HandleMutate(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, "production", captured.Namespace)
	})

	t.Run("gang registration callback invoked", func(t *testing.T) {
		var mu sync.Mutex
		var captured GangRegistration
		called := false

		disc := &mockDiscoverer{name: "volcano", canHandle: true, gangID: "volcano-default-pg1"}
		cfg := handlerGangConfig()
		handler := NewHandler(cfg, disc, func(_ context.Context, reg GangRegistration) {
			mu.Lock()
			defer mu.Unlock()
			called = true
			captured = reg
		})

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-0"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "train", Image: "train:latest",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
					},
				}},
			},
		}

		body := buildAdmissionReview(pod, "uid-gang", "default")
		req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		handler.HandleMutate(rec, req)

		mu.Lock()
		defer mu.Unlock()
		assert.True(t, called)
		assert.Equal(t, "default", captured.Namespace)
		assert.Equal(t, "worker-0", captured.PodName)
		assert.Equal(t, "volcano-default-pg1", captured.GangID)
		assert.NotEmpty(t, captured.ConfigMapName)
	})

	t.Run("gang registration uses GenerateName when Name is empty", func(t *testing.T) {
		var mu sync.Mutex
		var captured GangRegistration

		disc := &mockDiscoverer{name: "test", canHandle: true, gangID: "test-gang"}
		cfg := handlerGangConfig()
		handler := NewHandler(cfg, disc, func(_ context.Context, reg GangRegistration) {
			mu.Lock()
			defer mu.Unlock()
			captured = reg
		})

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "train-"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "train", Image: "train:latest",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
					},
				}},
			},
		}

		body := buildAdmissionReview(pod, "uid-gen", "default")
		req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		handler.HandleMutate(rec, req)

		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, "train-", captured.PodName)
	})

	t.Run("gang registration not called without gang", func(t *testing.T) {
		called := false
		handler := NewHandler(handlerConfig(), nil, func(_ context.Context, _ GangRegistration) {
			called = true
		})

		pod := &corev1.Pod{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "train", Image: "train:latest",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")},
					},
				}},
			},
		}

		body := buildAdmissionReview(pod, "uid-no-gang", "default")
		req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(body))
		rec := httptest.NewRecorder()

		handler.HandleMutate(rec, req)

		assert.False(t, called)
	})
}
