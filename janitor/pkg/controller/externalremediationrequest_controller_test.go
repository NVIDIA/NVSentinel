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

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	nvsentinelv1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	janitormetrics "github.com/nvidia/nvsentinel/janitor/pkg/metrics"
)

const testExtRRNamespace = "default"

// newExtRRReconciler returns a reconciler bound to the envtest API server.
// Must only be called from within Ginkgo blocks (BeforeSuite populates cfg).
// Includes a FakeRecorder with a generous buffer so test specs can pop events
// off the channel without the recorder blocking the controller.
func newExtRRReconciler() *ExternalRemediationRequestReconciler {
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	return &ExternalRemediationRequestReconciler{
		Client:   c,
		Scheme:   scheme.Scheme,
		Recorder: record.NewFakeRecorder(64),
	}
}

// drainEvents pops up to maxN events from the FakeRecorder channel. Used by
// the observability tests to assert which events fired during a reconcile
// sequence. Returns whatever's currently in the buffer without blocking.
func drainEvents(r *ExternalRemediationRequestReconciler) []string {
	fake, ok := r.Recorder.(*record.FakeRecorder)
	if !ok {
		return nil
	}

	var got []string

	for {
		select {
		case e := <-fake.Events:
			got = append(got, e)
		default:
			return got
		}
	}
}

// testRecommendedActionLabel matches what production fault-remediation puts on
// CUSTOM:external-remediation events. Tests should keep this stable so the
// Prometheus label values in observability assertions match production reality.
const testRecommendedActionLabel = "external-remediation"

// newTestExtRR returns a minimal ExternalRemediationRequest object.
func newTestExtRR(name, nodeName string) *nvsentinelv1.ExternalRemediationRequest {
	return &nvsentinelv1.ExternalRemediationRequest{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "nvsentinel.dgxc.nvidia.com/v1",
			Kind:       "ExternalRemediationRequest",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testExtRRNamespace,
		},
		Spec: &protos.ExternalRemediationRequestSpec{
			HealthEvent: &protos.HealthEvent{
				Id:                      "he-" + name,
				NodeName:                nodeName,
				IsFatal:                 true,
				RecommendedAction:       protos.RecommendedAction_CUSTOM,
				CustomRecommendedAction: testRecommendedActionLabel,
				Message:                 "synthetic test fault",
			},
		},
	}
}

// reconcileToSteadyState drives Reconcile repeatedly so the multi-pass init
// (finalizer Update, then status Patch) completes without the test caring
// about exact pass counts.
func reconcileToSteadyState(
	ctx context.Context,
	r *ExternalRemediationRequestReconciler,
	key ctrlclient.ObjectKey,
	maxPasses int,
) *nvsentinelv1.ExternalRemediationRequest {
	GinkgoHelper()

	for i := 0; i < maxPasses; i++ {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred(), "reconcile pass %d", i+1)
	}

	var out nvsentinelv1.ExternalRemediationRequest
	Expect(r.Client.Get(ctx, key, &out)).To(Succeed())

	return &out
}

var _ = Describe("ExternalRemediationRequest Controller", func() {
	var (
		ctx context.Context
		r   *ExternalRemediationRequestReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = newExtRRReconciler()
	})

	It("adds the cleanup finalizer and initial Unknown conditions on a fresh ExtRR", func() {
		extrrObj := newTestExtRR("fresh-extrr-1", "node-fresh-1")
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(deleteExtRRForCleanup, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		got := reconcileToSteadyState(ctx, r, key, 3)

		Expect(controllerutil.ContainsFinalizer(got, ExternalRemediationFinalizer)).
			To(BeTrue(), "cleanup finalizer must be added")

		Expect(got.Status).NotTo(BeNil(), "Status must be populated")
		Expect(got.Status.Conditions).To(HaveLen(2), "two initial conditions expected")

		released := findExtRRCondition(got, ConditionNVSentinelOwnershipReleased)
		Expect(released).NotTo(BeNil())
		Expect(released.Status).To(Equal("Unknown"))
		Expect(released.Reason).To(Equal(reasonInitializing))
		Expect(released.Message).NotTo(BeEmpty())
		Expect(released.LastTransitionTime).NotTo(BeNil())

		complete := findExtRRCondition(got, ConditionExternalRemediationComplete)
		Expect(complete).NotTo(BeNil())
		Expect(complete.Status).To(Equal("Unknown"))
		Expect(complete.Reason).To(Equal(reasonAwaitingExternalSystem))
		Expect(complete.Message).NotTo(BeEmpty())
		Expect(complete.LastTransitionTime).NotTo(BeNil())
	})

	It("is idempotent on re-reconcile (no LastTransitionTime flap)", func() {
		extrrObj := newTestExtRR("idempotent-extrr-1", "node-idem-1")
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(deleteExtRRForCleanup, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		got := reconcileToSteadyState(ctx, r, key, 3)
		Expect(got.Status.Conditions).To(HaveLen(2))

		beforeTimes := map[string]time.Time{}
		for _, c := range got.Status.Conditions {
			Expect(c.LastTransitionTime).NotTo(BeNil(), "%s must have a LastTransitionTime", c.Type)
			beforeTimes[c.Type] = c.LastTransitionTime.AsTime()
		}
		Expect(beforeTimes).To(HaveLen(2))

		// Sleep so any flap would produce a strictly later timestamp.
		time.Sleep(50 * time.Millisecond)

		got = reconcileToSteadyState(ctx, r, key, 3)
		Expect(got.Status.Conditions).To(HaveLen(2))

		for _, c := range got.Status.Conditions {
			Expect(c.LastTransitionTime).NotTo(BeNil())

			before, ok := beforeTimes[c.Type]
			Expect(ok).To(BeTrue(), "unexpected condition after re-reconcile: %s", c.Type)
			Expect(c.LastTransitionTime.AsTime()).To(Equal(before),
				"%s LastTransitionTime flapped", c.Type)
		}
	})

	It("removes the cleanup finalizer when the ExtRR is deleted before apply ran (Node missing)", func() {
		// Deletion-pending before any Node interaction happened. cleanup helper finds
		// no Node, treats as already-clean, removes finalizer.
		extrrObj := newTestExtRR("delete-no-apply-extrr-1", "node-never-existed")
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		// Drive init only (finalizer + initial conditions). Don't continue further;
		// the apply path would requeue forever waiting for the missing Node.
		for i := 0; i < 2; i++ {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred(), "init pass %d", i+1)
		}

		Expect(r.Client.Delete(ctx, extrrObj)).To(Succeed())

		// One reconcile to run cleanup + remove the finalizer.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		// ExtRR must be fully gone now — apiserver garbage-collects once the
		// finalizer is removed.
		var got nvsentinelv1.ExternalRemediationRequest
		err = r.Client.Get(ctx, key, &got)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"ExtRR must be garbage-collected after the cleanup finalizer is removed")
	})

	It("swallows reconciles for missing objects", func() {
		result, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "missing-err", Namespace: testExtRRNamespace},
		})
		Expect(err).NotTo(HaveOccurred(), "missing object must be swallowed via client.IgnoreNotFound")
		Expect(result.RequeueAfter).To(BeZero())
		Expect(result.Requeue).To(BeFalse())
	})
})

// prepareReleased drives an ExtRR through init + apply so the Node has taint+label
// and the ExtRR has NVSentinelOwnershipReleased=True. Tests that need a "post-
// applied" starting state call this first. Returns the ExtRR's ObjectKey.
func prepareReleased(
	ctx context.Context, r *ExternalRemediationRequestReconciler,
	extrrName, nodeName string,
) ctrlclient.ObjectKey {
	GinkgoHelper()

	Expect(r.Client.Create(ctx, newTestNode(nodeName, nil, nil))).To(Succeed())
	extrrObj := newTestExtRR(extrrName, nodeName)
	Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())

	key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
	got := reconcileToSteadyState(ctx, r, key, 3)

	released := findExtRRCondition(got, ConditionNVSentinelOwnershipReleased)
	Expect(released).NotTo(BeNil())
	Expect(released.Status).To(Equal("True"),
		"apply path must succeed before post-released tests can run")

	return key
}

// setExternalRemediationComplete sets the ExternalRemediationComplete condition
// to the given status by issuing a status subresource patch — simulates the
// external system reporting completion to the ExtRR.
func setExternalRemediationComplete(
	ctx context.Context, c ctrlclient.Client,
	extrrObj *nvsentinelv1.ExternalRemediationRequest,
	status string, reason string,
) {
	GinkgoHelper()

	key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}

	var fresh nvsentinelv1.ExternalRemediationRequest
	Expect(c.Get(ctx, key, &fresh)).To(Succeed())

	original := fresh.DeepCopy()

	conds := []metav1.Condition{}
	if fresh.Status != nil {
		for _, cnd := range fresh.Status.Conditions {
			conds = append(conds, metav1.Condition{
				Type:               cnd.Type,
				Status:             metav1.ConditionStatus(cnd.Status),
				Reason:             cnd.Reason,
				Message:            cnd.Message,
				LastTransitionTime: metav1.NewTime(cnd.LastTransitionTime.AsTime()),
			})
		}
	}

	// Find or replace ExternalRemediationComplete.
	replaced := false

	for i := range conds {
		if conds[i].Type == ConditionExternalRemediationComplete {
			conds[i] = metav1.Condition{
				Type:               ConditionExternalRemediationComplete,
				Status:             metav1.ConditionStatus(status),
				Reason:             reason,
				Message:            "set by test",
				LastTransitionTime: metav1.Now(),
			}
			replaced = true

			break
		}
	}

	if !replaced {
		conds = append(conds, metav1.Condition{
			Type:               ConditionExternalRemediationComplete,
			Status:             metav1.ConditionStatus(status),
			Reason:             reason,
			Message:            "set by test",
			LastTransitionTime: metav1.Now(),
		})
	}

	if fresh.Status == nil {
		fresh.Status = &protos.ExternalRemediationRequestStatus{}
	}

	fresh.Status.Conditions = nil
	for _, m := range conds {
		fresh.Status.Conditions = append(fresh.Status.Conditions, &protos.Condition{
			Type:               m.Type,
			Status:             string(m.Status),
			Reason:             m.Reason,
			Message:            m.Message,
			LastTransitionTime: timestamppb.New(m.LastTransitionTime.Time),
		})
	}

	Expect(c.Status().Patch(ctx, &fresh, ctrlclient.MergeFrom(original))).To(Succeed())
}

var _ = Describe("ExternalRemediationRequest Controller resolution paths (branches 2+4)", func() {
	var (
		ctx context.Context
		r   *ExternalRemediationRequestReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = newExtRRReconciler()
	})

	Context("branch 4: ExternalRemediationComplete=True (external system reports success)", func() {
		It("removes the release taint and managed=false label; ExtRR stays with finalizer", func() {
			nodeName := "node-true-1"
			key := prepareReleased(ctx, r, "true-extrr-1", nodeName)
			DeferCleanup(forceFinalizerRemovalByKey, ctx, r, key)
			DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

			// External system reports success.
			setExternalRemediationComplete(ctx, r.Client,
				&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
				}}, "True", "ExternalRemediationSucceeded")

			// One reconcile to run branch 4.
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			var node corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node)).To(Succeed())
			Expect(findTaintByKey(node.Spec.Taints, ReleaseTaintKey)).To(BeNil(),
				"release taint must be removed after Complete=True")
			Expect(node.Labels).NotTo(HaveKey(managed.ManagedLabelKey),
				"managed label must be removed entirely (absence is the default-managed state)")

			// ExtRR stays in the cluster as a historical record with the finalizer attached.
			var got nvsentinelv1.ExternalRemediationRequest
			Expect(r.Client.Get(ctx, key, &got)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(&got, ExternalRemediationFinalizer)).To(BeTrue(),
				"finalizer stays attached on True-driven cleanup (ExtRR is the historical record)")
		})

		It("does not re-PATCH the Node on subsequent reconciles after cleanup", func() {
			nodeName := "node-true-idem-1"
			key := prepareReleased(ctx, r, "true-idem-extrr-1", nodeName)
			DeferCleanup(forceFinalizerRemovalByKey, ctx, r, key)
			DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

			setExternalRemediationComplete(ctx, r.Client,
				&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
				}}, "True", "ExternalRemediationSucceeded")

			// First reconcile: cleanup happens.
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			var nodeAfterCleanup corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfterCleanup)).To(Succeed())
			rvAfterCleanup := nodeAfterCleanup.ResourceVersion

			// Subsequent reconciles re-enter branch 4 but should short-circuit (nothing to remove).
			for i := 0; i < 3; i++ {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}

			var nodeAfterRereconcile corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfterRereconcile)).To(Succeed())
			Expect(nodeAfterRereconcile.ResourceVersion).To(Equal(rvAfterCleanup),
				"Node ResourceVersion must not advance after the cleanup PATCH settles")
		})

		It("leaves a foreign taint in place when another ExtRR claims the node (drift on cleanup)", func() {
			// Drive an ExtRR to released, then sneak in a second taint with a different
			// value, then trigger Complete=True. The cleanup must remove the matching
			// taint and label but leave the foreign taint untouched.
			nodeName := "node-true-drift-1"
			key := prepareReleased(ctx, r, "true-drift-extrr-1", nodeName)
			DeferCleanup(forceFinalizerRemovalByKey, ctx, r, key)
			DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

			// Manually mutate the node: replace our taint with one owned by a hypothetical other ExtRR.
			// This simulates a drift state — should not happen in practice, but the helper must be safe.
			var node corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node)).To(Succeed())

			origNode := node.DeepCopy()
			for i := range node.Spec.Taints {
				if node.Spec.Taints[i].Key == ReleaseTaintKey {
					node.Spec.Taints[i].Value = "foreign-err"
					break
				}
			}

			Expect(r.Client.Patch(ctx, &node, ctrlclient.StrategicMergeFrom(origNode))).To(Succeed())

			setExternalRemediationComplete(ctx, r.Client,
				&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
				}}, "True", "ExternalRemediationSucceeded")

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node)).To(Succeed())
			taint := findTaintByKey(node.Spec.Taints, ReleaseTaintKey)
			Expect(taint).NotTo(BeNil(), "foreign taint must NOT be removed by our cleanup")
			Expect(taint.Value).To(Equal("foreign-err"))
			Expect(node.Labels).NotTo(HaveKey(managed.ManagedLabelKey),
				"label removal is unconditional (cluster-wide semantics)")
		})
	})

	Context("branch 2: deletionTimestamp set (operator-driven release)", func() {
		It("runs cleanup and removes the finalizer; ExtRR is garbage-collected", func() {
			nodeName := "node-del-1"
			key := prepareReleased(ctx, r, "del-extrr-1", nodeName)
			DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

			Expect(r.Client.Delete(ctx, &nvsentinelv1.ExternalRemediationRequest{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			})).To(Succeed())

			// One reconcile to handle branch 2 — cleanup + finalizer remove.
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Node should be clean.
			var node corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node)).To(Succeed())
			Expect(findTaintByKey(node.Spec.Taints, ReleaseTaintKey)).To(BeNil())
			Expect(node.Labels).NotTo(HaveKey(managed.ManagedLabelKey))

			// ExtRR should be garbage-collected.
			var got nvsentinelv1.ExternalRemediationRequest
			err = r.Client.Get(ctx, key, &got)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"ExtRR must be garbage-collected after operator-driven cleanup")
		})

		It("removes the finalizer cleanly when cleanup already ran via Complete=True", func() {
			nodeName := "node-stack-1"
			key := prepareReleased(ctx, r, "stack-extrr-1", nodeName)
			DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

			// First: external system reports success → branch 4 cleans up.
			setExternalRemediationComplete(ctx, r.Client,
				&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
				}}, "True", "ExternalRemediationSucceeded")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Then: operator deletes the (already-clean) ExtRR.
			Expect(r.Client.Delete(ctx, &nvsentinelv1.ExternalRemediationRequest{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			})).To(Succeed())

			var nodeBeforeDel corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeBeforeDel)).To(Succeed())
			rvBeforeDel := nodeBeforeDel.ResourceVersion

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			// Node was already clean: cleanup helper short-circuits, no PATCH.
			var nodeAfter corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfter)).To(Succeed())
			Expect(nodeAfter.ResourceVersion).To(Equal(rvBeforeDel),
				"already-clean cleanup must NOT re-PATCH the Node")

			// ExtRR is garbage-collected.
			var got nvsentinelv1.ExternalRemediationRequest
			err = r.Client.Get(ctx, key, &got)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"ExtRR must be garbage-collected after operator-delete on already-clean state")
		})

		It("removes the finalizer even when the target Node has already been deleted", func() {
			nodeName := "node-gone-1"
			key := prepareReleased(ctx, r, "gone-extrr-1", nodeName)
			// Simulate the external system terminating the Node mid-remediation.
			var node corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node)).To(Succeed())
			Expect(r.Client.Delete(ctx, &node)).To(Succeed())

			Expect(r.Client.Delete(ctx, &nvsentinelv1.ExternalRemediationRequest{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			})).To(Succeed())

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred(), "cleanup must treat a missing Node as already-clean")

			var got nvsentinelv1.ExternalRemediationRequest
			err = r.Client.Get(ctx, key, &got)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"ExtRR must be garbage-collected even when its Node is gone")
		})
	})

	It("runs the full happy-path lifecycle end-to-end (apply → Complete=True → cleanup)", func() {
		nodeName := "node-lifecycle-1"
		key := prepareReleased(ctx, r, "lifecycle-extrr-1", nodeName)
		DeferCleanup(forceFinalizerRemovalByKey, ctx, r, key)
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		var nodeAfterApply corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfterApply)).To(Succeed())
		Expect(findTaintByKey(nodeAfterApply.Spec.Taints, ReleaseTaintKey)).NotTo(BeNil())
		Expect(nodeAfterApply.Labels).To(HaveKeyWithValue(managed.ManagedLabelKey, managed.ManagedLabelValueFalse))

		setExternalRemediationComplete(ctx, r.Client,
			&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
			}}, "True", "ExternalRemediationSucceeded")

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var nodeAfterCleanup corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfterCleanup)).To(Succeed())
		Expect(findTaintByKey(nodeAfterCleanup.Spec.Taints, ReleaseTaintKey)).To(BeNil(),
			"end-of-lifecycle: release taint removed")
		Expect(nodeAfterCleanup.Labels).NotTo(HaveKey(managed.ManagedLabelKey),
			"end-of-lifecycle: managed label removed")
	})
})

var _ = Describe("ExternalRemediationRequest Controller asymmetric False handling (branch 5)", func() {
	var (
		ctx context.Context
		r   *ExternalRemediationRequestReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = newExtRRReconciler()
	})

	It("keeps the release taint and managed=false label in place when the external system reports failure", func() {
		nodeName := "node-false-1"
		key := prepareReleased(ctx, r, "false-extrr-1", nodeName)
		DeferCleanup(forceFinalizerRemovalByKey, ctx, r, key)
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		var nodeBefore corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeBefore)).To(Succeed())
		rvBefore := nodeBefore.ResourceVersion

		setExternalRemediationComplete(ctx, r.Client,
			&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
			}}, "False", "ExternalRemediationFailed")

		// One reconcile to run branch 5.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var nodeAfter corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfter)).To(Succeed())

		// Critical guarantee: branch 5 must NOT touch the Node.
		Expect(nodeAfter.ResourceVersion).To(Equal(rvBefore),
			"branch 5 must NOT PATCH the Node — taint+label remain because operator has no signal what state the external system left the node in")
		taint := findTaintByKey(nodeAfter.Spec.Taints, ReleaseTaintKey)
		Expect(taint).NotTo(BeNil(), "release taint must remain in place on Complete=False")
		Expect(taint.Value).To(Equal(key.Name))
		Expect(nodeAfter.Labels).To(HaveKeyWithValue(managed.ManagedLabelKey, managed.ManagedLabelValueFalse),
			"managed=false label must remain in place on Complete=False")
	})

	It("is idempotent: re-reconciles while at False do not PATCH the Node", func() {
		nodeName := "node-false-idem-1"
		key := prepareReleased(ctx, r, "false-idem-extrr-1", nodeName)
		DeferCleanup(forceFinalizerRemovalByKey, ctx, r, key)
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		setExternalRemediationComplete(ctx, r.Client,
			&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
			}}, "False", "ExternalRemediationFailed")

		// First reconcile to settle into branch 5.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var nodeBaseline corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeBaseline)).To(Succeed())
		rvBaseline := nodeBaseline.ResourceVersion

		// Subsequent reconciles must not PATCH the Node.
		for i := 0; i < 3; i++ {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}

		var nodeFinal corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeFinal)).To(Succeed())
		Expect(nodeFinal.ResourceVersion).To(Equal(rvBaseline),
			"branch 5 must remain a no-op across repeated reconciles")
	})

	It("recovers via branch 4 when the external system retries and patches True", func() {
		nodeName := "node-false-to-true-1"
		key := prepareReleased(ctx, r, "false-to-true-extrr-1", nodeName)
		DeferCleanup(forceFinalizerRemovalByKey, ctx, r, key)
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		// Failure path: external system reports False.
		setExternalRemediationComplete(ctx, r.Client,
			&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
			}}, "False", "ExternalRemediationFailed")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		// Verify taint+label still present (branch 5 left them alone).
		var nodeAtFalse corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAtFalse)).To(Succeed())
		Expect(findTaintByKey(nodeAtFalse.Spec.Taints, ReleaseTaintKey)).NotTo(BeNil())
		Expect(nodeAtFalse.Labels).To(HaveKeyWithValue(managed.ManagedLabelKey, managed.ManagedLabelValueFalse))

		// Retry: external system now reports True. Branch 4 must fire and clean up.
		setExternalRemediationComplete(ctx, r.Client,
			&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
			}}, "True", "ExternalRemediationSucceeded")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var nodeAfterCleanup corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfterCleanup)).To(Succeed())
		Expect(findTaintByKey(nodeAfterCleanup.Spec.Taints, ReleaseTaintKey)).To(BeNil(),
			"False->True retry must trigger branch 4 cleanup")
		Expect(nodeAfterCleanup.Labels).NotTo(HaveKey(managed.ManagedLabelKey))
	})

	It("releases the node via branch 2 when the operator deletes the ExtRR while it sits at False", func() {
		nodeName := "node-false-del-1"
		key := prepareReleased(ctx, r, "false-del-extrr-1", nodeName)
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		setExternalRemediationComplete(ctx, r.Client,
			&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
			}}, "False", "ExternalRemediationFailed")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		// Operator forces release.
		Expect(r.Client.Delete(ctx, &nvsentinelv1.ExternalRemediationRequest{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		})).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var nodeAfter corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfter)).To(Succeed())
		Expect(findTaintByKey(nodeAfter.Spec.Taints, ReleaseTaintKey)).To(BeNil(),
			"operator delete must trigger branch 2 cleanup even from the False state")
		Expect(nodeAfter.Labels).NotTo(HaveKey(managed.ManagedLabelKey))

		var got nvsentinelv1.ExternalRemediationRequest
		err = r.Client.Get(ctx, key, &got)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"ExtRR must be garbage-collected after operator delete from the False state")
	})
})

var _ = Describe("ExternalRemediationRequest Controller apply path (branch 3)", func() {
	var (
		ctx context.Context
		r   *ExternalRemediationRequestReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = newExtRRReconciler()
	})

	It("applies the release taint and managed=false label, transitions condition to True", func() {
		nodeName := "node-apply-1"
		Expect(r.Client.Create(ctx, newTestNode(nodeName, nil, nil))).To(Succeed())
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		extrrObj := newTestExtRR("apply-extrr-1", nodeName)
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(deleteExtRRForCleanup, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		got := reconcileToSteadyState(ctx, r, key, 3)

		released := findExtRRCondition(got, ConditionNVSentinelOwnershipReleased)
		Expect(released).NotTo(BeNil())
		Expect(released.Status).To(Equal("True"), "NVSentinelOwnershipReleased must transition to True on successful apply")
		Expect(released.Reason).To(Equal(ReasonReleaseTaintApplied))
		Expect(released.Message).To(ContainSubstring(ReleaseTaintKey))
		Expect(released.Message).To(ContainSubstring(extrrObj.Name))

		var node corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node)).To(Succeed())

		taint := findTaintByKey(node.Spec.Taints, ReleaseTaintKey)
		Expect(taint).NotTo(BeNil(), "release taint must be applied")
		Expect(taint.Value).To(Equal(extrrObj.Name), "taint value must carry owning ExtRR's name")
		Expect(taint.Effect).To(Equal(corev1.TaintEffectNoSchedule))
		Expect(node.Labels).To(HaveKeyWithValue(managed.ManagedLabelKey, managed.ManagedLabelValueFalse),
			"managed=false label must be set")
	})

	It("does not re-PATCH the Node on subsequent reconciles after a successful apply", func() {
		nodeName := "node-stable-1"
		Expect(r.Client.Create(ctx, newTestNode(nodeName, nil, nil))).To(Succeed())
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		extrrObj := newTestExtRR("stable-extrr-1", nodeName)
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(deleteExtRRForCleanup, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		reconcileToSteadyState(ctx, r, key, 3)

		var nodeAfterApply corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfterApply)).To(Succeed())
		rvAfterApply := nodeAfterApply.ResourceVersion

		// Reconcile several more times. With Released=True the dispatcher falls through to
		// branch 6 (no-op), so the Node's ResourceVersion must not advance.
		for i := 0; i < 3; i++ {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}

		var nodeAfterRereconcile corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfterRereconcile)).To(Succeed())
		Expect(nodeAfterRereconcile.ResourceVersion).To(Equal(rvAfterApply),
			"Node ResourceVersion must not change after the apply path settles")
	})

	It("recovers cleanly when a prior reconcile patched the Node but failed to transition the condition", func() {
		nodeName := "node-recover-1"
		// Pre-apply the taint + label as if a prior reconcile succeeded then crashed
		// before status was written. Verifies the already-applied detection.
		Expect(r.Client.Create(ctx, newTestNode(nodeName,
			map[string]string{managed.ManagedLabelKey: managed.ManagedLabelValueFalse},
			[]corev1.Taint{{Key: ReleaseTaintKey, Value: "recover-extrr-1", Effect: corev1.TaintEffectNoSchedule}}))).
			To(Succeed())
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		extrrObj := newTestExtRR("recover-extrr-1", nodeName)
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(deleteExtRRForCleanup, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		got := reconcileToSteadyState(ctx, r, key, 3)

		released := findExtRRCondition(got, ConditionNVSentinelOwnershipReleased)
		Expect(released.Status).To(Equal("True"))
		Expect(released.Reason).To(Equal(ReasonReleaseTaintApplied))
		Expect(released.Message).To(ContainSubstring("already present"),
			"already-applied message should call out the recovery case for operator visibility")
	})

	It("requeues without transitioning when the target Node does not exist", func() {
		extrrObj := newTestExtRR("missing-node-extrr-1", "node-does-not-exist")
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(deleteExtRRForCleanup, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}

		// reconcileInitialize uses two passes (finalizer Update, then status Patch).
		// Drive both to completion before checking branch 3 behavior.
		for i := 0; i < 2; i++ {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred(), "init pass %d", i+1)
		}

		// Third reconcile hits branch 3, finds the Node missing, requeues.
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred(), "missing Node must not propagate as a reconcile error")
		Expect(result.RequeueAfter).To(Equal(nodeMissingRequeue), "missing Node must requeue, not fail")

		var got nvsentinelv1.ExternalRemediationRequest
		Expect(r.Client.Get(ctx, key, &got)).To(Succeed())
		released := findExtRRCondition(&got, ConditionNVSentinelOwnershipReleased)
		Expect(released.Status).To(Equal("Unknown"),
			"missing Node must leave NVSentinelOwnershipReleased Unknown for retry")
		Expect(released.Reason).To(Equal(reasonInitializing))
	})

	// The empty-nodeName failure mode is enforced by the validating webhook
	// (see pkg/webhook/v1alpha1) — the apiserver rejects creation of an
	// ExtRR without spec.healthEvent.nodeName, so the reconciler never sees
	// one. Exercised in the webhook test suite, not here.

	It("transitions to False when the Node is already tainted by a different ExtRR (drift)", func() {
		nodeName := "node-drift-1"
		// Pre-apply the taint with a DIFFERENT ExtRR's name as the value.
		Expect(r.Client.Create(ctx, newTestNode(nodeName, nil,
			[]corev1.Taint{{Key: ReleaseTaintKey, Value: "some-other-err", Effect: corev1.TaintEffectNoSchedule}}))).
			To(Succeed())
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		extrrObj := newTestExtRR("drift-extrr-1", nodeName)
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(deleteExtRRForCleanup, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		got := reconcileToSteadyState(ctx, r, key, 3)

		released := findExtRRCondition(got, ConditionNVSentinelOwnershipReleased)
		Expect(released.Status).To(Equal("False"), "drift must transition condition to False")
		Expect(released.Reason).To(Equal(ReasonReleaseTaintFailed))
		Expect(released.Message).To(ContainSubstring("some-other-err"),
			"drift message must identify the existing taint owner")
		Expect(released.Message).To(ContainSubstring(nodeName))

		// Node taint must be unchanged — we don't overwrite another ExtRR's claim.
		var node corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node)).To(Succeed())
		taint := findTaintByKey(node.Spec.Taints, ReleaseTaintKey)
		Expect(taint).NotTo(BeNil())
		Expect(taint.Value).To(Equal("some-other-err"), "drift case must NOT overwrite the existing taint")
		Expect(node.Labels).NotTo(HaveKey(managed.ManagedLabelKey), "drift case must NOT set managed=false")
	})

	It("transitions to False when the Node patch is forbidden by RBAC", func() {
		nodeName := "node-rbac-1"

		// Build a watch-capable client for both the test setup and the interceptor
		// wrap; the interceptor requires WithWatch, not the plain Client interface.
		baseClient, err := ctrlclient.NewWithWatch(cfg, ctrlclient.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		Expect(baseClient.Create(ctx, newTestNode(nodeName, nil, nil))).To(Succeed())
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		// Wrap the reconciler's client with an interceptor that turns Node PATCHes
		// into HTTP 403 Forbidden, simulating a missing RBAC binding.
		r.Client = interceptor.NewClient(baseClient, interceptor.Funcs{
			Patch: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object,
				patch ctrlclient.Patch, opts ...ctrlclient.PatchOption,
			) error {
				if _, ok := obj.(*corev1.Node); ok {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, obj.GetName(),
						fmt.Errorf("simulated RBAC denial"))
				}

				return c.Patch(ctx, obj, patch, opts...)
			},
		})

		extrrObj := newTestExtRR("rbac-extrr-1", nodeName)
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(deleteExtRRForCleanup, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		got := reconcileToSteadyState(ctx, r, key, 3)

		released := findExtRRCondition(got, ConditionNVSentinelOwnershipReleased)
		Expect(released.Status).To(Equal("False"), "forbidden patch must transition condition to False")
		Expect(released.Reason).To(Equal(ReasonReleaseTaintFailed))
		Expect(released.Message).To(ContainSubstring("forbidden"))
	})
})

// TestExtRRReconciler_NeedsInitialization is a pure-unit test (no envtest needed),
// so it runs as a plain testing.T function alongside the ginkgo specs.
func TestExtRRReconciler_NeedsInitialization(t *testing.T) {
	r := &ExternalRemediationRequestReconciler{}

	t.Run("no finalizer, no conditions", func(t *testing.T) {
		extrrObj := newTestExtRR("a", "n")
		assert.True(t, r.needsInitialization(extrrObj))
	})

	t.Run("finalizer present, no conditions", func(t *testing.T) {
		extrrObj := newTestExtRR("a", "n")
		extrrObj.Finalizers = []string{ExternalRemediationFinalizer}
		assert.True(t, r.needsInitialization(extrrObj))
	})

	t.Run("finalizer absent, conditions present", func(t *testing.T) {
		extrrObj := newTestExtRR("a", "n")
		extrrObj.Status = &protos.ExternalRemediationRequestStatus{
			Conditions: []*protos.Condition{
				{Type: ConditionNVSentinelOwnershipReleased, Status: "Unknown"},
				{Type: ConditionExternalRemediationComplete, Status: "Unknown"},
			},
		}
		assert.True(t, r.needsInitialization(extrrObj))
	})

	t.Run("fully initialized", func(t *testing.T) {
		extrrObj := newTestExtRR("a", "n")
		extrrObj.Finalizers = []string{ExternalRemediationFinalizer}
		extrrObj.Status = &protos.ExternalRemediationRequestStatus{
			Conditions: []*protos.Condition{
				{Type: ConditionNVSentinelOwnershipReleased, Status: "Unknown"},
				{Type: ConditionExternalRemediationComplete, Status: "Unknown"},
			},
		}
		assert.False(t, r.needsInitialization(extrrObj))
	})

	t.Run("only one condition present", func(t *testing.T) {
		extrrObj := newTestExtRR("a", "n")
		extrrObj.Finalizers = []string{ExternalRemediationFinalizer}
		extrrObj.Status = &protos.ExternalRemediationRequestStatus{
			Conditions: []*protos.Condition{
				{Type: ConditionNVSentinelOwnershipReleased, Status: "Unknown"},
			},
		}
		assert.True(t, r.needsInitialization(extrrObj))
	})
}

// deleteExtRRForCleanup removes the cleanup finalizer and deletes the ExtRR so the
// next test starts clean. Used as a DeferCleanup target.
func deleteExtRRForCleanup(ctx context.Context, r *ExternalRemediationRequestReconciler, extrrObj *nvsentinelv1.ExternalRemediationRequest) {
	forceFinalizerRemoval(ctx, r, extrrObj)
}

// forceFinalizerRemoval strips the cleanup finalizer (if present) and ensures
// the object is fully deleted from the API server so tests don't bleed state.
func forceFinalizerRemoval(ctx context.Context, r *ExternalRemediationRequestReconciler, extrrObj *nvsentinelv1.ExternalRemediationRequest) {
	forceFinalizerRemovalByKey(ctx, r, ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace})
}

// forceFinalizerRemovalByKey is the same as forceFinalizerRemoval but takes a
// key directly — used by resolution-path tests where the test no longer holds
// a live ExtRR pointer (the object may be mid-deletion or fully garbage-collected).
// Errors during cleanup are logged to GinkgoWriter (not silently swallowed) so a
// flaky shared-state failure leaves a breadcrumb in the test output.
func forceFinalizerRemovalByKey(ctx context.Context, r *ExternalRemediationRequestReconciler, key ctrlclient.ObjectKey) {
	var fresh nvsentinelv1.ExternalRemediationRequest
	if err := r.Client.Get(ctx, key, &fresh); err != nil {
		if !apierrors.IsNotFound(err) {
			fmt.Fprintf(GinkgoWriter, "cleanup Get(%s/%s): %v\n", key.Namespace, key.Name, err)
		}

		return
	}

	if controllerutil.RemoveFinalizer(&fresh, ExternalRemediationFinalizer) {
		if err := r.Client.Update(ctx, &fresh); err != nil && !apierrors.IsNotFound(err) {
			fmt.Fprintf(GinkgoWriter, "cleanup Update(%s/%s): %v\n", key.Namespace, key.Name, err)
		}
	}

	if err := r.Client.Delete(ctx, &fresh); err != nil && !apierrors.IsNotFound(err) {
		fmt.Fprintf(GinkgoWriter, "cleanup Delete(%s/%s): %v\n", key.Namespace, key.Name, err)
	}
}

// newTestNode returns a minimal corev1.Node usable from envtest. labels/taints
// are optional; pass nil to start clean.
func newTestNode(name string, labels map[string]string, taints []corev1.Taint) *corev1.Node {
	return &corev1.Node{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Node",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Spec: corev1.NodeSpec{
			Taints: taints,
		},
	}
}

// deleteNodeForCleanup removes a Node so tests don't bleed state. Used as a
// DeferCleanup target. Errors are logged to GinkgoWriter.
func deleteNodeForCleanup(ctx context.Context, r *ExternalRemediationRequestReconciler, nodeName string) {
	var node corev1.Node
	if err := r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node); err != nil {
		if !apierrors.IsNotFound(err) {
			fmt.Fprintf(GinkgoWriter, "cleanup Get(node/%s): %v\n", nodeName, err)
		}

		return
	}

	if err := r.Client.Delete(ctx, &node); err != nil && !apierrors.IsNotFound(err) {
		fmt.Fprintf(GinkgoWriter, "cleanup Delete(node/%s): %v\n", nodeName, err)
	}
}

// findExtRRCondition returns the proto Condition with the given type, or nil.
func findExtRRCondition(extrrObj *nvsentinelv1.ExternalRemediationRequest, condType string) *protos.Condition {
	if extrrObj.Status == nil {
		return nil
	}

	for _, c := range extrrObj.Status.Conditions {
		if c.Type == condType {
			return c
		}
	}

	return nil
}

// snapshotConditions returns a stable string representation of the ExtRR's
// status conditions, sidestepping proto-message equality quirks
// (sync.Mutex in protoimpl.MessageState).
func snapshotConditions(extrrObj *nvsentinelv1.ExternalRemediationRequest) string {
	if extrrObj.Status == nil {
		return ""
	}

	var parts []string

	for _, c := range extrrObj.Status.Conditions {
		var ts string
		if c.LastTransitionTime != nil {
			ts = c.LastTransitionTime.AsTime().Format(time.RFC3339Nano)
		}

		parts = append(parts, c.Type+"="+c.Status+":"+c.Reason+":"+c.Message+"@"+ts)
	}

	return strings.Join(parts, "|")
}

var _ = Describe("ExternalRemediationRequest Controller observability", func() {
	var (
		ctx context.Context
		r   *ExternalRemediationRequestReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = newExtRRReconciler()
	})

	It("increments err_total{created} exactly once on first init", func() {
		extrrObj := newTestExtRR("obs-created-1", "node-obs-created-1")
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(forceFinalizerRemoval, ctx, r, extrrObj)

		before := testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseCreated, janitormetrics.ExtRRResultNone))

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		// Drive multiple reconciles; setInitialConditions only fires the
		// counter on the pass that actually writes them.
		for i := 0; i < 3; i++ {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}

		after := testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseCreated, janitormetrics.ExtRRResultNone))
		Expect(after-before).To(BeNumerically("==", 1.0),
			"err_total{created} must fire exactly once across an init + idempotent re-reconciles")
	})

	It("increments released{success} + err_open{awaiting} + emits ReleaseTaintApplied on apply", func() {
		nodeName := "node-obs-applied-1"
		Expect(r.Client.Create(ctx, newTestNode(nodeName, nil, nil))).To(Succeed())
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		releasedBefore := testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseReleased, janitormetrics.ExtRRResultSuccess))
		openBefore := testutil.ToFloat64(janitormetrics.ExtRROpen.WithLabelValues(
			nodeName, testRecommendedActionLabel, janitormetrics.ExtRROpenStateAwaiting))

		extrrObj := newTestExtRR("obs-applied-1", nodeName)
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(forceFinalizerRemoval, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		reconcileToSteadyState(ctx, r, key, 3)

		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseReleased, janitormetrics.ExtRRResultSuccess)) - releasedBefore).
			To(BeNumerically("==", 1.0))
		Expect(testutil.ToFloat64(janitormetrics.ExtRROpen.WithLabelValues(
			nodeName, testRecommendedActionLabel, janitormetrics.ExtRROpenStateAwaiting)) - openBefore).
			To(BeNumerically("==", 1.0))

		// Re-reconcile to confirm we don't double-count once the condition has
		// transitioned to True (the dispatcher exits branch 3 and stops calling
		// the transition helpers).
		for i := 0; i < 3; i++ {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseReleased, janitormetrics.ExtRRResultSuccess)) - releasedBefore).
			To(BeNumerically("==", 1.0), "released{success} must NOT double-count on re-reconciles")

		events := drainEvents(r)
		Expect(events).To(ContainElement(ContainSubstring(eventReasonReleaseTaintApplied)))
	})

	It("increments released{failure} + emits ReleaseTaintFailed on drift", func() {
		nodeName := "node-obs-drift-1"
		// Pre-existing taint with a DIFFERENT ExtRR's name.
		Expect(r.Client.Create(ctx, newTestNode(nodeName, nil,
			[]corev1.Taint{{Key: ReleaseTaintKey, Value: "foreign-owner", Effect: corev1.TaintEffectNoSchedule}}))).
			To(Succeed())
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		failureBefore := testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseReleased, janitormetrics.ExtRRResultFailure))

		extrrObj := newTestExtRR("obs-drift-1", nodeName)
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(forceFinalizerRemoval, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		reconcileToSteadyState(ctx, r, key, 3)

		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseReleased, janitormetrics.ExtRRResultFailure)) - failureBefore).
			To(BeNumerically("==", 1.0))

		events := drainEvents(r)
		Expect(events).To(ContainElement(ContainSubstring(eventReasonReleaseTaintFailed)))
	})

	It("increments closed{success} + external_response{success} + observes age on True cleanup", func() {
		nodeName := "node-obs-close-success-1"
		key := prepareReleased(ctx, r, "obs-close-success-1", nodeName)
		DeferCleanup(forceFinalizerRemovalByKey, ctx, r, key)
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		// Drain events fired during the apply phase; we only want to assert
		// the close-phase events here.
		drainEvents(r)

		closedBefore := testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseClosed, janitormetrics.ExtRRResultSuccess))
		extRespBefore := testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseExternalResponse, janitormetrics.ExtRRResultSuccess))

		setExternalRemediationComplete(ctx, r.Client,
			&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
			}}, "True", "ExternalRemediationSucceeded")

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseClosed, janitormetrics.ExtRRResultSuccess)) - closedBefore).
			To(BeNumerically("==", 1.0))
		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseExternalResponse, janitormetrics.ExtRRResultSuccess)) - extRespBefore).
			To(BeNumerically("==", 1.0))
		// err_age_seconds Observe is called unconditionally inside recordClose,
		// which only runs when the closed{success} counter above increments.
		// Asserting it here would require a per-label-tuple histogram-count
		// getter that doesn't exist in testutil; the counter check above is
		// sufficient proof that the histogram observation also fired.

		events := drainEvents(r)
		Expect(events).To(ContainElement(ContainSubstring(eventReasonReleaseTaintRemoved)))
		Expect(events).To(ContainElement(ContainSubstring(closeReasonExternalRemediationCompleteTrue)))

		// Subsequent reconciles should NOT double-count; reconcileCleanup is idempotent.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseClosed, janitormetrics.ExtRRResultSuccess)) - closedBefore).
			To(BeNumerically("==", 1.0), "closed{success} must NOT double-count after cleanup")
	})

	It("increments closed{operator_deleted} + emits OperatorDeleteRequested + ReleaseTaintRemoved on delete", func() {
		nodeName := "node-obs-close-deleted-1"
		key := prepareReleased(ctx, r, "obs-close-deleted-1", nodeName)
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		drainEvents(r) // discard apply-phase events

		closedBefore := testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseClosed, janitormetrics.ExtRRResultOperatorDeleted))

		Expect(r.Client.Delete(ctx, &nvsentinelv1.ExternalRemediationRequest{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		})).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseClosed, janitormetrics.ExtRRResultOperatorDeleted)) - closedBefore).
			To(BeNumerically("==", 1.0))

		events := drainEvents(r)
		Expect(events).To(ContainElement(ContainSubstring(eventReasonOperatorDeleteRequest)))
		Expect(events).To(ContainElement(ContainSubstring(eventReasonReleaseTaintRemoved)))
		Expect(events).To(ContainElement(ContainSubstring(closeReasonOperatorInitiated)))
	})
})
