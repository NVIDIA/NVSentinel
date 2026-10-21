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
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/commons/pkg/distributedlock"
	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

// fakePCClient implements pb.PlatformConnectorClient for tests.
type fakePCClient struct {
	calls      atomic.Int64
	events     atomic.Pointer[pb.HealthEvents]
	responseFn func(call int) error
}

func (f *fakePCClient) HealthEventOccurredV1(
	_ context.Context, events *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	n := int(f.calls.Add(1))
	f.events.Store(proto.Clone(events).(*pb.HealthEvents))
	if f.responseFn != nil {
		if err := f.responseFn(n); err != nil {
			return nil, err
		}
	}

	return &emptypb.Empty{}, nil
}

func newTestPublisher(fc *fakePCClient) *healthpub.Publisher {
	return healthpub.New(
		fc, "127.0.0.1:0", "test-controller",
		healthpub.WithRetryPolicy(1, time.Millisecond, 1.0, 0),
	)
}

func newTestMR(name, nodeName string) *v1alpha1.MaintenanceRequest {
	return &v1alpha1.MaintenanceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: &pb.MaintenanceRequestSpec{
			HealthEvent: &pb.HealthEvent{
				NodeName:          nodeName,
				Agent:             "maintenance-controller",
				CheckName:         "planned-maintenance",
				Version:           1,
				IsFatal:           true,
				IsHealthy:         false,
				RecommendedAction: pb.RecommendedAction_NONE,
				Message:           "Planned maintenance",
			},
			StartTime: timestamppb.New(time.Now().Add(time.Hour)),
		},
	}
}

func reconcileRequest(name string) reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name},
	}
}

var _ = Describe("MaintenanceRequest Controller", func() {
	var (
		r   *MaintenanceRequestReconciler
		fc  *fakePCClient
		ctx context.Context
	)

	const lockNamespace = "default"

	BeforeEach(func() {
		ctx = context.Background()
		fc = &fakePCClient{}
		r = &MaintenanceRequestReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Publisher: newTestPublisher(fc),
			NodeLock: distributedlock.NewNodeLock(
				k8sClient, scheme.Scheme, lockNamespace, nil,
			),
			NodeClaim: distributedlock.NewNodeLock(
				k8sClient, scheme.Scheme, lockNamespace, nil,
				distributedlock.WithLeaseName(ClaimLeaseName),
			),
		}
	})

	// getLease reads a lease directly; err is NotFound when it is absent.
	getLease := func(name string) (*coordinationv1.Lease, error) {
		var lease coordinationv1.Lease
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: lockNamespace}, &lease)

		return &lease, err
	}

	cleanupLeases := func(nodeName string) {
		deleteLease(ctx, nodeName, lockNamespace)
		deleteLease(ctx, ClaimLeaseName(nodeName), lockNamespace)
	}

	Context("Reconcile entry point", func() {
		It("returns no error when MR does not exist", func() {
			result, err := r.Reconcile(ctx, reconcileRequest("nonexistent"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})

		It("returns client errors", func() {
			expectedErr := fmt.Errorf("get maintenance request")
			r.Client = fake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithInterceptorFuncs(interceptor.Funcs{
					Get: func(
						_ context.Context, _ client.WithWatch,
						_ client.ObjectKey, _ client.Object,
						_ ...client.GetOption,
					) error {
						return expectedErr
					},
				}).
				Build()

			result, err := r.Reconcile(ctx, reconcileRequest("unavailable"))
			Expect(err).To(MatchError(expectedErr))
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})

	Context("handleCreateOrUpdate", func() {
		It("returns an error when adding the finalizer cannot be persisted", func() {
			expectedErr := fmt.Errorf("update finalizer")
			mr := newTestMR("mr-finalizer-update-fails", "node")
			r.Client = fake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(
						_ context.Context, _ client.WithWatch,
						_ client.Object, _ ...client.UpdateOption,
					) error {
						return expectedErr
					},
				}).
				Build()

			result, err := r.handleCreateOrUpdate(ctx, slog.Default(), mr)
			Expect(err).To(MatchError(expectedErr))
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(BeZero())
		})

		It("does not update the spec before publishing", func() {
			mr := newTestMR("mr-no-spec-update", "node")
			mr.Finalizers = []string{mrFinalizerName}
			r.Client = fake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithStatusSubresource(
					&v1alpha1.MaintenanceRequest{},
				).
				WithObjects(mr).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(
						_ context.Context, _ client.WithWatch,
						_ client.Object, _ ...client.UpdateOption,
					) error {
						return fmt.Errorf("unexpected spec update")
					},
				}).
				Build()
			r.NodeLock = &stubNodeLock{lockResult: true}
			r.NodeClaim = &stubNodeLock{lockResult: true}

			result, err := r.handleCreateOrUpdate(ctx, slog.Default(), mr)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(Equal(int64(1)))
		})

		It("adds finalizer and proceeds to emit in one reconcile", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node-init-fin"},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
			})

			mr := newTestMR("mr-init-finalizer", "node-init-fin")
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
				cleanupLeases("node-init-fin")
			})

			result, err := r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(Equal(int64(1)))

			var updated v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: mr.Name},
				&updated)).To(Succeed())

			Expect(controllerutil.ContainsFinalizer(
				&updated, mrFinalizerName)).To(BeTrue())
			Expect(isConditionTrue(
				&updated, conditionHealthEventEmitted)).To(BeTrue())
		})

		It("returns error and sets condition with nil spec", func() {
			mr := &v1alpha1.MaintenanceRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "mr-nil-spec",
					Finalizers: []string{mrFinalizerName},
				},
			}
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
			})

			_, err := r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(err).To(MatchError(ContainSubstring("spec.healthEvent is required")))
			Expect(fc.calls.Load()).To(BeZero())
		})

		It("returns error and sets condition with nil healthEvent", func() {
			mr := &v1alpha1.MaintenanceRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "mr-nil-he",
					Finalizers: []string{mrFinalizerName},
				},
				Spec: &pb.MaintenanceRequestSpec{},
			}
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
			})

			_, err := r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(err).To(MatchError(ContainSubstring("spec.healthEvent is required")))
			Expect(fc.calls.Load()).To(BeZero())
		})

		It("returns error and sets condition with empty nodeName", func() {
			mr := &v1alpha1.MaintenanceRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "mr-empty-node",
					Finalizers: []string{mrFinalizerName},
				},
				Spec: &pb.MaintenanceRequestSpec{
					HealthEvent: &pb.HealthEvent{NodeName: ""},
				},
			}
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
			})

			_, err := r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(err).To(MatchError(ContainSubstring("spec.healthEvent.nodeName is required")))
			Expect(fc.calls.Load()).To(BeZero())
		})

		It("does not re-emit when HealthEventEmitted is already True",
			func() {
				mr := newTestMR("mr-already-emitted", "node-emitted")
				mr.Finalizers = []string{mrFinalizerName}
				Expect(k8sClient.Create(ctx, mr)).To(Succeed())
				DeferCleanup(func() {
					removeFinalizer(ctx, mr.Name)
					cleanupLeases("node-emitted")
				})

				var fetched v1alpha1.MaintenanceRequest
				Expect(k8sClient.Get(ctx,
					types.NamespacedName{Name: mr.Name},
					&fetched)).To(Succeed())

				r.setCondition(&fetched, conditionHealthEventEmitted,
					"True", reasonEmitted, "already done")
				fetched.Status = &pb.MaintenanceRequestStatus{
					Conditions: fetched.Status.Conditions,
				}
				Expect(k8sClient.Status().Update(ctx, &fetched)).To(
					Succeed())

				result, err := r.Reconcile(ctx, reconcileRequest(mr.Name))
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
				Expect(fc.calls.Load()).To(BeZero())
			})

		It("claims node, emits event, and releases the janitor lock on happy path", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node-happy"},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
			})

			mr := newTestMR("mr-happy-path", "node-happy")
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
				cleanupLeases("node-happy")
			})

			result, err := r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(Equal(int64(1)))

			var updated v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: mr.Name},
				&updated)).To(Succeed())

			Expect(isConditionTrue(
				&updated, conditionHealthEventEmitted)).To(BeTrue())
			Expect(updated.Spec.HealthEvent.Id).To(BeEmpty())
			Expect(updated.Spec.HealthEvent.GeneratedTimestamp).To(
				BeNil())
			Expect(updated.Spec.HealthEvent.Metadata).To(BeNil())

			publishedEvent := fc.events.Load().Events[0]
			Expect(publishedEvent.Id).To(Equal(string(updated.UID)))
			Expect(publishedEvent.GeneratedTimestamp).NotTo(BeNil())
			Expect(publishedEvent.Metadata).To(HaveKeyWithValue(
				"maintenanceRequestName", updated.Name))
			Expect(publishedEvent.Metadata).To(HaveKeyWithValue(
				"maintenanceRequestUID", string(updated.UID)))

			claim, err := getLease(ClaimLeaseName("node-happy"))
			Expect(err).NotTo(HaveOccurred())
			Expect(claim.OwnerReferences).To(HaveLen(1))
			Expect(claim.OwnerReferences[0].Name).To(Equal(mr.Name))
			Expect(claim.OwnerReferences[0].UID).To(Equal(updated.UID))

			_, err = getLease("node-happy")
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"the janitor node lock must be released after emitting")
		})

		It("lets the janitor job triggered by its event lock the node", func() {
			mr := newTestMR("mr-janitor-handoff", "node-janitor-handoff")
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
				cleanupLeases("node-janitor-handoff")
			})

			_, err := r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(fc.calls.Load()).To(Equal(int64(1)))

			rebootJob := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: "reboot-job", Namespace: lockNamespace, UID: "reboot-job-uid",
			}}
			Expect(r.NodeLock.LockNode(ctx, rebootJob, "node-janitor-handoff")).To(BeTrue(),
				"an open MaintenanceRequest must not block the janitor job it triggered")

			_, err = r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())

			lock, err := getLease("node-janitor-handoff")
			Expect(err).NotTo(HaveOccurred())
			Expect(lock.OwnerReferences[0].UID).To(Equal(rebootJob.UID),
				"a later reconcile of the MaintenanceRequest must leave the janitor lock alone")
			Expect(fc.calls.Load()).To(Equal(int64(1)))
		})

		It("releases a janitor lock left by an older version and takes the claim", func() {
			mr := newTestMR("mr-legacy-lock", "node-legacy-lock")
			mr.Finalizers = []string{mrFinalizerName}
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
				cleanupLeases("node-legacy-lock")
			})

			var fetched v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: mr.Name}, &fetched)).To(Succeed())
			r.setCondition(&fetched, conditionHealthEventEmitted, "True", reasonEmitted, "emitted by older version")
			Expect(k8sClient.Status().Update(ctx, &fetched)).To(Succeed())
			Expect(r.NodeLock.LockNode(ctx, &fetched, "node-legacy-lock")).To(BeTrue())

			result, err := r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(BeZero())

			_, err = getLease("node-legacy-lock")
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			claim, err := getLease(ClaimLeaseName("node-legacy-lock"))
			Expect(err).NotTo(HaveOccurred())
			Expect(claim.OwnerReferences[0].UID).To(Equal(fetched.UID))
		})

		It("keeps an older version's janitor lock when another request took the claim first", func() {
			older := newTestMR("mr-legacy-open", "node-legacy-queue")
			older.Finalizers = []string{mrFinalizerName}
			Expect(k8sClient.Create(ctx, older)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, older.Name)
				cleanupLeases("node-legacy-queue")
			})

			var emitted v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: older.Name}, &emitted)).To(Succeed())
			r.setCondition(&emitted, conditionHealthEventEmitted, "True", reasonEmitted, "emitted by older version")
			Expect(k8sClient.Status().Update(ctx, &emitted)).To(Succeed())
			Expect(r.NodeLock.LockNode(ctx, &emitted, "node-legacy-queue")).To(BeTrue())

			// A request that was queued behind it reconciles first after the
			// upgrade and takes the claim, then waits on the janitor lock.
			queued := newTestMR("mr-legacy-queued", "node-legacy-queue")
			Expect(k8sClient.Create(ctx, queued)).To(Succeed())
			DeferCleanup(func() { removeFinalizer(ctx, queued.Name) })

			result, err := r.Reconcile(ctx, reconcileRequest(queued.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			result, err = r.Reconcile(ctx, reconcileRequest(older.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))

			lock, err := getLease("node-legacy-queue")
			Expect(err).NotTo(HaveOccurred())
			Expect(lock.OwnerReferences[0].UID).To(Equal(emitted.UID))

			_, err = r.Reconcile(ctx, reconcileRequest(queued.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(fc.calls.Load()).To(BeZero(),
				"the queued request must not emit while the older request is open")
		})

		It("rejects a second MaintenanceRequest for the same node and never retries it", func() {
			first := newTestMR("mr-first-open", "node-duplicate")
			Expect(k8sClient.Create(ctx, first)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, first.Name)
				cleanupLeases("node-duplicate")
			})

			_, err := r.Reconcile(ctx, reconcileRequest(first.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(fc.calls.Load()).To(Equal(int64(1)))

			second := newTestMR("mr-second-open", "node-duplicate")
			Expect(k8sClient.Create(ctx, second)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, second.Name)
			})

			for range 2 {
				result, err := r.Reconcile(ctx, reconcileRequest(second.Name))
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}), "a rejected request must not be requeued")
			}

			Expect(fc.calls.Load()).To(Equal(int64(1)), "a rejected request must not emit an event")

			var updated v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: second.Name}, &updated)).To(Succeed())
			cond := findCondition(&updated, conditionHealthEventEmitted)
			Expect(cond.Status).To(Equal("False"))
			Expect(cond.Reason).To(Equal(reasonRejected))
			Expect(cond.Message).To(ContainSubstring(first.Name))

			_, err = getLease("node-duplicate")
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a rejected request must not take the janitor lock")
		})

		It("waits instead of rejecting while the claim holder is being deleted", func() {
			first := newTestMR("mr-leaving", "node-handover")
			Expect(k8sClient.Create(ctx, first)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, first.Name)
				cleanupLeases("node-handover")
			})

			_, err := r.Reconcile(ctx, reconcileRequest(first.Name))
			Expect(err).NotTo(HaveOccurred())

			var leaving v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: first.Name}, &leaving)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &leaving)).To(Succeed())

			second := newTestMR("mr-arriving", "node-handover")
			Expect(k8sClient.Create(ctx, second)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, second.Name)
			})

			result, err := r.Reconcile(ctx, reconcileRequest(second.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(30 * time.Second))
			Expect(fc.calls.Load()).To(Equal(int64(1)))

			var waiting v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: second.Name}, &waiting)).To(Succeed())
			Expect(findCondition(&waiting, conditionHealthEventEmitted).Reason).To(Equal(reasonBlocked))

			// The first request finishes its cleanup and releases the claim.
			_, err = r.Reconcile(ctx, reconcileRequest(first.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(fc.calls.Load()).To(Equal(int64(2)))

			result, err = r.Reconcile(ctx, reconcileRequest(second.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(Equal(int64(3)))

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: second.Name}, &waiting)).To(Succeed())
			Expect(isConditionTrue(&waiting, conditionHealthEventEmitted)).To(BeTrue())
		})

		It("blocks when node is locked by another operation",
			func() {
				node := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-blocked",
					},
				}
				Expect(k8sClient.Create(ctx, node)).To(Succeed())
				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, node)
				})

				// Pre-create a lease held by a different resource
				existingLease := &coordinationv1.Lease{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "node-blocked",
						Namespace: lockNamespace,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: "janitor.dgxc.nvidia.com/v1alpha1",
								Kind:       "RebootNode",
								Name:       "other-reboot",
								UID:        "other-uid-123",
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, existingLease)).To(Succeed())
				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, existingLease)
				})

				mr := newTestMR("mr-blocked", "node-blocked")
				mr.Finalizers = []string{mrFinalizerName}
				Expect(k8sClient.Create(ctx, mr)).To(Succeed())
				DeferCleanup(func() {
					removeFinalizer(ctx, mr.Name)
					deleteLease(ctx, ClaimLeaseName("node-blocked"), lockNamespace)
				})

				result, err := r.Reconcile(
					ctx, reconcileRequest(mr.Name))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(
					Equal(30 * time.Second))
				Expect(fc.calls.Load()).To(BeZero())

				var updated v1alpha1.MaintenanceRequest
				Expect(k8sClient.Get(ctx,
					types.NamespacedName{Name: mr.Name},
					&updated)).To(Succeed())
				cond := findCondition(&updated, conditionHealthEventEmitted)
				Expect(cond.Reason).To(Equal(reasonBlocked))
				Expect(cond.Message).To(ContainSubstring("RebootNode/other-reboot"))

				lock, err := getLease("node-blocked")
				Expect(err).NotTo(HaveOccurred())
				Expect(lock.OwnerReferences[0].UID).To(Equal(types.UID("other-uid-123")),
					"a blocked request must leave the janitor job's lock alone")
			})

		It("retries when publisher fails", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-pub-fail",
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
				cleanupLeases("node-pub-fail")
			})

			fc.responseFn = func(_ int) error {
				return fmt.Errorf("publish error")
			}

			mr := newTestMR("mr-pub-fail", "node-pub-fail")
			mr.Finalizers = []string{mrFinalizerName}
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
			})

			_, err := r.Reconcile(
				ctx, reconcileRequest(mr.Name))
			Expect(err).To(HaveOccurred())

			var updated v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: mr.Name},
				&updated)).To(Succeed())
			Expect(findCondition(
				&updated, conditionHealthEventEmitted,
			).Reason).To(Equal(reasonEmitFailed))

			_, err = getLease("node-pub-fail")
			Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"a failed emit must not keep janitor jobs off the node while it retries")
		})

		It("proceeds when target node does not exist", func() {
			mr := newTestMR("mr-no-node", "nonexistent-node")
			mr.Finalizers = []string{mrFinalizerName}
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
				cleanupLeases("nonexistent-node")
			})

			result, err := r.Reconcile(
				ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(Equal(int64(1)))

			var updated v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: mr.Name},
				&updated)).To(Succeed())
			Expect(isConditionTrue(
				&updated, conditionHealthEventEmitted)).To(BeTrue())
		})
	})

	Context("handleDeletion", func() {
		It("requeues while node unlock needs to be retried", func() {
			mr := newTestMR("mr-unlock-retry", "node-unlock-retry")
			mr.Finalizers = []string{mrFinalizerName}
			mr.DeletionTimestamp = &metav1.Time{Time: time.Now()}
			r.NodeLock = &stubNodeLock{retryUnlock: true}

			result, err := r.handleDeletion(ctx, slog.Default(), mr)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Second))
			Expect(controllerutil.ContainsFinalizer(mr, mrFinalizerName)).To(BeTrue())
			Expect(fc.calls.Load()).To(BeZero())
		})

		It("returns an error when finalizer removal cannot be persisted", func() {
			expectedErr := fmt.Errorf("update removed finalizer")
			mr := &v1alpha1.MaintenanceRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "mr-finalizer-removal-fails",
					Finalizers:        []string{mrFinalizerName},
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
			}
			r.Client = fake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(
						_ context.Context, _ client.WithWatch,
						_ client.Object, _ ...client.UpdateOption,
					) error {
						return expectedErr
					},
				}).
				Build()

			result, err := r.handleDeletion(ctx, slog.Default(), mr)
			Expect(err).To(MatchError(expectedErr))
			Expect(result).To(Equal(reconcile.Result{}))
		})

		It("returns immediately when no finalizer is present",
			func() {
				mr := &v1alpha1.MaintenanceRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name: "mr-no-fin-del",
					},
				}
				Expect(k8sClient.Create(ctx, mr)).To(Succeed())
				Expect(k8sClient.Delete(ctx, mr)).To(Succeed())

				result, err := r.Reconcile(
					ctx, reconcileRequest(mr.Name))
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
				Expect(fc.calls.Load()).To(BeZero())
			})

		It("emits clearing event, releases both leases, "+
			"and removes finalizer", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-full-del",
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
				cleanupLeases("node-full-del")
			})

			mr := newTestMR("mr-full-del", "node-full-del")
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())

			_, _ = r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(fc.calls.Load()).To(Equal(int64(1)))

			var fetched v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: mr.Name},
				&fetched)).To(Succeed())

			// Simulate an interrupted emit that left the janitor lock held.
			Expect(r.NodeLock.LockNode(ctx, &fetched, "node-full-del")).To(BeTrue())

			Expect(k8sClient.Delete(ctx, &fetched)).To(Succeed())

			// Reconcile deletion
			result, err := r.Reconcile(
				ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(Equal(int64(2)))
			clearingEvent := fc.events.Load().Events[0]
			Expect(clearingEvent.Metadata).To(HaveKeyWithValue(
				"maintenanceRequestName", fetched.Name))
			Expect(clearingEvent.Metadata).To(HaveKeyWithValue(
				"maintenanceRequestUID", string(fetched.UID)))

			_, err = getLease("node-full-del")
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the janitor node lock must be released")

			_, err = getLease(ClaimLeaseName("node-full-del"))
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the claim must be released")

			err = k8sClient.Get(ctx, types.NamespacedName{Name: mr.Name}, &fetched)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the finalizer must be removed")
		})

		It("removes a rejected request without touching the open request's claim", func() {
			first := newTestMR("mr-del-first", "node-del-rejected")
			Expect(k8sClient.Create(ctx, first)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, first.Name)
				cleanupLeases("node-del-rejected")
			})

			_, err := r.Reconcile(ctx, reconcileRequest(first.Name))
			Expect(err).NotTo(HaveOccurred())

			second := newTestMR("mr-del-second", "node-del-rejected")
			Expect(k8sClient.Create(ctx, second)).To(Succeed())

			_, err = r.Reconcile(ctx, reconcileRequest(second.Name))
			Expect(err).NotTo(HaveOccurred())

			var rejected v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: second.Name}, &rejected)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &rejected)).To(Succeed())

			result, err := r.Reconcile(ctx, reconcileRequest(second.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(Equal(int64(1)), "a rejected request has nothing to clear")

			err = k8sClient.Get(ctx, types.NamespacedName{Name: second.Name}, &rejected)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			var open v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: first.Name}, &open)).To(Succeed())
			claim, err := getLease(ClaimLeaseName("node-del-rejected"))
			Expect(err).NotTo(HaveOccurred())
			Expect(claim.OwnerReferences[0].UID).To(Equal(open.UID))
		})

		It("skips clearing event when opening event was never emitted",
			func() {
				node := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-skip-clear",
					},
				}
				Expect(k8sClient.Create(ctx, node)).To(Succeed())
				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, node)
				})

				// Make the publisher fail so the emit never succeeds
				fc.responseFn = func(_ int) error {
					return fmt.Errorf("publisher unavailable")
				}

				mr := newTestMR("mr-skip-clear", "node-skip-clear")
				Expect(k8sClient.Create(ctx, mr)).To(Succeed())

				// Reconcile: adds finalizer, claims node, but emit fails
				_, _ = r.Reconcile(ctx, reconcileRequest(mr.Name))

				// Reset publisher for deletion
				fc.responseFn = nil
				fc.calls.Store(0)

				var fetched v1alpha1.MaintenanceRequest
				Expect(k8sClient.Get(ctx,
					types.NamespacedName{Name: mr.Name},
					&fetched)).To(Succeed())
				Expect(k8sClient.Delete(ctx, &fetched)).To(
					Succeed())

				result, err := r.Reconcile(
					ctx, reconcileRequest(mr.Name))
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
				Expect(fc.calls.Load()).To(BeZero())
			})

		It("retries clearing while keeping the node claimed", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-clear-fail",
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
				cleanupLeases("node-clear-fail")
			})

			mr := newTestMR("mr-clear-fail", "node-clear-fail")
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())

			_, _ = r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(fc.calls.Load()).To(Equal(int64(1)))

			_, err := getLease(ClaimLeaseName("node-clear-fail"))
			Expect(err).NotTo(HaveOccurred())

			// Fail on the clearing event
			fc.responseFn = func(call int) error {
				if call > 1 {
					return fmt.Errorf("clearing event failed")
				}

				return nil
			}

			var fetched v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: mr.Name},
				&fetched)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &fetched)).To(Succeed())

			_, err = r.Reconcile(
				ctx, reconcileRequest(mr.Name))
			Expect(err).To(HaveOccurred())

			// Finalizer should still be present (clearing not done)
			var updated v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: mr.Name},
				&updated)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(
				&updated, mrFinalizerName)).To(BeTrue())

			// The claim stays while clearing retries, so no other
			// MaintenanceRequest can open on the node before it clears.
			_, err = getLease(ClaimLeaseName("node-clear-fail"))
			Expect(err).NotTo(HaveOccurred())

			// Allow clearing to succeed and reconcile again
			fc.responseFn = nil
			result, err := r.Reconcile(
				ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			_, err = getLease(ClaimLeaseName("node-clear-fail"))
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("handles deletion with nil spec gracefully", func() {
			mr := &v1alpha1.MaintenanceRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "mr-nil-spec-del",
					Finalizers: []string{mrFinalizerName},
				},
			}
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())

			var fetched v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: mr.Name},
				&fetched)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &fetched)).To(Succeed())

			result, err := r.Reconcile(
				ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(BeZero())
		})
	})

	Context("NodeClaim re-acquire", func() {
		It("keeps its own claim across reconciles", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-self-claim",
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
			})

			mr := newTestMR("mr-self", "node-self-claim")
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
				cleanupLeases("node-self-claim")
			})

			for range 2 {
				result, err := r.Reconcile(
					ctx, reconcileRequest(mr.Name))
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
			}

			Expect(fc.calls.Load()).To(Equal(int64(1)))

			claim, err := getLease(ClaimLeaseName("node-self-claim"))
			Expect(err).NotTo(HaveOccurred())

			var updated v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: mr.Name},
				&updated)).To(Succeed())
			Expect(claim.OwnerReferences[0].UID).To(
				Equal(updated.UID))
		})
	})

	Context("ActiveClaimHolder", func() {
		claimOwnedBy := func(nodeName string, owner metav1.OwnerReference) {
			lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
				Name:            ClaimLeaseName(nodeName),
				Namespace:       lockNamespace,
				OwnerReferences: []metav1.OwnerReference{owner},
			}}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			DeferCleanup(func() { cleanupLeases(nodeName) })
		}

		mrOwner := func(mr *v1alpha1.MaintenanceRequest) metav1.OwnerReference {
			return metav1.OwnerReference{
				APIVersion: v1alpha1.MRGroupVersion.String(),
				Kind:       managed.MRKind,
				Name:       mr.Name,
				UID:        mr.UID,
			}
		}

		createMR := func(name, nodeName string) *v1alpha1.MaintenanceRequest {
			mr := newTestMR(name, nodeName)
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() { removeFinalizer(ctx, name) })

			return mr
		}

		It("reports no holder when the node has no claim", func() {
			mr := newTestMR("mr-holder-none", "node-holder-none")

			_, active, err := ActiveClaimHolder(ctx, k8sClient, r.NodeClaim, mr, "node-holder-none")
			Expect(err).NotTo(HaveOccurred())
			Expect(active).To(BeFalse())
		})

		It("does not treat the request itself as a duplicate", func() {
			mr := createMR("mr-holder-self", "node-holder-self")
			claimOwnedBy("node-holder-self", mrOwner(mr))

			_, active, err := ActiveClaimHolder(ctx, k8sClient, r.NodeClaim, mr, "node-holder-self")
			Expect(err).NotTo(HaveOccurred())
			Expect(active).To(BeFalse())
		})

		It("does not treat an owner of another kind as a duplicate", func() {
			mr := newTestMR("mr-holder-kind", "node-holder-kind")
			claimOwnedBy("node-holder-kind", metav1.OwnerReference{
				APIVersion: "janitor.dgxc.nvidia.com/v1alpha1",
				Kind:       "RebootNode",
				Name:       "mr-holder-kind-reboot",
				UID:        "reboot-uid",
			})

			_, active, err := ActiveClaimHolder(ctx, k8sClient, r.NodeClaim, mr, "node-holder-kind")
			Expect(err).NotTo(HaveOccurred())
			Expect(active).To(BeFalse())
		})

		It("does not treat a holder that no longer exists as a duplicate", func() {
			mr := newTestMR("mr-holder-gone", "node-holder-gone")
			claimOwnedBy("node-holder-gone", metav1.OwnerReference{
				APIVersion: v1alpha1.MRGroupVersion.String(),
				Kind:       managed.MRKind,
				Name:       "mr-already-deleted",
				UID:        "deleted-uid",
			})

			_, active, err := ActiveClaimHolder(ctx, k8sClient, r.NodeClaim, mr, "node-holder-gone")
			Expect(err).NotTo(HaveOccurred())
			Expect(active).To(BeFalse())
		})

		It("does not treat a holder for a different node as a duplicate", func() {
			// The janitor lock of a node literally named "mr-claim.x" shares
			// the claim name of node "x".
			other := createMR("mr-holder-other-node", "mr-claim.node-holder-collide")
			mr := newTestMR("mr-holder-collide", "node-holder-collide")
			claimOwnedBy("node-holder-collide", mrOwner(other))

			_, active, err := ActiveClaimHolder(ctx, k8sClient, r.NodeClaim, mr, "node-holder-collide")
			Expect(err).NotTo(HaveOccurred())
			Expect(active).To(BeFalse())
		})

		It("reports an open request for the same node as a duplicate", func() {
			open := createMR("mr-holder-open", "node-holder-open")
			mr := newTestMR("mr-holder-new", "node-holder-open")
			claimOwnedBy("node-holder-open", mrOwner(open))

			holder, active, err := ActiveClaimHolder(ctx, k8sClient, r.NodeClaim, mr, "node-holder-open")
			Expect(err).NotTo(HaveOccurred())
			Expect(active).To(BeTrue())
			Expect(holder.Name).To(Equal(open.Name))
		})
	})

	Context("eventForPublishing", func() {
		It("fills missing fields on a copy", func() {
			mr := newTestMR("mr-copy", "node-copy")
			mr.UID = types.UID("test-uid-123")
			mr.Spec.HealthEvent.Id = ""
			mr.Spec.HealthEvent.GeneratedTimestamp = nil
			mr.Spec.HealthEvent.Metadata = nil

			event := eventForPublishing(mr)

			Expect(event.Id).To(Equal("test-uid-123"))
			Expect(event.Agent).To(Equal("maintenance-controller"))
			Expect(event.GeneratedTimestamp).NotTo(BeNil())
			Expect(event.Metadata).To(HaveKeyWithValue(
				"maintenanceRequestName", "mr-copy"))
			Expect(event.Metadata).To(HaveKeyWithValue(
				"maintenanceRequestUID", "test-uid-123"))
			Expect(mr.Spec.HealthEvent.Id).To(BeEmpty())
			Expect(mr.Spec.HealthEvent.Agent).To(Equal("maintenance-controller"))
			Expect(mr.Spec.HealthEvent.GeneratedTimestamp).To(BeNil())
			Expect(mr.Spec.HealthEvent.Metadata).To(BeNil())
		})

		It("preserves existing fields and metadata", func() {
			ts := timestamppb.New(
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			mr := newTestMR("mr-keep", "node-keep")
			mr.UID = types.UID("uid-keep")
			mr.Spec.HealthEvent.Id = "custom-id"
			mr.Spec.HealthEvent.GeneratedTimestamp = ts
			mr.Spec.HealthEvent.Metadata = map[string]string{
				"existingKey": "existingValue",
			}

			event := eventForPublishing(mr)

			Expect(event.Id).To(Equal("custom-id"))
			Expect(event.Agent).To(Equal("maintenance-controller"))
			Expect(event.GeneratedTimestamp).To(Equal(ts))
			Expect(event.Metadata).To(
				HaveKeyWithValue("existingKey", "existingValue"))
			Expect(event.Metadata).To(
				HaveKeyWithValue(
					"maintenanceRequestName", "mr-keep"))
			Expect(event.Metadata).To(
				HaveKeyWithValue("maintenanceRequestUID", "uid-keep"))
			Expect(mr.Spec.HealthEvent.Metadata).NotTo(
				HaveKey("maintenanceRequestUID"))
		})
	})

	Context("setCondition", func() {
		It("creates a new condition", func() {
			mr := &v1alpha1.MaintenanceRequest{}
			r.setCondition(mr, "TestCond", "True", "TestReason",
				"test message")

			Expect(mr.Status).NotTo(BeNil())
			Expect(mr.Status.Conditions).To(HaveLen(1))
			Expect(mr.Status.Conditions[0].Type).To(
				Equal("TestCond"))
			Expect(mr.Status.Conditions[0].Status).To(
				Equal("True"))
			Expect(mr.Status.Conditions[0].Reason).To(
				Equal("TestReason"))
		})

		It("updates an existing condition and transition time "+
			"on status change", func() {
			mr := &v1alpha1.MaintenanceRequest{}
			r.setCondition(mr, "TestCond", "False", "Initial",
				"first")

			firstTransition := mr.Status.Conditions[0].
				LastTransitionTime

			// Small delay so transition times differ
			time.Sleep(2 * time.Millisecond)

			r.setCondition(mr, "TestCond", "True", "Updated",
				"second")

			Expect(mr.Status.Conditions).To(HaveLen(1))
			Expect(mr.Status.Conditions[0].Status).To(
				Equal("True"))
			Expect(mr.Status.Conditions[0].Reason).To(
				Equal("Updated"))
			Expect(mr.Status.Conditions[0].LastTransitionTime.
				AsTime().After(
				firstTransition.AsTime())).To(BeTrue())
		})

		It("updates reason and message without changing "+
			"transition time when status is unchanged", func() {
			mr := &v1alpha1.MaintenanceRequest{}
			r.setCondition(mr, "TestCond", "False", "ReasonA",
				"msg-a")

			firstTransition := mr.Status.Conditions[0].
				LastTransitionTime

			time.Sleep(2 * time.Millisecond)

			r.setCondition(mr, "TestCond", "False", "ReasonB",
				"msg-b")

			Expect(mr.Status.Conditions).To(HaveLen(1))
			Expect(mr.Status.Conditions[0].Reason).To(
				Equal("ReasonB"))
			Expect(mr.Status.Conditions[0].Message).To(
				Equal("msg-b"))
			Expect(mr.Status.Conditions[0].LastTransitionTime).To(
				Equal(firstTransition))
		})
	})

	Context("isConditionTrue", func() {
		It("returns false for nil status", func() {
			mr := &v1alpha1.MaintenanceRequest{}
			Expect(isConditionTrue(mr, "Anything")).To(BeFalse())
		})

		It("returns false when condition does not exist", func() {
			mr := &v1alpha1.MaintenanceRequest{
				Status: &pb.MaintenanceRequestStatus{
					Conditions: []*pb.Condition{
						{Type: "Other", Status: "True"},
					},
				},
			}
			Expect(isConditionTrue(mr, "Missing")).To(BeFalse())
		})

		It("returns true when condition status is True", func() {
			mr := &v1alpha1.MaintenanceRequest{
				Status: &pb.MaintenanceRequestStatus{
					Conditions: []*pb.Condition{
						{Type: "Ready", Status: "True"},
					},
				},
			}
			Expect(isConditionTrue(mr, "Ready")).To(BeTrue())
		})

		It("returns false when condition status is not True",
			func() {
				mr := &v1alpha1.MaintenanceRequest{
					Status: &pb.MaintenanceRequestStatus{
						Conditions: []*pb.Condition{
							{Type: "Ready", Status: "False"},
						},
					},
				}
				Expect(isConditionTrue(mr, "Ready")).To(BeFalse())
			})
	})
})

func findCondition(
	mr *v1alpha1.MaintenanceRequest, condType string,
) *pb.Condition {
	if mr.Status == nil {
		return nil
	}

	for _, c := range mr.Status.Conditions {
		if c.Type == condType {
			return c
		}
	}

	return nil
}

func removeFinalizer(ctx context.Context, name string) {
	var mr v1alpha1.MaintenanceRequest
	if err := k8sClient.Get(ctx,
		types.NamespacedName{Name: name}, &mr); err != nil {
		return
	}

	controllerutil.RemoveFinalizer(&mr, mrFinalizerName)
	_ = k8sClient.Update(ctx, &mr)
	_ = k8sClient.Delete(ctx, &mr)
}

func deleteLease(ctx context.Context, name, namespace string) {
	var lease coordinationv1.Lease
	if err := k8sClient.Get(ctx,
		types.NamespacedName{Name: name, Namespace: namespace},
		&lease); err != nil {
		return
	}

	_ = k8sClient.Delete(ctx, &lease)
}

type stubNodeLock struct {
	lockResult  bool
	retryUnlock bool
}

func (s *stubNodeLock) LockNode(
	_ context.Context, _ client.Object, _ string,
) bool {
	return s.lockResult
}

func (s *stubNodeLock) GetHolder(
	_ context.Context, _ string,
) (*metav1.OwnerReference, error) {
	return nil, nil
}

func (s *stubNodeLock) CheckUnlock(
	_ context.Context, _ client.Object, _ string,
) bool {
	return s.retryUnlock
}
