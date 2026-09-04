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

package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"k8s.io/apimachinery/pkg/types"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"

	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/publisher"
	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/state"
)

type publishedEvent struct {
	Node      string
	IsHealthy bool
	Message   string
	ErrorCode string
}

type testRecorder struct {
	events []publishedEvent
}

func newTestRecorder() *testRecorder {
	return &testRecorder{}
}

func (tr *testRecorder) publishFn(_ context.Context, nodeName string, isHealthy bool, message, errorCode string) error {
	tr.events = append(tr.events, publishedEvent{
		Node:      nodeName,
		IsHealthy: isHealthy,
		Message:   message,
		ErrorCode: errorCode,
	})
	return nil
}

func mustGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, err := w.Write(data)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func mustGzipJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return mustGzip(t, b)
}

func newCert(name, namespace string, condType, condStatus string, categories []nvcrev1alpha1.CertificationCategoryStatus) *nvcrev1alpha1.Certification {
	cert := &nvcrev1alpha1.Certification{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: nvcrev1alpha1.CertificationStatus{
			CategoryStatuses: categories,
		},
	}

	if condType != "" {
		cert.Status.Conditions = []metav1.Condition{
			{
				Type:               condType,
				Status:             metav1.ConditionStatus(condStatus),
				LastTransitionTime: metav1.NewTime(time.Now().Truncate(time.Second)),
			},
		}
	}

	return cert
}

func newNodeWithCertFailures(name, annotationValue string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{state.LabelKey: "true"},
			Annotations: map[string]string{state.AnnotationKey: annotationValue},
		},
	}
}

func newTestReconciler(t *testing.T, recorder *testRecorder, objects ...runtime.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objects...).
		Build()

	evaluator, err := config.NewEvaluator([]config.Policy{
		{Name: "test-policy", Match: "true"},
	})
	require.NoError(t, err)

	return &Reconciler{
		client:         fakeClient,
		publisher:      publisher.NewForTesting(recorder.publishFn),
		evaluator:      evaluator,
		annotator:      state.NewAnnotationManager(fakeClient),
		certAnnotator:  state.NewCertAnnotationHelper(fakeClient),
		resyncInterval: time.Minute,
		results:        make(map[types.NamespacedName]decodedResult),
	}
}

// --- processDesiredAndObserved tests ---

func TestProcessDesiredAndObserved_DesiredAndObserved_NoOp(t *testing.T) {
	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder)

	var certs []*nvcrev1alpha1.Certification

	desired := map[TupleKey]NodeCertFailure{
		{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}: {
			Message:  "test",
			CertRefs: []CertRef{{Name: "cert-1", Namespace: "ns"}},
		},
	}
	observed := map[string]map[string]struct{}{
		"gpu-01": {"nccl-all-gather/WorkloadFailed": {}},
	}

	certsToMark, err := r.processDesiredAndObserved(context.Background(), desired, observed, certTimesFor(t, certs...))
	require.NoError(t, err)
	assert.Empty(t, recorder.events)
	assert.Empty(t, certsToMark)
}

// A cert that completes while its tuple is already on the node publishes
// nothing and so was never stamped. It must be stamped from the observed
// branch, or an operator clear later re-publishes its failure as "new".
func TestProcessDesiredAndObserved_DesiredAndObserved_StampsUnstampedOwner(t *testing.T) {
	recorder := newTestRecorder()
	cats := []nvcrev1alpha1.CertificationCategoryStatus{{
		Domain:  "nccl",
		Variant: "nccl-all-gather",
		Status:  nvcrev1alpha1.CertificationFailed,
	}}
	stamped := newCert("cert-1", "ns", nvcrev1alpha1.CertificationFailed, "True", cats)
	stamped.Annotations = map[string]string{
		state.CertProcessedKey: stamped.Status.Conditions[0].LastTransitionTime.UTC().Format(time.RFC3339),
	}
	dup := newCert("cert-2", "ns", nvcrev1alpha1.CertificationFailed, "True", cats)
	r := newTestReconciler(t, recorder, stamped, dup)

	desired := map[TupleKey]NodeCertFailure{
		{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}: {
			Message:  "test",
			CertRefs: []CertRef{{Name: "cert-1", Namespace: "ns"}, {Name: "cert-2", Namespace: "ns"}},
		},
	}
	observed := map[string]map[string]struct{}{
		"gpu-01": {"nccl-all-gather/WorkloadFailed": {}},
	}

	certsToMark, err := r.processDesiredAndObserved(context.Background(), desired, observed, certTimesFor(t, stamped, dup))
	require.NoError(t, err)
	assert.Empty(t, recorder.events)
	assert.NotContains(t, certsToMark, CertRef{Name: "cert-1", Namespace: "ns"})
	assert.Contains(t, certsToMark, CertRef{Name: "cert-2", Namespace: "ns"})
}

func TestProcessDesiredAndObserved_ObservedNotDesired_PublishesHealthy(t *testing.T) {
	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder)

	var certs []*nvcrev1alpha1.Certification

	desired := map[TupleKey]NodeCertFailure{}
	observed := map[string]map[string]struct{}{
		"gpu-01": {"nccl-all-gather/WorkloadFailed": {}},
	}

	_, err := r.processDesiredAndObserved(context.Background(), desired, observed, certTimesFor(t, certs...))
	require.NoError(t, err)
	require.Len(t, recorder.events, 1)
	assert.True(t, recorder.events[0].IsHealthy)
	assert.Equal(t, "nccl-all-gather/WorkloadFailed", recorder.events[0].ErrorCode)
}

// A desired tuple whose node does not exist must not be published or marked:
// downstream consumers can only fail the condition update for a missing Node.
func TestProcessDesiredAndObserved_DesiredNotObserved_NodeMissing_Skips(t *testing.T) {
	recorder := newTestRecorder()
	cert := newCert("cert-1", "ns", nvcrev1alpha1.CertificationFailed, "True", nil)
	certs := []*nvcrev1alpha1.Certification{cert}
	r := newTestReconciler(t, recorder, cert)

	desired := map[TupleKey]NodeCertFailure{
		{Node: "gpu-gone", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}: {
			Message:  "test",
			CertRefs: []CertRef{{Name: "cert-1", Namespace: "ns"}},
		},
	}
	observed := map[string]map[string]struct{}{}

	certsToMark, err := r.processDesiredAndObserved(context.Background(), desired, observed, certTimesFor(t, certs...))
	require.NoError(t, err)
	assert.Empty(t, recorder.events)
	assert.Empty(t, certsToMark)
}

func TestProcessDesiredAndObserved_DesiredNotObserved_NodeExists_PublishesUnhealthy(t *testing.T) {
	recorder := newTestRecorder()
	cert := newCert("cert-1", "ns", nvcrev1alpha1.CertificationFailed, "True", nil)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-01"}}
	certs := []*nvcrev1alpha1.Certification{cert}
	r := newTestReconciler(t, recorder, cert, node)

	desired := map[TupleKey]NodeCertFailure{
		{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}: {
			Message:  "test",
			CertRefs: []CertRef{{Name: "cert-1", Namespace: "ns"}},
		},
	}
	observed := map[string]map[string]struct{}{}

	certsToMark, err := r.processDesiredAndObserved(context.Background(), desired, observed, certTimesFor(t, certs...))
	require.NoError(t, err)
	require.Len(t, recorder.events, 1)
	assert.False(t, recorder.events[0].IsHealthy)
	assert.Equal(t, "gpu-01", recorder.events[0].Node)
	assert.Equal(t, "nccl-all-gather/WorkloadFailed", recorder.events[0].ErrorCode)
	assert.Contains(t, certsToMark, CertRef{Name: "cert-1", Namespace: "ns"})

	got := &corev1.Node{}
	require.NoError(t, r.client.Get(context.Background(), client.ObjectKey{Name: "gpu-01"}, got))
	assert.Equal(t, "true", got.Labels[state.LabelKey])
	assert.Contains(t, got.Annotations[state.AnnotationKey], "nccl-all-gather/WorkloadFailed")
}

// CRE writes every terminal condition on a finished cert; the one that did not
// happen is Status=False but still carries the time it was last set (creation).
// The completion time must come from the True condition, otherwise a Succeeded
// cert reports its creation time and sorts ahead of failures its pass should
// override.
func TestGetCompletionTime_UsesTrueTerminalCondition(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 9, 4, 7, 0, 0, 0, time.UTC))
	finished := metav1.NewTime(created.Add(10 * time.Minute))

	cert := &nvcrev1alpha1.Certification{
		ObjectMeta: metav1.ObjectMeta{Name: "cert-ok", Namespace: "ns"},
		Status: nvcrev1alpha1.CertificationStatus{
			Conditions: []metav1.Condition{
				{Type: nvcrev1alpha1.CertificationFailed, Status: metav1.ConditionFalse, LastTransitionTime: created},
				{Type: nvcrev1alpha1.CertificationSucceeded, Status: metav1.ConditionTrue, LastTransitionTime: finished},
			},
		},
	}

	got, err := getCompletionTime(cert)
	require.NoError(t, err)
	assert.Equal(t, finished.Time, got)

	cert.Status.Conditions[1].Status = metav1.ConditionFalse
	_, err = getCompletionTime(cert)
	require.Error(t, err, "a cert with no True terminal condition has no completion time")
}

func certTimesFor(t *testing.T, certs ...*nvcrev1alpha1.Certification) map[CertRef]time.Time {
	t.Helper()

	out := make(map[CertRef]time.Time, len(certs))

	for _, c := range certs {
		ct, err := getCompletionTime(c)
		require.NoError(t, err)

		out[CertRef{Name: c.Name, Namespace: c.Namespace}] = ct
	}

	return out
}

// CRE can flip a Failed cert back to InProgress (repeatCount iteration restart)
// and fail it again with a newer lastTransitionTime. A cert-processed stamp
// older than the current terminal time must be treated as unprocessed, so the
// rows are published as new holds rather than "operator recovered".
func TestProcessDesiredAndObserved_ReopenedCert_RepublishesUnhealthy(t *testing.T) {
	recorder := newTestRecorder()
	cert := newCert("cert-1", "ns", nvcrev1alpha1.CertificationFailed, "True", nil)
	t3 := cert.Status.Conditions[0].LastTransitionTime.Time
	t1 := t3.Add(-10 * time.Minute)
	cert.Annotations = map[string]string{state.CertProcessedKey: t1.UTC().Format(time.RFC3339)}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-01"}}
	r := newTestReconciler(t, recorder, cert, node)

	desired := map[TupleKey]NodeCertFailure{
		{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}: {
			Message:  "test",
			CertRefs: []CertRef{{Name: "cert-1", Namespace: "ns"}},
		},
	}
	observed := map[string]map[string]struct{}{}

	certsToMark, err := r.processDesiredAndObserved(context.Background(), desired, observed, certTimesFor(t, cert))
	require.NoError(t, err)
	require.Len(t, recorder.events, 1)
	assert.False(t, recorder.events[0].IsHealthy, "reopened cert must publish a hold, not a recovery")
	assert.Equal(t, t3, certsToMark[CertRef{Name: "cert-1", Namespace: "ns"}])

	got := &nvcrev1alpha1.Certification{}
	require.NoError(t, r.client.Get(context.Background(), client.ObjectKey{Name: "cert-1", Namespace: "ns"}, got))
	assert.Empty(t, got.Annotations[state.ErrorRecoveredKey])
}

// A Failed category whose result ConfigMap is gone must not abort the sweep.
// The category contributes nothing and the sweep continues.
func TestBuildDesired_MissingFailedNodesConfigMap_SkipsCategory(t *testing.T) {
	recorder := newTestRecorder()
	cert := newCert("cert-1", "ns", nvcrev1alpha1.CertificationFailed, "True",
		[]nvcrev1alpha1.CertificationCategoryStatus{{
			Domain:         "nccl",
			Variant:        "nccl-all-gather",
			Status:         nvcrev1alpha1.CertificationFailed,
			FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "does-not-exist"},
		}})
	r := newTestReconciler(t, recorder, cert)

	desired, err := r.buildDesired(context.Background(), []nvcrev1alpha1.Certification{*cert})
	require.NoError(t, err)
	assert.Empty(t, desired)
}

// A Failed category whose result ConfigMap exists but cannot be decoded aborts
// the sweep: treating it as empty would heal every hold the category asserts.
func TestBuildDesired_CorruptFailedNodesConfigMap_AbortsSweep(t *testing.T) {
	recorder := newTestRecorder()
	cert := newCert("cert-1", "ns", nvcrev1alpha1.CertificationFailed, "True",
		[]nvcrev1alpha1.CertificationCategoryStatus{{
			Domain:         "nccl",
			Variant:        "nccl-all-gather",
			Status:         nvcrev1alpha1.CertificationFailed,
			FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "results"},
		}})
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "results", Namespace: "ns"},
		BinaryData: map[string][]byte{"failed-nodes.json.gz": []byte("not gzip")},
	}
	r := newTestReconciler(t, recorder, cert, cm)

	desired, err := r.buildDesired(context.Background(), []nvcrev1alpha1.Certification{*cert})
	require.ErrorContains(t, err, "failed to decode failed-nodes ConfigMap ns/results")
	assert.Nil(t, desired)
}

func gzipJSON(t *testing.T, v any) []byte {
	t.Helper()

	raw, err := json.Marshal(v)
	require.NoError(t, err)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err = zw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	return buf.Bytes()
}

func failedCert(cmName string) *nvcrev1alpha1.Certification {
	return newCert("cert-1", "ns", nvcrev1alpha1.CertificationFailed, "True",
		[]nvcrev1alpha1.CertificationCategoryStatus{{
			Domain:         "nccl",
			Variant:        "nccl-all-gather",
			Status:         nvcrev1alpha1.CertificationFailed,
			FailedNodesRef: &corev1.TypedLocalObjectReference{Name: cmName},
		}})
}

func failedRowsCM(t *testing.T, name, rv string, nodes ...string) *corev1.ConfigMap {
	t.Helper()

	rows := make([]nvcrev1alpha1.FailedNode, 0, len(nodes))
	for _, n := range nodes {
		rows = append(rows, nvcrev1alpha1.FailedNode{Name: n, Reason: "WorkloadFailed", Message: "m"})
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", ResourceVersion: rv},
		BinaryData: map[string][]byte{"failed-nodes.json.gz": gzipJSON(t, rows)},
	}
}

// The decoded result is memoised per ConfigMap resourceVersion: an unchanged
// ConfigMap is served from the memo, a changed one is decoded again.
func TestBuildDesired_ResultDecodeIsMemoisedByResourceVersion(t *testing.T) {
	cert := failedCert("results")
	cm := failedRowsCM(t, "results", "", "gpu-01")
	r := newTestReconciler(t, newTestRecorder(), cert, cm)
	certs := []nvcrev1alpha1.Certification{*cert}
	key := types.NamespacedName{Namespace: "ns", Name: "results"}

	desired, err := r.buildDesired(context.Background(), certs)
	require.NoError(t, err)
	require.Len(t, desired, 1)
	require.Contains(t, r.results, key)

	// Plant a different decoded result at the current resourceVersion. If the
	// memo is honoured, the planted rows win over the stored payload.
	planted := r.results[key]
	planted.failed = []nvcrev1alpha1.FailedNode{
		{Name: "gpu-01", Reason: "WorkloadFailed", Message: "m"},
		{Name: "gpu-02", Reason: "WorkloadFailed", Message: "m"},
	}
	r.results[key] = planted

	desired, err = r.buildDesired(context.Background(), certs)
	require.NoError(t, err)
	assert.Len(t, desired, 2, "memoised rows must be reused while resourceVersion is unchanged")

	// Updating the ConfigMap bumps its resourceVersion; the new payload must be decoded.
	got := &corev1.ConfigMap{}
	require.NoError(t, r.client.Get(context.Background(), key, got))
	got.BinaryData = failedRowsCM(t, "results", "", "gpu-03").BinaryData
	require.NoError(t, r.client.Update(context.Background(), got))

	desired, err = r.buildDesired(context.Background(), certs)
	require.NoError(t, err)
	require.Len(t, desired, 1, "changed ConfigMap must be re-decoded")
	assert.Contains(t, desired, TupleKey{Node: "gpu-03", Variant: "nccl-all-gather", Reason: "WorkloadFailed"})
}

// Memo entries for ConfigMaps no longer referenced by any cert are pruned.
func TestBuildDesired_PrunesMemoForRetiredCerts(t *testing.T) {
	cert := failedCert("results")
	cm := failedRowsCM(t, "results", "1", "gpu-01")
	r := newTestReconciler(t, newTestRecorder(), cert, cm)
	key := types.NamespacedName{Namespace: "ns", Name: "results"}

	_, err := r.buildDesired(context.Background(), []nvcrev1alpha1.Certification{*cert})
	require.NoError(t, err)
	require.Contains(t, r.results, key)

	_, err = r.buildDesired(context.Background(), nil)
	require.NoError(t, err)
	assert.NotContains(t, r.results, key)
}

// A node whose annotation cannot be parsed is skipped with a warning and
// contributes nothing to observed; the other nodes are read as usual.
func TestGetObserved_MalformedAnnotation_SkipsNode(t *testing.T) {
	recorder := newTestRecorder()
	good := newNodeWithCertFailures("gpu-01", `["nccl-all-gather/WorkloadFailed"]`)
	bad := newNodeWithCertFailures("gpu-02", `[nccl-all-gather/WorkloadFailed]`) // missing quotes
	r := newTestReconciler(t, recorder, good, bad)

	observed, err := r.getObserved(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[string]map[string]struct{}{
		"gpu-01": {"nccl-all-gather/WorkloadFailed": {}},
	}, observed)
}

// A node whose annotation is malformed is read as holding nothing, so a tuple
// on it that the monitor already published is treated as an operator clear:
// healthy is published and the cert's error-recovered list records it. The
// annotation itself is left for the operator to repair and the other nodes
// are processed as usual.
func TestProcessCertificationCRs_MalformedNodeAnnotation_ReadAsOperatorClear(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-nodes-cm", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, []nvcrev1alpha1.FailedNode{
				{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "held"},
				{Name: "gpu-02", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "held"},
			}),
		},
	}
	cert := newCert("cert-1", "test-ns", "Failed", "True", []nvcrev1alpha1.CertificationCategoryStatus{{
		Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationFailed,
		FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-nodes-cm"},
	}})
	terminal := cert.Status.Conditions[0].LastTransitionTime.Time
	cert.Annotations = map[string]string{state.CertProcessedKey: terminal.UTC().Format(time.RFC3339)}
	good := newNodeWithCertFailures("gpu-01", `["nccl-all-gather/WorkloadFailed"]`)
	bad := newNodeWithCertFailures("gpu-02", "not-json")

	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder, cm, cert, good, bad)
	ctx := context.Background()

	require.NoError(t, r.processCertificationCRs(ctx, []nvcrev1alpha1.Certification{*cert}, certTimesFor(t, cert)))

	require.Len(t, recorder.events, 1, "only the tuple on the malformed node is acted on")
	assert.Equal(t, "gpu-02", recorder.events[0].Node)
	assert.True(t, recorder.events[0].IsHealthy)
	assert.Equal(t, "nccl-all-gather/WorkloadFailed", recorder.events[0].ErrorCode)

	gotCert := &nvcrev1alpha1.Certification{}
	require.NoError(t, r.client.Get(ctx, types.NamespacedName{Name: "cert-1", Namespace: "test-ns"}, gotCert))
	assert.Contains(t, gotCert.Annotations[state.ErrorRecoveredKey], "gpu-02#nccl-all-gather/WorkloadFailed")

	gotNode := &corev1.Node{}
	require.NoError(t, r.client.Get(ctx, types.NamespacedName{Name: "gpu-02"}, gotNode))
	assert.Equal(t, "not-json", gotNode.Annotations[state.AnnotationKey], "the malformed value is left for the operator")
}

// --- handleCategoryFailure tests ---

func TestHandleCategoryFailure_AddsToDesired(t *testing.T) {
	failedRows := []nvcrev1alpha1.FailedNode{
		{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "workload deleted"},
		{Name: "gpu-02", Reason: nvcrev1alpha1.NodeFailureThresholdViolation, Message: "bandwidth low"},
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-nodes-cm", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, failedRows),
		},
	}

	cert := newCert("cert-1", "test-ns", "Failed", "True", nil)
	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder, cm)

	desired := make(map[TupleKey]NodeCertFailure)
	cat := nvcrev1alpha1.CertificationCategoryStatus{
		Domain: "communication", Variant: "nccl-all-gather",
		Status:         nvcrev1alpha1.CertificationFailed,
		FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-nodes-cm"},
	}

	err := r.handleCategoryFailure(context.Background(), cert, desired, cat, CertRef{Name: "cert-1", Namespace: "test-ns"}, map[types.NamespacedName]struct{}{})
	require.NoError(t, err)

	assert.Len(t, desired, 2)
	key1 := TupleKey{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}
	assert.Contains(t, desired, key1)
	assert.Equal(t, "workload deleted", desired[key1].Message)
	assert.Equal(t, "cert-1", desired[key1].CertRefs[0].Name)
}

func TestHandleCategoryFailure_FCFS_KeepsFirstCert_AppendsCertRef(t *testing.T) {
	failedRows := []nvcrev1alpha1.FailedNode{
		{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "second cert message"},
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-nodes-cm", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, failedRows),
		},
	}

	cert2 := newCert("cert-2", "test-ns", "Failed", "True", nil)
	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder, cm)

	key := TupleKey{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}
	desired := map[TupleKey]NodeCertFailure{
		key: {
			Message:  "first cert message",
			CertRefs: []CertRef{{Name: "cert-1", Namespace: "test-ns"}},
		},
	}

	cat := nvcrev1alpha1.CertificationCategoryStatus{
		Domain: "communication", Variant: "nccl-all-gather",
		Status:         nvcrev1alpha1.CertificationFailed,
		FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-nodes-cm"},
	}

	err := r.handleCategoryFailure(context.Background(), cert2, desired, cat, CertRef{Name: "cert-2", Namespace: "test-ns"}, map[types.NamespacedName]struct{}{})
	require.NoError(t, err)

	assert.Len(t, desired, 1)
	assert.Equal(t, "first cert message", desired[key].Message)
	assert.Len(t, desired[key].CertRefs, 2)
	assert.Equal(t, "cert-1", desired[key].CertRefs[0].Name)
	assert.Equal(t, "cert-2", desired[key].CertRefs[1].Name)
}

func TestHandleCategoryFailure_ErrorRecoveredSkip(t *testing.T) {
	failedRows := []nvcrev1alpha1.FailedNode{
		{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "should be skipped"},
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-nodes-cm", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, failedRows),
		},
	}

	cert := newCert("cert-1", "test-ns", "Failed", "True", nil)
	cert.Annotations = map[string]string{
		state.ErrorRecoveredKey: `["gpu-01#nccl-all-gather/WorkloadFailed"]`,
		state.CertProcessedKey:  cert.Status.Conditions[0].LastTransitionTime.UTC().Format(time.RFC3339),
	}

	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder, cm)

	desired := make(map[TupleKey]NodeCertFailure)
	cat := nvcrev1alpha1.CertificationCategoryStatus{
		Domain: "communication", Variant: "nccl-all-gather",
		Status:         nvcrev1alpha1.CertificationFailed,
		FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-nodes-cm"},
	}

	err := r.handleCategoryFailure(context.Background(), cert, desired, cat, CertRef{Name: "cert-1", Namespace: "test-ns"}, map[types.NamespacedName]struct{}{})
	require.NoError(t, err)

	assert.Empty(t, desired, "recovered tuple should be skipped")
}

func TestHandleCategoryFailure_PolicyFilter(t *testing.T) {
	failedRows := []nvcrev1alpha1.FailedNode{
		{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "should match"},
		{Name: "gpu-02", Reason: nvcrev1alpha1.NodeFailureHardwareDetected, Message: "should not match"},
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-nodes-cm", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, failedRows),
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(cm).Build()

	evaluator, err := config.NewEvaluator([]config.Policy{
		{Name: "filter", Match: "failedNode.reason == 'WorkloadFailed'"},
	})
	require.NoError(t, err)

	r := &Reconciler{
		client:        fakeClient,
		evaluator:     evaluator,
		certAnnotator: state.NewCertAnnotationHelper(fakeClient),
		results:       make(map[types.NamespacedName]decodedResult),
	}

	cert := newCert("cert-1", "test-ns", "Failed", "True", nil)
	desired := make(map[TupleKey]NodeCertFailure)
	cat := nvcrev1alpha1.CertificationCategoryStatus{
		Domain: "communication", Variant: "nccl-all-gather",
		Status:         nvcrev1alpha1.CertificationFailed,
		FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-nodes-cm"},
	}

	err = r.handleCategoryFailure(context.Background(), cert, desired, cat, CertRef{Name: "cert-1", Namespace: "test-ns"}, map[types.NamespacedName]struct{}{})
	require.NoError(t, err)

	assert.Len(t, desired, 1)
	assert.Contains(t, desired, TupleKey{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"})
}

// --- handleCategorySuccess tests ---

func TestHandleCategorySuccess_ClearsAllReasons(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "succeeded-nodes-cm", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"succeeded-nodes.csv.gz": mustGzip(t, []byte("gpu-01,gpu-02")),
		},
	}

	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder, cm)

	desired := map[TupleKey]NodeCertFailure{
		{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}:     {},
		{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "ThresholdViolation"}: {},
		{Node: "gpu-02", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}:     {},
		{Node: "gpu-03", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}:     {},
		{Node: "gpu-01", Variant: "nemotron5-8b", Reason: "WorkloadFailed"}:        {},
	}

	cat := nvcrev1alpha1.CertificationCategoryStatus{
		Domain: "communication", Variant: "nccl-all-gather",
		Status:            nvcrev1alpha1.CertificationSucceeded,
		SucceededNodesRef: &corev1.TypedLocalObjectReference{Name: "succeeded-nodes-cm"},
	}

	err := r.handleCategorySuccess(context.Background(), desired, cat, "test-ns", map[types.NamespacedName]struct{}{})
	require.NoError(t, err)

	assert.Len(t, desired, 2)
	assert.Contains(t, desired, TupleKey{Node: "gpu-03", Variant: "nccl-all-gather", Reason: "WorkloadFailed"})
	assert.Contains(t, desired, TupleKey{Node: "gpu-01", Variant: "nemotron5-8b", Reason: "WorkloadFailed"})
}

// --- buildDesired integration tests ---

func TestBuildDesired_SingleFailedCert(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-nodes-cm", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, []nvcrev1alpha1.FailedNode{
				{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "workload deleted"},
			}),
		},
	}

	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder, cm)

	certs := []nvcrev1alpha1.Certification{
		*newCert("cert-1", "test-ns", "Failed", "True", []nvcrev1alpha1.CertificationCategoryStatus{
			{
				Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationFailed,
				FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-nodes-cm"},
			},
		}),
	}

	desired, err := r.buildDesired(context.Background(), certs)
	require.NoError(t, err)
	assert.Len(t, desired, 1)

	key := TupleKey{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}
	assert.Contains(t, desired, key)
	assert.Equal(t, "workload deleted", desired[key].Message)
	assert.Equal(t, "cert-1", desired[key].CertRefs[0].Name)
}

func TestBuildDesired_RerunRecovery_SucceededClearsFailure(t *testing.T) {
	failedCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-nodes-cm", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, []nvcrev1alpha1.FailedNode{
				{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "workload deleted"},
			}),
		},
	}
	succeededCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "succeeded-nodes-cm", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"succeeded-nodes.csv.gz": mustGzip(t, []byte("gpu-01")),
		},
	}

	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder, failedCM, succeededCM)

	certs := []nvcrev1alpha1.Certification{
		*newCert("cert-old", "test-ns", "Failed", "True", []nvcrev1alpha1.CertificationCategoryStatus{
			{
				Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationFailed,
				FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-nodes-cm"},
			},
		}),
		*newCert("cert-rerun", "test-ns", "Succeeded", "True", []nvcrev1alpha1.CertificationCategoryStatus{
			{
				Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationSucceeded,
				SucceededNodesRef: &corev1.TypedLocalObjectReference{Name: "succeeded-nodes-cm"},
			},
		}),
	}

	desired, err := r.buildDesired(context.Background(), certs)
	require.NoError(t, err)
	assert.Empty(t, desired, "succeeded rerun should clear the failure")
}

// Certs are folded in completion order, so a Succeeded category only clears
// failures asserted by certs that completed before it. A fails at T1, C passes
// at T2, B fails at T3: C clears A's tuple, B re-adds it, and the tuple stays
// desired, owned by B alone.
func TestBuildDesired_RerunRecovery_LaterFailureSurvivesEarlierPass(t *testing.T) {
	failedA := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-a", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, []nvcrev1alpha1.FailedNode{
				{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "first failure"},
			}),
		},
	}
	succeededC := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "succeeded-c", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"succeeded-nodes.csv.gz": mustGzip(t, []byte("gpu-01")),
		},
	}
	failedB := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-b", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, []nvcrev1alpha1.FailedNode{
				{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "second failure"},
			}),
		},
	}

	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder, failedA, succeededC, failedB)

	certA := newCert("cert-a", "test-ns", "Failed", "True", []nvcrev1alpha1.CertificationCategoryStatus{
		{
			Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationFailed,
			FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-a"},
		},
	})
	certC := newCert("cert-c", "test-ns", "Succeeded", "True", []nvcrev1alpha1.CertificationCategoryStatus{
		{
			Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationSucceeded,
			SucceededNodesRef: &corev1.TypedLocalObjectReference{Name: "succeeded-c"},
		},
	})
	certB := newCert("cert-b", "test-ns", "Failed", "True", []nvcrev1alpha1.CertificationCategoryStatus{
		{
			Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationFailed,
			FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-b"},
		},
	})

	base := time.Now().Truncate(time.Second)
	certA.Status.Conditions[0].LastTransitionTime = metav1.NewTime(base)
	certC.Status.Conditions[0].LastTransitionTime = metav1.NewTime(base.Add(time.Minute))
	certB.Status.Conditions[0].LastTransitionTime = metav1.NewTime(base.Add(2 * time.Minute))

	// buildDesired receives the certs already sorted by completion time.
	desired, err := r.buildDesired(context.Background(), []nvcrev1alpha1.Certification{*certA, *certC, *certB})
	require.NoError(t, err)

	key := TupleKey{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}
	require.Contains(t, desired, key, "a failure asserted after the pass must stay desired")
	assert.Equal(t, "second failure", desired[key].Message)
	require.Len(t, desired[key].CertRefs, 1, "only the later failing cert owns the tuple")
	assert.Equal(t, "cert-b", desired[key].CertRefs[0].Name)
}

func TestBuildDesired_FCFS_OlderCertOwns_BothContribute(t *testing.T) {
	cm1 := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-cm-1", Namespace: "ns-1"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, []nvcrev1alpha1.FailedNode{
				{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "first cert"},
			}),
		},
	}
	cm2 := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-cm-2", Namespace: "ns-2"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, []nvcrev1alpha1.FailedNode{
				{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "second cert"},
			}),
		},
	}

	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder, cm1, cm2)

	certs := []nvcrev1alpha1.Certification{
		*newCert("cert-1", "ns-1", "Failed", "True", []nvcrev1alpha1.CertificationCategoryStatus{
			{
				Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationFailed,
				FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-cm-1"},
			},
		}),
		*newCert("cert-2", "ns-2", "Failed", "True", []nvcrev1alpha1.CertificationCategoryStatus{
			{
				Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationFailed,
				FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-cm-2"},
			},
		}),
	}

	desired, err := r.buildDesired(context.Background(), certs)
	require.NoError(t, err)
	assert.Len(t, desired, 1)

	key := TupleKey{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}
	assert.Equal(t, "first cert", desired[key].Message)
	assert.Len(t, desired[key].CertRefs, 2)
	assert.Equal(t, "cert-1", desired[key].CertRefs[0].Name)
	assert.Equal(t, "cert-2", desired[key].CertRefs[1].Name)
}

// A cert that CRE reopens and re-fails carries a stale cert-processed stamp.
// Its error-recovered list described the previous terminal state, so the
// released tuple is a new failure again: publish it, move the stamp, drop the
// list, and stay quiet on the following sweep.
func TestProcessCertificationCRs_ReopenedCertRepublishesReleasedTuple(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-nodes-cm", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, []nvcrev1alpha1.FailedNode{
				{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "failed again"},
			}),
		},
	}

	cert := newCert("cert-1", "test-ns", "Failed", "True", []nvcrev1alpha1.CertificationCategoryStatus{
		{
			Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationFailed,
			FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-nodes-cm"},
		},
	})
	terminal := cert.Status.Conditions[0].LastTransitionTime.Time
	cert.Annotations = map[string]string{
		state.CertProcessedKey:  terminal.Add(-10 * time.Minute).UTC().Format(time.RFC3339),
		state.ErrorRecoveredKey: `["gpu-01#nccl-all-gather/WorkloadFailed"]`,
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-01"}}

	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder, cm, cert, node)
	ctx := context.Background()

	require.NoError(t, r.processCertificationCRs(ctx, []nvcrev1alpha1.Certification{*cert}, certTimesFor(t, cert)))

	require.Len(t, recorder.events, 1, "the re-failed tuple is a new failure")
	assert.Equal(t, "gpu-01", recorder.events[0].Node)
	assert.False(t, recorder.events[0].IsHealthy)
	assert.Equal(t, "nccl-all-gather/WorkloadFailed", recorder.events[0].ErrorCode)

	got := &nvcrev1alpha1.Certification{}
	require.NoError(t, r.client.Get(ctx, types.NamespacedName{Name: "cert-1", Namespace: "test-ns"}, got))
	assert.Equal(t, terminal.UTC().Format(time.RFC3339), got.Annotations[state.CertProcessedKey])
	assert.NotContains(t, got.Annotations, state.ErrorRecoveredKey)

	require.NoError(t, r.processCertificationCRs(ctx, []nvcrev1alpha1.Certification{*got}, certTimesFor(t, got)))
	assert.Len(t, recorder.events, 1, "desired and observed: nothing more to publish")
}

func TestBuildDesired_ErrorRecoveredSkip_FallsThrough(t *testing.T) {
	cm1 := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-cm-1", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, []nvcrev1alpha1.FailedNode{
				{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "from cert-old"},
			}),
		},
	}
	cm2 := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "failed-cm-2", Namespace: "test-ns"},
		BinaryData: map[string][]byte{
			"failed-nodes.json.gz": mustGzipJSON(t, []nvcrev1alpha1.FailedNode{
				{Name: "gpu-01", Reason: nvcrev1alpha1.NodeFailureWorkloadFailed, Message: "from cert-new"},
			}),
		},
	}

	recorder := newTestRecorder()
	r := newTestReconciler(t, recorder, cm1, cm2)

	certOld := newCert("cert-old", "test-ns", "Failed", "True", []nvcrev1alpha1.CertificationCategoryStatus{
		{
			Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationFailed,
			FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-cm-1"},
		},
	})
	certOld.Annotations = map[string]string{
		state.ErrorRecoveredKey: `["gpu-01#nccl-all-gather/WorkloadFailed"]`,
		state.CertProcessedKey:  "true",
	}

	certNew := newCert("cert-new", "test-ns", "Failed", "True", []nvcrev1alpha1.CertificationCategoryStatus{
		{
			Domain: "communication", Variant: "nccl-all-gather", Status: nvcrev1alpha1.CertificationFailed,
			FailedNodesRef: &corev1.TypedLocalObjectReference{Name: "failed-cm-2"},
		},
	})

	certs := []nvcrev1alpha1.Certification{*certOld, *certNew}

	desired, err := r.buildDesired(context.Background(), certs)
	require.NoError(t, err)
	assert.Len(t, desired, 1)

	key := TupleKey{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}
	assert.Equal(t, "from cert-new", desired[key].Message)
	assert.Equal(t, "cert-new", desired[key].CertRefs[0].Name)
}

// An operator can corrupt the node annotation between the sweep's read and its
// write. The write must leave the annotation alone and the tuple is skipped for
// this sweep. The owning cert must not be stamped, otherwise the first sweep
// after the operator repairs the annotation would read the tuple as "operator
// recovered" instead of republishing it.
// A cert whose other tuple was written fine must still not be stamped when one
// of its tuples was skipped: with the stamp in place, the skipped tuple would
// read as an operator clear once the annotation is repaired.
func TestProcessDesiredAndObserved_MalformedAnnotationAtWrite_HoldsStampForWholeCert(t *testing.T) {
	recorder := newTestRecorder()
	cats := []nvcrev1alpha1.CertificationCategoryStatus{{
		Domain:  "nccl",
		Variant: "nccl-all-gather",
		Status:  nvcrev1alpha1.CertificationFailed,
	}}
	cert := newCert("cert-1", "ns", nvcrev1alpha1.CertificationFailed, "True", cats)
	good := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-01"}}
	bad := newNodeWithCertFailures("gpu-02", "not-json")
	r := newTestReconciler(t, recorder, cert, good, bad)

	owner := []CertRef{{Name: "cert-1", Namespace: "ns"}}
	desired := map[TupleKey]NodeCertFailure{
		{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}: {Message: "test", CertRefs: owner},
		{Node: "gpu-02", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}: {Message: "test", CertRefs: owner},
	}
	observed := map[string]map[string]struct{}{}

	certsToMark, err := r.processDesiredAndObserved(context.Background(), desired, observed, certTimesFor(t, cert))
	require.NoError(t, err)
	require.Len(t, recorder.events, 2, "both tuples are published; only the annotation write on gpu-02 fails")
	assert.Empty(t, certsToMark, "the cert must stay unstamped while any of its tuples is unwritten")

	got := &corev1.Node{}
	require.NoError(t, r.client.Get(context.Background(), types.NamespacedName{Name: "gpu-01"}, got))
	assert.Contains(t, got.Annotations[state.AnnotationKey], "nccl-all-gather/WorkloadFailed")
}

func TestProcessDesiredAndObserved_MalformedAnnotationAtWrite_SkipsTupleWithoutStamping(t *testing.T) {
	recorder := newTestRecorder()
	cats := []nvcrev1alpha1.CertificationCategoryStatus{{
		Domain:  "nccl",
		Variant: "nccl-all-gather",
		Status:  nvcrev1alpha1.CertificationFailed,
	}}
	cert := newCert("cert-1", "ns", nvcrev1alpha1.CertificationFailed, "True", cats)
	node := newNodeWithCertFailures("gpu-01", "not-json")
	r := newTestReconciler(t, recorder, cert, node)

	desired := map[TupleKey]NodeCertFailure{
		{Node: "gpu-01", Variant: "nccl-all-gather", Reason: "WorkloadFailed"}: {
			Message:  "test",
			CertRefs: []CertRef{{Name: "cert-1", Namespace: "ns"}},
		},
	}
	// The read side saw no annotation; the corruption landed after it.
	observed := map[string]map[string]struct{}{}

	certsToMark, err := r.processDesiredAndObserved(context.Background(), desired, observed, certTimesFor(t, cert))
	require.NoError(t, err)
	require.Len(t, recorder.events, 1)
	assert.False(t, recorder.events[0].IsHealthy)
	assert.Empty(t, certsToMark, "owner must stay unstamped so the tuple is republished once the annotation is repaired")

	got := &corev1.Node{}
	require.NoError(t, r.client.Get(context.Background(), types.NamespacedName{Name: "gpu-01"}, got))
	assert.Equal(t, "not-json", got.Annotations[state.AnnotationKey])
}
