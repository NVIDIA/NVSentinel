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
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	"github.com/nvidia/nvsentinel/commons/pkg/managed"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

// fakePCClient implements pb.PlatformConnectorClient for tests.
type fakePCClient struct {
	calls      atomic.Int64
	responseFn func(call int) error
}

func (f *fakePCClient) HealthEventOccurredV1(
	_ context.Context, _ *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	n := int(f.calls.Add(1))
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

	BeforeEach(func() {
		ctx = context.Background()
		fc = &fakePCClient{}
		r = &MaintenanceRequestReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Publisher: newTestPublisher(fc),
		}
	})

	Context("Reconcile entry point", func() {
		It("returns no error when MR does not exist", func() {
			result, err := r.Reconcile(ctx, reconcileRequest("nonexistent"))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})

	Context("handleCreateOrUpdate", func() {
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

		It("is a no-op when HealthEventEmitted is already True",
			func() {
				mr := newTestMR("mr-already-emitted", "node-emitted")
				mr.Finalizers = []string{mrFinalizerName}
				Expect(k8sClient.Create(ctx, mr)).To(Succeed())
				DeferCleanup(func() {
					removeFinalizer(ctx, mr.Name)
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

		It("claims node and emits event on happy path", func() {
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
			Expect(updated.Spec.HealthEvent.Id).NotTo(BeEmpty())
			Expect(updated.Spec.HealthEvent.Version).To(
				Equal(uint32(1)))
			Expect(updated.Spec.HealthEvent.GeneratedTimestamp).NotTo(
				BeNil())
			Expect(updated.Spec.HealthEvent.Metadata).To(
				HaveKey("maintenanceRequestName"))

			var updatedNode corev1.Node
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "node-happy"},
				&updatedNode)).To(Succeed())
			Expect(updatedNode.Annotations[managed.AnnotationActiveMR]).To(
				Equal(mr.Name))
		})

		It("blocks when node has active MR from another request",
			func() {
				node := &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node-blocked",
						Annotations: map[string]string{
							managed.AnnotationActiveMR: "other-mr",
						},
					},
				}
				Expect(k8sClient.Create(ctx, node)).To(Succeed())
				DeferCleanup(func() {
					_ = k8sClient.Delete(ctx, node)
				})

				mr := newTestMR("mr-blocked", "node-blocked")
				mr.Finalizers = []string{mrFinalizerName}
				Expect(k8sClient.Create(ctx, mr)).To(Succeed())
				DeferCleanup(func() {
					removeFinalizer(ctx, mr.Name)
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
				Expect(findCondition(
					&updated, conditionHealthEventEmitted,
				).Reason).To(Equal(reasonBlocked))
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
		})

		It("proceeds when target node does not exist", func() {
			mr := newTestMR("mr-no-node", "nonexistent-node")
			mr.Finalizers = []string{mrFinalizerName}
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
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

		It("emits clearing event, removes annotation, "+
			"and removes finalizer", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-full-del",
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
			})

			mr := newTestMR("mr-full-del", "node-full-del")
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())

			_, _ = r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(fc.calls.Load()).To(Equal(int64(1)))

			// Trigger deletion
			var fetched v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: mr.Name},
				&fetched)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &fetched)).To(Succeed())

			// Reconcile deletion
			result, err := r.Reconcile(
				ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(Equal(int64(2)))

			var updatedNode corev1.Node
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "node-full-del"},
				&updatedNode)).To(Succeed())
			Expect(updatedNode.Annotations).NotTo(
				HaveKey(managed.AnnotationActiveMR))
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

		It("retries clearing but removes annotation immediately "+
			"so node is not permanently blocked", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-clear-fail",
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
			})

			mr := newTestMR("mr-clear-fail", "node-clear-fail")
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())

			_, _ = r.Reconcile(ctx, reconcileRequest(mr.Name))
			Expect(fc.calls.Load()).To(Equal(int64(1)))

			// Verify the node was claimed
			var claimedNode corev1.Node
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "node-clear-fail"},
				&claimedNode)).To(Succeed())
			Expect(claimedNode.Annotations[managed.AnnotationActiveMR]).To(
				Equal(mr.Name))

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

			_, err := r.Reconcile(
				ctx, reconcileRequest(mr.Name))
			Expect(err).To(HaveOccurred())

			// Finalizer should still be present (clearing not done)
			var updated v1alpha1.MaintenanceRequest
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: mr.Name},
				&updated)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(
				&updated, mrFinalizerName)).To(BeTrue())

			// Annotation must already be removed even though
			// clearing is still retrying — this is the bug fix.
			var freedNode corev1.Node
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: "node-clear-fail"},
				&freedNode)).To(Succeed())
			Expect(freedNode.Annotations).NotTo(
				HaveKey(managed.AnnotationActiveMR))

			// Allow clearing to succeed and reconcile again
			fc.responseFn = nil
			result, err := r.Reconcile(
				ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
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

	Context("claimNode", func() {
		It("returns true when already claimed by self", func() {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node-self-claim",
					Annotations: map[string]string{
						managed.AnnotationActiveMR: "mr-self",
					},
				},
			}
			Expect(k8sClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() {
				_ = k8sClient.Delete(ctx, node)
			})

			mr := newTestMR("mr-self", "node-self-claim")
			mr.Finalizers = []string{mrFinalizerName}
			Expect(k8sClient.Create(ctx, mr)).To(Succeed())
			DeferCleanup(func() {
				removeFinalizer(ctx, mr.Name)
			})

			result, err := r.Reconcile(
				ctx, reconcileRequest(mr.Name))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fc.calls.Load()).To(Equal(int64(1)))
		})
	})

	Context("autoPopulateEventFields", func() {
		It("fills missing id and timestamp", func() {
			mr := newTestMR("mr-auto", "node-auto")
			mr.UID = types.UID("test-uid-123")
			mr.Spec.HealthEvent.Id = ""
			mr.Spec.HealthEvent.GeneratedTimestamp = nil

			r.autoPopulateEventFields(mr)

			Expect(mr.Spec.HealthEvent.Id).To(
				Equal("test-uid-123"))
			Expect(mr.Spec.HealthEvent.GeneratedTimestamp).NotTo(
				BeNil())
		})

		It("does not overwrite existing values", func() {
			ts := timestamppb.New(
				time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			mr := newTestMR("mr-keep", "node-keep")
			mr.Spec.HealthEvent.Id = "custom-id"
			mr.Spec.HealthEvent.GeneratedTimestamp = ts

			r.autoPopulateEventFields(mr)

			Expect(mr.Spec.HealthEvent.Id).To(Equal("custom-id"))
			Expect(mr.Spec.HealthEvent.GeneratedTimestamp).To(
				Equal(ts))
		})
	})

	Context("stampTraceability", func() {
		It("sets metadata with MR name and UID", func() {
			mr := newTestMR("mr-trace", "node-trace")
			mr.UID = types.UID("uid-abc")
			mr.Spec.HealthEvent.Metadata = nil

			r.stampTraceability(mr)

			Expect(mr.Spec.HealthEvent.Metadata).To(
				HaveKeyWithValue(
					"maintenanceRequestName", "mr-trace"))
			Expect(mr.Spec.HealthEvent.Metadata).To(
				HaveKeyWithValue(
					"maintenanceRequestUID", "uid-abc"))
		})

		It("preserves existing metadata entries", func() {
			mr := newTestMR("mr-trace2", "node-trace2")
			mr.UID = types.UID("uid-def")
			mr.Spec.HealthEvent.Metadata = map[string]string{
				"existingKey": "existingValue",
			}

			r.stampTraceability(mr)

			Expect(mr.Spec.HealthEvent.Metadata).To(
				HaveKeyWithValue("existingKey", "existingValue"))
			Expect(mr.Spec.HealthEvent.Metadata).To(
				HaveKeyWithValue(
					"maintenanceRequestName", "mr-trace2"))
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
