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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

var _ = Describe("ValidationRequest provider cleanup", func() {
	DescribeTable("persists completed attempts before cleanup and retries cleanup failures",
		func(terminalRequest, statusConflict bool) {
			suffix := fmt.Sprintf("%d", time.Now().UnixNano())
			vrName, nodeName := "vr-cleanup-"+suffix, "node-cleanup-"+suffix
			testConfig := defaultTestConfig()
			if !terminalRequest {
				testConfig = twoTestConfig(false, false)
			}

			injectFailure := false
			intercepted := interceptor.NewClient(k8sClient.(client.WithWatch), interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string,
					obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if _, ok := obj.(*v1alpha1.ValidationRequest); ok && injectFailure && statusConflict {
						injectFailure = false
						return apierrors.NewConflict(schema.GroupResource{
							Group: v1alpha1.GroupVersion.Group, Resource: "validationrequests",
						}, obj.GetName(), fmt.Errorf("injected status conflict"))
					}

					return c.SubResource(subresource).Update(ctx, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if injectFailure && !statusConflict {
						injectFailure = false
						return fmt.Errorf("injected provider deletion failure")
					}

					return c.Delete(ctx, obj, opts...)
				},
			})
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config: testConfig, client: intercepted, nodeNames: []string{nodeName},
				spec: v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})
			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, maxReconcileIterations)
			objectName := vr.Status.TestGroups[0].Attempts[0].ObjectName
			Expect(updateObjectStatus(ctx, objectName, testSuccessCondType)).To(Succeed())

			injectFailure = true
			_, err := r.Reconcile(ctx, req)
			Expect(err).To(HaveOccurred())
			Expect(injectFailure).To(BeFalse())

			provider := &unstructured.Unstructured{}
			provider.SetGroupVersionKind(testJobGVK)
			key := types.NamespacedName{Name: objectName, Namespace: testNamespace}
			Expect(k8sClient.Get(ctx, key, provider)).To(Succeed(), "retain the result until status is durable")
			Expect(k8sClient.Get(ctx, req.NamespacedName, &vr)).To(Succeed())
			if statusConflict {
				Expect(vr.Status.TestGroups[0].Attempts[0].Phase).To(Equal(v1alpha1.PhaseRunning))
			} else {
				Expect(vr.Status.TestGroups[0].Attempts[0].Phase).To(Equal(v1alpha1.PhaseSucceeded))
			}

			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, req.NamespacedName, &vr)).To(Succeed())
			Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(1))
			Expect(vr.Status.TestGroups[0].Attempts[0].Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.TestGroups[0].Attempts[0].FailureReason).To(BeEmpty())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, provider))).To(BeTrue())
		},
		Entry("running request with status conflict", false, true),
		Entry("terminal request with status conflict", true, true),
		Entry("running request with deletion failure", false, false),
		Entry("terminal request with deletion failure", true, false),
	)

	It("does not fail a terminal request whose provider is no longer configured", func() {
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		vrName, nodeName := "vr-cleanup-removed-"+suffix, "node-cleanup-removed-"+suffix
		r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
			config: defaultTestConfig(), nodeNames: []string{nodeName},
			spec: v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
		})
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, maxReconcileIterations)
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		vr = reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, maxReconcileIterations)

		delete(r.Config.Validation.Spec.Providers, vr.Status.TestGroups[0].Provider)

		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
	})

	It("keeps the node claim until the provider delete of a timed-out attempt is accepted", func() {
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		vrName, nodeName := "vr-cleanup-claim-"+suffix, "node-cleanup-claim-"+suffix
		testConfig := defaultTestConfig()
		provider := testConfig.Validation.Spec.Providers["test-provider"]
		provider.TimeoutSeconds = 0
		testConfig.Validation.Spec.Providers["test-provider"] = provider

		failDelete := false
		intercepted := interceptor.NewClient(k8sClient.(client.WithWatch), interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if failDelete {
					return fmt.Errorf("injected provider deletion failure")
				}

				return c.Delete(ctx, obj, opts...)
			},
		})
		r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
			config: testConfig, client: intercepted, nodeNames: []string{nodeName},
			spec: v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
		})
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, maxReconcileIterations)
		objectName := vr.Status.TestGroups[0].Attempts[0].ObjectName

		failDelete = true
		_, err := r.Reconcile(ctx, req)
		Expect(err).To(HaveOccurred())

		Expect(k8sClient.Get(ctx, req.NamespacedName, &vr)).To(Succeed())
		Expect(vr.Status.TestGroups[0].Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonTestTimeout))
		Expect(getNode(ctx, nodeName).Annotations[annotationActiveValidationRequest]).To(Equal(vrName),
			"the node stays claimed while the provider object still exists")

		failDelete = false
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, req.NamespacedName, &vr)).To(Succeed())
		Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
		Expect(getNode(ctx, nodeName).Annotations[annotationActiveValidationRequest]).To(BeEmpty())

		stale := &unstructured.Unstructured{}
		stale.SetGroupVersionKind(testJobGVK)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
			Name: objectName, Namespace: testNamespace}, stale))).To(BeTrue())
	})

	It("retries cleanup of persisted attempts when another group fails to reconcile", func() {
		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		vrName := "vr-cleanup-indep-" + suffix
		nodeNames := []string{"node-cleanup-indep-a-" + suffix, "node-cleanup-indep-b-" + suffix}

		failDelete, failReadOf := false, ""
		intercepted := interceptor.NewClient(k8sClient.(client.WithWatch), interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if failDelete {
					return fmt.Errorf("injected provider deletion failure")
				}

				return c.Delete(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
				opts ...client.GetOption) error {
				if _, ok := obj.(*unstructured.Unstructured); ok && failReadOf != "" && key.Name == failReadOf {
					return fmt.Errorf("injected provider read failure")
				}

				return c.Get(ctx, key, obj, opts...)
			},
		})
		r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
			config: twoTestConfig(false, false), client: intercepted, nodeNames: nodeNames,
			spec: v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{
				{Name: nodeNames[0]}, {Name: nodeNames[1]}}},
		})
		vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, maxReconcileIterations)

		var running []*v1alpha1.TestGroupStatus

		for i := range vr.Status.TestGroups {
			if vr.Status.TestGroups[i].Phase == v1alpha1.PhaseRunning {
				running = append(running, &vr.Status.TestGroups[i])
			}
		}

		Expect(running).To(HaveLen(2))

		objectA := running[0].Attempts[0].ObjectName
		objectB := running[1].Attempts[0].ObjectName
		Expect(updateObjectStatus(ctx, objectA, testSuccessCondType)).To(Succeed())

		failDelete = true
		_, err := r.Reconcile(ctx, req)
		Expect(err).To(HaveOccurred())
		Expect(k8sClient.Get(ctx, req.NamespacedName, &vr)).To(Succeed())
		Expect(attemptPhaseOf(&vr, objectA)).To(Equal(v1alpha1.PhaseSucceeded))

		failDelete, failReadOf = false, objectB
		_, err = r.Reconcile(ctx, req)
		Expect(err).To(HaveOccurred())

		providerA := &unstructured.Unstructured{}
		providerA.SetGroupVersionKind(testJobGVK)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{
			Name: objectA, Namespace: testNamespace}, providerA))).To(BeTrue(),
			"the persisted attempt is cleaned up even though the other group could not be read")
	})
})

func attemptPhaseOf(vr *v1alpha1.ValidationRequest, objectName string) v1alpha1.Phase {
	for _, g := range vr.Status.TestGroups {
		for _, a := range g.Attempts {
			if a.ObjectName == objectName {
				return a.Phase
			}
		}
	}

	return ""
}
