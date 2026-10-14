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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

// Hold a snapshot read from envtest to model a delayed Node informer update.
// All writes and other reads still use the real API server, including APIReader.
type releaseLaggingNodeClient struct {
	client.Client
	snapshot   *corev1.Node
	staleReads int
}

func (c *releaseLaggingNodeClient) Get(ctx context.Context, key client.ObjectKey,
	obj client.Object, opts ...client.GetOption) error {
	if node, ok := obj.(*corev1.Node); ok && c.snapshot != nil && key.Name == c.snapshot.Name {
		c.snapshot.DeepCopyInto(node)
		c.staleReads++
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

type releaseNodeErrorReader struct {
	client.Reader
	nodeName string
	err      error
}

func (r *releaseNodeErrorReader) Get(ctx context.Context, key client.ObjectKey,
	obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Node); ok && key.Name == r.nodeName {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func expectReleaseRetryPending(vr v1alpha1.ValidationRequest) {
	Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseRunning))
	Expect(vr.Status.TestGroups).To(HaveLen(1))
	group := vr.Status.TestGroups[0]
	Expect(group.Phase).To(Equal(v1alpha1.PhasePending))
	Expect(group.Attempts).To(HaveLen(1), "waiting for quarantine must not consume a retry")
	provider := &unstructured.Unstructured{}
	provider.SetGroupVersionKind(testJobGVK)
	err := k8sClient.Get(ctx, client.ObjectKey{
		Namespace: testNamespace,
		Name:      attemptObjectName(vr.Name, group.Name, 2),
	}, provider)
	Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no retry provider may exist while the group is blocked")
}

var _ = Describe("Validation release retry with stale Node cache", func() {
	DescribeTable("waits for the live quarantine before starting a fresh attempt",
		func(name string, withCriterion bool) {
			nodeName := "release-cache-" + name
			fixture := newReleaseRaceFixture("release-cache-"+name, 1, nodeName)
			if !withCriterion {
				fixture.config.Validation.Spec.ReadinessCriteria = defaultTestConfig().Validation.Spec.ReadinessCriteria
			}
			vr := fixture.running()
			snapshot := getNode(ctx, nodeName)
			Expect(snapshot.Annotations).NotTo(HaveKey(annotationQuarantineHealthEvent))
			lagging := &releaseLaggingNodeClient{Client: fixture.client, snapshot: snapshot.DeepCopy()}
			fixture.reconciler.Client = lagging
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			injected := fixture.injectAfterReadiness(nodeName, releaseRaceEvent)

			vr = fixture.reconcile()
			Expect(*injected).To(BeTrue())
			Expect(lagging.staleReads).To(BeNumerically(">", 0))
			Expect(fixture.client.quarantinedReads).To(BeNumerically(">", 0))
			expectReleaseRetryPending(vr)
			expectReleaseRaceGate(nodeName)
			for range 3 {
				vr = fixture.reconcile()
				expectReleaseRetryPending(vr)
			}

			clearReleaseRaceQuarantine(nodeName)
			vr = fixture.reconcile()
			Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseRunning))
			Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(2))
			expectReleaseRaceGate(nodeName)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			vr = reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhaseSucceeded, maxReconcileIterations)
			Expect(vr.Status.TestGroups[0].Attempts[1].Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeFalse())
		},
		Entry("with the quarantine readiness criterion", "with-criterion", true),
		Entry("without the quarantine readiness criterion", "no-criterion", false),
	)

	It("does not start a pending retry when the live Node read fails", func() {
		nodeName := "release-read-error-node"
		fixture := newReleaseRaceFixture("release-read-error", 1, nodeName)
		vr := fixture.running()
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		injected := fixture.injectAfterReadiness(nodeName, releaseRaceEvent)
		vr = fixture.reconcile()
		Expect(*injected).To(BeTrue())
		expectReleaseRetryPending(vr)
		clearReleaseRaceQuarantine(nodeName)

		liveReader := fixture.reconciler.APIReader
		readFailure := errors.New("injected live Node read failure")
		fixture.reconciler.APIReader = &releaseNodeErrorReader{
			Reader: liveReader, nodeName: nodeName, err: readFailure,
		}
		DeferCleanup(func() { fixture.reconciler.APIReader = liveReader })
		for range 2 {
			_, err := fixture.reconciler.Reconcile(ctx, fixture.request)
			Expect(errors.Is(err, readFailure)).To(BeTrue(), "a failed live read must block the provider create")
			Expect(k8sClient.Get(ctx, fixture.request.NamespacedName, &vr)).To(Succeed())
			expectReleaseRetryPending(vr)
			expectReleaseRaceGate(nodeName)
		}

		fixture.reconciler.APIReader = liveReader
		vr = fixture.reconcile()
		Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(2))
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		vr = reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhaseSucceeded, maxReconcileIterations)
		Expect(vr.Status.TestGroups[0].Attempts[1].Phase).To(Equal(v1alpha1.PhaseSucceeded))
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeFalse())
	})
})
