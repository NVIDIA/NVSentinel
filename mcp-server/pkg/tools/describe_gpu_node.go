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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

const (
	describeGPUNodeAPIVersion = "1"

	// successStatus is the canonical "success" value for the Status field of
	// every tool's *Output struct. Centralised here so all tools agree on the
	// spelling and the goconst linter does not flag nine separate occurrences.
	successStatus = "success"
)

// DescribeGPUNodeInput is the structured input for the describe_gpu_node tool.
type DescribeGPUNodeInput struct {
	Node string `json:"node"`
}

// EventSummary is a flat, MCP-client-friendly view of a HealthEvent suitable
// for embedding in tool responses.
type EventSummary struct {
	Agent          string    `json:"agent,omitempty"`
	ComponentClass string    `json:"component_class,omitempty"`
	CheckName      string    `json:"check_name,omitempty"`
	IsHealthy      bool      `json:"is_healthy"`
	Message        string    `json:"message,omitempty"`
	ErrorCodes     []string  `json:"error_codes,omitempty"`
	StoredAt       time.Time `json:"stored_at"`
}

// TaintSummary mirrors corev1.Taint as a flat structure with string Effect.
type TaintSummary struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// ConditionSummary mirrors corev1.NodeCondition in a flat shape.
type ConditionSummary struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// K8sNodeSummary carries the slice of Kubernetes Node state that is most
// useful for GPU triage. Quantities are flattened to strings so MCP clients
// without K8s libraries can read them.
type K8sNodeSummary struct {
	Name        string             `json:"name,omitempty"`
	Labels      map[string]string  `json:"labels,omitempty"`
	Annotations map[string]string  `json:"annotations,omitempty"`
	Taints      []TaintSummary     `json:"taints,omitempty"`
	Conditions  []ConditionSummary `json:"conditions,omitempty"`
	Capacity    map[string]string  `json:"capacity,omitempty"`
	Allocatable map[string]string  `json:"allocatable,omitempty"`
	Ready       bool               `json:"ready"`
}

// DescribeGPUNodeOutput is the structured output for describe_gpu_node. Both
// LatestEvent and K8sNode are independently nullable: if one data source is
// unavailable, the other is still returned and the gap is reported via
// Warnings.
type DescribeGPUNodeOutput struct {
	APIVersion  string         `json:"api_version"`
	Status      string         `json:"status"`
	Node        string         `json:"node"`
	LatestEvent *EventSummary  `json:"latest_event,omitempty"`
	K8sNode     K8sNodeSummary `json:"k8s_node"`
	Warnings    []string       `json:"warnings,omitempty"`
}

// DescribeGPUNodeHandler handles the describe_gpu_node MCP tool. The zero
// value is not usable; construct with NewDescribeGPUNodeHandler.
type DescribeGPUNodeHandler struct {
	reader    store.Reader
	k8sClient kubernetes.Interface
}

// NewDescribeGPUNodeHandler wires the handler with a Reader and an optional
// kubernetes.Interface. A nil k8sClient is supported: the handler then omits
// the K8s portion of the response and adds a warning.
func NewDescribeGPUNodeHandler(r store.Reader, k kubernetes.Interface) *DescribeGPUNodeHandler {
	return &DescribeGPUNodeHandler{reader: r, k8sClient: k}
}

// Handle returns the latest store event plus a flattened K8s Node description
// for the requested node. Missing-data conditions on either source are turned
// into structured warnings; the handler returns an error only when input is
// invalid or one of the data sources errors in an unexpected way.
func (h *DescribeGPUNodeHandler) Handle(ctx context.Context, in DescribeGPUNodeInput) (DescribeGPUNodeOutput, error) {
	if in.Node == "" {
		return DescribeGPUNodeOutput{}, errors.New("describe_gpu_node: node is required")
	}

	out := DescribeGPUNodeOutput{
		APIVersion: describeGPUNodeAPIVersion,
		Status:     successStatus,
		Node:       in.Node,
	}

	if ev, warning, err := h.fetchLatestEvent(ctx, in.Node); err != nil {
		return DescribeGPUNodeOutput{}, err
	} else {
		out.LatestEvent = ev
		if warning != "" {
			out.Warnings = append(out.Warnings, warning)
		}
	}

	if summary, warning, err := h.fetchK8sNode(ctx, in.Node); err != nil {
		return DescribeGPUNodeOutput{}, err
	} else {
		out.K8sNode = summary
		if warning != "" {
			out.Warnings = append(out.Warnings, warning)
		}
	}

	return out, nil
}

func (h *DescribeGPUNodeHandler) fetchLatestEvent(ctx context.Context, node string) (*EventSummary, string, error) {
	ev, err := h.reader.LatestEventForNode(ctx, node)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Sprintf("no events for node %q in store", node), nil
		}

		return nil, "", fmt.Errorf("describe_gpu_node: latest event: %w", err)
	}

	return eventSummaryFromStored(ev), "", nil
}

func (h *DescribeGPUNodeHandler) fetchK8sNode(ctx context.Context, node string) (K8sNodeSummary, string, error) {
	if h.k8sClient == nil {
		return K8sNodeSummary{}, "k8s API not configured; node detail unavailable", nil
	}

	n, err := h.k8sClient.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return K8sNodeSummary{}, fmt.Sprintf("k8s node %q not found", node), nil
		}

		return K8sNodeSummary{}, "", fmt.Errorf("describe_gpu_node: k8s Get: %w", err)
	}

	return k8sNodeSummaryFrom(n), "", nil
}

func eventSummaryFromStored(ews *datastore.HealthEventWithStatus) *EventSummary {
	if ews == nil {
		return nil
	}

	summary := &EventSummary{StoredAt: ews.CreatedAt}

	if he, ok := ews.HealthEvent.(*protos.HealthEvent); ok && he != nil {
		summary.Agent = he.GetAgent()
		summary.ComponentClass = he.GetComponentClass()
		summary.CheckName = he.GetCheckName()
		summary.IsHealthy = he.GetIsHealthy()
		summary.Message = he.GetMessage()
		summary.ErrorCodes = append([]string{}, he.GetErrorCode()...)
	}

	return summary
}

func k8sNodeSummaryFrom(n *corev1.Node) K8sNodeSummary {
	out := K8sNodeSummary{
		Name:        n.Name,
		Labels:      n.Labels,
		Annotations: n.Annotations,
	}

	for _, t := range n.Spec.Taints {
		out.Taints = append(out.Taints, TaintSummary{
			Key:    t.Key,
			Value:  t.Value,
			Effect: string(t.Effect),
		})
	}

	for _, c := range n.Status.Conditions {
		out.Conditions = append(out.Conditions, ConditionSummary{
			Type:    string(c.Type),
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})

		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			out.Ready = true
		}
	}

	if len(n.Status.Capacity) > 0 {
		out.Capacity = make(map[string]string, len(n.Status.Capacity))
		for k, v := range n.Status.Capacity {
			out.Capacity[string(k)] = v.String()
		}
	}

	if len(n.Status.Allocatable) > 0 {
		out.Allocatable = make(map[string]string, len(n.Status.Allocatable))
		for k, v := range n.Status.Allocatable {
			out.Allocatable[string(k)] = v.String()
		}
	}

	return out
}
