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
// NVCRE controller. These helpers play the controller's part: they walk a
// Certification through the same status transitions (categories Pending,
// InProgress, results published, terminal condition) and write the gzip'd
// result ConfigMaps, so the monitor can be exercised end to end without GPUs.
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
	nvcreConditionInProgress        = "InProgress"
	nvcreConditionFailed            = "Failed"
	nvcreConditionSucceeded         = "Succeeded"
	nvcreCategoryPending            = "Pending"
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

// NVCRECategoryResult is the outcome of one certification category in a
// faked run. A non-empty Failed list makes the category Failed and writes a
// failed-nodes ConfigMap; otherwise the category Succeeds for Succeeded and a
// succeeded-nodes ConfigMap is written.
type NVCRECategoryResult struct {
	Domain        string
	Variant       string
	ConfigMapName string
	Failed        []NVCREFailedNode
	Succeeded     []string
}

func (r NVCRECategoryResult) status() string {
	if len(r.Failed) > 0 {
		return nvcreConditionFailed
	}

	return nvcreConditionSucceeded
}

// CreateFailedCertification creates a Certification with a single category
// that Failed for the given nodes, walking the same states as the NVCRE
// controller (see CreateCertification).
func CreateFailedCertification(ctx context.Context, t *testing.T, c klient.Client,
	namespace, certName, configMapName, domain, variant string, failed []NVCREFailedNode,
) {
	t.Helper()

	nodeNames := make([]string, 0, len(failed))
	for _, f := range failed {
		nodeNames = append(nodeNames, f.Name)
	}

	CreateCertification(ctx, t, c, namespace, certName, nodeNames, []NVCRECategoryResult{
		{Domain: domain, Variant: variant, ConfigMapName: configMapName, Failed: failed},
	})
}

// CreateSucceededCertification creates a Certification with a single category
// that Succeeded for the given nodes, walking the same states as the NVCRE
// controller (see CreateCertification).
func CreateSucceededCertification(ctx context.Context, t *testing.T, c klient.Client,
	namespace, certName, configMapName, domain, variant string, nodeNames []string,
) {
	t.Helper()

	CreateCertification(ctx, t, c, namespace, certName, nodeNames, []NVCRECategoryResult{
		{Domain: domain, Variant: variant, ConfigMapName: configMapName, Succeeded: nodeNames},
	})
}

// CreateCertification creates a Certification and drives it through the
// states the NVCRE controller writes: categories Pending with InProgress=True,
// categories InProgress, category results published with the Certification
// still InProgress, and finally the terminal Failed or Succeeded condition.
func CreateCertification(ctx context.Context, t *testing.T, c klient.Client,
	namespace, certName string, nodeNames []string, categories []NVCRECategoryResult,
) {
	t.Helper()

	StartCertification(ctx, t, c, namespace, certName, nodeNames, categories)
	CompleteCategories(ctx, t, c, namespace, certName, categories)
	FinishCertification(ctx, t, c, namespace, certName, categories)
}

// StartCertification creates the Certification and writes the two states the
// controller goes through before any result exists: every category Pending
// with InProgress=True (reason WorkflowCreated), then the categories
// InProgress (reason WorkflowRunning).
func StartCertification(ctx context.Context, t *testing.T, c klient.Client,
	namespace, certName string, nodeNames []string, categories []NVCRECategoryResult,
) {
	t.Helper()

	targets := make([]any, 0, len(nodeNames))
	for _, n := range nodeNames {
		targets = append(targets, n)
	}

	specCategories := make([]any, 0, len(categories))
	for _, cat := range categories {
		specCategories = append(specCategories, map[string]any{"domain": cat.Domain, "variant": cat.Variant})
	}

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(NVCRECertificationGVK)
	cert.SetName(certName)
	cert.SetNamespace(namespace)
	cert.Object["spec"] = map[string]any{
		"categories": specCategories,
		"target":     map[string]any{"nodeNames": targets},
	}
	require.NoError(t, c.Resources().Create(ctx, cert), "failed to create Certification")

	updateCertificationStatus(ctx, t, c, namespace, certName, func(status map[string]any) {
		status["conditions"] = certificationConditions(nvcreConditionInProgress, "WorkflowCreated",
			"Created Workflow for the first category")
		status["categoryStatuses"] = categoryStatuses(categories, func(NVCRECategoryResult) map[string]any {
			return map[string]any{nvcreStatusField: nvcreCategoryPending}
		})
	})

	updateCertificationStatus(ctx, t, c, namespace, certName, func(status map[string]any) {
		status["conditions"] = certificationConditions(nvcreConditionInProgress, "WorkflowRunning",
			"Certification in progress")
		status["categoryStatuses"] = categoryStatuses(categories, func(NVCRECategoryResult) map[string]any {
			return map[string]any{nvcreStatusField: nvcreConditionInProgress}
		})
	})

	t.Logf("Started Certification %s/%s for nodes %v", namespace, certName, nodeNames)
}

// CompleteCategories writes each category's result ConfigMap and marks the
// category Failed or Succeeded with its result reference, exactly as the
// controller does when a Workflow finishes. The Certification itself stays
// InProgress: the monitor must not act on these results yet.
func CompleteCategories(ctx context.Context, t *testing.T, c klient.Client,
	namespace, certName string, categories []NVCRECategoryResult,
) {
	t.Helper()

	for _, cat := range categories {
		var cm *v1.ConfigMap

		if len(cat.Failed) > 0 {
			raw, err := json.Marshal(cat.Failed)
			require.NoError(t, err, "failed to marshal failed nodes")

			cm = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: cat.ConfigMapName, Namespace: namespace},
				BinaryData: map[string][]byte{nvcreFailedNodesConfigMapKey: gzipBytes(t, raw)},
			}
		} else {
			cm = &v1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: cat.ConfigMapName, Namespace: namespace},
				BinaryData: map[string][]byte{
					nvcreSucceededNodesConfigMapKey: gzipBytes(t, []byte(strings.Join(cat.Succeeded, ","))),
				},
			}
		}

		require.NoError(t, c.Resources().Create(ctx, cm), "failed to create result ConfigMap")
	}

	updateCertificationStatus(ctx, t, c, namespace, certName, func(status map[string]any) {
		status["conditions"] = certificationConditions(nvcreConditionInProgress, "WorkflowRunning",
			"Workflow finished, advancing to next category")
		status["categoryStatuses"] = categoryStatuses(categories, func(cat NVCRECategoryResult) map[string]any {
			refField := "succeededNodesRef"
			if len(cat.Failed) > 0 {
				refField = "failedNodesRef"
			}

			return map[string]any{
				nvcreStatusField: cat.status(),
				refField:         map[string]any{"kind": "ConfigMap", "name": cat.ConfigMapName},
			}
		})
	})

	t.Logf("Published category results on Certification %s/%s (still InProgress)", namespace, certName)
}

// FinishCertification sets the terminal condition: Failed if any category
// failed, Succeeded otherwise, with InProgress set back to False.
func FinishCertification(ctx context.Context, t *testing.T, c klient.Client,
	namespace, certName string, categories []NVCRECategoryResult,
) {
	t.Helper()

	terminal, reason, message := nvcreConditionSucceeded, "AllWorkflowsSucceeded",
		"All certification categories completed successfully"

	for _, cat := range categories {
		if len(cat.Failed) > 0 {
			terminal, reason, message = nvcreConditionFailed, "WorkflowFailed",
				"One or more certification categories failed"

			break
		}
	}

	updateCertificationStatus(ctx, t, c, namespace, certName, func(status map[string]any) {
		status["conditions"] = certificationConditions(terminal, reason, message)
	})

	t.Logf("Certification %s/%s is now %s", namespace, certName, terminal)
}

// updateCertificationStatus re-reads the Certification, lets mutate edit its
// status map and writes it back through the status subresource.
func updateCertificationStatus(ctx context.Context, t *testing.T, c klient.Client,
	namespace, certName string, mutate func(status map[string]any),
) {
	t.Helper()

	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(NVCRECertificationGVK)
	require.NoError(t, c.Resources().Get(ctx, certName, namespace, cert), "failed to re-read Certification")

	status, _ := cert.Object[nvcreStatusField].(map[string]any)
	if status == nil {
		status = map[string]any{}
	}

	mutate(status)
	cert.Object[nvcreStatusField] = status

	require.NoError(t, c.Resources().UpdateStatus(ctx, cert), "failed to set Certification status")
}

// certificationConditions builds the mutually exclusive InProgress, Succeeded
// and Failed triple the controller maintains: active is True with the given
// reason, the other two are False with reason NotApplicable.
func certificationConditions(active, reason, message string) []any {
	now := metav1.NewTime(time.Now()).UTC().Format(time.RFC3339)
	conds := make([]any, 0, 3)

	for _, ct := range []string{nvcreConditionInProgress, nvcreConditionSucceeded, nvcreConditionFailed} {
		cond := map[string]any{
			"type":               ct,
			nvcreStatusField:     "False",
			"reason":             "NotApplicable",
			"message":            "",
			"lastTransitionTime": now,
		}
		if ct == active {
			cond[nvcreStatusField] = "True"
			cond["reason"] = reason
			cond["message"] = message
		}

		conds = append(conds, cond)
	}

	return conds
}

// categoryStatuses renders status.categoryStatuses, merging the per-category
// fields returned by extra into the domain/variant identity of each category.
func categoryStatuses(categories []NVCRECategoryResult, extra func(NVCRECategoryResult) map[string]any) []any {
	out := make([]any, 0, len(categories))

	for _, cat := range categories {
		entry := map[string]any{"domain": cat.Domain, "variant": cat.Variant}
		for k, v := range extra(cat) {
			entry[k] = v
		}

		out = append(out, entry)
	}

	return out
}

// DeleteCertification removes the Certification and its result ConfigMaps.
// Missing objects are not an error.
func DeleteCertification(ctx context.Context, c klient.Client, namespace, certName string,
	configMapNames ...string,
) error {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(NVCRECertificationGVK)
	cert.SetName(certName)
	cert.SetNamespace(namespace)

	if err := c.Resources().Delete(ctx, cert); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete Certification %s/%s: %w", namespace, certName, err)
	}

	for _, name := range configMapNames {
		cm := &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		if err := c.Resources().Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete ConfigMap %s/%s: %w", namespace, name, err)
		}
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
