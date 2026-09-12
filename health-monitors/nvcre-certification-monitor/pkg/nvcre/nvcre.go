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

// Package nvcre reads NVCRE Certification objects and their result ConfigMaps
// without importing the NVCRE Go API. The monitor needs only a handful of
// status fields and two ConfigMap encodings, so it works on unstructured
// objects and decodes just those fields here. This keeps the module free of
// the NVCRE API's transitive dependencies (kubeflow/trainer, jobset, volcano),
// which pin an older k8s.io release than the rest of NVSentinel uses.
//
// The field names and ConfigMap encodings below track the NVCRE API
// (nvcre.nvidia.com/v1alpha1 Certification) and its pkg/noderesults writer.
package nvcre

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// CertificationFailed is both the terminal condition type and the
	// per-category status NVCRE sets when a certification fails.
	CertificationFailed = "Failed"
	// CertificationSucceeded is both the terminal condition type and the
	// per-category status NVCRE sets when a certification passes.
	CertificationSucceeded = "Succeeded"

	// NodeFailureHardwareDetected marks a node that reported a hardware fault.
	NodeFailureHardwareDetected = "HardwareDetected"
	// NodeFailureThresholdViolation marks a node that missed a performance threshold.
	NodeFailureThresholdViolation = "ThresholdViolation"
	// NodeFailureWorkloadFailed marks a node whose certification workload failed.
	NodeFailureWorkloadFailed = "WorkloadFailed"

	// FailedNodesConfigMapKey is the binaryData key holding the gzip'd JSON
	// array of FailedNode rows in a failed-nodes result ConfigMap.
	FailedNodesConfigMapKey = "failed-nodes.json.gz"
	// SucceededNodesConfigMapKey is the binaryData key holding the gzip'd
	// comma-separated node list in a succeeded-nodes result ConfigMap.
	SucceededNodesConfigMapKey = "succeeded-nodes.csv.gz"
)

// CertificationGVK identifies the NVCRE Certification custom resource.
var CertificationGVK = schema.GroupVersionKind{
	Group:   "nvcre.nvidia.com",
	Version: "v1alpha1",
	Kind:    "Certification",
}

// CertificationListGVK identifies the list kind of CertificationGVK.
var CertificationListGVK = schema.GroupVersionKind{
	Group:   CertificationGVK.Group,
	Version: CertificationGVK.Version,
	Kind:    CertificationGVK.Kind + "List",
}

// Status is the part of a Certification's status the monitor reads.
type Status struct {
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
	CategoryStatuses []CategoryStatus   `json:"categoryStatuses,omitempty"`
}

// CategoryStatus is one entry of status.categoryStatuses.
type CategoryStatus struct {
	Domain            string                `json:"domain"`
	Variant           string                `json:"variant"`
	Status            string                `json:"status"`
	SucceededNodesRef *LocalObjectReference `json:"succeededNodesRef,omitempty"`
	FailedNodesRef    *LocalObjectReference `json:"failedNodesRef,omitempty"`
}

// LocalObjectReference names a result ConfigMap in the Certification's namespace.
type LocalObjectReference struct {
	Name string `json:"name"`
}

// FailedNode is one row of the failed-nodes result ConfigMap.
type FailedNode struct {
	Name    string `json:"name"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// NewCertification returns an empty Certification ready for client.Get or
// client.Patch.
func NewCertification() *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(CertificationGVK)

	return cert
}

// NewCertificationList returns an empty CertificationList ready for client.List.
func NewCertificationList() *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(CertificationListGVK)

	return list
}

// GetStatus decodes the fields of the Certification's status the monitor
// reads. A missing status yields the zero Status.
func GetStatus(cert *unstructured.Unstructured) (Status, error) {
	var status Status

	raw, found, err := unstructured.NestedMap(cert.Object, "status")
	if err != nil {
		return Status{}, fmt.Errorf("cert %s/%s: status is not an object: %w", cert.GetNamespace(), cert.GetName(), err)
	}

	if !found {
		return status, nil
	}

	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &status); err != nil {
		return Status{}, fmt.Errorf("cert %s/%s: failed to decode status: %w", cert.GetNamespace(), cert.GetName(), err)
	}

	return status, nil
}

// SetStatus replaces the Certification's status with the given fields. It is
// the inverse of GetStatus and exists for building test fixtures.
func SetStatus(cert *unstructured.Unstructured, status Status) error {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("failed to encode status: %w", err)
	}

	cert.Object["status"] = raw

	return nil
}

// DecodeFailedNodes parses the failed-nodes entry (a gzip'd JSON array of
// {name, reason, message} objects) from a result ConfigMap.
func DecodeFailedNodes(cm *corev1.ConfigMap) ([]FailedNode, error) {
	raw := cm.BinaryData[FailedNodesConfigMapKey]
	if len(raw) == 0 {
		return nil, nil
	}

	decoded, err := gunzip(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to decode failed-nodes entry: %w", err)
	}

	var rows []FailedNode
	if err := json.Unmarshal(decoded, &rows); err != nil {
		return nil, fmt.Errorf("failed to parse failed-nodes entry: %w", err)
	}

	return rows, nil
}

// DecodeSucceededNodes parses the succeeded-nodes entry (a gzip'd
// comma-separated node list) from a result ConfigMap.
func DecodeSucceededNodes(cm *corev1.ConfigMap) ([]string, error) {
	raw := cm.BinaryData[SucceededNodesConfigMapKey]
	if len(raw) == 0 {
		return nil, nil
	}

	decoded, err := gunzip(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to decode succeeded-nodes entry: %w", err)
	}

	var names []string

	for _, name := range strings.Split(string(decoded), ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}

	return names, nil
}

func gunzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}

	defer func() { _ = zr.Close() }()

	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("failed to read gzip data: %w", err)
	}

	return out, nil
}
