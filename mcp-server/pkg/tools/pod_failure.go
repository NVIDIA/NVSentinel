// Copyright 2026 k8s-gpu-mcp-server contributors
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

package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
)

const podFailureAPIVersion = "1"

// PodFailureInput is the structured input for the pod_failure tool.
type PodFailureInput struct {
	Pod       string `json:"pod"`
	Namespace string `json:"namespace"`
}

// K8sEventSummary is a flat view of a Kubernetes Event.
type K8sEventSummary struct {
	Type           string    `json:"type"`
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	Count          int32     `json:"count"`
	FirstTimestamp time.Time `json:"first_timestamp"`
	LastTimestamp  time.Time `json:"last_timestamp"`
}

// PodFailureOutput is the structured output for pod_failure. It stitches
// three data sources: K8s Pod state, K8s Events scoped to the pod, and
// NVSentinel store health events that name the pod in entitiesImpacted.
type PodFailureOutput struct {
	APIVersion          string            `json:"api_version"`
	Status              string            `json:"status"`
	Pod                 string            `json:"pod"`
	Namespace           string            `json:"namespace"`
	Node                string            `json:"node,omitempty"`
	Phase               string            `json:"phase,omitempty"`
	RestartCount        int               `json:"restart_count"`
	RecentEvents        []K8sEventSummary `json:"recent_events,omitempty"`
	RelatedHealthEvents []EventSummary    `json:"related_health_events,omitempty"`
	Warnings            []string          `json:"warnings,omitempty"`
}

// PodFailureHandler handles the pod_failure MCP tool. The zero value is not
// usable; construct with NewPodFailureHandler.
type PodFailureHandler struct {
	reader    store.Reader
	k8sClient kubernetes.Interface
}

// NewPodFailureHandler wires the handler with a Reader (for store health
// events) and a kubernetes.Interface (for Pod and Event objects). A nil
// k8sClient is rejected at Handle time because this tool's primary data
// source is the K8s API.
func NewPodFailureHandler(r store.Reader, k kubernetes.Interface) *PodFailureHandler {
	return &PodFailureHandler{reader: r, k8sClient: k}
}

// Handle answers a pod_failure request by fetching the K8s Pod, listing its
// Events, and surfacing NVSentinel store health events that name the pod.
// A missing pod (apierrors.IsNotFound) is returned as an error; other
// soft-failures (no node assigned, store error) become warnings.
func (h *PodFailureHandler) Handle(ctx context.Context, in PodFailureInput) (PodFailureOutput, error) {
	if err := validatePodFailureInput(in); err != nil {
		return PodFailureOutput{}, err
	}

	if h.k8sClient == nil {
		return PodFailureOutput{}, errors.New("pod_failure: k8s API not configured")
	}

	pod, err := h.k8sClient.CoreV1().Pods(in.Namespace).Get(ctx, in.Pod, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return PodFailureOutput{}, fmt.Errorf("pod_failure: pod %q not found in namespace %q", in.Pod, in.Namespace)
		}

		return PodFailureOutput{}, fmt.Errorf("pod_failure: get pod: %w", err)
	}

	out := PodFailureOutput{
		APIVersion: podFailureAPIVersion,
		Status:     "success",
		Pod:        pod.Name,
		Namespace:  pod.Namespace,
		Node:       pod.Spec.NodeName,
		Phase:      string(pod.Status.Phase),
	}

	for _, cs := range pod.Status.ContainerStatuses {
		out.RestartCount += int(cs.RestartCount)
	}

	recent, err := h.listPodEvents(ctx, in)
	if err != nil {
		return PodFailureOutput{}, err
	}

	out.RecentEvents = recent

	related, warning, err := h.relatedStoreEvents(ctx, in, out.Node)
	if err != nil {
		return PodFailureOutput{}, err
	}

	out.RelatedHealthEvents = related

	if warning != "" {
		out.Warnings = append(out.Warnings, warning)
	}

	return out, nil
}

func validatePodFailureInput(in PodFailureInput) error {
	if in.Pod == "" {
		return errors.New("pod_failure: pod is required")
	}

	if in.Namespace == "" {
		return errors.New("pod_failure: namespace is required")
	}

	return nil
}

func (h *PodFailureHandler) listPodEvents(ctx context.Context, in PodFailureInput) ([]K8sEventSummary, error) {
	evList, err := h.k8sClient.CoreV1().Events(in.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("pod_failure: list events: %w", err)
	}

	out := make([]K8sEventSummary, 0)

	for i := range evList.Items {
		ev := &evList.Items[i]
		if ev.InvolvedObject.Name != in.Pod || ev.InvolvedObject.Namespace != in.Namespace {
			continue
		}

		out = append(out, K8sEventSummary{
			Type:           ev.Type,
			Reason:         ev.Reason,
			Message:        ev.Message,
			Count:          ev.Count,
			FirstTimestamp: ev.FirstTimestamp.Time,
			LastTimestamp:  ev.LastTimestamp.Time,
		})
	}

	return out, nil
}

func (h *PodFailureHandler) relatedStoreEvents(ctx context.Context, in PodFailureInput, node string) ([]EventSummary, string, error) {
	if node == "" {
		return nil, "pod has no assigned node; skipping store events", nil
	}

	events, err := h.reader.EventsByNode(ctx, node)
	if err != nil {
		return nil, "", fmt.Errorf("pod_failure: events by node: %w", err)
	}

	out := make([]EventSummary, 0)

	for i := range events {
		ews := &events[i]

		he, ok := ews.HealthEvent.(*protos.HealthEvent)
		if !ok || he == nil {
			continue
		}

		if !podMentionedInEvent(he, in.Namespace, in.Pod) {
			continue
		}

		summary := eventSummaryFromStored(ews)
		if summary != nil {
			out = append(out, *summary)
		}
	}

	return out, "", nil
}

// podMentionedInEvent reports whether the event names the given pod via its
// entitiesImpacted list. Two value conventions are accepted: bare name and
// "namespace/name". Match is case-insensitive on entityType to tolerate
// monitor-side casing drift.
func podMentionedInEvent(he *protos.HealthEvent, ns, name string) bool {
	qualified := ns + "/" + name

	for _, e := range he.GetEntitiesImpacted() {
		if !strings.EqualFold(e.GetEntityType(), "Pod") {
			continue
		}

		v := e.GetEntityValue()
		if v == name || v == qualified {
			return true
		}
	}

	return false
}
