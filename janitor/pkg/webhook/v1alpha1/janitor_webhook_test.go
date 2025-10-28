/*
Copyright 2025 NVIDIA.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
)

var _ = Describe("Janitor Webhook", func() {
	var (
		ctx        context.Context
		validator  JanitorCustomValidator
		fakeClient client.Client
		testNode   *corev1.Node
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Create a test node
		testNode = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-node",
			},
			Spec: corev1.NodeSpec{},
		}

		// Create a fake client with the test node
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(janitordgxcnvidiacomv1alpha1.AddToScheme(scheme)).To(Succeed())
		fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(testNode).Build()
	})

	Context("When controller is enabled and node exists", func() {
		BeforeEach(func() {
			validator = JanitorCustomValidator{
				Config: &config.Config{
					Global: config.GlobalConfig{
						Timeout: 30 * time.Minute,
					},
					RebootNode: config.RebootNodeControllerConfig{
						Enabled:    true,
						Timeout:    30 * time.Minute,
						ManualMode: false,
					},
					TerminateNode: config.TerminateNodeControllerConfig{
						Enabled:    true,
						Timeout:    30 * time.Minute,
						ManualMode: false,
					},
				},
				Client: fakeClient,
			}
		})

		It("Should admit RebootNode creation when node exists", func() {
			obj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should admit TerminateNode creation when node exists", func() {
			obj := &janitordgxcnvidiacomv1alpha1.TerminateNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-terminate",
				},
				Spec: janitordgxcnvidiacomv1alpha1.TerminateNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should admit RebootNode updates when node exists", func() {
			oldObj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}
			newObj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    true,
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, newObj)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should admit RebootNode deletions", func() {
			obj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}
			_, err := validator.ValidateDelete(ctx, obj)
			Expect(err).ToNot(HaveOccurred())
		})
	})

	Context("When controller is enabled but node does not exist", func() {
		BeforeEach(func() {
			validator = JanitorCustomValidator{
				Config: &config.Config{
					Global: config.GlobalConfig{
						Timeout: 30 * time.Minute,
					},
					RebootNode: config.RebootNodeControllerConfig{
						Enabled:    true,
						Timeout:    30 * time.Minute,
						ManualMode: false,
					},
					TerminateNode: config.TerminateNodeControllerConfig{
						Enabled:    true,
						Timeout:    30 * time.Minute,
						ManualMode: false,
					},
				},
				Client: fakeClient,
			}
		})

		It("Should reject RebootNode creation when node does not exist", func() {
			obj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "non-existent-node",
					Force:    false,
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("node 'non-existent-node' does not exist in the cluster"))
		})

		It("Should reject TerminateNode creation when node does not exist", func() {
			obj := &janitordgxcnvidiacomv1alpha1.TerminateNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-terminate",
				},
				Spec: janitordgxcnvidiacomv1alpha1.TerminateNodeSpec{
					NodeName: "non-existent-node",
					Force:    false,
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("node 'non-existent-node' does not exist in the cluster"))
		})

		It("Should reject RebootNode updates when node does not exist", func() {
			oldObj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "non-existent-node",
					Force:    false,
				},
			}
			newObj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "non-existent-node",
					Force:    true,
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, newObj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("node 'non-existent-node' does not exist in the cluster"))
		})
	})

	Context("When controller is disabled", func() {
		BeforeEach(func() {
			validator = JanitorCustomValidator{
				Config: &config.Config{
					Global: config.GlobalConfig{
						Timeout: 30 * time.Minute,
					},
					RebootNode: config.RebootNodeControllerConfig{
						Enabled:    false,
						Timeout:    30 * time.Minute,
						ManualMode: false,
					},
					TerminateNode: config.TerminateNodeControllerConfig{
						Enabled:    false,
						Timeout:    30 * time.Minute,
						ManualMode: false,
					},
				},
				Client: fakeClient,
			}
		})

		It("Should reject RebootNode creation when controller disabled", func() {
			obj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("RebootNode controller is disabled"))
		})

		It("Should reject TerminateNode creation when controller disabled", func() {
			obj := &janitordgxcnvidiacomv1alpha1.TerminateNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-terminate",
				},
				Spec: janitordgxcnvidiacomv1alpha1.TerminateNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("TerminateNode controller is disabled"))
		})

		It("Should reject RebootNode updates when controller disabled", func() {
			oldObj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}
			newObj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    true,
				},
			}
			_, err := validator.ValidateUpdate(ctx, oldObj, newObj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("RebootNode controller is disabled"))
		})

		It("Should reject RebootNode deletions when controller disabled", func() {
			obj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}
			_, err := validator.ValidateDelete(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("RebootNode controller is disabled"))
		})
	})

	Context("When configuration is nil", func() {
		BeforeEach(func() {
			validator = JanitorCustomValidator{
				Config: nil,
				Client: fakeClient,
			}
		})

		It("Should reject any CRD creation when config is nil", func() {
			obj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("RebootNode controller is disabled"))
		})
	})

	Context("When client is nil", func() {
		BeforeEach(func() {
			validator = JanitorCustomValidator{
				Config: &config.Config{
					Global: config.GlobalConfig{
						Timeout: 30 * time.Minute,
					},
					RebootNode: config.RebootNodeControllerConfig{
						Enabled:    true,
						Timeout:    30 * time.Minute,
						ManualMode: false,
					},
				},
				Client: nil,
			}
		})

		It("Should reject CRD creation when client is nil", func() {
			obj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}
			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("kubernetes client not available"))
		})
	})

	Context("When invalid object type is provided", func() {
		BeforeEach(func() {
			validator = JanitorCustomValidator{
				Config: &config.Config{
					Global: config.GlobalConfig{
						Timeout: 30 * time.Minute,
					},
				},
				Client: fakeClient,
			}
		})

		It("Should return error for unknown object type", func() {
			// Using a Pod instead of a Janitor CRD type
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
				},
			}
			// The validator should reject non-Janitor objects
			_, err := validator.ValidateCreate(ctx, pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("expected a Janitor CR object but got"))
		})
	})

	Context("When node exclusions are configured", func() {
		It("Should reject RebootNode creation when node matches exclusion label", func() {
			// Create a node with an exclusion label
			excludedNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "excluded-node",
					Labels: map[string]string{
						"node-role.kubernetes.io/control-plane": "",
						"environment":                           "production",
					},
				},
			}

			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			Expect(janitordgxcnvidiacomv1alpha1.AddToScheme(scheme)).To(Succeed())
			clientWithExcludedNode := fake.NewClientBuilder().WithScheme(scheme).WithObjects(excludedNode).Build()

			validator = JanitorCustomValidator{
				Config: &config.Config{
					Global: config.GlobalConfig{
						Timeout: 30 * time.Minute,
						Nodes: config.NodeConfig{
							Exclusions: []metav1.LabelSelector{
								{
									MatchLabels: map[string]string{
										"node-role.kubernetes.io/control-plane": "",
									},
								},
							},
						},
					},
					RebootNode: config.RebootNodeControllerConfig{
						Enabled: true,
						Timeout: 30 * time.Minute,
						NodeExclusions: []metav1.LabelSelector{
							{
								MatchLabels: map[string]string{
									"node-role.kubernetes.io/control-plane": "",
								},
							},
						},
					},
				},
				Client: clientWithExcludedNode,
			}

			obj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot-excluded",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "excluded-node",
					Force:    false,
				},
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("is excluded from janitor"))
			Expect(err.Error()).To(ContainSubstring("global.nodes.exclusions"))
		})

		It("Should admit RebootNode creation when node does not match exclusion labels", func() {
			// Create a node without exclusion labels
			normalNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "normal-node",
					Labels: map[string]string{
						"environment": "production",
					},
				},
			}

			scheme := runtime.NewScheme()
			Expect(corev1.AddToScheme(scheme)).To(Succeed())
			Expect(janitordgxcnvidiacomv1alpha1.AddToScheme(scheme)).To(Succeed())
			clientWithNormalNode := fake.NewClientBuilder().WithScheme(scheme).WithObjects(normalNode).Build()

			validator = JanitorCustomValidator{
				Config: &config.Config{
					Global: config.GlobalConfig{
						Timeout: 30 * time.Minute,
						Nodes: config.NodeConfig{
							Exclusions: []metav1.LabelSelector{
								{
									MatchLabels: map[string]string{
										"node-role.kubernetes.io/control-plane": "",
									},
								},
							},
						},
					},
					RebootNode: config.RebootNodeControllerConfig{
						Enabled: true,
						Timeout: 30 * time.Minute,
						NodeExclusions: []metav1.LabelSelector{
							{
								MatchLabels: map[string]string{
									"node-role.kubernetes.io/control-plane": "",
								},
							},
						},
					},
				},
				Client: clientWithNormalNode,
			}

			obj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot-normal",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "normal-node",
					Force:    false,
				},
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should admit RebootNode creation when no exclusions are configured", func() {
			validator = JanitorCustomValidator{
				Config: &config.Config{
					Global: config.GlobalConfig{
						Timeout: 30 * time.Minute,
						Nodes: config.NodeConfig{
							Exclusions: []metav1.LabelSelector{},
						},
					},
					RebootNode: config.RebootNodeControllerConfig{
						Enabled:        true,
						Timeout:        30 * time.Minute,
						NodeExclusions: []metav1.LabelSelector{},
					},
				},
				Client: fakeClient,
			}

			obj := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-reboot",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}

			_, err := validator.ValidateCreate(ctx, obj)
			Expect(err).ToNot(HaveOccurred())
		})
	})
})
