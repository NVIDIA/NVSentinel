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
	"sync/atomic"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testNodeUID   = "0f43ccca-2918-4b33-a42a-81916841de1f"
	testNamespace = "default"
)

// fakeLockMetrics records lock/unlock failure counts for test assertions.
type fakeLockMetrics struct {
	lockFailures   atomic.Int64
	unlockFailures atomic.Int64
}

func (f *fakeLockMetrics) IncLockFailure(_ string)   { f.lockFailures.Add(1) }
func (f *fakeLockMetrics) IncUnlockFailure(_ string) { f.unlockFailures.Add(1) }

func TestNodeLock_GetHolder_ExpectedLeaseOwnerReference(t *testing.T) {
	t.Parallel()

	testScheme := runtime.NewScheme()
	require.NoError(t, coordinationv1.AddToScheme(testScheme))

	expected := metav1.OwnerReference{
		APIVersion: "test.example.com/v1",
		Kind:       "TestResource",
		Name:       "test-resource",
		UID:        types.UID(testNodeUID),
	}
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-node",
			Namespace:       testNamespace,
			OwnerReferences: []metav1.OwnerReference{expected},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(lease).Build()
	lock := NewNodeLock(kubeClient, testScheme, testNamespace, nil)

	holder, err := lock.GetHolder(context.Background(), lease.Name)
	require.NoError(t, err)
	require.NotNil(t, holder)
	assert.Equal(t, expected, *holder)
}

// testResource is a simple object that satisfies client.Object for tests.
// We use ConfigMap because it's registered in the corev1 scheme.
func newTestResource(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			UID:       types.UID(testNodeUID),
		},
	}
}

var _ = Describe("NodeLock", func() {
	var (
		ctx                        context.Context
		lock                       *nodeLock
		k8sClient                  client.Client
		testScheme                 *runtime.Scheme
		testNode                   *corev1.Node
		testResource               *corev1.ConfigMap
		testLeaseLock              *coordinationv1.Lease
		testLeaseLockNamespaceName types.NamespacedName
		fm                         *fakeLockMetrics
	)

	BeforeEach(func() {
		ctx = context.Background()
		fm = &fakeLockMetrics{}

		testScheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(testScheme)).To(Succeed())
		Expect(coordinationv1.AddToScheme(testScheme)).To(Succeed())

		testNode = &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
		}
		testResource = newTestResource("test-resource")
		testLeaseLock = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testNode.GetName(),
				Namespace: testNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "/v1",
						Kind:               "ConfigMap",
						Name:               testResource.GetName(),
						UID:                testResource.GetUID(),
						BlockOwnerDeletion: new(true),
					},
				},
			},
		}
		testLeaseLockNamespaceName = types.NamespacedName{
			Name:      testNode.GetName(),
			Namespace: testNamespace,
		}
		lock, k8sClient = newTestNodeLock(
			testScheme, fm, interceptor.Funcs{},
			testNode, testResource, testLeaseLock,
		)
	})

	Context("Testing GetHolder", func() {
		It("returns a contextual error when the lease cannot be fetched", func() {
			holder, err := lock.GetHolder(ctx, "missing-node")
			Expect(holder).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring(
				`getting node lock holder for node "missing-node"`)))
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("Testing LockNode", func() {
		It("lock re-acquired: node lease lock exists and matches current resource", func() {
			isLocked := lock.LockNode(ctx, testResource, testNode.GetName())
			Expect(isLocked).To(BeTrue())
		})

		It("lock not acquired: node lease lock exists does not match current resource", func() {
			testLeaseLock.GetOwnerReferences()[0].UID = "non-matching-uid"
			err := k8sClient.Update(ctx, testLeaseLock)
			Expect(err).NotTo(HaveOccurred())

			isLocked := lock.LockNode(ctx, testResource, testNode.GetName())
			Expect(isLocked).To(BeFalse())
		})

		It("lock not acquired: get node lease lock error", func() {
			interceptorFuncs := interceptor.Funcs{
				Get: func(
					ctx context.Context, c client.WithWatch,
					key client.ObjectKey, obj client.Object, opts ...client.GetOption,
				) error {
					return fmt.Errorf("fake get error")
				},
			}
			lock, k8sClient = newTestNodeLock(
				testScheme, fm, interceptorFuncs,
				testNode, testResource, testLeaseLock,
			)

			isLocked := lock.LockNode(ctx, testResource, testNode.GetName())
			Expect(isLocked).To(BeFalse())
			Expect(fm.lockFailures.Load()).To(Equal(int64(1)))
		})

		It("lock not acquired: unexpected ownerReferences", func() {
			testLeaseLock.SetOwnerReferences(nil)
			err := k8sClient.Update(ctx, testLeaseLock)
			Expect(err).NotTo(HaveOccurred())

			isLocked := lock.LockNode(ctx, testResource, testNode.GetName())
			Expect(isLocked).To(BeFalse())
			Expect(fm.lockFailures.Load()).To(Equal(int64(1)))
		})

		It("lock acquired: successfully create node lease lock", func() {
			err := k8sClient.Delete(ctx, testLeaseLock)
			Expect(err).NotTo(HaveOccurred())

			isLocked := lock.LockNode(ctx, testResource, testNode.GetName())
			Expect(isLocked).To(BeTrue())

			var createdLease coordinationv1.Lease
			err = k8sClient.Get(ctx, testLeaseLockNamespaceName, &createdLease)
			Expect(err).NotTo(HaveOccurred())
			Expect(createdLease.OwnerReferences).To(HaveLen(1))
			Expect(createdLease.OwnerReferences[0].Kind).To(Equal("ConfigMap"))
			Expect(createdLease.OwnerReferences[0].Name).To(
				Equal(testResource.GetName()))
		})

		It("lock not acquired: create node lease lock error", func() {
			interceptorFuncs := interceptor.Funcs{
				Create: func(
					ctx context.Context, c client.WithWatch,
					obj client.Object, opts ...client.CreateOption,
				) error {
					return fmt.Errorf("fake create error")
				},
			}
			lock, k8sClient = newTestNodeLock(
				testScheme, fm, interceptorFuncs,
				testNode, testResource,
			)

			isLocked := lock.LockNode(ctx, testResource, testNode.GetName())
			Expect(isLocked).To(BeFalse())
			Expect(fm.lockFailures.Load()).To(Equal(int64(1)))
		})

		It("lock not acquired: node lease lock already exists", func() {
			interceptorFuncs := interceptor.Funcs{
				Create: func(
					ctx context.Context, c client.WithWatch,
					obj client.Object, opts ...client.CreateOption,
				) error {
					return apierrors.NewAlreadyExists(
						schema.GroupResource{}, testLeaseLock.GetName())
				},
			}
			lock, k8sClient = newTestNodeLock(
				testScheme, fm, interceptorFuncs,
				testNode, testResource,
			)

			isLocked := lock.LockNode(ctx, testResource, testNode.GetName())
			Expect(isLocked).To(BeFalse())
		})
	})

	Context("Testing CheckUnlock", func() {
		It("lock not released: get node lease lock error", func() {
			interceptorFuncs := interceptor.Funcs{
				Get: func(
					ctx context.Context, c client.WithWatch,
					key client.ObjectKey, obj client.Object, opts ...client.GetOption,
				) error {
					return fmt.Errorf("fake get error")
				},
			}
			lock, k8sClient = newTestNodeLock(
				testScheme, fm, interceptorFuncs,
				testNode, testResource, testLeaseLock,
			)

			retryUnlock := lock.CheckUnlock(ctx, testResource, testNode.GetName())
			Expect(retryUnlock).To(BeTrue())
			Expect(fm.unlockFailures.Load()).To(Equal(int64(1)))
		})

		It("lock released: node lease lock does not exist", func() {
			err := k8sClient.Delete(ctx, testLeaseLock)
			Expect(err).NotTo(HaveOccurred())

			retryUnlock := lock.CheckUnlock(ctx, testResource, testNode.GetName())
			Expect(retryUnlock).To(BeFalse())
		})

		It("lock released: lease object matches resource, lease successfully deleted", func() {
			retryUnlock := lock.CheckUnlock(ctx, testResource, testNode.GetName())
			Expect(retryUnlock).To(BeFalse())
		})

		It("lock not released (not held by current resource): "+
			"lease object does not match resource", func() {
			testLeaseLock.GetOwnerReferences()[0].UID = "non-matching-uid"
			err := k8sClient.Update(ctx, testLeaseLock)
			Expect(err).NotTo(HaveOccurred())

			retryUnlock := lock.CheckUnlock(ctx, testResource, testNode.GetName())
			Expect(retryUnlock).To(BeFalse())
		})

		It("lock not released: lease object matches, delete lease failure", func() {
			interceptorFuncs := interceptor.Funcs{
				Delete: func(
					ctx context.Context, c client.WithWatch,
					obj client.Object, opts ...client.DeleteOption,
				) error {
					return fmt.Errorf("fake delete error")
				},
			}
			lock, k8sClient = newTestNodeLock(
				testScheme, fm, interceptorFuncs,
				testNode, testResource, testLeaseLock,
			)

			retryUnlock := lock.CheckUnlock(ctx, testResource, testNode.GetName())
			Expect(retryUnlock).To(BeTrue())
			Expect(fm.unlockFailures.Load()).To(Equal(int64(1)))
		})

		It("lock released: lease already deleted (not found)", func() {
			interceptorFuncs := interceptor.Funcs{
				Delete: func(
					ctx context.Context, c client.WithWatch,
					obj client.Object, opts ...client.DeleteOption,
				) error {
					return apierrors.NewNotFound(
						schema.GroupResource{}, testLeaseLock.GetName())
				},
			}
			lock, k8sClient = newTestNodeLock(
				testScheme, fm, interceptorFuncs,
				testNode, testResource, testLeaseLock,
			)

			retryUnlock := lock.CheckUnlock(ctx, testResource, testNode.GetName())
			Expect(retryUnlock).To(BeFalse())
		})
	})

	Context("Testing resolveGVK", func() {
		It("uses the GVK set on the object", func() {
			testResource.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "maintenance.nvidia.com",
				Version: "v1alpha1",
				Kind:    "Maintenance",
			})

			apiVersion, kind := lock.resolveGVK(testResource)
			Expect(kind).To(Equal("Maintenance"))
			Expect(apiVersion).To(Equal("maintenance.nvidia.com/v1alpha1"))
		})

		It("uses scheme when object GVK is not set", func() {
			apiVersion, kind := lock.resolveGVK(testResource)
			Expect(kind).To(Equal("ConfigMap"))
			Expect(apiVersion).To(Equal("v1"))
		})

		It("returns empty values when neither object nor scheme resolves a GVK", func() {
			lock.scheme = nil

			apiVersion, kind := lock.resolveGVK(testResource)
			Expect(kind).To(BeEmpty())
			Expect(apiVersion).To(BeEmpty())
		})
	})

	Context("Testing nil metrics", func() {
		It("does not panic when metrics is nil", func() {
			lock, _ = newTestNodeLock(
				testScheme, nil, interceptor.Funcs{
					Get: func(
						ctx context.Context, c client.WithWatch,
						key client.ObjectKey, obj client.Object, opts ...client.GetOption,
					) error {
						return fmt.Errorf("fake get error")
					},
				},
				testNode, testResource,
			)

			isLocked := lock.LockNode(ctx, testResource, testNode.GetName())
			Expect(isLocked).To(BeFalse())
		})
	})
})

func newTestNodeLock(
	scheme *runtime.Scheme, metrics LockMetrics,
	interceptorFuncs interceptor.Funcs, initObjs ...client.Object,
) (*nodeLock, client.Client) {
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(initObjs...).
		WithInterceptorFuncs(interceptorFuncs).
		Build()

	return &nodeLock{
		Client:    k8sClient,
		scheme:    scheme,
		namespace: testNamespace,
		metrics:   metrics,
	}, k8sClient
}
