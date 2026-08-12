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
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	nvsentinelv1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/distributedlock"
	janitormetrics "github.com/nvidia/nvsentinel/janitor/pkg/metrics"
)

const testExtRRNamespace = "default"

// newExtRRReconciler is Ginkgo-only — BeforeSuite populates cfg.
func newExtRRReconciler() *ExternalRemediationRequestReconciler {
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	return &ExternalRemediationRequestReconciler{
		Client:   c,
		Scheme:   scheme.Scheme,
		Recorder: record.NewFakeRecorder(64),
		NodeLock: distributedlock.NewNodeLock(c, testExtRRNamespace),
	}
}

// drainEvents non-blockingly drains the FakeRecorder channel.
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

// testRecommendedActionLabel matches what fault-remediation stamps on
// CUSTOM:external-remediation events, so observability assertions match
// production label values.
const testRecommendedActionLabel = "external-remediation"

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

// reconcileToSteadyState drives the multi-pass init without specs caring
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
		nodeName := "node-fresh-1"
		Expect(r.Client.Create(ctx, newTestNode(nodeName, nil, nil))).To(Succeed())
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		extrrObj := newTestExtRR("fresh-extrr-1", nodeName)
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(deleteExtRRForCleanup, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		// One pass: seeds the finalizer and Unknown conditions. Subsequent passes
		// would run apply and transition Released=True; this test is about init only.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var got nvsentinelv1.ExternalRemediationRequest
		Expect(r.Client.Get(ctx, key, &got)).To(Succeed())
		gotPtr := &got

		Expect(controllerutil.ContainsFinalizer(gotPtr, ExternalRemediationFinalizer)).
			To(BeTrue(), "cleanup finalizer must be added")

		Expect(gotPtr.Status).NotTo(BeNil(), "Status must be populated")
		Expect(gotPtr.Status.Conditions).To(HaveLen(2), "two initial conditions expected")

		released := findExtRRCondition(gotPtr, ConditionNVSentinelOwnershipReleased)
		Expect(released).NotTo(BeNil())
		Expect(released.Status).To(Equal("Unknown"))
		Expect(released.Reason).To(Equal(reasonInitializing))
		Expect(released.Message).NotTo(BeEmpty())
		Expect(released.LastTransitionTime).NotTo(BeNil())

		complete := findExtRRCondition(gotPtr, ConditionExternalRemediationComplete)
		Expect(complete).NotTo(BeNil())
		Expect(complete.Status).To(Equal("Unknown"))
		Expect(complete.Reason).To(Equal(reasonAwaitingExternalSystem))
		Expect(complete.Message).NotTo(BeEmpty())
		Expect(complete.LastTransitionTime).NotTo(BeNil())
	})

	It("is idempotent on re-reconcile (no LastTransitionTime flap)", func() {
		nodeName := "node-idem-1"
		Expect(r.Client.Create(ctx, newTestNode(nodeName, nil, nil))).To(Succeed())
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		extrrObj := newTestExtRR("idempotent-extrr-1", nodeName)
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
		extrrObj := newTestExtRR("delete-no-apply-extrr-1", "node-never-existed")
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		// One pass to init; the next reconcile would terminate Released=False
		// (NodeNotFound) but we delete first to exercise the cleanup-before-apply path.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(r.Client.Delete(ctx, extrrObj)).To(Succeed())

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

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

// prepareReleased drives an ExtRR through init + apply, leaving it at
// NVSentinelOwnershipReleased=True for post-applied tests to extend.
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

// setExternalRemediationComplete simulates the external system reporting
// completion via a status subresource patch.
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

			setExternalRemediationComplete(ctx, r.Client,
				&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
				}}, "True", "ExternalRemediationSucceeded")

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			var node corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node)).To(Succeed())
			Expect(findTaintByKey(node.Spec.Taints, ReleaseTaintKey)).To(BeNil(),
				"release taint must be removed after Complete=True")
			Expect(node.Labels).NotTo(HaveKey(managed.ManagedLabelKey),
				"managed label must be removed entirely (absence is the default-managed state)")

			var got nvsentinelv1.ExternalRemediationRequest
			Expect(r.Client.Get(ctx, key, &got)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(&got, ExternalRemediationFinalizer)).To(BeTrue(),
				"finalizer stays attached on True-driven cleanup (ExtRR is the historical record)")
			Expect(got.Status).NotTo(BeNil())
			Expect(got.Status.CompletionTime).NotTo(BeNil(),
				"Complete=True scrub must stamp Status.CompletionTime")
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

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			var nodeAfterCleanup corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfterCleanup)).To(Succeed())
			rvAfterCleanup := nodeAfterCleanup.ResourceVersion

			for i := 0; i < 3; i++ {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}

			var nodeAfterRereconcile corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfterRereconcile)).To(Succeed())
			Expect(nodeAfterRereconcile.ResourceVersion).To(Equal(rvAfterCleanup),
				"Node ResourceVersion must not advance after the cleanup PATCH settles")
		})

		It("releases the node lock and stops emitting metrics/events after CompletionTime is set", func() {
			nodeName := "node-true-shortcircuit-1"
			key := prepareReleased(ctx, r, "true-shortcircuit-extrr-1", nodeName)
			DeferCleanup(forceFinalizerRemovalByKey, ctx, r, key)
			DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

			// Apply has run — the lock lease should exist under this node's name.
			leaseKey := ctrlclient.ObjectKey{Name: nodeName, Namespace: testExtRRNamespace}
			var lease coordinationv1.Lease
			Expect(r.Client.Get(ctx, leaseKey, &lease)).To(Succeed(),
				"reconcileApply must acquire the node-lock lease")

			setExternalRemediationComplete(ctx, r.Client,
				&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
				}}, "True", "ExternalRemediationSucceeded")

			// First reconcile after Complete=True runs the close path
			// (scrub + close metrics + markCompletionTime). Drain events here.
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			drainEvents(r)

			closedBefore := testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
				janitormetrics.ExtRRPhaseClosed, janitormetrics.ExtRRResultSuccess))

			// Subsequent reconciles must short-circuit: CheckUnlock-only.
			// Drives several to make sure the lease is gone AND no further
			// metric/event firing occurs.
			Eventually(func() error {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				return err
			}, "2s", "20ms").Should(Succeed())

			err = r.Client.Get(ctx, leaseKey, &lease)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"lease must be released after CompletionTime + dispatch short-circuit fires CheckUnlock")

			// closed{success} stays at the snapshot — no double-count.
			Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
				janitormetrics.ExtRRPhaseClosed, janitormetrics.ExtRRResultSuccess))).
				To(Equal(closedBefore), "closed counter must not refire after CompletionTime is set")

			// And no further events.
			Expect(drainEvents(r)).To(BeEmpty(),
				"post-CompletionTime reconciles must be event-free")
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

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			var node corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node)).To(Succeed())
			Expect(findTaintByKey(node.Spec.Taints, ReleaseTaintKey)).To(BeNil())
			Expect(node.Labels).NotTo(HaveKey(managed.ManagedLabelKey))

			var got nvsentinelv1.ExternalRemediationRequest
			err = r.Client.Get(ctx, key, &got)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"ExtRR must be garbage-collected after operator-driven cleanup")
		})

		It("removes the finalizer cleanly when cleanup already ran via Complete=True", func() {
			nodeName := "node-stack-1"
			key := prepareReleased(ctx, r, "stack-extrr-1", nodeName)
			DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

			setExternalRemediationComplete(ctx, r.Client,
				&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace,
				}}, "True", "ExternalRemediationSucceeded")
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(r.Client.Delete(ctx, &nvsentinelv1.ExternalRemediationRequest{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			})).To(Succeed())

			var nodeBeforeDel corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeBeforeDel)).To(Succeed())
			rvBeforeDel := nodeBeforeDel.ResourceVersion

			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			var nodeAfter corev1.Node
			Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfter)).To(Succeed())
			Expect(nodeAfter.ResourceVersion).To(Equal(rvBeforeDel),
				"already-clean cleanup must NOT re-PATCH the Node")

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

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var nodeAfter corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAfter)).To(Succeed())

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

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var nodeBaseline corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeBaseline)).To(Succeed())
		rvBaseline := nodeBaseline.ResourceVersion

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

		setExternalRemediationComplete(ctx, r.Client,
			&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
			}}, "False", "ExternalRemediationFailed")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		var nodeAtFalse corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &nodeAtFalse)).To(Succeed())
		Expect(findTaintByKey(nodeAtFalse.Spec.Taints, ReleaseTaintKey)).NotTo(BeNil())
		Expect(nodeAtFalse.Labels).To(HaveKeyWithValue(managed.ManagedLabelKey, managed.ManagedLabelValueFalse))

		// True retry → branch 4 cleanup.
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

		// With Released=True the dispatcher falls through to the no-op branch.
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
		// Pre-apply: simulate a reconcile that PATCHed the Node then crashed.
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

	It("fails Released=False immediately when the target Node does not exist", func() {
		extrrObj := newTestExtRR("missing-node-extrr-1", "node-does-not-exist")
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(deleteExtRRForCleanup, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		before := testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseReleased, janitormetrics.ExtRRResultNodeNotFound))

		// First pass inits; second hits the apply branch and terminates immediately.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred(), "missing Node must not propagate as a reconcile error")
		Expect(result.RequeueAfter).To(BeZero(), "missing Node must terminate, not requeue")

		var got nvsentinelv1.ExternalRemediationRequest
		Expect(r.Client.Get(ctx, key, &got)).To(Succeed())
		released := findExtRRCondition(&got, ConditionNVSentinelOwnershipReleased)
		Expect(released.Status).To(Equal("False"))
		Expect(released.Reason).To(Equal(ReasonNodeNotFound))
		Expect(released.Message).To(ContainSubstring("does not exist"))
		Expect(got.Status.CompletionTime).NotTo(BeNil(),
			"terminal NodeNotFound must stamp CompletionTime so dispatch short-circuits")

		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseReleased, janitormetrics.ExtRRResultNodeNotFound)) - before).
			To(BeNumerically("==", 1.0))

		events := drainEvents(r)
		Expect(events).To(ContainElement(ContainSubstring(eventReasonNodeNotFound)))

		// Subsequent reconciles short-circuit via the CompletionTime check in
		// dispatch — no further metric, no event.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseReleased, janitormetrics.ExtRRResultNodeNotFound)) - before).
			To(BeNumerically("==", 1.0), "subsequent reconciles must not re-fire the metric")
		Expect(drainEvents(r)).To(BeEmpty())
	})

	// Empty-nodeName rejection is covered by the webhook test suite.

	It("requeues without acting when another maintenance resource holds the node lock", func() {
		nodeName := "node-locked-1"
		Expect(r.Client.Create(ctx, newTestNode(nodeName, nil, nil))).To(Succeed())
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		// Pre-create a lease as if a sibling maintenance CR (e.g. RebootNode)
		// already holds the lock for this node.
		foreignLease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      nodeName,
				Namespace: testExtRRNamespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "janitor.dgxc.nvidia.com/v1alpha1",
					Kind:       "RebootNode",
					Name:       "foreign-reboot",
					UID:        "foreign-uid",
				}},
			},
		}
		Expect(r.Client.Create(ctx, foreignLease)).To(Succeed())
		DeferCleanup(func() { _ = r.Client.Delete(ctx, foreignLease) })

		extrrObj := newTestExtRR("locked-extrr-1", nodeName)
		Expect(r.Client.Create(ctx, extrrObj)).To(Succeed())
		DeferCleanup(deleteExtRRForCleanup, ctx, r, extrrObj)

		key := ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace}
		// Init completes (no Node interaction); second reconcile reaches the
		// dispatch lock gate, finds the foreign lock, requeues without acting.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred(), "lock contention must not propagate as a reconcile error")
		Expect(result.RequeueAfter).To(Equal(nodeLockRequeue), "lock contention must requeue with the lock-retry cadence")

		// Released stays Unknown — apply never ran.
		var got nvsentinelv1.ExternalRemediationRequest
		Expect(r.Client.Get(ctx, key, &got)).To(Succeed())
		released := findExtRRCondition(&got, ConditionNVSentinelOwnershipReleased)
		Expect(released.Status).To(Equal("Unknown"))

		// And the Node is untouched.
		var node corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node)).To(Succeed())
		Expect(findTaintByKey(node.Spec.Taints, ReleaseTaintKey)).To(BeNil())
		Expect(node.Labels).NotTo(HaveKey(managed.ManagedLabelKey))
	})

})

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

func deleteExtRRForCleanup(ctx context.Context, r *ExternalRemediationRequestReconciler, extrrObj *nvsentinelv1.ExternalRemediationRequest) {
	forceFinalizerRemoval(ctx, r, extrrObj)
}

func forceFinalizerRemoval(ctx context.Context, r *ExternalRemediationRequestReconciler, extrrObj *nvsentinelv1.ExternalRemediationRequest) {
	forceFinalizerRemovalByKey(ctx, r, ctrlclient.ObjectKey{Name: extrrObj.Name, Namespace: extrrObj.Namespace})
}

// forceFinalizerRemovalByKey strips the finalizer + deletes; errors go to
// GinkgoWriter so a flaky shared-state failure leaves a breadcrumb.
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

// snapshotConditions sidesteps proto-message equality quirks (sync.Mutex).
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
		// setInitialConditions only fires the counter on the pass that writes.
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

		// Re-reconciles must not double-count once Released=True.
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

	It("increments closed{success} + external_response{success} + observes age on True cleanup", func() {
		nodeName := "node-obs-close-success-1"
		key := prepareReleased(ctx, r, "obs-close-success-1", nodeName)
		DeferCleanup(forceFinalizerRemovalByKey, ctx, r, key)
		DeferCleanup(deleteNodeForCleanup, ctx, r, nodeName)

		drainEvents(r) // discard apply-phase events

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
		// ExtRRAgeSeconds is co-emitted inside recordClose, gated on the same
		// counter; testutil has no per-label-tuple histogram getter to assert.

		events := drainEvents(r)
		Expect(events).To(ContainElement(ContainSubstring(eventReasonReleaseTaintRemoved)))
		Expect(events).To(ContainElement(ContainSubstring(closeReasonExternalRemediationCompleteTrue)))

		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseClosed, janitormetrics.ExtRRResultSuccess)) - closedBefore).
			To(BeNumerically("==", 1.0), "closed{success} must NOT double-count after cleanup")
	})

	It("fires closed{success} + external_response{success} on Complete=True even when the Node has been deleted", func() {
		nodeName := "node-obs-terminate-1"
		key := prepareReleased(ctx, r, "obs-terminate-1", nodeName)
		DeferCleanup(forceFinalizerRemovalByKey, ctx, r, key)

		// Simulate Maestro terminating the Node as part of the repair workflow.
		var node corev1.Node
		Expect(r.Client.Get(ctx, ctrlclient.ObjectKey{Name: nodeName}, &node)).To(Succeed())
		Expect(r.Client.Delete(ctx, &node)).To(Succeed())

		drainEvents(r)
		closedBefore := testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseClosed, janitormetrics.ExtRRResultSuccess))
		extRespBefore := testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseExternalResponse, janitormetrics.ExtRRResultSuccess))

		// Maestro signals success after the Node is gone.
		setExternalRemediationComplete(ctx, r.Client,
			&nvsentinelv1.ExternalRemediationRequest{ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
			}}, "True", "ExternalRemediationSucceeded")

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseClosed, janitormetrics.ExtRRResultSuccess)) - closedBefore).
			To(BeNumerically("==", 1.0),
				"closed{success} must fire even when the Node has been deleted")
		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseExternalResponse, janitormetrics.ExtRRResultSuccess)) - extRespBefore).
			To(BeNumerically("==", 1.0),
				"external_response{success} must fire even when the Node has been deleted")

		var got nvsentinelv1.ExternalRemediationRequest
		Expect(r.Client.Get(ctx, key, &got)).To(Succeed())
		Expect(got.Status).NotTo(BeNil())
		Expect(got.Status.CompletionTime).NotTo(BeNil(),
			"close path must stamp Status.CompletionTime so dispatch short-circuits subsequent reconciles")

		// Subsequent reconciles must NOT re-fire the metrics.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(testutil.ToFloat64(janitormetrics.ExtRRTotal.WithLabelValues(
			janitormetrics.ExtRRPhaseClosed, janitormetrics.ExtRRResultSuccess)) - closedBefore).
			To(BeNumerically("==", 1.0),
				"closed{success} must NOT double-count across re-reconciles")
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
