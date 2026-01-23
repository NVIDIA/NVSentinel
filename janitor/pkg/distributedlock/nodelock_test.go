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

package distributedlock

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	janitordgxcnvidiacomv1alpha1 "github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/metrics"
)

const (
	testNodeUID       = "0f43ccca-2918-4b33-a42a-81916841de1f"
	testNamespace     = "default"
)

type mockReconciler struct {
	client.Client
	nodeLock *nodeLock
	result   ctrl.Result
	err      error
}

func (r *mockReconciler) reconcileHelper(_ context.Context, _ ctrl.Request, _ client.Object) (ctrl.Result, error) {
	return r.result, r.err
}

// Example implementation for a controller's Reconcile function leveraging NodeLock
func (r *mockReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rebootNode janitordgxcnvidiacomv1alpha1.RebootNode
	if err := r.Get(ctx, req.NamespacedName, &rebootNode); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	completedReconciling := rebootNode.Status.CompletionTime != nil
	if !completedReconciling {
		locked := r.nodeLock.LockNode(ctx, &rebootNode, rebootNode.Spec.NodeName)
		if !locked {
			return ctrl.Result{Requeue: true}, nil
		}
		result, err := r.reconcileHelper(ctx, req, &rebootNode)
		if err != nil || result.Requeue || result.RequeueAfter > 0 {
			return result, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	retryUnlock := r.nodeLock.CheckUnlock(ctx, &rebootNode, rebootNode.Spec.NodeName)
	if retryUnlock {
		return ctrl.Result{Requeue: true}, nil
	}
	return ctrl.Result{}, nil
}

var _ = Describe("NodeLock", func() {
	var (
		ctx                          context.Context
		statusSubresource            client.Object
		nodeLock                     *nodeLock
		reconciler                   *mockReconciler
		k8sClient                    client.Client
		scheme                       *runtime.Scheme
		testNode                     *corev1.Node
		testRebootNodeNamespacedName types.NamespacedName
		req                          ctrl.Request
		testLeaseLockNamespacedName  types.NamespacedName
		testRebootNode               *janitordgxcnvidiacomv1alpha1.RebootNode
		testLeaseLock                *coordinationv1.Lease
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Create scheme and add types
		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(coordinationv1.AddToScheme(scheme)).To(Succeed())
		Expect(janitordgxcnvidiacomv1alpha1.AddToScheme(scheme)).To(Succeed())
		statusSubresource = &janitordgxcnvidiacomv1alpha1.RebootNode{}

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
		testRebootNode = &janitordgxcnvidiacomv1alpha1.RebootNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-rebootnode",
				UID:  testNodeUID,
			},
			Spec: janitordgxcnvidiacomv1alpha1.RebootNodeSpec{
				NodeName: "test-node",
				Force:    false,
			},
		}
		testRebootNodeNamespacedName = types.NamespacedName{
			Name: testRebootNode.Name,
		}
		testLeaseLock = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testNode.GetName(),
				Namespace: testNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         testRebootNode.GetObjectKind().GroupVersionKind().GroupVersion().String(),
						Kind:               testRebootNode.GetObjectKind().GroupVersionKind().Kind,
						Name:               testRebootNode.GetName(),
						UID:                testRebootNode.GetUID(),
						BlockOwnerDeletion: ptr.To(true),
					},
				},
			},
		}
		testLeaseLockNamespacedName = types.NamespacedName{
			Name:      testNode.GetName(),
			Namespace: testNamespace,
		}
		req = ctrl.Request{
			NamespacedName: testRebootNodeNamespacedName,
		}
		// Create fake client with test objects
		nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptor.Funcs{},
			testNode, testRebootNode, testLeaseLock)

		reconciler = &mockReconciler{
			Client:   k8sClient,
			nodeLock: nodeLock,
			result:   ctrl.Result{},
			err:      nil,
		}
	})

	Context("Testing LockNode", func() {
		It("lock re-acquired: node lease lock exists and matches current maintenance resource", func() {
			isLocked := nodeLock.LockNode(ctx, testRebootNode, testNode.GetName())
			Expect(isLocked).To(BeTrue())
		})
		It("lock not acquired: node lease lock exists does not match current maintenance resource", func() {
			testLeaseLock.GetOwnerReferences()[0].UID = "non-matching-uid"
			err := k8sClient.Update(ctx, testLeaseLock)
			Expect(err).NotTo(HaveOccurred())

			isLocked := nodeLock.LockNode(ctx, testRebootNode, testNode.GetName())
			Expect(isLocked).To(BeFalse())
		})
		It("lock not acquired: get node lease lock error", func() {
			initialCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeLock, metrics.StatusFailed, testNode.GetName())
			interceptorFuncs := interceptor.Funcs{
				Get: func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					return fmt.Errorf("fake get error")
				},
			}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptorFuncs,
				testNode, testRebootNode, testLeaseLock)

			isLocked := nodeLock.LockNode(ctx, testRebootNode, testNode.GetName())
			Expect(isLocked).To(BeFalse())

			newCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeLock, metrics.StatusFailed, testNode.GetName())
			Expect(newCounterVal - initialCounterVal).To(Equal(1.0))
		})
		It("lock not acquired: unexpected ownerReferences", func() {
			initialCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeLock, metrics.StatusFailed, testNode.GetName())
			testLeaseLock.SetOwnerReferences(nil)
			err := k8sClient.Update(ctx, testLeaseLock)
			Expect(err).NotTo(HaveOccurred())

			isLocked := nodeLock.LockNode(ctx, testRebootNode, testNode.GetName())
			Expect(isLocked).To(BeFalse())
			newCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeLock, metrics.StatusFailed, testNode.GetName())
			Expect(newCounterVal - initialCounterVal).To(Equal(1.0))
		})
		It("lock acquired: successfully create node lease lock", func() {
			// Default K8s client has testLeaseLock already created, alternatively could've called newTestNodeLock
			// and not have passed testLeaseLock.
			err := k8sClient.Delete(ctx, testLeaseLock)
			Expect(err).NotTo(HaveOccurred())

			isLocked := nodeLock.LockNode(ctx, testRebootNode, testNode.GetName())
			Expect(isLocked).To(BeTrue())

			// confirm that the lease object created has the expected OwnerReference configured
			var createdLease coordinationv1.Lease
			err = k8sClient.Get(ctx, testLeaseLockNamespacedName, &createdLease)
			Expect(err).NotTo(HaveOccurred())
			// GVK inference should set these values when the test object doesn't have them
			expectedOwnerReferences := []metav1.OwnerReference{
				{
					APIVersion:         "janitor.dgxc.nvidia.com/v1alpha1",
					Kind:               "RebootNode",
					Name:               testRebootNode.GetName(),
					UID:                testRebootNode.GetUID(),
					BlockOwnerDeletion: ptr.To(true),
				},
			}
			Expect(createdLease.OwnerReferences).To(Equal(expectedOwnerReferences))
		})
		It("lock not acquired: create node lease lock error", func() {
			initialCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeLock, metrics.StatusFailed, testNode.GetName())
			interceptorFuncs := interceptor.Funcs{
				Create: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					return fmt.Errorf("fake create error")
				},
			}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptorFuncs,
				testNode, testRebootNode)

			isLocked := nodeLock.LockNode(ctx, testRebootNode, testNode.GetName())
			Expect(isLocked).To(BeFalse())

			newCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeLock, metrics.StatusFailed, testNode.GetName())
			Expect(newCounterVal - initialCounterVal).To(Equal(1.0))
		})
		It("lock not acquired: node lease lock already exists", func() {
			// We need the lock to not exist at the beginning of the execution and then for it to exist at the end
			// so we need to use an interceptor since mocking the K8s client object state cannot accomplish this.
			interceptorFuncs := interceptor.Funcs{
				Create: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					return apierrors.NewAlreadyExists(schema.GroupResource{}, testLeaseLock.GetName())
				},
			}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptorFuncs,
				testNode, testRebootNode)

			isLocked := nodeLock.LockNode(ctx, testRebootNode, testNode.GetName())
			Expect(isLocked).To(BeFalse())
		})
	})

	Context("Testing CheckUnlock", func() {
		It("lock not released: get node lease lock error", func() {
			initialCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeUnlock, metrics.StatusFailed, testNode.GetName())
			interceptorFuncs := interceptor.Funcs{
				Get: func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					return fmt.Errorf("fake get error")
				},
			}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptorFuncs,
				testNode, testRebootNode, testLeaseLock)

			retryUnlock := nodeLock.CheckUnlock(ctx, testRebootNode, testNode.GetName())
			Expect(retryUnlock).To(BeTrue())
			newCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeUnlock, metrics.StatusFailed, testNode.GetName())
			Expect(newCounterVal - initialCounterVal).To(Equal(1.0))
		})
		It("lock released: node lease lock does not exist", func() {
			// Default K8s client has testRebootNode already created, alternatively could've called newTestNodeLock
			// and not have passed testRebootNode.
			err := k8sClient.Delete(ctx, testRebootNode)
			Expect(err).NotTo(HaveOccurred())

			retryUnlock := nodeLock.CheckUnlock(ctx, testRebootNode, testNode.GetName())
			Expect(retryUnlock).To(BeFalse())
		})
		It("lock not released: completion timestamp not set", func() {
			// default testRebootNode does not have completion timestamp set
			retryUnlock := nodeLock.CheckUnlock(ctx, testRebootNode, testNode.GetName())
			Expect(retryUnlock).To(BeFalse())
		})
		It("lock released: completion timestamp set but lease object does not exist", func() {
			testRebootNode.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
			err := k8sClient.Status().Update(ctx, testRebootNode)
			Expect(err).NotTo(HaveOccurred())
			err = k8sClient.Delete(ctx, testLeaseLock)
			Expect(err).NotTo(HaveOccurred())

			retryUnlock := nodeLock.CheckUnlock(ctx, testRebootNode, testNode.GetName())
			Expect(retryUnlock).To(BeFalse())
		})
		It("lock not released: completion timestamp set but get lease object error", func() {
			initialCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeUnlock, metrics.StatusFailed, testNode.GetName())
			interceptorFuncs := interceptor.Funcs{
				Get: func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					return fmt.Errorf("fake get error")
				},
			}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptorFuncs,
				testNode, testRebootNode, testLeaseLock)

			testRebootNode.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
			err := k8sClient.Status().Update(ctx, testRebootNode)
			Expect(err).NotTo(HaveOccurred())

			retryUnlock := nodeLock.CheckUnlock(ctx, testRebootNode, testNode.GetName())
			Expect(retryUnlock).To(BeTrue())
			newCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeUnlock, metrics.StatusFailed, testNode.GetName())
			Expect(newCounterVal - initialCounterVal).To(Equal(1.0))
		})
		It("lock not released (not held by current resource): completion timestamp set but lease object does not match maintenance resource", func() {
			testRebootNode.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
			err := k8sClient.Status().Update(ctx, testRebootNode)
			Expect(err).NotTo(HaveOccurred())
			testLeaseLock.GetOwnerReferences()[0].UID = "non-matching-uid"
			err = k8sClient.Update(ctx, testLeaseLock)
			Expect(err).NotTo(HaveOccurred())

			retryUnlock := nodeLock.CheckUnlock(ctx, testRebootNode, testNode.GetName())
			Expect(retryUnlock).To(BeFalse())
		})
		It("lock not released: completion timestamp set, lease object matches maintenance resource, delete lease failure", func() {
			initialCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeUnlock, metrics.StatusFailed, testNode.GetName())
			interceptorFuncs := interceptor.Funcs{
				Delete: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					return fmt.Errorf("fake delete error")
				},
			}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptorFuncs,
				testNode, testRebootNode, testLeaseLock)

			testRebootNode.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
			err := k8sClient.Status().Update(ctx, testRebootNode)
			Expect(err).NotTo(HaveOccurred())

			retryUnlock := nodeLock.CheckUnlock(ctx, testRebootNode, testNode.GetName())
			Expect(retryUnlock).To(BeTrue())
			newCounterVal := metrics.GlobalMetrics.GetActionsCountValue(metrics.ActionTypeUnlock, metrics.StatusFailed, testNode.GetName())
			Expect(newCounterVal - initialCounterVal).To(Equal(1.0))
		})
		It("lock released: completion timestamp set, lease object matches maintenance resource, lease already deleted", func() {
			interceptorFuncs := interceptor.Funcs{
				Delete: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					return apierrors.NewNotFound(schema.GroupResource{}, testLeaseLock.GetName())
				},
			}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptorFuncs,
				testNode, testRebootNode, testLeaseLock)

			testRebootNode.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
			err := k8sClient.Status().Update(ctx, testRebootNode)
			Expect(err).NotTo(HaveOccurred())

			retryUnlock := nodeLock.CheckUnlock(ctx, testRebootNode, testNode.GetName())
			Expect(retryUnlock).To(BeFalse())
		})
		It("lock released: completion timestamp set, lease object matches maintenance resource, lease successfully deleted", func() {
			testRebootNode.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
			err := k8sClient.Status().Update(ctx, testRebootNode)
			Expect(err).NotTo(HaveOccurred())

			retryUnlock := nodeLock.CheckUnlock(ctx, testRebootNode, testNode.GetName())
			Expect(retryUnlock).To(BeFalse())
		})
	})

	Context("Testing Reconcile implementation for NodeLock", func() {
		It("get maintenance resource error", func() {
			interceptorFuncs := interceptor.Funcs{
				Get: func(ctx context.Context, client client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					return fmt.Errorf("fake get error")
				},
			}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptorFuncs,
				testNode, testRebootNode, testLeaseLock)
			reconciler = newReconciler(nodeLock, k8sClient, ctrl.Result{})

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("fake get error"))
			Expect(result).To(Equal(ctrl.Result{}))
		})
		It("maintenance resource already deleted", func() {
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptor.Funcs{}, testNode)
			reconciler = newReconciler(nodeLock, k8sClient, ctrl.Result{})

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
		It("reconciling not complete with lock failure", func() {
			interceptorFuncs := interceptor.Funcs{
				Create: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					return fmt.Errorf("fake create error")
				},
			}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptorFuncs,
				testNode, testRebootNode)
			reconciler = newReconciler(nodeLock, k8sClient, ctrl.Result{})

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{Requeue: true}))
		})
		It("reconciling not complete, lock acquired, controller re-queued", func() {
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptor.Funcs{},
				testNode, testRebootNode)
			reconciler = newReconciler(nodeLock, k8sClient, ctrl.Result{RequeueAfter: 2 * time.Second})

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{RequeueAfter: 2 * time.Second}))
		})
		It("reconciling not complete, lock already acquired, force re-queued", func() {
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptor.Funcs{},
				testNode, testRebootNode, testLeaseLock)
			reconciler = newReconciler(nodeLock, k8sClient, ctrl.Result{})

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{Requeue: true}))
		})
		It("reconciling complete, lock not held", func() {
			testRebootNode.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptor.Funcs{},
				testNode, testRebootNode)
			reconciler = newReconciler(nodeLock, k8sClient, ctrl.Result{})

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
		It("reconciling complete, lock held and successfully released", func() {
			testRebootNode.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptor.Funcs{},
				testNode, testRebootNode, testLeaseLock)
			reconciler = newReconciler(nodeLock, k8sClient, ctrl.Result{})

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
		It("reconciling complete, lock held and release failed", func() {
			interceptorFuncs := interceptor.Funcs{
				Delete: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					return fmt.Errorf("fake delete error")
				},
			}
			testRebootNode.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
			nodeLock, k8sClient = newTestNodeLock(scheme, statusSubresource, interceptorFuncs,
				testNode, testRebootNode, testLeaseLock)
			reconciler = newReconciler(nodeLock, k8sClient, ctrl.Result{})

			result, err := reconciler.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{Requeue: true}))
		})
	})
})

func newReconciler(nodeLock *nodeLock, k8sClient client.Client, result ctrl.Result) *mockReconciler {
	return &mockReconciler{
		Client:   k8sClient,
		nodeLock: nodeLock,
		result:   result,
	}
}

func newTestNodeLock(scheme *runtime.Scheme, statusSubresource client.Object, interceptorFuncs interceptor.Funcs,
	initObjs ...client.Object) (*nodeLock, client.Client) {
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(initObjs...).
		WithStatusSubresource(statusSubresource).
		WithInterceptorFuncs(interceptorFuncs).
		Build()
	return &nodeLock{
		Client:    k8sClient,
		namespace: testNamespace,
	}, k8sClient
}
