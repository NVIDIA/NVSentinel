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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/model"
)

// Mock CSP client for testing
type mockCSPClient struct {
	sendRebootSignalCalled int
	sendRebootSignalError  error
	sendRebootSignalResult model.ResetSignalRequestRef
	isNodeReadyResult      bool
	isNodeReadyError       error
}

func (m *mockCSPClient) SendRebootSignal(ctx context.Context, node corev1.Node) (model.ResetSignalRequestRef, error) {
	m.sendRebootSignalCalled++
	return m.sendRebootSignalResult, m.sendRebootSignalError
}

func (m *mockCSPClient) IsNodeReady(ctx context.Context, node corev1.Node, reqRef string) (bool, error) {
	return m.isNodeReadyResult, m.isNodeReadyError
}

func (m *mockCSPClient) SendTerminateSignal(ctx context.Context, node corev1.Node) (model.TerminateNodeRequestRef, error) {
	return model.TerminateNodeRequestRef(""), nil
}

func TestRebootNodeReconciler_getRebootTimeout(t *testing.T) {
	tests := []struct {
		name            string
		config          *config.RebootNodeControllerConfig
		expectedTimeout time.Duration
	}{
		{
			name:            "no config - uses fallback default",
			config:          nil,
			expectedTimeout: 30 * time.Minute,
		},
		{
			name: "uses specific rebootNodeController timeout when available",
			config: &config.RebootNodeControllerConfig{
				Timeout: 20 * time.Minute,
			},
			expectedTimeout: 20 * time.Minute,
		},
		{
			name: "falls back to default when rebootNodeController timeout is zero",
			config: &config.RebootNodeControllerConfig{
				Timeout: 0, // zero means not set
			},
			expectedTimeout: 30 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &RebootNodeReconciler{
				Config: tt.config,
			}

			timeout := r.getRebootTimeout()
			if timeout != tt.expectedTimeout {
				t.Errorf("getRebootTimeout() = %v, want %v", timeout, tt.expectedTimeout)
			}
		})
	}
}

var _ = Describe("RebootNode Controller", func() {
	var (
		ctx            context.Context
		reconciler     *RebootNodeReconciler
		mockCSP        *mockCSPClient
		k8sClient      client.Client
		scheme         *runtime.Scheme
		testNode       *corev1.Node
		testRebootNode *janitordgxcnvidiacomv1alpha1.RebootNode
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Create scheme and add types
		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(janitordgxcnvidiacomv1alpha1.AddToScheme(scheme)).To(Succeed())

		// Create test node
		testNode = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-node",
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

		// Create test RebootNode
		testRebootNode = &janitordgxcnvidiacomv1alpha1.RebootNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-rebootnode",
			},
			Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
				NodeName: "test-node",
				Force:    false,
			},
		}

		// Create fake client with test objects
		k8sClient = fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(testNode, testRebootNode).
			WithStatusSubresource(&janitordgxcnvidiacomv1alpha1.RebootNode{}).
			Build()

		// Create mock CSP client
		mockCSP = &mockCSPClient{
			sendRebootSignalResult: model.ResetSignalRequestRef("test-request-ref"),
		}

		// Create reconciler
		reconciler = &RebootNodeReconciler{
			Client:    k8sClient,
			Scheme:    scheme,
			CSPClient: mockCSP,
			Config: &config.RebootNodeControllerConfig{
				Timeout: 30 * time.Minute,
			},
		}
	})

	AfterEach(func() {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: testRebootNode.Name}, testRebootNode)
		Expect(err).NotTo(HaveOccurred())
		// Ensure that the RebootNode conditions are valid
		checkStatusConditions(testRebootNode.Status.Conditions)
	})

	Context("when RebootNode is first created", func() {
		It("should initialize conditions and send reboot signal exactly once", func() {
			// First reconciliation
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: testRebootNode.Name,
				},
			}

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			// Verify reboot signal was sent exactly once
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(1))

			// Get updated RebootNode
			var updatedRebootNode janitordgxcnvidiacomv1alpha1.RebootNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testRebootNode.Name}, &updatedRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify conditions are properly set
			Expect(updatedRebootNode.Status.Conditions).To(HaveLen(2))

			// Check SignalSent condition
			signalSentCondition := findCondition(updatedRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent)
			Expect(signalSentCondition).NotTo(BeNil())
			Expect(signalSentCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(signalSentCondition.Reason).To(Equal("Succeeded"))
			Expect(signalSentCondition.Message).To(Equal("test-request-ref"))

			// Check NodeReady condition (should be Unknown initially)
			nodeReadyCondition := findCondition(updatedRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady)
			Expect(nodeReadyCondition).NotTo(BeNil())
			Expect(nodeReadyCondition.Status).To(Equal(metav1.ConditionUnknown))

			// Verify StartTime is set
			Expect(updatedRebootNode.Status.StartTime).NotTo(BeNil())

			// Verify IsRebootInProgress returns true
			Expect(updatedRebootNode.IsRebootInProgress()).To(BeTrue())
		})

		It("should NOT send multiple reboot signals on subsequent reconciliations", func() {
			// First reconciliation - should send reboot signal
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: testRebootNode.Name,
				},
			}

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(1))

			// Second reconciliation - should NOT send another reboot signal
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(1)) // Still 1, not 2!

			// Third reconciliation - should STILL NOT send another reboot signal
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(1)) // Still 1, not 3!
		})
	})

	Context("when reboot is in progress", func() {
		BeforeEach(func() {
			// Set up RebootNode as if reboot signal was already sent
			testRebootNode.Status.StartTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
			testRebootNode.Status.Conditions = []metav1.Condition{
				{
					Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent,
					Status:             metav1.ConditionTrue,
					Reason:             "Succeeded",
					Message:            "test-request-ref",
					LastTransitionTime: metav1.Now(),
				},
				{
					Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady,
					Status:             metav1.ConditionUnknown,
					Reason:             "Initializing",
					Message:            "Node ready state not yet determined",
					LastTransitionTime: metav1.Now(),
				},
			}

			// Update the object in the fake client
			err := k8sClient.Status().Update(ctx, testRebootNode)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should monitor node status and complete when node is ready", func() {
			// Set mock to return node as ready
			mockCSP.isNodeReadyResult = true

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: testRebootNode.Name,
				},
			}

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0))) // Should not requeue on completion

			// Verify no additional reboot signals were sent
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(0))

			// Get updated RebootNode
			var updatedRebootNode janitordgxcnvidiacomv1alpha1.RebootNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testRebootNode.Name}, &updatedRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify completion
			Expect(updatedRebootNode.Status.CompletionTime).NotTo(BeNil())

			nodeReadyCondition := findCondition(updatedRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady)
			Expect(nodeReadyCondition).NotTo(BeNil())
			Expect(nodeReadyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(nodeReadyCondition.Reason).To(Equal("Succeeded"))
		})

		It("should continue monitoring when node is not ready", func() {
			// Set mock to return node as not ready
			mockCSP.isNodeReadyResult = false

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: testRebootNode.Name,
				},
			}

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second)) // Should requeue for monitoring

			// Verify no additional reboot signals were sent
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(0))

			// Get updated RebootNode
			var updatedRebootNode janitordgxcnvidiacomv1alpha1.RebootNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testRebootNode.Name}, &updatedRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify still in progress
			Expect(updatedRebootNode.Status.CompletionTime).To(BeNil())
			Expect(updatedRebootNode.IsRebootInProgress()).To(BeTrue())
		})

		It("should timeout after configured duration", func() {
			// Set start time to be past the timeout
			pastTime := time.Now().Add(-35 * time.Minute) // Past 30 minute timeout
			testRebootNode.Status.StartTime = &metav1.Time{Time: pastTime}
			err := k8sClient.Status().Update(ctx, testRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Set mock to return node as not ready
			mockCSP.isNodeReadyResult = false

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: testRebootNode.Name,
				},
			}

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0))) // Should not requeue on timeout

			// Get updated RebootNode
			var updatedRebootNode janitordgxcnvidiacomv1alpha1.RebootNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testRebootNode.Name}, &updatedRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify timeout
			Expect(updatedRebootNode.Status.CompletionTime).NotTo(BeNil())

			nodeReadyCondition := findCondition(updatedRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady)
			Expect(nodeReadyCondition).NotTo(BeNil())
			Expect(nodeReadyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(nodeReadyCondition.Reason).To(Equal("Timeout"))
		})
	})

	Context("when node ready check fails", func() {
		BeforeEach(func() {

			// Set up RebootNode as if reboot signal was already sent
			testRebootNode.Status.StartTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
			testRebootNode.Status.Conditions = []metav1.Condition{
				{
					Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent,
					Status:             metav1.ConditionTrue,
					Reason:             "Succeeded",
					Message:            "test-request-ref",
					LastTransitionTime: metav1.Now(),
				},
				{
					Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady,
					Status:             metav1.ConditionUnknown,
					Reason:             "Initializing",
					Message:            "Node ready state not yet determined",
					LastTransitionTime: metav1.Now(),
				},
			}

			// Update the object in the fake client
			err := k8sClient.Status().Update(ctx, testRebootNode)
			Expect(err).NotTo(HaveOccurred())

			mockCSP.isNodeReadyError = errors.New("CSP error")
		})

		It("should set node ready condition to False and not requeue", func() {
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: testRebootNode.Name,
				},
			}

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0))) // Should not requeue on failure

			// Get updated RebootNode
			var updatedRebootNode janitordgxcnvidiacomv1alpha1.RebootNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testRebootNode.Name}, &updatedRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify NodeReady condition is False
			nodeReadyCondition := findCondition(updatedRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady)
			Expect(nodeReadyCondition).NotTo(BeNil())
			Expect(nodeReadyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(nodeReadyCondition.Reason).To(Equal("Failed"))
			Expect(nodeReadyCondition.Message).To(Equal("Node status could not be checked from CSP: CSP error"))

			// Verify IsRebootInProgress returns true
			Expect(updatedRebootNode.IsRebootInProgress()).To(BeTrue())

			// Verify completion
			Expect(updatedRebootNode.Status.CompletionTime).NotTo(BeNil())
		})
	})

	Context("when reboot signal fails", func() {
		BeforeEach(func() {
			mockCSP.sendRebootSignalError = errors.New("CSP error")
		})

		It("should set SignalSent condition to False and not requeue", func() {
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: testRebootNode.Name,
				},
			}

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0))) // Should not requeue on failure

			// Get updated RebootNode
			var updatedRebootNode janitordgxcnvidiacomv1alpha1.RebootNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testRebootNode.Name}, &updatedRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify SignalSent condition is False
			signalSentCondition := findCondition(updatedRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent)
			Expect(signalSentCondition).NotTo(BeNil())
			Expect(signalSentCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(signalSentCondition.Reason).To(Equal("Failed"))
			Expect(signalSentCondition.Message).To(Equal("CSP error"))

			// Verify IsRebootInProgress returns false (since signal failed)
			Expect(updatedRebootNode.IsRebootInProgress()).To(BeFalse())

			// Verify completion
			Expect(updatedRebootNode.Status.CompletionTime).NotTo(BeNil())
		})
	})

	Context("testing race condition prevention", func() {
		It("should properly handle the initialization race condition", func() {
			// This test specifically targets the race condition where:
			// 1. InitializeConditionsIfNeeded sets SignalSent to Unknown
			// 2. IsRebootInProgress should return false (not true)
			// 3. Controller should send reboot signal

			// Create a fresh RebootNode with no status
			freshRebootNode := &janitordgxcnvidiacomv1alpha1.RebootNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "fresh-rebootnode",
				},
				Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
					NodeName: "test-node",
					Force:    false,
				},
			}

			err := k8sClient.Create(ctx, freshRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Test the initialization logic directly
			freshRebootNode.SetInitialConditions()
			freshRebootNode.SetStartTime()

			// After initialization, IsRebootInProgress should return FALSE
			// because SignalSent condition exists but has status Unknown (not True)
			Expect(freshRebootNode.IsRebootInProgress()).To(BeFalse())

			// Verify conditions exist but with Unknown status
			signalSentCondition := findCondition(freshRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent)
			Expect(signalSentCondition).NotTo(BeNil())
			Expect(signalSentCondition.Status).To(Equal(metav1.ConditionUnknown))

			// Now simulate setting SignalSent to True (after successful reboot signal)
			freshRebootNode.SetCondition(metav1.Condition{
				Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent,
				Status:             metav1.ConditionTrue,
				Reason:             "Succeeded",
				Message:            "test-ref",
				LastTransitionTime: metav1.Now(),
			})

			// NOW IsRebootInProgress should return TRUE
			Expect(freshRebootNode.IsRebootInProgress()).To(BeTrue())
		})
	})

	Context("when manual mode is enabled", func() {
		BeforeEach(func() {
			// Enable manual mode in the reconciler config
			reconciler.Config.ManualMode = true
		})

		It("should set ManualMode condition on the first reconciliation", func() {
			// First reconciliation with manual mode enabled
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: testRebootNode.Name,
				},
			}

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			// In manual mode, controller doesn't requeue after setting ManualMode condition
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify reboot signal was NOT sent
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(0))

			// Get updated RebootNode
			var updatedRebootNode janitordgxcnvidiacomv1alpha1.RebootNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testRebootNode.Name}, &updatedRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify ManualMode condition is set correctly
			manualModeCondition := findCondition(updatedRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.ManualModeConditionType)
			Expect(manualModeCondition).NotTo(BeNil())
			Expect(manualModeCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(manualModeCondition.Reason).To(Equal("OutsideActorRequired"))
			Expect(manualModeCondition.Message).To(Equal("Janitor is in manual mode, outside actor required to send reboot signal"))

			// Verify SignalSent condition is still Unknown (not True)
			signalSentCondition := findCondition(updatedRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent)
			Expect(signalSentCondition).NotTo(BeNil())
			Expect(signalSentCondition.Status).To(Equal(metav1.ConditionUnknown))

			// Verify StartTime is set
			Expect(updatedRebootNode.Status.StartTime).NotTo(BeNil())
		})

		It("should not ever send reboot signal", func() {
			// Multiple reconciliations should never trigger reboot signal in manual mode
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: testRebootNode.Name,
				},
			}

			// First reconciliation
			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(0))

			// Second reconciliation
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(0))

			// Third reconciliation
			_, err = reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(0))

			// Verify ManualMode condition remains set
			var updatedRebootNode janitordgxcnvidiacomv1alpha1.RebootNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testRebootNode.Name}, &updatedRebootNode)
			Expect(err).NotTo(HaveOccurred())

			manualModeCondition := findCondition(updatedRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.ManualModeConditionType)
			Expect(manualModeCondition).NotTo(BeNil())
			Expect(manualModeCondition.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should continue monitoring the node if an outside actor sends a reboot signal", func() {
			// First reconciliation - sets up manual mode
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name: testRebootNode.Name,
				},
			}

			_, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(0))

			// Get the current RebootNode to simulate outside actor setting SignalSent condition
			var currentRebootNode janitordgxcnvidiacomv1alpha1.RebootNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testRebootNode.Name}, &currentRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Simulate outside actor sending reboot signal by setting SignalSent condition to True
			currentRebootNode.SetCondition(metav1.Condition{
				Type:               janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent,
				Status:             metav1.ConditionTrue,
				Reason:             "OutsideActor",
				Message:            "external-request-ref",
				LastTransitionTime: metav1.Now(),
			})

			// Update the status to reflect outside actor's action
			err = k8sClient.Status().Update(ctx, &currentRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify IsRebootInProgress now returns true (since SignalSent is True)
			Expect(currentRebootNode.IsRebootInProgress()).To(BeTrue())

			// Configure mock to simulate node becoming ready after reboot
			mockCSP.isNodeReadyResult = true
			mockCSP.isNodeReadyError = nil

			// Next reconciliation should complete the reboot since node is ready
			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			// In manual mode, when both CSP (always true) and Kubernetes report ready, reboot completes
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify janitor still did not send any reboot signals
			Expect(mockCSP.sendRebootSignalCalled).To(Equal(0))

			// Get final state
			var finalRebootNode janitordgxcnvidiacomv1alpha1.RebootNode
			err = k8sClient.Get(ctx, types.NamespacedName{Name: testRebootNode.Name}, &finalRebootNode)
			Expect(err).NotTo(HaveOccurred())

			// Verify ManualMode condition still exists
			manualModeCondition := findCondition(finalRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.ManualModeConditionType)
			Expect(manualModeCondition).NotTo(BeNil())
			Expect(manualModeCondition.Status).To(Equal(metav1.ConditionTrue))

			// Verify SignalSent condition remains True (from outside actor)
			signalSentCondition := findCondition(finalRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.RebootNodeConditionSignalSent)
			Expect(signalSentCondition).NotTo(BeNil())
			Expect(signalSentCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(signalSentCondition.Message).To(Equal("external-request-ref"))

			// In manual mode, when IsNodeReady check is performed, it should assume ready=true
			// So NodeReady condition should be set to True and reboot should complete
			nodeReadyCondition := findCondition(finalRebootNode.Status.Conditions, janitordgxcnvidiacomv1alpha1.RebootNodeConditionNodeReady)
			Expect(nodeReadyCondition).NotTo(BeNil())
			Expect(nodeReadyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(nodeReadyCondition.Reason).To(Equal("Succeeded"))

			// Verify reboot completed successfully (CompletionTime should be set)
			Expect(finalRebootNode.Status.CompletionTime).NotTo(BeNil())
		})
	})
})

// Helper function to find a condition by type
func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// Test conditionsChanged helper function
func TestConditionsChanged(t *testing.T) {
	tests := []struct {
		name     string
		original []metav1.Condition
		updated  []metav1.Condition
		want     bool
	}{
		{
			name:     "both empty",
			original: []metav1.Condition{},
			updated:  []metav1.Condition{},
			want:     false,
		},
		{
			name:     "different lengths",
			original: []metav1.Condition{},
			updated: []metav1.Condition{
				{Type: "Test", Status: metav1.ConditionTrue},
			},
			want: true,
		},
		{
			name: "same conditions",
			original: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent"},
			},
			updated: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent"},
			},
			want: false,
		},
		{
			name: "status changed",
			original: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionFalse, Reason: "Pending", Message: "Waiting"},
			},
			updated: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent"},
			},
			want: true,
		},
		{
			name: "reason changed",
			original: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent"},
			},
			updated: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Retry", Message: "Signal sent"},
			},
			want: true,
		},
		{
			name: "message changed",
			original: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent"},
			},
			updated: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent successfully"},
			},
			want: true,
		},
		{
			name: "new condition type added",
			original: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent"},
			},
			updated: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent"},
				{Type: "NodeReady", Status: metav1.ConditionTrue, Reason: "Ready", Message: "Node is ready"},
			},
			want: true,
		},
		{
			name: "multiple conditions unchanged",
			original: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent"},
				{Type: "NodeReady", Status: metav1.ConditionFalse, Reason: "NotReady", Message: "Node not ready"},
			},
			updated: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent"},
				{Type: "NodeReady", Status: metav1.ConditionFalse, Reason: "NotReady", Message: "Node not ready"},
			},
			want: false,
		},
		{
			name: "one of multiple conditions changed",
			original: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent"},
				{Type: "NodeReady", Status: metav1.ConditionFalse, Reason: "NotReady", Message: "Node not ready"},
			},
			updated: []metav1.Condition{
				{Type: "SignalSent", Status: metav1.ConditionTrue, Reason: "Success", Message: "Signal sent"},
				{Type: "NodeReady", Status: metav1.ConditionTrue, Reason: "Ready", Message: "Node is ready"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := conditionsChanged(tt.original, tt.updated)
			if got != tt.want {
				t.Errorf("conditionsChanged() = %v, want %v", got, tt.want)
			}
		})
	}
}
