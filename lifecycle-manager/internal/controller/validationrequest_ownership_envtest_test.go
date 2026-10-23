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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
)

func ownedRequestSetup(name, nodeName, ownership string, createNode, withOwner bool) (
	*ValidationRequestReconciler, reconcile.Request, *config.Config) {
	cfg := defaultTestConfig()
	cfg.Validation.Spec.SchedulingGate = &v1alpha1.SchedulingGateConfig{
		Cordon: v1alpha1.CordonConfig{Remove: true, OnlyOwned: true},
		Taints: []v1alpha1.TaintConfig{{Key: releaseRaceGateTaint.Key, Effect: releaseRaceGateTaint.Effect, Remove: true}},
	}
	var nodes []string
	if createNode {
		nodes = []string{nodeName}
	}
	r, req := newValidationRequestTestSetup(ctx, name, validationRequestTestCase{
		config: cfg, nodeNames: nodes,
		spec: v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
		beforeInit: func(ctx context.Context, request *v1alpha1.ValidationRequest) error {
			node := getNode(ctx, nodeName)
			request.Annotations = map[string]string{annotationCordonOwnership: ownership}
			if withOwner {
				request.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "Node", Name: nodeName, UID: node.UID,
				}}
			}
			if err := k8sClient.Update(ctx, request); err != nil {
				return err
			}
			if !createNode {
				return nil
			}
			if err := cordonNode(ctx, nodeName); err != nil {
				return err
			}
			return addNodeTaint(ctx, nodeName, releaseRaceGateTaint)
		},
	})
	return r, req, cfg
}

func restartOwnedReconciler(cfg *config.Config) *ValidationRequestReconciler {
	r, err := NewValidationRequestReconciler(k8sClient, k8sClient, k8sClient.Scheme(), cfg, testNamespace)
	Expect(err).NotTo(HaveOccurred())
	return r
}

var _ = Describe("Validation cordon ownership", func() {
	DescribeTable("requires a proven grant and successful tests across a restart", func(id, ownership string, withOwner, release bool) {
		nodeName := "ownership-" + id
		r, req, cfg := ownedRequestSetup("vr-"+nodeName, nodeName, ownership, true, withOwner)
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
		node := getNode(ctx, nodeName)
		entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1))
		Expect(entries[0].NodeUID).To(Equal(node.UID))
		Expect(entries[0].RequestUID).To(Equal(vr.UID))
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		r = restartOwnedReconciler(cfg)
		completed := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 10)
		Expect(completed.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
		node = getNode(ctx, nodeName)
		Expect(node.Spec.Unschedulable).To(Equal(!release))
		// Ownership gates only the cordon; the ended session lifts the configured taint.
		Expect(node.Spec.Taints).NotTo(ContainElement(releaseRaceGateTaint))
		Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
	},
		Entry("owned releases", "owned", cordonOwned, true, true),
		Entry("pre-existing stays cordoned", "manual", cordonPreExisting, true, false),
		Entry("unknown stays cordoned", "unknown", cordonUnknown, true, false),
		Entry("missing annotation stays cordoned", "missing", "", true, false),
		Entry("malformed annotation stays cordoned", "invalid", "OWNED", true, false),
		Entry("missing Node owner stays cordoned", "unbound", cordonOwned, false, false),
	)

	DescribeTable("preserves a manual decision across overlapping completion order", func(manualFirst bool) {
		id := "owned-first"
		firstOwnership, secondOwnership := cordonOwned, cordonPreExisting
		if manualFirst {
			id = "manual-first"
			firstOwnership, secondOwnership = secondOwnership, firstOwnership
		}
		nodeName := "ownership-" + id
		first, firstKey, _ := ownedRequestSetup("vr-"+id+"-a", nodeName, firstOwnership, true, true)
		vr := reconcileUntilPhase(ctx, first, firstKey, v1alpha1.PhaseRunning, 10)
		second, secondKey, cfg := ownedRequestSetup("vr-"+id+"-b", nodeName, secondOwnership, false, true)
		reconcileForIterations(ctx, second, secondKey, 3)
		entries, err := parseSessionAnnotation(getNode(ctx, nodeName).Annotations[annotationValidationSession])
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(2))
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		reconcileUntilPhase(ctx, first, firstKey, v1alpha1.PhaseSucceeded, 10)
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeTrue())
		second = restartOwnedReconciler(cfg)
		vr = reconcileUntilPhase(ctx, second, secondKey, v1alpha1.PhaseRunning, 10)
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		reconcileUntilPhase(ctx, second, secondKey, v1alpha1.PhaseSucceeded, 10)
		node := getNode(ctx, nodeName)
		Expect(node.Spec.Unschedulable).To(BeTrue())
		Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
	}, Entry("owned request finishes first", false), Entry("manual request finishes first", true))

	It("deletes a request association without releasing the cordon", func() {
		name := "ownership-delete"
		r, req, _ := ownedRequestSetup("vr-"+name, name, cordonOwned, true, true)
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
		Expect(k8sClient.Delete(ctx, &vr)).To(Succeed())
		reconcileForIterations(ctx, r, req, 5)
		node := getNode(ctx, name)
		Expect(node.Spec.Unschedulable).To(BeTrue())
		// Deletion ends the session, so the configured taint is lifted as in the default policy.
		Expect(node.Spec.Taints).NotTo(ContainElement(releaseRaceGateTaint))
		Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
		Expect(node.Annotations).NotTo(HaveKey(annotationActiveValidationRequest))
	})

	It("does not launch validation on a recreated Node with the same name", func() {
		name := "ownership-replaced"
		r, req, _ := ownedRequestSetup("vr-"+name, name, cordonOwned, true, true)
		old := getNode(ctx, name)
		Expect(k8sClient.Delete(ctx, &old)).To(Succeed())
		replacement := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"ready": "true"}},
			Spec: corev1.NodeSpec{Unschedulable: true}}
		Expect(k8sClient.Create(ctx, replacement)).To(Succeed())
		Expect(replacement.UID).NotTo(Equal(old.UID))
		vr := reconcileForIterations(ctx, r, req, 5)
		Expect(vr.Status.Phase).To(Equal(v1alpha1.PhasePending))
		for _, group := range vr.Status.TestGroups {
			Expect(group.Attempts).To(BeEmpty())
		}
		current := getNode(ctx, name)
		Expect(current.Spec.Unschedulable).To(BeTrue())
		Expect(current.Annotations).NotTo(HaveKey(annotationValidationSession))
	})

	It("keeps a new quarantine that races an owned successful release", func() {
		name := "ownership-quarantine-race"
		r, req, _ := ownedRequestSetup("vr-"+name, name, cordonOwned, true, true)
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		injected := false
		hook := &releaseRaceClient{Client: k8sClient}
		hook.beforePatch = func(ctx context.Context, object client.Object) error {
			if node, ok := object.(*corev1.Node); ok && !node.Spec.Unschedulable && !injected {
				injected = true
				return setReleaseRaceQuarantine(ctx, name, releaseRaceEvent)
			}
			return nil
		}
		r.Client = hook
		reconcileForIterations(ctx, r, req, 1)
		Expect(injected).To(BeTrue())
		Expect(hook.conflicts).To(BeNumerically(">", 0))
		expectReleaseRaceGate(name)
	})
})
