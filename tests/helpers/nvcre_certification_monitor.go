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

package helpers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/e2e-framework/klient"
)

// The Tilt/kind cluster installs only the NVCRE Certification CRD, not the
// NVCRE controller. The monitor reads three things from NVCRE: the terminal
// condition on the Certification, the per-category failedNodesRef ConfigMap
// name, and the gzip'd JSON inside that ConfigMap. These helpers fake exactly
// that so the monitor can be exercised end to end without GPUs.
const (
	// NVCRECertFailuresAnnotationKey is the node annotation written by the monitor.
	NVCRECertFailuresAnnotationKey = "nvsentinel.dgxc.nvidia.com/nvcre-cert-failures-details"
	// NVCRECertFailureLabelKey is the node label set while the node holds a failure.
	NVCRECertFailureLabelKey = "nvsentinel.dgxc.nvidia.com/nvcre-cert-failure"
	// NVCRECertProcessedAnnotationKey is stamped on the Certification once handled.
	NVCRECertProcessedAnnotationKey = "nvsentinel.dgxc.nvidia.com/cert-processed"
	// NVCRECertFailedTaintKey is the taint applied by the fault-quarantine ruleset.
	NVCRECertFailedTaintKey = "nvsentinel.dgxc.nvidia.com/nvcre-cert-failed"
	// NVCRECertFailedCheckName is the health event checkName and node condition type.
	NVCRECertFailedCheckName = "NVCRECertFailed"

	nvcreFailedNodesConfigMapKey    = "failed-nodes.json.gz"
	nvcreSucceededNodesConfigMapKey = "succeeded-nodes.csv.gz"
	nvcreStatusField                = "status"
	nvcreConditionFailed            = "Failed"
	nvcreConditionSucceeded         = "Succeeded"
)

// NVCRECertificationGVK identifies the NVCRE Certification custom resource.
var NVCRECertificationGVK = schema.GroupVersionKind{
	Group:   "nvcre.nvidia.com",
	Version: "v1alpha1",
	Kind:    "Certification",
}

// NVCREFailedNode mirrors the FailedNode row NVCRE writes into the
// failed-nodes ConfigMap.
type NVCREFailedNode struct {
	Name    string `json:"name"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// CreateFailedCertification creates a failed-nodes ConfigMap and a
// Certification whose status reports the given category as Failed, the same
// shape NVCRE produces after a real run.
func CreateFailedCertification(ctx context.Context, t *testing.T, c klient.Client,
	namespace, certName, configMapName, domain, variant string, failed []NVCREFailedNode,
) {
	t.Helper()

	raw, err := json.Marshal(failed)
	require.NoError(t, err, "failed to marshal failed nodes")

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: namespace},
		BinaryData: map[string][]byte{nvcreFailedNodesConfigMapKey: gzipBytes(t, raw)},
	}
	require.NoError(t, c.Resources().Create(ctx, cm), "failed to create failed-nodes ConfigMap")

	nodeNames := make([]string, 0, len(failed))
	for _, f := range failed {
		nodeNames = append(nodeNames, f.Name)
	}

	createTerminalCertification(ctx, t, c, namespace, certName, domain, variant, nodeNames,
		nvcreConditionFailed, "failedNodesRef", configMapName)
}

// CreateSucceededCertification creates a succeeded-nodes ConfigMap and a
// Certification whose status reports the given category as Succeeded for the
// given nodes, the same shape NVCRE produces after a passing run.
func CreateSucceededCertification(ctx context.Context, t *testing.T, c klient.Client,
	namespace, certName, configMapName, domain, variant string, nodeNames []string,
) {
	t.Helper()

	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: namespace},
		BinaryData: map[string][]byte{nvcreSucceededNodesConfigMapKey: gzipBytes(t, []byte(strings.Join(nodeNames, ",")))},
	}
	require.NoError(t, c.Resources().Create(ctx, cm), "failed to create succeeded-nodes ConfigMap")

	createTerminalCertification(ctx, t, c, namespace, certName, domain, variant, nodeNames,
		nvcreConditionSucceeded, "succeededNodesRef", configMapName)
}

// createTerminalCertification creates a Certification targeting nodeNames and
// writes a status with the terminal condition set to True and a single
// category whose result ConfigMap is referenced through refField.
func createTerminalCertification(ctx context.Context, t *testing.T, c klient.Client,
	namespace, certName, domain, variant string, nodeNames []string, terminal, refField, configMapName string,
) {
	t.Helper()

	targets := make([]any, 0, len(nodeNames))
	for _, n := range nodeNames {
		targets = append(targets, n)
	}

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(NVCRECertificationGVK)
	cert.SetName(certName)
	cert.SetNamespace(namespace)
	cert.Object["spec"] = map[string]any{
		"categories": []any{
			map[string]any{"domain": domain, "variant": variant},
		},
		"target": map[string]any{"nodeNames": targets},
	}
	require.NoError(t, c.Resources().Create(ctx, cert), "failed to create Certification")

	require.NoError(t, c.Resources().Get(ctx, certName, namespace, cert), "failed to re-read Certification")

	now := metav1.NewTime(time.Now()).UTC().Format(time.RFC3339)
	cert.Object[nvcreStatusField] = map[string]any{
		"conditions": []any{
			map[string]any{
				"type":               terminal,
				nvcreStatusField:     "True",
				"reason":             "Certification" + terminal,
				"message":            "e2e: category " + strings.ToLower(terminal),
				"lastTransitionTime": now,
			},
		},
		"categoryStatuses": []any{
			map[string]any{
				"domain":         domain,
				"variant":        variant,
				nvcreStatusField: terminal,
				refField: map[string]any{
					"kind": "ConfigMap",
					"name": configMapName,
				},
			},
		},
	}
	require.NoError(t, c.Resources().UpdateStatus(ctx, cert), "failed to set Certification status")

	t.Logf("Created %s Certification %s/%s (%s/%s) for nodes %v",
		terminal, namespace, certName, domain, variant, nodeNames)
}

// DeleteCertification removes the Certification and its failed-nodes
// ConfigMap. Missing objects are not an error.
func DeleteCertification(ctx context.Context, c klient.Client, namespace, certName, configMapName string) error {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(NVCRECertificationGVK)
	cert.SetName(certName)
	cert.SetNamespace(namespace)

	if err := c.Resources().Delete(ctx, cert); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete Certification %s/%s: %w", namespace, certName, err)
	}

	cm := &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: namespace}}
	if err := c.Resources().Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete ConfigMap %s/%s: %w", namespace, configMapName, err)
	}

	return nil
}

// GetCertificationAnnotation returns the value of an annotation on the
// Certification and whether it is present.
func GetCertificationAnnotation(
	ctx context.Context, c klient.Client, namespace, certName, key string,
) (string, bool, error) {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(NVCRECertificationGVK)

	if err := c.Resources().Get(ctx, certName, namespace, cert); err != nil {
		return "", false, err
	}

	val, ok := cert.GetAnnotations()[key]

	return val, ok, nil
}

// ClearNVCRENodeState removes the monitor's annotation and label from the
// node so a test leaves no trace behind.
func ClearNVCRENodeState(ctx context.Context, c klient.Client, nodeName string) error {
	node, err := GetNodeByName(ctx, c, nodeName)
	if err != nil {
		return err
	}

	_, hasAnn := node.Annotations[NVCRECertFailuresAnnotationKey]
	_, hasLabel := node.Labels[NVCRECertFailureLabelKey]

	if !hasAnn && !hasLabel {
		return nil
	}

	delete(node.Annotations, NVCRECertFailuresAnnotationKey)
	delete(node.Labels, NVCRECertFailureLabelKey)

	return c.Resources().Update(ctx, node)
}

// NodeHasNVCRETaint reports whether the node carries the fault-quarantine
// taint for certification failures.
func NodeHasNVCRETaint(node *v1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == NVCRECertFailedTaintKey {
			return true
		}
	}

	return false
}

// NodeHasNVCRECondition reports whether the NVCRECertFailed condition is
// present with Status=True.
func NodeHasNVCRECondition(node *v1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if string(cond.Type) == NVCRECertFailedCheckName && cond.Status == v1.ConditionTrue {
			return true
		}
	}

	return false
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(raw)
	require.NoError(t, err, "failed to gzip data")
	require.NoError(t, zw.Close(), "failed to close gzip writer")

	return buf.Bytes()
}
