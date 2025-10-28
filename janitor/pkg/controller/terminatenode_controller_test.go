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
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"

	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("TerminateNodeReconciler", func() {
	const (
		timeout  = time.Second * 30
		interval = time.Second * 1
	)

	var (
		ctx           context.Context
		mockCSPClient *MockCSPClient
		terminateNode *janitordgxcnvidiacomv1alpha1.TerminateNode
		node          *corev1.Node
		reconciler    *TerminateNodeReconciler
		nodeName      string
		crName        string
		uniqueSuffix  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		mockCSPClient = &MockCSPClient{}

		// Generate unique suffix using GinkgoRandomSeed to avoid conflicts
		uniqueSuffix = fmt.Sprintf("%d", time.Now().UnixNano())
		nodeName = "test-node-" + uniqueSuffix
		crName = "test-terminate-node-" + uniqueSuffix

		// Create a test node
		node = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{
						Type:   corev1.NodeReady,
						Status: corev1.ConditionTrue,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, node)).Should(Succeed())

		// Create a test TerminateNode resource
		terminateNode = &janitordgxcnvidiacomv1alpha1.TerminateNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: crName,
			},
			Spec: janitordgxcnvidiacomv1alpha1.TerminateNodeSpec{
				NodeName: nodeName,
				Force:    false,
			},
		}
		Expect(k8sClient.Create(ctx, terminateNode)).Should(Succeed())

		// Create the reconciler with the mock CSP client
		reconciler = &TerminateNodeReconciler{
			Client: k8sClient,
			Scheme: scheme.Scheme,
			Config: &config.TerminateNodeControllerConfig{
				ManualMode: false,
			},
			CSPClient: mockCSPClient,
		}
	})

	AfterEach(func() {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: crName}, terminateNode)
		Expect(err).NotTo(HaveOccurred())
		// Ensure that the TerminateNode conditions are valid
		checkStatusConditions(terminateNode.Status.Conditions)
	})

	Context("When creating a new TerminateNode resource", func() {
		It("Should set the start time", func() {
			// Trigger initial reconciliation to set start time
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				if err != nil {
					return false
				}
				return updatedTerminateNode.Status.StartTime != nil
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("When sending terminate signal", func() {
		It("Should successfully send terminate signal and update condition", func() {
			// Trigger initial reconciliation to set start time
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				if err != nil {
					return false
				}
				return updatedTerminateNode.Status.StartTime != nil
			}, timeout, interval).Should(BeTrue())

			// Trigger reconciliation again to send terminate signal
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify CSP client was called
			Expect(mockCSPClient.terminateSignalSent).To(BeTrue())

			// Verify condition was updated
			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				if err != nil {
					return false
				}
				for _, condition := range updatedTerminateNode.Status.Conditions {
					if condition.Type == "SignalSent" && condition.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())
		})

		It("Should fail to send terminate signal and update condition", func() {
			mockCSPClient.terminateError = errors.New("CSP error")
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify CSP client was called
			Expect(mockCSPClient.terminateSignalSent).To(BeTrue())

			// Verify condition was updated
			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				if err != nil {
					return false
				}
				if updatedTerminateNode.Status.CompletionTime == nil {
					return false
				}
				for _, condition := range updatedTerminateNode.Status.Conditions {
					if condition.Type == "SignalSent" && condition.Status == metav1.ConditionFalse {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

		})
	})

	Context("When node is not ready", func() {
		It("Should delete the node", func() {
			// Trigger initial reconciliation to set start time
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				if err != nil {
					return false
				}
				return updatedTerminateNode.Status.StartTime != nil
			}, timeout, interval).Should(BeTrue())

			// Trigger reconciliation to send terminate signal
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				if err != nil {
					return false
				}
				for _, condition := range updatedTerminateNode.Status.Conditions {
					if condition.Type == "SignalSent" && condition.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Update node to not ready using Status() subresource
			node.Status.Conditions[0].Status = corev1.ConditionFalse
			Expect(k8sClient.Status().Update(ctx, node)).Should(Succeed())

			// Trigger reconciliation to delete node
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify node was deleted
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, node)
				return apierrors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())

			// Trigger reconciliation again to mark as complete
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify completion time is set and condition is updated
			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				if err != nil {
					return false
				}
				if updatedTerminateNode.Status.CompletionTime == nil {
					return false
				}
				for _, condition := range updatedTerminateNode.Status.Conditions {
					if condition.Type == janitordgxcnvidiacomv1alpha1.TerminateNodeConditionNodeTerminated && condition.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("When node is already deleted", func() {
		It("Should mark termination as complete", func() {
			// Trigger initial reconciliation to set start time
			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Wait for start time to be set
			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				if err != nil {
					return false
				}
				return updatedTerminateNode.Status.StartTime != nil
			}, timeout, interval).Should(BeTrue())

			// Trigger reconciliation to send terminate signal
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Wait for SignalSent condition
			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				if err != nil {
					return false
				}
				for _, condition := range updatedTerminateNode.Status.Conditions {
					if condition.Type == "SignalSent" && condition.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			// Delete the node
			Expect(k8sClient.Delete(ctx, node)).Should(Succeed())

			// Trigger reconciliation to mark as complete
			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify completion time is set and condition is updated
			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				if err != nil {
					return false
				}
				if updatedTerminateNode.Status.CompletionTime == nil {
					return false
				}
				for _, condition := range updatedTerminateNode.Status.Conditions {
					if condition.Type == janitordgxcnvidiacomv1alpha1.TerminateNodeConditionNodeTerminated && condition.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("When termination times out", func() {
		It("Should update the status to reflect the timeout", func() {
			// Set a short timeout for this specific test
			reconciler.Config.Timeout = time.Second * 1

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				return err == nil && updatedTerminateNode.Status.StartTime != nil
			}, timeout, interval).Should(BeTrue())

			time.Sleep(reconciler.Config.Timeout + time.Second*1)

			_, err = reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify that completion time is set and the condition is updated to reflect the timeout
			Eventually(func() bool {
				var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name: crName,
				}, &updatedTerminateNode)
				if err != nil {
					return false
				}
				if updatedTerminateNode.Status.CompletionTime == nil {
					return false
				}
				for _, condition := range updatedTerminateNode.Status.Conditions {
					if condition.Type == janitordgxcnvidiacomv1alpha1.TerminateNodeConditionNodeTerminated &&
						condition.Status == metav1.ConditionFalse &&
						condition.Reason == "Timeout" {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("when manual mode is enabled", func() {
		BeforeEach(func() {
			// Enable manual mode in the reconciler config
			reconciler.Config.ManualMode = true
		})

		It("should set ManualMode condition on the first reconciliation", func() {
			// First reconciliation with manual mode enabled
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			}

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			// In manual mode, controller doesn't requeue after setting ManualMode condition
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify terminate signal was NOT sent
			Expect(mockCSPClient.terminateSignalSent).To(BeFalse())

			// Get updated TerminateNode
			var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: crName}, &updatedTerminateNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify ManualMode condition is set correctly
			manualModeCondition := findTerminateCondition(updatedTerminateNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.ManualModeConditionType)
			Expect(manualModeCondition).NotTo(BeNil())
			Expect(manualModeCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(manualModeCondition.Reason).To(Equal("OutsideActorRequired"))
			Expect(manualModeCondition.Message).To(Equal("Janitor is in manual mode, outside actor required to send terminate signal"))

			// Verify SignalSent condition is still Unknown (not True)
			signalSentCondition := findTerminateCondition(updatedTerminateNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.TerminateNodeConditionSignalSent)
			Expect(signalSentCondition).NotTo(BeNil())
			Expect(signalSentCondition.Status).To(Equal(metav1.ConditionUnknown))

			// Verify StartTime is set
			Expect(updatedTerminateNode.Status.StartTime).NotTo(BeNil())
		})

		It("should not ever send terminate signal", func() {
			// Multiple reconciliations should never trigger terminate signal in manual mode
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			}

			// First reconciliation
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockCSPClient.terminateSignalSent).To(BeFalse())

			// Second reconciliation
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockCSPClient.terminateSignalSent).To(BeFalse())

			// Third reconciliation
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockCSPClient.terminateSignalSent).To(BeFalse())

			// Verify ManualMode condition remains set
			var updatedTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: crName}, &updatedTerminateNode)
			Expect(err).NotTo(HaveOccurred())

			manualModeCondition := findTerminateCondition(updatedTerminateNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.ManualModeConditionType)
			Expect(manualModeCondition).NotTo(BeNil())
			Expect(manualModeCondition.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should continue monitoring the node if an outside actor sends a terminate signal", func() {
			// First reconciliation - sets up manual mode
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name: crName,
				},
			}

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockCSPClient.terminateSignalSent).To(BeFalse())

			// Get the current TerminateNode to simulate outside actor setting SignalSent condition
			var currentTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: crName}, &currentTerminateNode)
			Expect(err).NotTo(HaveOccurred())

			// Simulate outside actor sending terminate signal by setting SignalSent condition to True
			currentTerminateNode.SetCondition(metav1.Condition{
				Type:               janitordgxcnvidiacomv1alpha1.TerminateNodeConditionSignalSent,
				Status:             metav1.ConditionTrue,
				Reason:             "OutsideActor",
				Message:            "Terminate signal sent to CSP",
				LastTransitionTime: metav1.Now(),
			})

			// Update the status to reflect outside actor's action
			err = k8sClient.Status().Update(ctx, &currentTerminateNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify IsTerminateInProgress now returns true (since SignalSent is True)
			Expect(currentTerminateNode.IsTerminateInProgress()).To(BeTrue())

			// Simulate node becoming not ready (which typically happens during termination)
			// Update the node to NotReady status
			node.Status.Conditions = []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionFalse,
				},
			}
			err = k8sClient.Status().Update(ctx, node)
			Expect(err).NotTo(HaveOccurred())

			// Next reconciliation should detect not ready node and delete it, completing termination immediately
			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			// Controller deletes the node and completes termination immediately, so no requeue
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify janitor still did not send any terminate signals
			Expect(mockCSPClient.terminateSignalSent).To(BeFalse())

			// Get final state
			var finalTerminateNode janitordgxcnvidiacomv1alpha1.TerminateNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: crName}, &finalTerminateNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify ManualMode condition still exists
			manualModeCondition := findTerminateCondition(finalTerminateNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.ManualModeConditionType)
			Expect(manualModeCondition).NotTo(BeNil())
			Expect(manualModeCondition.Status).To(Equal(metav1.ConditionTrue))

			// Verify SignalSent condition remains True (from outside actor)
			signalSentCondition := findTerminateCondition(finalTerminateNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.TerminateNodeConditionSignalSent)
			Expect(signalSentCondition).NotTo(BeNil())
			Expect(signalSentCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(signalSentCondition.Message).To(Equal("Terminate signal sent to CSP"))

			// Verify NodeTerminated condition is set to True and termination completed
			nodeTerminatedCondition := findTerminateCondition(finalTerminateNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.TerminateNodeConditionNodeTerminated)
			Expect(nodeTerminatedCondition).NotTo(BeNil())
			Expect(nodeTerminatedCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(nodeTerminatedCondition.Reason).To(Equal("Succeeded"))

			// Verify termination completed successfully (CompletionTime should be set)
			Expect(finalTerminateNode.Status.CompletionTime).NotTo(BeNil())
		})
	})

})

// Helper function to find a condition by type for TerminateNode
func findTerminateCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
