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

package webhook

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Handler struct {
	injector *Injector
}

func NewHandler(cfg *config.Config) *Handler {
	return &Handler{
		injector: NewInjector(cfg),
	}
}

func (h *Handler) HandleMutate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.Error("Failed to read request body", "error", err)
		http.Error(w, "failed to read request body", http.StatusBadRequest)

		return
	}

	var admissionReview admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &admissionReview); err != nil {
		slog.Error("Failed to unmarshal admission review", "error", err)
		http.Error(w, "failed to unmarshal admission review", http.StatusBadRequest)

		return
	}

	response := h.mutate(admissionReview.Request)
	admissionReview.Response = response
	admissionReview.Response.UID = admissionReview.Request.UID

	respBytes, err := json.Marshal(admissionReview)
	if err != nil {
		slog.Error("Failed to marshal response", "error", err)
		http.Error(w, "failed to marshal response", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")

	if _, err := w.Write(respBytes); err != nil {
		slog.Error("Failed to write response", "error", err)
	}
}

func (h *Handler) mutate(req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req == nil {
		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}

	slog.Debug("Processing admission request",
		"namespace", req.Namespace,
		"name", req.Name,
		"operation", req.Operation)

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		slog.Error("Failed to unmarshal pod", "error", err)

		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("failed to unmarshal pod: %v", err),
			},
		}
	}

	patch, err := h.injector.InjectInitContainers(&pod)
	if err != nil {
		slog.Error("Failed to inject init containers", "error", err)

		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("failed to inject init containers: %v", err),
			},
		}
	}

	if patch == nil {
		slog.Debug("No mutation needed", "pod", pod.Name)

		return &admissionv1.AdmissionResponse{
			Allowed: true,
		}
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		slog.Error("Failed to marshal patch", "error", err)

		return &admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("failed to marshal patch: %v", err),
			},
		}
	}

	slog.Info("Injecting preflight init containers",
		"namespace", req.Namespace,
		"pod", pod.Name,
		"initContainers", len(h.injector.cfg.InitContainers))

	patchType := admissionv1.PatchTypeJSONPatch

	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		Patch:     patchBytes,
		PatchType: &patchType,
	}
}
