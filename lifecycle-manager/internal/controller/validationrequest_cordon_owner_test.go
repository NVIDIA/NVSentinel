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
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
)

const cordonOwnerLabelPrefix = "k8s.nvidia.com/"

var cordonOwnerGateTaint = corev1.Taint{Key: "nvsentinel", Effect: corev1.TaintEffectNoSchedule}

func cordonOwnerConfig() *config.Config {
	cfg := defaultTestConfig()
	cfg.Validation.Spec.SchedulingGate = &v1alpha1.SchedulingGateConfig{
		Cordon: v1alpha1.CordonConfig{Remove: true, LabelPrefix: cordonOwnerLabelPrefix},
		Taints: []v1alpha1.TaintConfig{{Key: cordonOwnerGateTaint.Key, Effect: cordonOwnerGateTaint.Effect, Remove: true}},
	}

	return cfg
}

// newCordonOwnerRequest creates a cordoned node with the gate taint and a request on it labelled as created by
// createdBy. With nvsentinelCordon, the node also carries the cordon-by label fault-quarantine sets when it cordons.
func newCordonOwnerRequest(name, createdBy string, nvsentinelCordon bool, preExisting ...string) (*ValidationRequestReconciler,
	reconcile.Request, string) {
	nodeName := name + "-node"
	r, req := newValidationRequestTestSetup(ctx, name, validationRequestTestCase{
		config:    cordonOwnerConfig(),
		nodeNames: []string{nodeName},
		spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
		beforeInit: func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
			if err := cordonNode(ctx, nodeName); err != nil {
				return err
			}
			if err := addNodeTaint(ctx, nodeName, cordonOwnerGateTaint); err != nil {
				return err
			}
			if nvsentinelCordon {
				if err := patchNodeLabel(ctx, nodeName, cordonOwnerLabelPrefix+"cordon-by", "NVSentinel"); err != nil {
					return err
				}
			}
			var current v1alpha1.ValidationRequest
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(vr), &current); err != nil {
				return err
			}
			current.Labels = map[string]string{labelCreatedBy: createdBy}
			if len(preExisting) > 0 {
				current.Annotations = map[string]string{annotationCordonPreExisting: preExisting[0]}
			}
			return k8sClient.Update(ctx, &current)
		},
	})

	return r, req, nodeName
}

func newOverlappingFaultRequest(name, nodeName string) (*ValidationRequestReconciler, reconcile.Request) {
	return newValidationRequestTestSetup(ctx, name, validationRequestTestCase{
		config: cordonOwnerConfig(),
		spec:   v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
		beforeInit: func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
			vr.Labels = map[string]string{labelCreatedBy: createdByFaultQuarantine}
			vr.Annotations = map[string]string{annotationCordonPreExisting: "true"}
			return k8sClient.Update(ctx, vr)
		},
	})
}

func succeedCordonOwnerRequest(r *ValidationRequestReconciler, req reconcile.Request) {
	GinkgoHelper()
	vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, maxReconcileIterations)
	Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
	reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, maxReconcileIterations)
}

func expectCordonKeptTaintReleased(nodeName string) {
	GinkgoHelper()
	node := getNode(ctx, nodeName)
	Expect(node.Spec.Unschedulable).To(BeTrue(), "the cordon NVSentinel did not apply stays")
	Expect(node.Spec.Taints).NotTo(ContainElement(HaveField("Key", Equal(cordonOwnerGateTaint.Key))))
	Expect(node.Labels).NotTo(HaveKey(cordonOwnerLabelPrefix + "uncordon-by"))
	Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
}

func expectCordonReleased(nodeName string) {
	GinkgoHelper()
	node := getNode(ctx, nodeName)
	Expect(node.Spec.Unschedulable).To(BeFalse())
	Expect(node.Spec.Taints).NotTo(ContainElement(HaveField("Key", Equal(cordonOwnerGateTaint.Key))))
	Expect(node.Labels).NotTo(HaveKey(cordonOwnerLabelPrefix + "cordon-by"))
	Expect(node.Labels).To(HaveKeyWithValue(cordonOwnerLabelPrefix+"uncordon-by", "NVSentinel"))
}

var _ = Describe("Validation release of a cordon NVSentinel did not apply", func() {
	It("keeps a cordon without the cordon-by label after post-remediation validation succeeds", func() {
		r, req, nodeName := newCordonOwnerRequest("cordon-foreign", createdByFaultQuarantine, false)
		succeedCordonOwnerRequest(r, req)
		expectCordonKeptTaintReleased(nodeName)
	})

	It("releases the cordon fault-quarantine applied after post-remediation validation succeeds", func() {
		r, req, nodeName := newCordonOwnerRequest("cordon-owned", createdByFaultQuarantine, true)
		succeedCordonOwnerRequest(r, req)
		expectCordonReleased(nodeName)
	})

	It("releases an externally applied cordon after new-node validation succeeds", func() {
		r, req, nodeName := newCordonOwnerRequest("cordon-new-node", createdByLifecycleManager, false)
		succeedCordonOwnerRequest(r, req)
		node := getNode(ctx, nodeName)
		Expect(node.Spec.Unschedulable).To(BeFalse())
		Expect(node.Spec.Taints).NotTo(ContainElement(HaveField("Key", Equal(cordonOwnerGateTaint.Key))))
	})

	It("keeps a cordon without the cordon-by label when the post-remediation request is deleted", func() {
		r, req, nodeName := newCordonOwnerRequest("cordon-foreign-delete", createdByFaultQuarantine, false)
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, maxReconcileIterations)
		Expect(k8sClient.Delete(ctx, &vr)).To(Succeed())
		reconcileForIterations(ctx, r, req, 1)
		expectCordonKeptTaintReleased(nodeName)
	})

	It("preserves an explicitly pre-existing cordon despite a historical NVSentinel label", func() {
		r, req, nodeName := newCordonOwnerRequest("cordon-explicit-preserve", createdByFaultQuarantine, true, "true")
		succeedCordonOwnerRequest(r, req)
		expectCordonKeptTaintReleased(nodeName)
	})

	It("releases an explicitly acquired cordon without an audit label", func() {
		r, req, nodeName := newCordonOwnerRequest("cordon-explicit-release", createdByFaultQuarantine, false, "false")
		succeedCordonOwnerRequest(r, req)
		expectCordonReleased(nodeName)
	})

	for _, first := range []string{"startup", "fault"} {
		It("releases an overlapping startup gate when "+first+" validation finishes first", func() {
			rNew, reqNew, nodeName := newCordonOwnerRequest("cordon-overlap-"+first, createdByLifecycleManager, false)
			rFault, reqFault := newOverlappingFaultRequest("cordon-fault-"+first, nodeName)
			rFirst, reqFirst, rLast, reqLast := rNew, reqNew, rFault, reqFault
			if first == "fault" {
				rFirst, reqFirst, rLast, reqLast = rFault, reqFault, rNew, reqNew
			}
			reconcileForIterations(ctx, rFirst, reqFirst, 2)
			reconcileForIterations(ctx, rLast, reqLast, 2)
			entries, err := parseSessionAnnotation(getNode(ctx, nodeName).Annotations[annotationValidationSession])
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			succeedCordonOwnerRequest(rFirst, reqFirst)
			Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeTrue(), "the other validation still holds the gate")
			succeedCordonOwnerRequest(rLast, reqLast)
			expectCordonReleased(nodeName)
		})
	}

	It("keeps startup release permission while cordon removal is disabled", func() {
		rNew, reqNew, nodeName := newCordonOwnerRequest("cordon-disabled", createdByLifecycleManager, false)
		rNew.Config.Validation.Spec.SchedulingGate.Cordon.Remove = false
		rFault, reqFault := newOverlappingFaultRequest("cordon-disabled-successor", nodeName)
		reconcileForIterations(ctx, rNew, reqNew, 2)
		reconcileForIterations(ctx, rFault, reqFault, 2)
		succeedCordonOwnerRequest(rNew, reqNew)
		Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeTrue())
		succeedCordonOwnerRequest(rFault, reqFault)
		expectCordonReleased(nodeName)
	})

	for _, outcome := range []string{"deleted", "failed", "legacy-failed"} {
		It("retains startup release permission when its request is "+outcome, func() {
			rNew, reqNew, nodeName := newCordonOwnerRequest("cordon-startup-"+outcome, createdByLifecycleManager, false)
			rFault, reqFault := newOverlappingFaultRequest("cordon-successor-"+outcome, nodeName)
			startup := reconcileUntilPhase(ctx, rNew, reqNew, v1alpha1.PhaseRunning, maxReconcileIterations)
			reconcileForIterations(ctx, rFault, reqFault, 2)
			if outcome == "deleted" {
				Expect(k8sClient.Delete(ctx, &startup)).To(Succeed())
				reconcileForIterations(ctx, rNew, reqNew, 1)
			} else {
				Expect(updateObjectStatusForAllTestGroups(ctx, &startup, testFailedCondType)).To(Succeed())
				reconcileUntilPhase(ctx, rNew, reqNew, v1alpha1.PhaseFailed, maxReconcileIterations)
			}
			if outcome == "legacy-failed" {
				node := getNode(ctx, nodeName)
				entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
				Expect(err).NotTo(HaveOccurred())
				for i := range entries {
					if entries[i].Name == reqNew.Name {
						entries[i].CordonReleaseAllowed = nil
					}
				}
				node.Annotations[annotationValidationSession], err = marshalSessionAnnotation(entries)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Update(ctx, &node)).To(Succeed())
			}
			Expect(getNode(ctx, nodeName).Spec.Unschedulable).To(BeTrue(), "fault validation still holds the gate")
			succeedCordonOwnerRequest(rFault, reqFault)
			expectCordonReleased(nodeName)
			Expect(getNode(ctx, nodeName).Annotations).NotTo(HaveKey(annotationValidationSession))
		})
	}
})

func TestSchedulingGateForRelease(t *testing.T) {
	g := NewWithT(t)
	gate := &v1alpha1.SchedulingGateConfig{
		Cordon: v1alpha1.CordonConfig{Remove: true, LabelPrefix: cordonOwnerLabelPrefix},
		Taints: []v1alpha1.TaintConfig{{Key: cordonOwnerGateTaint.Key, Effect: cordonOwnerGateTaint.Effect, Remove: true}},
	}
	cfg := &config.Config{Validation: &v1alpha1.ValidationConfiguration{}}
	cfg.Validation.Spec.SchedulingGate = gate
	r := &ValidationRequestReconciler{Config: cfg}

	request := func(createdBy string) *v1alpha1.ValidationRequest {
		return &v1alpha1.ValidationRequest{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{labelCreatedBy: createdBy}}}
	}
	node := func(cordoned bool, labels map[string]string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.NodeSpec{Unschedulable: cordoned}}
	}

	kept := r.schedulingGateForRelease(request(createdByFaultQuarantine), node(true, nil))
	g.Expect(kept.Cordon.Remove).To(BeFalse())
	g.Expect(kept.Cordon.LabelPrefix).To(Equal(cordonOwnerLabelPrefix))
	g.Expect(kept.Taints).To(Equal(gate.Taints), "configured taints are still released")
	g.Expect(gate.Cordon.Remove).To(BeTrue(), "the configured gate is not modified")

	owned := node(true, map[string]string{cordonOwnerLabelPrefix + "cordon-by": "NVSentinel"})
	g.Expect(r.schedulingGateForRelease(request(createdByFaultQuarantine), owned)).To(BeIdenticalTo(gate))
	g.Expect(r.schedulingGateForRelease(request(createdByLifecycleManager), node(true, nil))).To(BeIdenticalTo(gate))
	g.Expect(r.schedulingGateForRelease(&v1alpha1.ValidationRequest{}, node(true, nil))).To(BeIdenticalTo(gate))
	g.Expect(r.schedulingGateForRelease(request(createdByFaultQuarantine), node(false, nil))).To(BeIdenticalTo(gate))

	for _, tc := range []struct {
		createdBy   string
		preExisting string
		labelOwner  string
		release     bool
	}{
		{createdByFaultQuarantine, "true", "NVSentinel", false},
		{createdByFaultQuarantine, "true", "external-controller", false},
		{createdByFaultQuarantine, "false", "", true},
		{createdByFaultQuarantine, "false", "external-controller", true},
		{createdByFaultQuarantine, "invalid", "NVSentinel", false},
		{"", "true", "NVSentinel", false},
		{"", "false", "", true},
		{"custom-template", "true", "NVSentinel", false},
		{"custom-template", "false", "", true},
	} {
		req := request(tc.createdBy)
		req.Annotations = map[string]string{annotationCordonPreExisting: tc.preExisting}
		n := node(true, map[string]string{cordonOwnerLabelPrefix + "cordon-by": tc.labelOwner})
		g.Expect(r.schedulingGateForRelease(req, n).Cordon.Remove).To(Equal(tc.release))
	}

	noPrefix := *gate
	noPrefix.Cordon.LabelPrefix = ""
	cfg.Validation.Spec.SchedulingGate = &noPrefix
	g.Expect(r.schedulingGateForRelease(request(createdByFaultQuarantine), node(true, nil))).To(BeIdenticalTo(&noPrefix),
		"without a label prefix the owner cannot be read and the configured gate is released")
	explicit := request(createdByFaultQuarantine)
	explicit.Annotations = map[string]string{annotationCordonPreExisting: "true"}
	g.Expect(r.schedulingGateForRelease(explicit, node(true, nil)).Cordon.Remove).To(BeFalse(),
		"an explicit preservation decision does not depend on audit labels")

	cfg.Validation.Spec.SchedulingGate = nil
	g.Expect(r.schedulingGateForRelease(request(createdByFaultQuarantine), node(true, nil))).To(BeNil())
}
