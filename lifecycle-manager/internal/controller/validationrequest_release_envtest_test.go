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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
)

// Every operation delegates to envtest's real API server. Hooks deterministically
// schedule another controller's Node update without changing the reconciler.
type releaseRaceClient struct {
	client.Client
	afterDelete      func(context.Context, client.Object) error
	beforePatch      func(context.Context, client.Object) error
	quarantinedReads int
	conflicts        int
}

func (c *releaseRaceClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if node, ok := obj.(*corev1.Node); err == nil && ok {
		if _, exists := node.Annotations[annotationQuarantineHealthEvent]; exists {
			c.quarantinedReads++
		}
	}
	return err
}

func (c *releaseRaceClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if err := c.Client.Delete(ctx, obj, opts...); err != nil {
		return err
	}
	if c.afterDelete != nil {
		return c.afterDelete(ctx, obj)
	}
	return nil
}

func (c *releaseRaceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.beforePatch != nil {
		if err := c.beforePatch(ctx, obj); err != nil {
			return err
		}
	}
	err := c.Client.Patch(ctx, obj, patch, opts...)
	if apierrors.IsConflict(err) {
		c.conflicts++
	}
	return err
}

var releaseRaceGateTaint = corev1.Taint{Key: "test.example/quarantine", Effect: corev1.TaintEffectNoSchedule}

const releaseRaceEvent = `{"new-fatal-event":{}}`

type releaseRaceFixture struct {
	reconciler *ValidationRequestReconciler
	request    reconcile.Request
	client     *releaseRaceClient
	config     *config.Config
	nodeNames  []string
}

func newReleaseRaceFixture(name string, retries int, nodeNames ...string) *releaseRaceFixture {
	testConfig := defaultTestConfig()
	testConfig.Validation.Spec.ReadinessCriteria = append(
		testConfig.Validation.Spec.ReadinessCriteria, notUnderQuarantineCriteria...)
	testConfig.Validation.Spec.SchedulingGate = &v1alpha1.SchedulingGateConfig{
		Cordon: v1alpha1.CordonConfig{Remove: true},
		Taints: []v1alpha1.TaintConfig{{Key: releaseRaceGateTaint.Key, Effect: releaseRaceGateTaint.Effect, Remove: true}},
	}
	provider := testConfig.Validation.Spec.Providers["test-provider"]
	provider.Retries = retries
	testConfig.Validation.Spec.Providers["test-provider"] = provider
	var nodeSpecs []v1alpha1.NodeSpec
	for _, name := range nodeNames {
		nodeSpecs = append(nodeSpecs, v1alpha1.NodeSpec{Name: name})
	}
	raceClient := &releaseRaceClient{Client: k8sClient}
	r, req := newValidationRequestTestSetup(ctx, name, validationRequestTestCase{
		config:    testConfig,
		client:    raceClient,
		nodeNames: nodeNames,
		spec:      v1alpha1.ValidationRequestSpec{Nodes: nodeSpecs},
		beforeInit: func(ctx context.Context, _ *v1alpha1.ValidationRequest) error {
			for _, nodeName := range nodeNames {
				if err := cordonNode(ctx, nodeName); err != nil {
					return err
				}
				if err := addNodeTaint(ctx, nodeName, releaseRaceGateTaint); err != nil {
					return err
				}
			}
			return nil
		},
	})
	DeferCleanup(func() { raceClient.afterDelete = nil; raceClient.beforePatch = nil })
	return &releaseRaceFixture{reconciler: r, request: req, client: raceClient, config: testConfig, nodeNames: nodeNames}
}

func (f *releaseRaceFixture) running() v1alpha1.ValidationRequest {
	return reconcileUntilPhase(ctx, f.reconciler, f.request, v1alpha1.PhaseRunning, maxReconcileIterations)
}

func (f *releaseRaceFixture) reconcile() v1alpha1.ValidationRequest {
	return reconcileForIterations(ctx, f.reconciler, f.request, 1)
}

func (f *releaseRaceFixture) injectAfterReadiness(nodeName, value string) *bool {
	injected := false
	f.client.afterDelete = func(ctx context.Context, obj client.Object) error {
		if !injected && obj.GetObjectKind().GroupVersionKind() == testJobGVK {
			if err := setReleaseRaceQuarantine(ctx, nodeName, value); err != nil {
				return err
			}
			injected = true
		}
		return nil
	}
	return &injected
}

func setReleaseRaceQuarantine(ctx context.Context, nodeName, value string) error {
	var node corev1.Node
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return err
	}
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[annotationQuarantineHealthEvent] = value
	node.Spec.Unschedulable = true
	return k8sClient.Update(ctx, &node)
}

func clearReleaseRaceQuarantine(nodeName string) {
	node := getNode(ctx, nodeName)
	delete(node.Annotations, annotationQuarantineHealthEvent)
	Expect(k8sClient.Update(ctx, &node)).To(Succeed())
}

func expectReleaseRaceFailure(vr v1alpha1.ValidationRequest, nodeNames ...string) {
	Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
	Expect(vr.Status.TestGroups).To(HaveLen(1))
	group := vr.Status.TestGroups[0]
	Expect(group.Phase).To(Equal(v1alpha1.PhaseFailed))
	Expect(group.Attempts).To(HaveLen(1))
	Expect(group.Attempts[0].Phase).To(Equal(v1alpha1.PhaseFailed))
	Expect(group.Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonNodeReadinessViolation))
	Expect(group.Attempts[0].FailedNodes).To(ConsistOf(nodeNames))
	Expect(vr.Status.CompletionTime).NotTo(BeNil())
}

func expectReleaseRaceGate(nodeName string) {
	node := getNode(ctx, nodeName)
	Expect(node.Spec.Unschedulable).To(BeTrue(), "an older validation must not release a new quarantine")
	Expect(node.Spec.Taints).To(ContainElement(releaseRaceGateTaint))
}

var _ = Describe("Validation release quarantine regression", func() {
	DescribeTable("retains the scheduling gate whenever a new quarantine is present",
		func(timing, annotation string) {
			nodeName := "release-race-" + timing
			fixture := newReleaseRaceFixture("validation-"+timing, 0, nodeName)
			vr := fixture.running()
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			injected := false
			switch timing {
			case "early":
				Expect(setReleaseRaceQuarantine(ctx, nodeName, annotation)).To(Succeed())
				injected = true
			case "after-readiness", "empty-annotation":
				injectedPtr := fixture.injectAfterReadiness(nodeName, annotation)
				DeferCleanup(func() { Expect(*injectedPtr).To(BeTrue()) })
			case "patch-conflict":
				fixture.client.beforePatch = func(ctx context.Context, obj client.Object) error {
					if node, ok := obj.(*corev1.Node); !injected && ok && !node.Spec.Unschedulable {
						if err := setReleaseRaceQuarantine(ctx, nodeName, annotation); err != nil {
							return err
						}
						injected = true
					}
					return nil
				}
			}
			vr = fixture.reconcile()
			node := getNode(ctx, nodeName)
			fmt.Fprintf(GinkgoWriter, "timing=%s phase=%s cordoned=%t freshQuarantinedReads=%d conflicts=%d\n",
				timing, vr.Status.Phase, node.Spec.Unschedulable, fixture.client.quarantinedReads, fixture.client.conflicts)
			if timing == "healthy" {
				Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
				Expect(node.Spec.Unschedulable).To(BeFalse())
				Expect(node.Spec.Taints).NotTo(ContainElement(releaseRaceGateTaint))
				checkNodeValidationState(ctx, vr.Name, []string{nodeName}, v1alpha1.PhaseSucceeded)
				return
			}
			Expect(node.Annotations).To(HaveKeyWithValue(annotationQuarantineHealthEvent, annotation))
			Expect(fixture.client.quarantinedReads).To(BeNumerically(">", 0))
			if timing == "patch-conflict" {
				Expect(injected).To(BeTrue())
				Expect(fixture.client.conflicts).To(BeNumerically(">", 0))
			}
			expectReleaseRaceFailure(vr, nodeName)
			expectReleaseRaceGate(nodeName)
			checkNodeValidationState(ctx, vr.Name, []string{nodeName}, v1alpha1.PhaseFailed)
		},
		Entry("healthy control releases normally", "healthy", releaseRaceEvent),
		Entry("quarantine before readiness is caught", "early", releaseRaceEvent),
		Entry("quarantine after readiness remains isolated", "after-readiness", releaseRaceEvent),
		Entry("quarantine causing patch conflict remains isolated", "patch-conflict", releaseRaceEvent),
		Entry("an empty quarantine annotation still preserves isolation", "empty-annotation", ""),
	)

	It("waits for the late quarantine to clear and requires a fresh successful attempt", func() {
		nodeName := "release-retry-node"
		fixture := newReleaseRaceFixture("release-retry", 1, nodeName)
		vr := fixture.running()
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		injected := fixture.injectAfterReadiness(nodeName, releaseRaceEvent)
		vr = fixture.reconcile()
		Expect(*injected).To(BeTrue())
		Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhasePending))
		Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(1))
		Expect(vr.Status.TestGroups[0].Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonNodeReadinessViolation))
		expectReleaseRaceGate(nodeName)
		checkNodeValidationState(ctx, vr.Name, []string{nodeName}, v1alpha1.PhaseRunning)

		vr = fixture.reconcile()
		Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(1), "no test starts while the new quarantine is active")
		clearReleaseRaceQuarantine(nodeName)
		vr = fixture.reconcile()
		Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(2))
		Expect(vr.Status.TestGroups[0].Attempts[1].ObjectName).NotTo(Equal(vr.Status.TestGroups[0].Attempts[0].ObjectName))
		expectReleaseRaceGate(nodeName)
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		vr = reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhaseSucceeded, maxReconcileIterations)
		Expect(vr.Status.TestGroups[0].Attempts[1].Phase).To(Equal(v1alpha1.PhaseSucceeded))
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeFalse())
		checkNodeValidationState(ctx, vr.Name, []string{nodeName}, v1alpha1.PhaseSucceeded)
	})

	It("lets a fresh request supersede a failed late-quarantine validation", func() {
		nodeName := "release-takeover-node"
		fixture := newReleaseRaceFixture("release-old", 0, nodeName)
		vr := fixture.running()
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		injected := fixture.injectAfterReadiness(nodeName, releaseRaceEvent)
		vr = fixture.reconcile()
		Expect(*injected).To(BeTrue())
		expectReleaseRaceFailure(vr, nodeName)
		checkNodeValidationState(ctx, vr.Name, []string{nodeName}, v1alpha1.PhaseFailed)

		r, req := newValidationRequestTestSetup(ctx, "release-new", validationRequestTestCase{
			config: fixture.config,
			spec:   v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
		})
		pending := reconcileForIterations(ctx, r, req, 2)
		Expect(pending.Status.Phase).To(Equal(v1alpha1.PhasePending))
		expectReleaseRaceGate(nodeName)
		clearReleaseRaceQuarantine(nodeName)
		fresh := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, maxReconcileIterations)
		Expect(fresh.Status.TestGroups[0].Attempts).To(HaveLen(1))
		expectReleaseRaceGate(nodeName)
		Expect(updateObjectStatusForAllTestGroups(ctx, &fresh, testSuccessCondType)).To(Succeed())
		fresh = reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, maxReconcileIterations)
		node := getNode(ctx, nodeName)
		Expect(node.Spec.Unschedulable).To(BeFalse())
		Expect(node.Spec.Taints).NotTo(ContainElement(releaseRaceGateTaint))
		Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
		checkNodeValidationState(ctx, fresh.Name, []string{nodeName}, v1alpha1.PhaseSucceeded)
	})

	It("deletes the old request and session without releasing a new quarantine", func() {
		nodeName := "release-delete-node"
		fixture := newReleaseRaceFixture("release-delete", 0, nodeName)
		vr := fixture.running()
		Expect(setReleaseRaceQuarantine(ctx, nodeName, releaseRaceEvent)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &vr)).To(Succeed())
		_, err := fixture.reconciler.Reconcile(ctx, fixture.request)
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, fixture.request.NamespacedName, &v1alpha1.ValidationRequest{}))).To(BeTrue())
		node := getNode(ctx, nodeName)
		Expect(node.Annotations).To(HaveKeyWithValue(annotationQuarantineHealthEvent, releaseRaceEvent))
		Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
		Expect(node.Annotations).NotTo(HaveKey(annotationActiveValidationRequest))
		expectReleaseRaceGate(nodeName)

		// FQ preserves a gate that existed before its quarantine. If it does not
		// create a replacement VR, deleting the last session requires operator
		// recovery; clearing quarantine alone must not resurrect the deleted VR.
		clearReleaseRaceQuarantine(nodeName)
		_, err = fixture.reconciler.Reconcile(ctx, fixture.request)
		Expect(err).NotTo(HaveOccurred())
		expectReleaseRaceGate(nodeName)
		Expect(getNode(ctx, nodeName).Labels).NotTo(HaveKey(validationStateLabelKey))

		r, req := newValidationRequestTestSetup(ctx, "release-delete-replacement", validationRequestTestCase{
			config: fixture.config,
			spec:   v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
		})
		fresh := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, maxReconcileIterations)
		Expect(updateObjectStatusForAllTestGroups(ctx, &fresh, testSuccessCondType)).To(Succeed())
		reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, maxReconcileIterations)
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeFalse())
		Expect(getNode(ctx, nodeName).Spec.Taints).NotTo(ContainElement(releaseRaceGateTaint))
	})

	It("releases the last deleted request's gate when there is no quarantine", func() {
		nodeName := "release-delete-healthy-node"
		fixture := newReleaseRaceFixture("release-delete-healthy", 0, nodeName)
		vr := fixture.running()
		Expect(k8sClient.Delete(ctx, &vr)).To(Succeed())
		_, err := fixture.reconciler.Reconcile(ctx, fixture.request)
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, fixture.request.NamespacedName,
			&v1alpha1.ValidationRequest{}))).To(BeTrue())
		node := getNode(ctx, nodeName)
		Expect(node.Spec.Unschedulable).To(BeFalse())
		Expect(node.Spec.Taints).NotTo(ContainElement(releaseRaceGateTaint))
		Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
	})

	It("never retries a batch on an already released member after a later member is quarantined", func() {
		nodeA, nodeB := "release-batch-a", "release-batch-b"
		fixture := newReleaseRaceFixture("release-batch", 1, nodeA, nodeB)
		vr := fixture.running()
		Expect(vr.Status.TestGroups).To(HaveLen(1))
		Expect(vr.Status.TestGroups[0].Nodes).To(ConsistOf(nodeA, nodeB))
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		injected := false
		fixture.client.beforePatch = func(ctx context.Context, obj client.Object) error {
			node, ok := obj.(*corev1.Node)
			if !injected && ok && node.Name == nodeB && !node.Spec.Unschedulable {
				Expect(getNode(ctx, nodeA).Spec.Unschedulable).To(BeFalse(), "the first member must already be released")
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
		expectReleaseRaceFailure(vr, nodeB)
		checkNodeValidationState(ctx, vr.Name, []string{nodeB}, v1alpha1.PhaseFailed)
		Expect(getNode(ctx, nodeA).Spec.Unschedulable).To(BeFalse())
		Expect(getNode(ctx, nodeA).Annotations).NotTo(HaveKey(annotationValidationSession))
		expectReleaseRaceGate(nodeB)

		clearReleaseRaceQuarantine(nodeB)
		vr = reconcileForIterations(ctx, fixture.reconciler, fixture.request, 3)
		expectReleaseRaceFailure(vr, nodeB)
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(testJobGVK)
		err := k8sClient.Get(ctx, client.ObjectKey{
			Namespace: testNamespace,
			Name:      attemptObjectName(vr.Name, vr.Status.TestGroups[0].Name, 2),
		}, u)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no second batch may run on the released node")
		expectReleaseRaceGate(nodeB)
	})
})
