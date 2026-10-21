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
	"crypto/sha256"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

const handoffSource = `[{"id":"new-fault","tests":["basic"]}]`

type handoffStatusConflictClient struct {
	client.Client
	injected    bool
	afterUpdate func(context.Context, client.Object) error
}

func (c *handoffStatusConflictClient) Status() client.SubResourceWriter {
	return &handoffStatusConflictWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

type handoffStatusConflictWriter struct {
	client.SubResourceWriter
	parent *handoffStatusConflictClient
}

func (w *handoffStatusConflictWriter) Update(ctx context.Context, object client.Object,
	opts ...client.SubResourceUpdateOption) error {
	if request, ok := object.(*v1alpha1.ValidationRequest); ok && !w.parent.injected &&
		len(request.Status.TestGroups) > 0 && request.Status.TestGroups[0].Phase == v1alpha1.PhaseSucceeded {
		w.parent.injected = true
		var current v1alpha1.ValidationRequest
		if err := w.parent.Get(ctx, client.ObjectKeyFromObject(request), &current); err != nil {
			return err
		}
		current.Annotations["test.example/status-race"] = "changed"
		if err := w.parent.Update(ctx, &current); err != nil {
			return err
		}
	}
	if err := w.SubResourceWriter.Update(ctx, object, opts...); err != nil {
		return err
	}
	if w.parent.afterUpdate != nil {
		return w.parent.afterUpdate(ctx, object)
	}
	return nil
}

func setHandoffSource(ctx context.Context, nodeName, source string) error {
	node := getNode(ctx, nodeName)
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[annotationQuarantineValidationHealthEvent] = source
	return k8sClient.Update(ctx, &node)
}

func setRequestHandoffHash(req reconcile.Request, source string) v1alpha1.ValidationRequest {
	var request v1alpha1.ValidationRequest
	Expect(k8sClient.Get(ctx, req.NamespacedName, &request)).To(Succeed())
	request.Annotations[annotationValidationSourceHash] = fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
	Expect(k8sClient.Update(ctx, &request)).To(Succeed())
	return request
}

var _ = Describe("Validation persistent handoff", func() {
	It("does not register a late request after a newer handoff already completed", func() {
		nodeName := "handoff-late-request"
		first, firstKey, _ := ownedRequestSetup("vr-handoff-late-a", nodeName, cordonOwned, true, true)
		setRequestHandoffHash(firstKey, "older-source")
		second, secondKey, cfg := ownedRequestSetup("vr-handoff-late-b", nodeName, cordonOwned, false, true)
		setRequestHandoffHash(secondKey, handoffSource)
		Expect(setHandoffSource(ctx, nodeName, handoffSource)).To(Succeed())
		b := reconcileUntilPhase(ctx, second, secondKey, v1alpha1.PhaseRunning, 10)
		second = restartOwnedReconciler(cfg)
		Expect(updateObjectStatusForAllTestGroups(ctx, &b, testSuccessCondType)).To(Succeed())
		reconcileUntilPhase(ctx, second, secondKey, v1alpha1.PhaseSucceeded, 10)
		node := getNode(ctx, nodeName)
		Expect(node.Spec.Unschedulable).To(BeFalse())
		Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
		Expect(node.Annotations).NotTo(HaveKey(annotationQuarantineValidationHealthEvent))
		a := reconcileForIterations(ctx, first, firstKey, 5)
		Expect(a.Status.Phase).To(Equal(v1alpha1.PhasePending))
		for _, group := range a.Status.TestGroups {
			Expect(group.Attempts).To(BeEmpty())
		}
		node = getNode(ctx, nodeName)
		Expect(node.Spec.Unschedulable).To(BeFalse())
		Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
		Expect(node.Annotations).NotTo(HaveKey(annotationActiveValidationRequest))
	})

	It("persists late-quarantine invalidation before a controller crash", func() {
		nodeName := "handoff-invalidation-crash"
		fixture := newReleaseRaceFixture("vr-handoff-invalidation-crash", 1, nodeName)
		vr := fixture.running()
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		quarantined := fixture.injectAfterReadiness(nodeName, releaseRaceEvent)
		crash := errors.New("controller stopped after persisting invalidation")
		stopped := false
		hook := &handoffStatusConflictClient{Client: fixture.client, injected: true}
		hook.afterUpdate = func(_ context.Context, object client.Object) error {
			request, ok := object.(*v1alpha1.ValidationRequest)
			if ok && !stopped && request.Status.TestGroups[0].Phase == v1alpha1.PhasePending {
				stopped = true
				return crash
			}
			return nil
		}
		fixture.reconciler.Client = hook
		_, err := fixture.reconciler.Reconcile(ctx, fixture.request)
		Expect(errors.Is(err, crash)).To(BeTrue())
		Expect(stopped).To(BeTrue())
		Expect(*quarantined).To(BeTrue())
		clearReleaseRaceQuarantine(nodeName)
		r := restartOwnedReconciler(fixture.config)
		vr = reconcileForIterations(ctx, r, fixture.request, 1)
		Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(2))
		Expect(vr.Status.TestGroups[0].Attempts[0].Phase).To(Equal(v1alpha1.PhaseFailed))
		Expect(vr.Status.TestGroups[0].Attempts[1].Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeTrue())
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		reconcileUntilPhase(ctx, r, fixture.request, v1alpha1.PhaseSucceeded, 10)
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeFalse())
	})

	It("keeps the successful provider until its status is durably saved", func() {
		nodeName := "handoff-status-conflict"
		r, req, cfg := ownedRequestSetup("vr-handoff-status-conflict", nodeName, cordonOwned, true, true)
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		Expect(setHandoffSource(ctx, nodeName, handoffSource)).To(Succeed())
		hook := &handoffStatusConflictClient{Client: k8sClient}
		r.Client = hook
		_, err := r.Reconcile(ctx, req)
		Expect(apierrors.IsConflict(err)).To(BeTrue())
		Expect(hook.injected).To(BeTrue())
		provider := &unstructured.Unstructured{}
		provider.SetGroupVersionKind(testJobGVK)
		Expect(k8sClient.Get(ctx, client.ObjectKey{
			Namespace: testNamespace, Name: vr.Status.TestGroups[0].Attempts[0].ObjectName,
		}, provider)).To(Succeed())
		r = restartOwnedReconciler(cfg)
		vr = reconcileForIterations(ctx, r, req, 3)
		Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseSucceeded))
		Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(1))
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeTrue())
	})

	DescribeTable("keeps A cordoned until B registers after the producer cleared quarantine", func(ownership string, release bool) {
		nodeName := "handoff-overlap-" + ownership
		first, firstKey, cfg := ownedRequestSetup("vr-handoff-a-"+ownership, nodeName, cordonOwned, true, true)
		a := reconcileUntilPhase(ctx, first, firstKey, v1alpha1.PhaseRunning, 10)
		second, secondKey, _ := ownedRequestSetup("vr-handoff-b-"+ownership, nodeName, ownership, false, true)
		setRequestHandoffHash(secondKey, handoffSource)
		Expect(setHandoffSource(ctx, nodeName, handoffSource)).To(Succeed())
		// B exists in the real API but has never been reconciled into the session.
		node := getNode(ctx, nodeName)
		Expect(node.Annotations).NotTo(HaveKey(annotationQuarantineHealthEvent))
		entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].Name).To(Equal(a.Name))
		Expect(updateObjectStatusForAllTestGroups(ctx, &a, testSuccessCondType)).To(Succeed())
		first = restartOwnedReconciler(cfg)
		a = reconcileForIterations(ctx, first, firstKey, 3)
		Expect(a.Status.Phase).To(Equal(v1alpha1.PhaseRunning))
		Expect(a.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseSucceeded))
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeTrue())
		result, err := first.Reconcile(ctx, firstKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		reconcileForIterations(ctx, second, secondKey, 2)
		node = getNode(ctx, nodeName)
		Expect(node.Annotations).NotTo(HaveKey(annotationQuarantineValidationHealthEvent))
		entries, err = parseSessionAnnotation(node.Annotations[annotationValidationSession])
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(2))
		// Both controllers restart after the atomic session/source handoff.
		first = restartOwnedReconciler(cfg)
		reconcileUntilPhase(ctx, first, firstKey, v1alpha1.PhaseSucceeded, 10)
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeTrue())
		second = restartOwnedReconciler(cfg)
		b := reconcileUntilPhase(ctx, second, secondKey, v1alpha1.PhaseRunning, 10)
		Expect(updateObjectStatusForAllTestGroups(ctx, &b, testSuccessCondType)).To(Succeed())
		reconcileUntilPhase(ctx, second, secondKey, v1alpha1.PhaseSucceeded, 10)
		node = getNode(ctx, nodeName)
		Expect(node.Spec.Unschedulable).To(Equal(!release))
		Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
	}, Entry("owned B releases after both tests", cordonOwned, true),
		Entry("B observed an existing cordon", cordonPreExisting, false))

	It("preserves a source added between the release read and patch", func() {
		nodeName := "handoff-release-conflict"
		r, req, _ := ownedRequestSetup("vr-handoff-conflict", nodeName, cordonOwned, true, true)
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		injected := false
		hook := &releaseRaceClient{Client: k8sClient}
		hook.beforePatch = func(ctx context.Context, object client.Object) error {
			if node, ok := object.(*corev1.Node); ok && !node.Spec.Unschedulable && !injected {
				injected = true
				return setHandoffSource(ctx, nodeName, handoffSource)
			}
			return nil
		}
		r.Client = hook
		vr = reconcileForIterations(ctx, r, req, 2)
		Expect(injected).To(BeTrue())
		Expect(hook.conflicts).To(BeNumerically(">", 0))
		Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseRunning))
		node := getNode(ctx, nodeName)
		Expect(node.Spec.Unschedulable).To(BeTrue())
		Expect(node.Spec.Taints).To(ContainElement(releaseRaceGateTaint))
		Expect(node.Annotations[annotationQuarantineValidationHealthEvent]).To(Equal(handoffSource))
		Expect(node.Annotations).To(HaveKey(annotationValidationSession))
	})

	It("waits for quarantine cleanup before consuming a matching source", func() {
		nodeName := "handoff-quarantined"
		r, req, cfg := ownedRequestSetup("vr-handoff-quarantined", nodeName, cordonOwned, true, true)
		vr := setRequestHandoffHash(req, handoffSource)
		Expect(setHandoffSource(ctx, nodeName, handoffSource)).To(Succeed())
		Expect(setReleaseRaceQuarantine(ctx, nodeName, releaseRaceEvent)).To(Succeed())
		Expect(r.updateValidationSession(ctx, nodeName, &vr, []string{"basic"})).To(Succeed())
		node := getNode(ctx, nodeName)
		Expect(node.Annotations[annotationQuarantineValidationHealthEvent]).To(Equal(handoffSource))
		Expect(node.Annotations).To(HaveKey(annotationValidationSession))
		delete(node.Annotations, annotationQuarantineHealthEvent)
		Expect(k8sClient.Update(ctx, &node)).To(Succeed())
		r = restartOwnedReconciler(cfg)
		// An existing session entry must not bypass source consumption on retry.
		Expect(r.updateValidationSession(ctx, nodeName, &vr, []string{"basic"})).To(Succeed())
		node = getNode(ctx, nodeName)
		Expect(node.Annotations).NotTo(HaveKey(annotationQuarantineValidationHealthEvent))
		Expect(node.Annotations).To(HaveKey(annotationValidationSession))
	})

	It("preserves a newer source when it races session registration", func() {
		nodeName := "handoff-consume-conflict"
		r, req, cfg := ownedRequestSetup("vr-handoff-consume-conflict", nodeName, cordonOwned, true, true)
		vr := setRequestHandoffHash(req, handoffSource)
		Expect(setHandoffSource(ctx, nodeName, handoffSource)).To(Succeed())
		newSource := `[{"id":"newer-fault","tests":["basic"]}]`
		injected := false
		hook := &releaseRaceClient{Client: k8sClient}
		hook.beforePatch = func(ctx context.Context, object client.Object) error {
			if node, ok := object.(*corev1.Node); ok && !hasPendingValidationHandoff(node) && !injected {
				injected = true
				return setHandoffSource(ctx, nodeName, newSource)
			}
			return nil
		}
		r.Client = hook
		Expect(r.updateValidationSession(ctx, nodeName, &vr, []string{"basic"})).To(Succeed())
		Expect(injected).To(BeTrue())
		Expect(hook.conflicts).To(BeNumerically(">", 0))
		node := getNode(ctx, nodeName)
		Expect(node.Annotations[annotationQuarantineValidationHealthEvent]).To(Equal(newSource))
		Expect(node.Annotations).To(HaveKey(annotationValidationSession))
		r = restartOwnedReconciler(cfg)
		Expect(r.updateValidationSession(ctx, nodeName, &vr, []string{"basic"})).To(Succeed())
		Expect(getNode(ctx, nodeName).Annotations[annotationQuarantineValidationHealthEvent]).To(Equal(newSource))
	})

	DescribeTable("retains unmatched or unbound sources", func(id string, mutate func(*v1alpha1.ValidationRequest)) {
		nodeName := "handoff-" + id
		r, req, _ := ownedRequestSetup("vr-"+nodeName, nodeName, cordonOwned, true, true)
		vr := setRequestHandoffHash(req, handoffSource)
		Expect(setHandoffSource(ctx, nodeName, handoffSource)).To(Succeed())
		mutate(&vr)
		Expect(k8sClient.Update(ctx, &vr)).To(Succeed())
		Expect(r.updateValidationSession(ctx, nodeName, &vr, []string{"basic"})).To(Succeed())
		Expect(getNode(ctx, nodeName).Annotations[annotationQuarantineValidationHealthEvent]).To(Equal(handoffSource))
		vr = reconcileForIterations(ctx, r, req, 5)
		for _, group := range vr.Status.TestGroups {
			Expect(group.Attempts).To(BeEmpty())
		}
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeTrue())
	},
		Entry("missing hash", "missing-hash", func(r *v1alpha1.ValidationRequest) { delete(r.Annotations, annotationValidationSourceHash) }),
		Entry("mismatched hash", "wrong-hash", func(r *v1alpha1.ValidationRequest) { r.Annotations[annotationValidationSourceHash] = "wrong" }),
		Entry("missing owner", "no-owner", func(r *v1alpha1.ValidationRequest) { r.OwnerReferences = nil }),
		Entry("different Node UID", "wrong-uid", func(r *v1alpha1.ValidationRequest) { r.OwnerReferences[0].UID = "other-node" }),
	)

	It("does not consume the source when its unregistered request is deleted", func() {
		nodeName := "handoff-deleted"
		r, req, _ := ownedRequestSetup("vr-handoff-deleted", nodeName, cordonOwned, true, true)
		vr := setRequestHandoffHash(req, handoffSource)
		Expect(setHandoffSource(ctx, nodeName, handoffSource)).To(Succeed())
		reconcileForIterations(ctx, r, req, 1)
		Expect(k8sClient.Get(ctx, req.NamespacedName, &vr)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &vr)).To(Succeed())
		reconcileForIterations(ctx, r, req, 3)
		node := getNode(ctx, nodeName)
		Expect(node.Spec.Unschedulable).To(BeTrue())
		Expect(node.Annotations[annotationQuarantineValidationHealthEvent]).To(Equal(handoffSource))
		Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
	})

	It("consumes opt-in producer handoffs even when ownership enforcement is disabled", func() {
		nodeName := "handoff-default-mode"
		r, req, cfg := ownedRequestSetup("vr-handoff-default-mode", nodeName, cordonOwned, true, true)
		cfg.Validation.Spec.SchedulingGate.Cordon.OnlyOwned = false
		r = restartOwnedReconciler(cfg)
		vr := setRequestHandoffHash(req, handoffSource)
		Expect(setHandoffSource(ctx, nodeName, handoffSource)).To(Succeed())
		Expect(r.updateValidationSession(ctx, nodeName, &vr, []string{"basic"})).To(Succeed())
		node := getNode(ctx, nodeName)
		Expect(node.Annotations).NotTo(HaveKey(annotationQuarantineValidationHealthEvent))
		Expect(node.Annotations).To(HaveKey(annotationValidationSession))
	})
})
