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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

func setAPIRequestClaim(req reconcile.Request, claim cordonownership.Claim) {
	var vr v1alpha1.ValidationRequest
	Expect(k8sClient.Get(ctx, req.NamespacedName, &vr)).To(Succeed())
	vr.Annotations[cordonownership.RequestClaimIDAnnotationKey] = claim.ID
	Expect(k8sClient.Update(ctx, &vr)).To(Succeed())
}

func createAPIClaim(nodeName string, req reconcile.Request) cordonownership.Claim {
	node := getNode(ctx, nodeName)
	claim := cordonownership.New(&node)
	_, err := cordonownership.Store(&node, claim)
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient.Update(ctx, &node)).To(Succeed())
	setAPIRequestClaim(req, claim)
	return claim
}

var _ = Describe("Validation continuous cordon claim", func() {
	DescribeTable("keeps an owned successor eligible across deletion and restart",
		func(id string, deleteFirst, registerSecond bool, ownership string) {
			nodeName := "claim-" + id
			first, firstKey, cfg := ownedRequestSetup("vr-"+id+"-a", nodeName, ownership, true, true)
			claim := createAPIClaim(nodeName, firstKey)
			a := reconcileUntilPhase(ctx, first, firstKey, v1alpha1.PhaseRunning, 10)
			second, secondKey, _ := ownedRequestSetup("vr-"+id+"-b", nodeName, cordonOwned, false, true)
			setAPIRequestClaim(secondKey, claim)
			setRequestHandoffHash(secondKey, handoffSource)
			Expect(setHandoffSource(ctx, nodeName, handoffSource)).To(Succeed())
			if registerSecond {
				reconcileForIterations(ctx, second, secondKey, 2)
			}
			if deleteFirst {
				Expect(k8sClient.Delete(ctx, &a)).To(Succeed())
				reconcileForIterations(ctx, first, firstKey, 5)
			} else {
				Expect(updateObjectStatusForAllTestGroups(ctx, &a, testSuccessCondType)).To(Succeed())
				reconcileUntilPhase(ctx, first, firstKey, v1alpha1.PhaseSucceeded, 10)
			}
			node := getNode(ctx, nodeName)
			Expect(node.Spec.Unschedulable).To(BeTrue())
			current, valid := cordonownership.Read(&node)
			Expect(valid).To(BeTrue())
			Expect(current.ID).To(Equal(claim.ID))
			Expect(current.PreserveCordon).To(Equal(ownership != cordonOwned))
			if !registerSecond {
				Expect(node.Annotations[annotationQuarantineValidationHealthEvent]).To(Equal(handoffSource))
				Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
			}
			second = restartOwnedReconciler(cfg)
			b := reconcileUntilPhase(ctx, second, secondKey, v1alpha1.PhaseRunning, 10)
			node = getNode(ctx, nodeName)
			Expect(node.Annotations).NotTo(HaveKey(annotationQuarantineValidationHealthEvent))
			entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].ClaimID).To(Equal(claim.ID))
			Expect(entries[0].RequestUID).To(Equal(b.UID))
			Expect(updateObjectStatusForAllTestGroups(ctx, &b, testSuccessCondType)).To(Succeed())
			second = restartOwnedReconciler(cfg)
			reconcileUntilPhase(ctx, second, secondKey, v1alpha1.PhaseSucceeded, 10)
			node = getNode(ctx, nodeName)
			Expect(node.Spec.Unschedulable).To(Equal(ownership != cordonOwned))
			Expect(node.Annotations).NotTo(HaveKey(cordonownership.AnnotationKey))
			Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
		},
		Entry("both validations succeed", "success", false, true, cordonOwned),
		Entry("owned A deleted with registered B", "delete-owned", true, true, cordonOwned),
		Entry("owned A deleted before B registers", "delete-pending", true, false, cordonOwned),
		Entry("manual A deletion preserves pending B", "delete-manual", true, false, cordonPreExisting),
		Entry("unknown A deletion preserves registered B", "delete-unknown", true, true, cordonUnknown),
	)

	It("retires the final deleted claim and refuses its late request", func() {
		name := "claim-delete-final"
		r, req, cfg := ownedRequestSetup("vr-"+name, name, cordonOwned, true, true)
		claim := createAPIClaim(name, req)
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
		Expect(k8sClient.Delete(ctx, &vr)).To(Succeed())
		reconcileForIterations(ctx, r, req, 5)
		node := getNode(ctx, name)
		Expect(node.Spec.Unschedulable).To(BeTrue())
		Expect(node.Spec.Taints).NotTo(ContainElement(releaseRaceGateTaint))
		Expect(node.Annotations).NotTo(HaveKey(cordonownership.AnnotationKey))
		_, lateKey, _ := ownedRequestSetup("vr-"+name+"-late", name, cordonOwned, false, true)
		setAPIRequestClaim(lateKey, claim)
		r = restartOwnedReconciler(cfg)
		late := reconcileForIterations(ctx, r, lateKey, 5)
		Expect(late.Status.Phase).To(Equal(v1alpha1.PhasePending))
		for _, group := range late.Status.TestGroups {
			Expect(group.Attempts).To(BeEmpty())
		}
	})

	It("does not clear or release a replacement claim racing the final patch", func() {
		name := "claim-release-conflict"
		r, req, _ := ownedRequestSetup("vr-"+name, name, cordonOwned, true, true)
		original := createAPIClaim(name, req)
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		injected := false
		var replacement cordonownership.Claim
		hook := &releaseRaceClient{Client: k8sClient}
		hook.beforePatch = func(ctx context.Context, object client.Object) error {
			node, ok := object.(*corev1.Node)
			if !ok || node.Spec.Unschedulable || injected {
				return nil
			}
			injected = true
			current := getNode(ctx, name)
			replacement = cordonownership.New(&current)
			_, err := cordonownership.Store(&current, replacement)
			if err != nil {
				return err
			}
			current.Annotations[annotationQuarantineValidationHealthEvent] = handoffSource
			return k8sClient.Update(ctx, &current)
		}
		r.Client = hook
		reconcileForIterations(ctx, r, req, 2)
		Expect(injected).To(BeTrue())
		Expect(hook.conflicts).To(BeNumerically(">", 0))
		node := getNode(ctx, name)
		current, valid := cordonownership.Read(&node)
		Expect(valid).To(BeTrue())
		Expect(current.ID).To(Equal(replacement.ID))
		Expect(current.ID).NotTo(Equal(original.ID))
		Expect(node.Spec.Unschedulable).To(BeTrue())
		Expect(node.Annotations[annotationQuarantineValidationHealthEvent]).To(Equal(handoffSource))
	})

	DescribeTable("does not promote legacy sessions when enabling strict ownership", func(pending bool) {
		id := "running"
		if pending {
			id = "pending"
		}
		name := "claim-upgrade-" + id
		r, req, cfg := ownedRequestSetup("vr-"+name, name, cordonOwned, true, true)
		cfg.Validation.Spec.SchedulingGate.Cordon.OnlyOwned = false
		r = restartOwnedReconciler(cfg)
		if pending {
			node := getNode(ctx, name)
			delete(node.Labels, "ready")
			Expect(k8sClient.Update(ctx, &node)).To(Succeed())
			reconcileForIterations(ctx, r, req, 3)
		} else {
			reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
		}
		node := getNode(ctx, name)
		entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].NodeUID).To(BeEmpty())
		Expect(entries[0].RequestUID).To(BeEmpty())
		cfg.Validation.Spec.SchedulingGate.Cordon.OnlyOwned = true
		r = restartOwnedReconciler(cfg)
		if pending {
			node.Labels["ready"] = "true"
			Expect(k8sClient.Update(ctx, &node)).To(Succeed())
		}
		if pending {
			vr := reconcileForIterations(ctx, r, req, 5)
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhasePending))
			for _, group := range vr.Status.TestGroups {
				Expect(group.Attempts).To(BeEmpty())
			}
			node = getNode(ctx, name)
			entries, err = parseSessionAnnotation(node.Annotations[annotationValidationSession])
			Expect(err).NotTo(HaveOccurred())
			Expect(entries[0].NodeUID).To(BeEmpty())
			Expect(entries[0].RequestUID).To(BeEmpty())
		} else {
			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 10)
		}
		node = getNode(ctx, name)
		Expect(node.Spec.Unschedulable).To(BeTrue())
		if pending {
			Expect(node.Spec.Taints).To(ContainElement(releaseRaceGateTaint))
		} else {
			// The legacy entry grants no cordon release; the ended session still lifts the taint.
			Expect(node.Spec.Taints).NotTo(ContainElement(releaseRaceGateTaint))
		}
	}, Entry("Pending legacy session", true), Entry("Running legacy session", false))

	It("retires a released claim in compatible mode", func() {
		name := "claim-compatible-release"
		r, req, cfg := ownedRequestSetup("vr-"+name, name, cordonOwned, true, true)
		createAPIClaim(name, req)
		cfg.Validation.Spec.SchedulingGate.Cordon.OnlyOwned = false
		r = restartOwnedReconciler(cfg)
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 10)
		node := getNode(ctx, name)
		Expect(node.Spec.Unschedulable).To(BeFalse())
		Expect(node.Annotations).NotTo(HaveKey(cordonownership.AnnotationKey))
	})

	DescribeTable("cleans an old association without changing a newer claim", func(oldClaim bool) {
		id := "legacy"
		if oldClaim {
			id = "different"
		}
		name := "claim-old-cleanup-" + id
		first, firstKey, cfg := ownedRequestSetup("vr-"+name+"-a", name, cordonOwned, true, true)
		if oldClaim {
			createAPIClaim(name, firstKey)
		}
		a := reconcileUntilPhase(ctx, first, firstKey, v1alpha1.PhaseRunning, 10)
		second, secondKey, _ := ownedRequestSetup("vr-"+name+"-b", name, cordonOwned, false, true)
		createAPIClaim(name, secondKey)
		original := getNode(ctx, name).Annotations[cordonownership.AnnotationKey]
		reconcileForIterations(ctx, second, secondKey, 2)
		Expect(first.updateValidationSession(ctx, name, &a, []string{"basic"})).To(Succeed())
		Expect(getNode(ctx, name).Annotations[cordonownership.AnnotationKey]).To(Equal(original))
		Expect(k8sClient.Delete(ctx, &a)).To(Succeed())
		reconcileForIterations(ctx, first, firstKey, 5)
		Expect(getNode(ctx, name).Annotations[cordonownership.AnnotationKey]).To(Equal(original))
		second = restartOwnedReconciler(cfg)
		b := reconcileUntilPhase(ctx, second, secondKey, v1alpha1.PhaseRunning, 10)
		Expect(updateObjectStatusForAllTestGroups(ctx, &b, testSuccessCondType)).To(Succeed())
		reconcileUntilPhase(ctx, second, secondKey, v1alpha1.PhaseSucceeded, 10)
		node := getNode(ctx, name)
		Expect(node.Spec.Unschedulable).To(BeFalse())
		Expect(node.Annotations).NotTo(HaveKey(cordonownership.AnnotationKey))
	}, Entry("legacy request", false), Entry("different claim", true))

	It("retains handoff when a legacy session cannot be registered", func() {
		name := "claim-legacy-handoff"
		r, req, cfg := ownedRequestSetup("vr-"+name, name, cordonOwned, true, true)
		cfg.Validation.Spec.SchedulingGate.Cordon.OnlyOwned = false
		r = restartOwnedReconciler(cfg)
		reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
		createAPIClaim(name, req)
		vr := setRequestHandoffHash(req, handoffSource)
		Expect(setHandoffSource(ctx, name, handoffSource)).To(Succeed())
		cfg.Validation.Spec.SchedulingGate.Cordon.OnlyOwned = true
		r = restartOwnedReconciler(cfg)
		Expect(r.updateValidationSession(ctx, name, &vr, []string{"basic"})).To(Succeed())
		node := getNode(ctx, name)
		Expect(node.Annotations[annotationQuarantineValidationHealthEvent]).To(Equal(handoffSource))
		entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].NodeUID).To(BeEmpty())
		Expect(entries[0].RequestUID).To(BeEmpty())
		Expect(entries[0].ClaimID).To(BeEmpty())
		Expect(node.Spec.Unschedulable).To(BeTrue())
	})
})
