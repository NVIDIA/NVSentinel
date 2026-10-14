// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

func releaseGroupForNode(vr v1alpha1.ValidationRequest, nodeName string) v1alpha1.TestGroupStatus {
	GinkgoHelper()

	for _, group := range vr.Status.TestGroups {
		if slices.Contains(group.Nodes, nodeName) {
			return group
		}
	}

	Fail("missing test group for node " + nodeName)

	return v1alpha1.TestGroupStatus{}
}

func injectQuarantinesAfterGroupCompletion(fixture *releaseRaceFixture, groupCount int, nodeNames ...string) *bool {
	deletedJobs := 0
	injected := false
	fixture.client.afterDelete = func(ctx context.Context, obj client.Object) error {
		if obj.GetObjectKind().GroupVersionKind() != testJobGVK {
			return nil
		}

		deletedJobs++
		if deletedJobs != groupCount {
			return nil
		}

		for _, nodeName := range nodeNames {
			if err := setReleaseRaceQuarantine(ctx, nodeName, releaseRaceEvent); err != nil {
				return err
			}
		}

		injected = true

		return nil
	}

	return &injected
}

var _ = Describe("Validation release group retries", func() {
	It("preserves the recorded test end time when a late quarantine invalidates its success", func() {
		nodeName := "release-preserve-end-time"
		fixture := newReleaseRaceFixture("release-preserve-end-time", 0, nodeName)
		vr := fixture.running()
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())

		_, _, _, err := fixture.reconciler.reconcileTestGroups(ctx, &vr)
		Expect(err).NotTo(HaveOccurred())
		Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseSucceeded))

		// Model a successful attempt persisted by an earlier reconcile, before
		// release was retried. Distinct historical timestamps avoid relying on
		// wall-clock sleeps or subsecond Kubernetes timestamp precision.
		completedAt := metav1.NewTime(time.Now().UTC().Add(-time.Minute).Truncate(time.Second))
		attemptStartedAt := metav1.NewTime(completedAt.Add(-time.Minute))
		requestStartedAt := metav1.NewTime(attemptStartedAt.Add(-time.Minute))
		vr.Status.StartTime = &requestStartedAt
		vr.Status.TestGroups[0].Attempts[0].StartTime = &attemptStartedAt
		vr.Status.TestGroups[0].Attempts[0].EndTime = &completedAt
		Expect(k8sClient.Status().Update(ctx, &vr)).To(Succeed())
		Expect(setReleaseRaceQuarantine(ctx, nodeName, releaseRaceEvent)).To(Succeed())

		vr = fixture.reconcile()
		expectReleaseRaceFailure(vr, nodeName)
		Expect(vr.Status.TestGroups[0].Attempts[0].EndTime).NotTo(BeNil())
		Expect(vr.Status.TestGroups[0].Attempts[0].EndTime.Equal(&completedAt)).To(BeTrue())
		expectReleaseRaceGate(nodeName)
	})

	It("waits without an error when all tests are skipped and a late quarantine blocks release", func() {
		nodeName := "release-skipped-tests"
		fixture := newReleaseRaceFixture("release-skipped-tests", 0, nodeName)
		testCfg := fixture.config.Validation.Spec.Tests["basic"]
		testCfg.MinimumNodesPerBatch = 2
		testCfg.BatchFailurePolicy = v1alpha1.BatchFailurePolicyIgnore
		fixture.config.Validation.Spec.Tests["basic"] = testCfg

		vr := reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhasePending, maxReconcileIterations)
		injected := false
		fixture.client.beforePatch = func(ctx context.Context, obj client.Object) error {
			node, ok := obj.(*corev1.Node)
			if !injected && ok && node.Name == nodeName && len(node.Annotations[annotationValidationSession]) > 0 {
				// Pending readiness uses the Node read before the session patch.
				// Inject quarantine here so the live release precheck discovers it.
				if err := setReleaseRaceQuarantine(ctx, nodeName, releaseRaceEvent); err != nil {
					return err
				}

				injected = true
			}

			return nil
		}

		result, err := fixture.reconciler.Reconcile(ctx, fixture.request)
		Expect(err).NotTo(HaveOccurred())
		Expect(injected).To(BeTrue())
		Expect(result.RequeueAfter).To(Equal(30 * time.Second))
		Expect(k8sClient.Get(ctx, fixture.request.NamespacedName, &vr)).To(Succeed())
		Expect(vr.Status.Phase).To(Equal(v1alpha1.PhasePending))
		Expect(vr.Status.TestGroups).To(BeEmpty())
		expectReleaseRaceGate(nodeName)

		clearReleaseRaceQuarantine(nodeName)
		vr = reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhaseSucceeded, maxReconcileIterations)
		Expect(vr.Status.TestGroups).To(BeEmpty())
		Expect(vr.Status.Skipped).NotTo(BeNil())
		Expect(vr.Status.Skipped.Tests).To(ConsistOf("basic"))
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeFalse())
	})

	It("retries a surviving group when a different request node was deleted before testing", func() {
		nodeA, nodeB := "release-skipped-a", "release-skipped-b"
		fixture := newReleaseRaceFixture("release-skipped", 1, nodeA, nodeB)
		deletedNode := getNode(ctx, nodeA)
		Expect(k8sClient.Delete(ctx, &deletedNode)).To(Succeed())

		vr := fixture.running()
		Expect(vr.Status.Skipped).NotTo(BeNil())
		Expect(vr.Status.Skipped.Nodes).To(ContainElement(nodeA))
		Expect(vr.Status.TestGroups).To(HaveLen(1))
		Expect(vr.Status.TestGroups[0].Nodes).To(ConsistOf(nodeB))
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())

		injected := fixture.injectAfterReadiness(nodeB, releaseRaceEvent)
		vr = fixture.reconcile()
		Expect(*injected).To(BeTrue())
		Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhasePending))
		Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(1))
		expectReleaseRaceGate(nodeB)

		clearReleaseRaceQuarantine(nodeB)
		vr = fixture.reconcile()
		Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(2))
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())

		vr = reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhaseSucceeded, maxReconcileIterations)
		Expect(vr.Status.Skipped.Nodes).To(ContainElement(nodeA))
		Expect(getNode(ctx, nodeB).Spec.Unschedulable).To(BeFalse())
	})

	It("retries a disjoint group without running another test on the released node", func() {
		nodeA, nodeB := "release-disjoint-a", "release-disjoint-b"
		fixture := newReleaseRaceFixture("release-disjoint", 1, nodeA, nodeB)
		testCfg := fixture.config.Validation.Spec.Tests["basic"]
		testCfg.SupportsBatchingNodes = false
		fixture.config.Validation.Spec.Tests["basic"] = testCfg

		vr := fixture.running()
		Expect(vr.Status.TestGroups).To(HaveLen(2))
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())

		injected := false
		fixture.client.beforePatch = func(ctx context.Context, obj client.Object) error {
			node, ok := obj.(*corev1.Node)
			if !injected && ok && node.Name == nodeB && !node.Spec.Unschedulable {
				Expect(getNode(ctx, nodeA).Spec.Unschedulable).To(BeFalse())

				if err := setReleaseRaceQuarantine(ctx, nodeB, releaseRaceEvent); err != nil {
					return err
				}

				injected = true
			}

			return nil
		}

		vr = fixture.reconcile()
		Expect(injected).To(BeTrue())
		Expect(fixture.client.conflicts).To(BeNumerically(">", 0))
		Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(releaseGroupForNode(vr, nodeA).Phase).To(Equal(v1alpha1.PhaseSucceeded))
		Expect(releaseGroupForNode(vr, nodeB).Phase).To(Equal(v1alpha1.PhasePending))
		Expect(releaseGroupForNode(vr, nodeB).Nodes).To(ConsistOf(nodeB))
		expectReleaseRaceGate(nodeB)

		clearReleaseRaceQuarantine(nodeB)
		vr = fixture.reconcile()
		Expect(releaseGroupForNode(vr, nodeB).Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(releaseGroupForNode(vr, nodeB).Attempts).To(HaveLen(2))
		Expect(releaseGroupForNode(vr, nodeA).Attempts).To(HaveLen(1))
		Expect(getNode(ctx, nodeA).Spec.Unschedulable).To(BeFalse())
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())

		vr = reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhaseSucceeded, maxReconcileIterations)
		Expect(releaseGroupForNode(vr, nodeA).Attempts).To(HaveLen(1))
		Expect(getNode(ctx, nodeB).Spec.Unschedulable).To(BeFalse())
	})

	It("does not treat a deleted batch member as released when quarantine starts during another member's patch", func() {
		nodeA, nodeB := "release-late-delete-a", "release-late-delete-b"
		fixture := newReleaseRaceFixture("release-late-delete", 1, nodeA, nodeB)
		vr := fixture.running()
		Expect(vr.Status.TestGroups[0].Nodes).To(ConsistOf(nodeA, nodeB))
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())

		// Persist the completed batch before its nodes are released. Its node
		// list still includes A, which is deleted before the release retry.
		_, _, _, err := fixture.reconciler.reconcileTestGroups(ctx, &vr)
		Expect(err).NotTo(HaveOccurred())
		Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseSucceeded))
		Expect(k8sClient.Status().Update(ctx, &vr)).To(Succeed())
		deletedNode := getNode(ctx, nodeA)
		Expect(k8sClient.Delete(ctx, &deletedNode)).To(Succeed())

		injected := false
		fixture.client.beforePatch = func(ctx context.Context, obj client.Object) error {
			node, ok := obj.(*corev1.Node)
			if !injected && ok && node.Name == nodeB && !node.Spec.Unschedulable {
				if err := setReleaseRaceQuarantine(ctx, nodeB, releaseRaceEvent); err != nil {
					return err
				}

				injected = true
			}

			return nil
		}

		vr = fixture.reconcile()
		Expect(injected).To(BeTrue())
		Expect(fixture.client.conflicts).To(BeNumerically(">", 0))
		Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhasePending))
		Expect(vr.Status.TestGroups[0].Nodes).To(ConsistOf(nodeB))
		Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(1))
		Expect(vr.Status.Skipped).NotTo(BeNil())
		Expect(vr.Status.Skipped.Nodes).To(ContainElement(nodeA))
		expectReleaseRaceGate(nodeB)

		clearReleaseRaceQuarantine(nodeB)
		vr = fixture.reconcile()
		Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(vr.Status.TestGroups[0].Nodes).To(ConsistOf(nodeB))
		Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(2))
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())

		vr = reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhaseSucceeded, maxReconcileIterations)
		Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(2))
		Expect(getNode(ctx, nodeB).Spec.Unschedulable).To(BeFalse())
	})

	DescribeTable("invalidates every group affected by the quarantines observed before release",
		func(name string, batchNodes bool, expectedGroups int) {
			nodeA, nodeB := name+"-a", name+"-b"
			fixture := newReleaseRaceFixture(name, 1, nodeA, nodeB)
			testCfg := fixture.config.Validation.Spec.Tests["basic"]
			testCfg.SupportsBatchingNodes = batchNodes
			fixture.config.Validation.Spec.Tests["basic"] = testCfg

			vr := fixture.running()
			Expect(vr.Status.TestGroups).To(HaveLen(expectedGroups))
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())

			injected := injectQuarantinesAfterGroupCompletion(fixture, expectedGroups, nodeA, nodeB)
			vr = fixture.reconcile()
			Expect(*injected).To(BeTrue())
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseRunning))

			for _, group := range vr.Status.TestGroups {
				Expect(group.Phase).To(Equal(v1alpha1.PhasePending))
				Expect(group.Attempts).To(HaveLen(1))
				Expect(group.Attempts[0].Phase).To(Equal(v1alpha1.PhaseFailed))
				Expect(group.Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonNodeReadinessViolation))
				Expect(group.Attempts[0].FailedNodes).To(ConsistOf(group.Nodes))
			}

			vr = fixture.reconcile()
			for _, group := range vr.Status.TestGroups {
				Expect(group.Phase).To(Equal(v1alpha1.PhasePending))
				Expect(group.Attempts).To(HaveLen(1), "an active quarantine must not consume another attempt")
			}

			expectReleaseRaceGate(nodeA)
			expectReleaseRaceGate(nodeB)
			clearReleaseRaceQuarantine(nodeA)
			clearReleaseRaceQuarantine(nodeB)
			vr = fixture.reconcile()

			for _, group := range vr.Status.TestGroups {
				Expect(group.Phase).To(Equal(v1alpha1.PhaseRunning))
				Expect(group.Attempts).To(HaveLen(2), "each affected group must execute a fresh attempt")
			}

			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			vr = reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhaseSucceeded, maxReconcileIterations)

			for _, group := range vr.Status.TestGroups {
				Expect(group.Attempts).To(HaveLen(2))
				Expect(group.Attempts[1].Phase).To(Equal(v1alpha1.PhaseSucceeded))
			}

			Expect(getNode(ctx, nodeA).Spec.Unschedulable).To(BeFalse())
			Expect(getNode(ctx, nodeB).Spec.Unschedulable).To(BeFalse())
		},
		Entry("independent groups each require a fresh attempt", "release-all-groups", false, 2),
		Entry("a shared group records every affected node once", "release-shared-group", true, 1),
	)
})
