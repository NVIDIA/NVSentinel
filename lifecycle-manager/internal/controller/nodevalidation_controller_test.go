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
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/metrics"
)

const nodeValidationBatchWait = 1200 * time.Millisecond

var nodeValidationCriteria = []v1alpha1.CriteriaSpec{
	{Name: "recently-joined", Expression: `has(node.metadata.creationTimestamp) && now() - timestamp(node.metadata.creationTimestamp) < duration("15m")`},
	{Name: "gpu-present", Expression: `has(node.metadata.labels) && "nvidia.com/gpu.present" in node.metadata.labels`},
}

func defaultNodeValidationConfig() *config.Config {
	return &config.Config{
		Validation: &v1alpha1.ValidationConfiguration{
			Spec: v1alpha1.ValidationConfigurationSpec{
				NewNodeValidation: &v1alpha1.NewNodeValidationConfig{
					Condition:          "NewNodeValidated",
					Criteria:           nodeValidationCriteria,
					NewNodeTests:       []string{"nccl-all-reduce"},
					BatchPeriodSeconds: 1,
				},
			},
		},
	}
}

type nodeValidationTestCase struct {
	// Provide a custom ValidationConfiguration for the given test. If not provided, the test will use the
	// defaultNodeValidationConfig
	config *config.Config
	// The nodes to create prior to running the given test case.
	nodes []*corev1.Node
}

func newNodeValidationTestSetup(ctx context.Context, testCase nodeValidationTestCase) *NodeValidationReconciler {
	cfg := testCase.config
	if cfg == nil {
		cfg = defaultNodeValidationConfig()
	}

	reconciler, err := NewNodeValidationReconciler(k8sClient, k8sClient, k8sClient.Scheme(), cfg)
	Expect(err).NotTo(HaveOccurred())

	names := make([]string, 0, len(testCase.nodes))
	for _, n := range testCase.nodes {
		Expect(k8sClient.Create(ctx, n)).To(Succeed())
		names = append(names, n.Name)
	}

	DeferCleanup(func() {
		cleanupNodeValidationTest(ctx, names)
	})

	return reconciler
}

func cleanupNodeValidationTest(ctx context.Context, nodeNames []string) {
	var list v1alpha1.ValidationRequestList
	Expect(k8sClient.List(ctx, &list)).To(Succeed())

	for _, vr := range list.Items {
		for _, n := range vr.Spec.Nodes {
			if slices.Contains(nodeNames, n.Name) {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &vr))).To(Succeed())
				break
			}
		}
	}

	for _, name := range nodeNames {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}))).To(Succeed())
	}
}

func reconcileNode(ctx context.Context, r *NodeValidationReconciler, name string) reconcile.Result {
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	Expect(err).NotTo(HaveOccurred())

	return res
}

func reconcileNodeExpectError(ctx context.Context, r *NodeValidationReconciler, name string) {
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	Expect(err).To(HaveOccurred())
}

var _ = Describe("NodeValidationReconciler", func() {
	var (
		ctx    context.Context
		suffix string
	)

	BeforeEach(func() {
		ctx = context.Background()
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	Context("batching new nodes", func() {
		It("batches one node into a ValidationRequest", func() {
			nodeName := "node-" + suffix
			reconciler := newNodeValidationTestSetup(ctx, nodeValidationTestCase{
				nodes: []*corev1.Node{newEligibleNode(nodeName)},
			})

			reconcileNode(ctx, reconciler, nodeName)
			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")

			vr := findValidationRequestForNodes(ctx, nodeName)
			Expect(vr).NotTo(BeNil())
			Expect(vr.Spec.Tests).To(Equal([]string{"nccl-all-reduce"}))

			cond := nodeCondition(ctx, nodeName)
			Expect(cond).NotTo(BeNil())
			Expect(*cond).To(Equal(corev1.NodeCondition{
				Type:               "NewNodeValidated",
				Status:             corev1.ConditionTrue,
				Reason:             newNodeValidationReason,
				Message:            fmt.Sprintf("New node validation requested via ValidationRequest %q", vr.Name),
				LastHeartbeatTime:  cond.LastHeartbeatTime,
				LastTransitionTime: cond.LastTransitionTime,
			}))
		})

		It("batches multiple nodes into a ValidationRequest", func() {
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			reconciler := newNodeValidationTestSetup(ctx, nodeValidationTestCase{
				nodes: []*corev1.Node{newEligibleNode(node1), newEligibleNode(node2)},
			})

			firstRes := reconcileNode(ctx, reconciler, node1)
			Expect(firstRes.RequeueAfter).To(BeNumerically(">", 0),
				"first node into a batch should get a non-zero RequeueAfter")

			secondRes := reconcileNode(ctx, reconciler, node2)
			Expect(secondRes.RequeueAfter).To(BeZero(),
				"a node joining an already existing batch should get a zero RequeueAfter")

			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")

			vr := findValidationRequestForNodes(ctx, node1, node2)
			Expect(vr).NotTo(BeNil())
			Expect(nodeCondition(ctx, node1).Status).To(Equal(corev1.ConditionTrue))
			Expect(nodeCondition(ctx, node2).Status).To(Equal(corev1.ConditionTrue))
		})

		It("creates two ValidationRequests for two batches", func() {
			node1 := newEligibleNode("node1-" + suffix)
			reconciler := newNodeValidationTestSetup(ctx, nodeValidationTestCase{nodes: []*corev1.Node{node1}})

			reconcileNode(ctx, reconciler, node1.Name)
			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")
			firstVR := findValidationRequestForNodes(ctx, node1.Name)
			Expect(firstVR).NotTo(BeNil())

			node2 := newEligibleNode("node2-" + suffix)
			Expect(k8sClient.Create(ctx, node2)).To(Succeed())

			DeferCleanup(func() {
				cleanupNodeValidationTest(ctx, []string{node2.Name})
			})

			reconcileNode(ctx, reconciler, node2.Name)
			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")

			secondVR := findValidationRequestForNodes(ctx, node2.Name)
			Expect(secondVR).NotTo(BeNil())
			Expect(secondVR.Name).NotTo(Equal(firstVR.Name))
		})

		It("creates no ValidationRequest when batch becomes empty", func() {
			node1 := newEligibleNode("node1-" + suffix)
			reconciler := newNodeValidationTestSetup(ctx, nodeValidationTestCase{nodes: []*corev1.Node{node1}})

			reconcileNode(ctx, reconciler, node1.Name)
			Expect(reconciler.nodesInBatch).To(HaveKey(node1.Name))
			removeGPULabel(ctx, node1)

			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")

			Expect(findValidationRequestForNodes(ctx, node1.Name)).To(BeNil())
			Expect(nodeCondition(ctx, node1.Name)).To(BeNil())
		})

		It("does not add ineligible nodes to a pending batch", func() {
			persist := newEligibleNode("persist-" + suffix)
			labelFail := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "label-fail-" + suffix}}
			conditionFail := newEligibleNode("condition-fail-" + suffix)

			reconciler := newNodeValidationTestSetup(ctx, nodeValidationTestCase{
				nodes: []*corev1.Node{persist, labelFail, conditionFail},
			})
			markAlreadyValidated(ctx, conditionFail)

			reconcileNode(ctx, reconciler, persist.Name)
			reconcileNode(ctx, reconciler, labelFail.Name)
			reconcileNode(ctx, reconciler, conditionFail.Name)

			Expect(reconciler.nodesInBatch).NotTo(HaveKey(labelFail.Name))
			Expect(reconciler.nodesInBatch).NotTo(HaveKey(conditionFail.Name))
			Expect(reconciler.nodesInBatch).To(HaveKey(persist.Name))

			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")

			vr := findValidationRequestForNodes(ctx, persist.Name)
			Expect(vr).NotTo(BeNil())
			Expect(nodeCondition(ctx, persist.Name).Status).To(Equal(corev1.ConditionTrue))
			Expect(nodeCondition(ctx, labelFail.Name)).To(BeNil())
			Expect(nodeCondition(ctx, conditionFail.Name).Reason).To(Equal("PreExisting"),
				"the controller should never re-touch a node whose condition is already True")
		})

		It("removes ineligible nodes from a pending batch before it is flushed", func() {
			persist := newEligibleNode("persist-" + suffix)
			labelFail := newEligibleNode("label-fail-" + suffix)
			conditionFail := newEligibleNode("condition-fail-" + suffix)

			reconciler := newNodeValidationTestSetup(ctx, nodeValidationTestCase{
				nodes: []*corev1.Node{persist, labelFail, conditionFail},
			})

			reconcileNode(ctx, reconciler, persist.Name)
			reconcileNode(ctx, reconciler, labelFail.Name)
			reconcileNode(ctx, reconciler, conditionFail.Name)

			Expect(reconciler.nodesInBatch).To(HaveKey(persist.Name))
			Expect(reconciler.nodesInBatch).To(HaveKey(labelFail.Name))
			Expect(reconciler.nodesInBatch).To(HaveKey(conditionFail.Name))

			removeGPULabel(ctx, labelFail)
			reconcileNode(ctx, reconciler, labelFail.Name)
			markAlreadyValidated(ctx, conditionFail)
			reconcileNode(ctx, reconciler, conditionFail.Name)

			Expect(reconciler.nodesInBatch).NotTo(HaveKey(labelFail.Name))
			Expect(reconciler.nodesInBatch).NotTo(HaveKey(conditionFail.Name))
			Expect(reconciler.nodesInBatch).To(HaveKey(persist.Name))

			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")

			vr := findValidationRequestForNodes(ctx, persist.Name)
			Expect(vr).NotTo(BeNil())
			Expect(nodeCondition(ctx, persist.Name).Status).To(Equal(corev1.ConditionTrue))
			Expect(nodeCondition(ctx, labelFail.Name)).To(BeNil())
			Expect(nodeCondition(ctx, conditionFail.Name).Reason).To(Equal("PreExisting"))
		})

		It("removes ineligible nodes from the batch at flush time", func() {
			persist := newEligibleNode("persist-" + suffix)
			labelFail := newEligibleNode("label-fail-" + suffix)
			conditionFail := newEligibleNode("condition-fail-" + suffix)

			reconciler := newNodeValidationTestSetup(ctx, nodeValidationTestCase{
				nodes: []*corev1.Node{persist, labelFail, conditionFail},
			})

			reconcileNode(ctx, reconciler, persist.Name)
			reconcileNode(ctx, reconciler, labelFail.Name)
			reconcileNode(ctx, reconciler, conditionFail.Name)

			Expect(reconciler.nodesInBatch).To(HaveKey(persist.Name))
			Expect(reconciler.nodesInBatch).To(HaveKey(labelFail.Name))
			Expect(reconciler.nodesInBatch).To(HaveKey(conditionFail.Name))

			removeGPULabel(ctx, labelFail)
			markAlreadyValidated(ctx, conditionFail)

			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")

			vr := findValidationRequestForNodes(ctx, persist.Name)
			Expect(vr).NotTo(BeNil())
			Expect(nodeCondition(ctx, persist.Name).Status).To(Equal(corev1.ConditionTrue))
			Expect(nodeCondition(ctx, labelFail.Name)).To(BeNil())
			Expect(nodeCondition(ctx, conditionFail.Name).Reason).To(Equal("PreExisting"))
		})

		It("adds eligible node when criteria is empty", func() {
			cfg := defaultNodeValidationConfig()
			cfg.Validation.Spec.NewNodeValidation.Criteria = nil

			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-" + suffix}}
			reconciler := newNodeValidationTestSetup(ctx, nodeValidationTestCase{config: cfg, nodes: []*corev1.Node{node}})

			reconcileNode(ctx, reconciler, node.Name)
			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")

			Expect(findValidationRequestForNodes(ctx, node.Name)).NotTo(BeNil())
		})

		It("records a successful batch in metrics", func() {
			nodeName := "node-" + suffix
			reconciler := newNodeValidationTestSetup(ctx, nodeValidationTestCase{
				nodes: []*corev1.Node{newEligibleNode(nodeName)},
			})

			successBefore := counterValue(metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusSuccess))
			failureBefore := counterValue(metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusFailure))
			sizeCountBefore := histogramSampleCount(metrics.NewNodeValidationBatchSize)

			reconcileNode(ctx, reconciler, nodeName)
			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")

			Expect(findValidationRequestForNodes(ctx, nodeName)).NotTo(BeNil())

			successDelta := counterValue(metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusSuccess)) - successBefore
			failureDelta := counterValue(metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusFailure)) - failureBefore
			sizeCountDelta := histogramSampleCount(metrics.NewNodeValidationBatchSize) - sizeCountBefore
			Expect(successDelta).To(Equal(float64(1)))
			Expect(failureDelta).To(Equal(float64(0)))
			Expect(sizeCountDelta).To(Equal(uint64(1)))
		})
	})

	Context("node deletion with a pending batch", func() {
		It("removes a deleted node from a pending batch before it is flushed", func() {
			node := newEligibleNode("node-" + suffix)
			reconciler := newNodeValidationTestSetup(ctx, nodeValidationTestCase{nodes: []*corev1.Node{node}})

			reconcileNode(ctx, reconciler, node.Name)
			Expect(reconciler.nodesInBatch).To(HaveKey(node.Name))

			Expect(k8sClient.Delete(ctx, node)).To(Succeed())
			reconcileNode(ctx, reconciler, node.Name)
			Expect(reconciler.nodesInBatch).NotTo(HaveKey(node.Name))

			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")

			Expect(findValidationRequestForNodes(ctx, node.Name)).To(BeNil())
		})

		It("removes a deleted node from a pending batch before when it is flushed", func() {
			node1, node2 := newEligibleNode("node1-"+suffix), newEligibleNode("node2-"+suffix)
			reconciler := newNodeValidationTestSetup(ctx, nodeValidationTestCase{nodes: []*corev1.Node{node1, node2}})

			reconcileNode(ctx, reconciler, node1.Name)
			reconcileNode(ctx, reconciler, node2.Name)

			Expect(reconciler.nodesInBatch).To(HaveKey(node1.Name))
			Expect(reconciler.nodesInBatch).To(HaveKey(node2.Name))

			Expect(k8sClient.Delete(ctx, node2)).To(Succeed())
			time.Sleep(nodeValidationBatchWait)
			reconcileNode(ctx, reconciler, "flush-trigger")

			vr := findValidationRequestForNodes(ctx, node1.Name)
			Expect(vr).NotTo(BeNil())
			Expect(nodeCondition(ctx, node1.Name).Status).To(Equal(corev1.ConditionTrue))
		})
	})

	Context("retrying on failures", func() {
		It("recovers after encountering an AlreadyExists error on ValidationRequest creation", func() {
			node := newEligibleNode("node-" + suffix)

			createFailuresRemaining := 2
			watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			interceptedClient := interceptor.NewClient(watchClient, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*v1alpha1.ValidationRequest); ok && createFailuresRemaining > 0 {
						createFailuresRemaining--
						_ = c.Create(ctx, obj, opts...)

						return apierrors.NewInternalError(fmt.Errorf("injected create failure"))
					}

					return c.Create(ctx, obj, opts...)
				},
			})

			reconciler, err := NewNodeValidationReconciler(interceptedClient, interceptedClient, scheme.Scheme, defaultNodeValidationConfig())
			Expect(err).NotTo(HaveOccurred())

			Expect(interceptedClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() { cleanupNodeValidationTest(ctx, []string{node.Name}) })

			reconcileNode(ctx, reconciler, node.Name)
			time.Sleep(nodeValidationBatchWait)

			successBefore := counterValue(metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusSuccess))
			failureBefore := counterValue(metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusFailure))

			reconcileNodeExpectError(ctx, reconciler, "flush-trigger")
			Expect(reconciler.nodesInBatch).To(HaveKey(node.Name))

			reconcileNodeExpectError(ctx, reconciler, "flush-trigger")
			Expect(reconciler.nodesInBatch).To(HaveKey(node.Name))

			reconcileNode(ctx, reconciler, "flush-trigger")
			Expect(reconciler.nodesInBatch).To(BeNil())

			var list v1alpha1.ValidationRequestList
			Expect(k8sClient.List(ctx, &list)).To(Succeed())

			matching := 0

			for _, vr := range list.Items {
				if len(vr.Spec.Nodes) == 1 && vr.Spec.Nodes[0].Name == node.Name {
					matching++
				}
			}

			Expect(matching).To(Equal(1), "exactly one ValidationRequest should exist")
			Expect(nodeCondition(ctx, node.Name).Status).To(Equal(corev1.ConditionTrue))

			successDelta := counterValue(metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusSuccess)) - successBefore
			failureDelta := counterValue(metrics.NewNodeValidationBatchesTotal.WithLabelValues(metrics.StatusFailure)) - failureBefore
			Expect(successDelta).To(Equal(float64(1)))
			Expect(failureDelta).To(Equal(float64(2)))
		})

		It("retries updating the newNodeValidation.condition for all nodes in the batch after a failure", func() {
			node := newEligibleNode("node-" + suffix)

			var failPatchesPermanently bool

			watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())

			interceptedClient := interceptor.NewClient(watchClient, interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object,
					patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if subResourceName == "status" && failPatchesPermanently {
						if _, ok := obj.(*corev1.Node); ok {
							return apierrors.NewConflict(corev1.Resource("nodes"), node.Name, fmt.Errorf("injected conflict"))
						}
					}

					return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
				},
			})

			reconciler, err := NewNodeValidationReconciler(interceptedClient, interceptedClient, scheme.Scheme, defaultNodeValidationConfig())
			Expect(err).NotTo(HaveOccurred())

			Expect(interceptedClient.Create(ctx, node)).To(Succeed())
			DeferCleanup(func() { cleanupNodeValidationTest(ctx, []string{node.Name}) })

			reconcileNode(ctx, reconciler, node.Name)
			time.Sleep(nodeValidationBatchWait)

			failPatchesPermanently = true
			reconcileNodeExpectError(ctx, reconciler, "flush-trigger")
			Expect(findValidationRequestForNodes(ctx, node.Name)).NotTo(BeNil())
			Expect(nodeCondition(ctx, node.Name)).To(BeNil())
			Expect(reconciler.nodesInBatch).To(HaveKey(node.Name))

			failPatchesPermanently = false
			reconcileNode(ctx, reconciler, "flush-trigger")
			Expect(nodeCondition(ctx, node.Name).Status).To(Equal(corev1.ConditionTrue))
			Expect(reconciler.nodesInBatch).To(BeNil())
		})

		It("uses the node batch defined in the ValidationRequest after encountering an AlreadyExists error", func() {
			node1, node2 := newEligibleNode("node1-"+suffix), newEligibleNode("node2-"+suffix)

			var createFailuresRemaining int

			watchClient, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			interceptedClient := interceptor.NewClient(watchClient, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*v1alpha1.ValidationRequest); ok && createFailuresRemaining > 0 {
						createFailuresRemaining--
						_ = c.Create(ctx, obj, opts...)

						return apierrors.NewInternalError(fmt.Errorf("injected create failure"))
					}

					return c.Create(ctx, obj, opts...)
				},
			})

			reconciler, err := NewNodeValidationReconciler(interceptedClient, interceptedClient, scheme.Scheme, defaultNodeValidationConfig())
			Expect(err).NotTo(HaveOccurred())

			Expect(interceptedClient.Create(ctx, node1)).To(Succeed())
			Expect(interceptedClient.Create(ctx, node2)).To(Succeed())
			DeferCleanup(func() { cleanupNodeValidationTest(ctx, []string{node1.Name, node2.Name}) })

			reconcileNode(ctx, reconciler, node1.Name)
			reconcileNode(ctx, reconciler, node2.Name)
			time.Sleep(nodeValidationBatchWait)

			createFailuresRemaining = 1
			reconcileNodeExpectError(ctx, reconciler, "flush-trigger")
			Expect(nodeCondition(ctx, node1.Name)).To(BeNil())
			Expect(nodeCondition(ctx, node2.Name)).To(BeNil())

			removeGPULabel(ctx, node2)
			reconcileNode(ctx, reconciler, "flush-trigger")

			vr := findValidationRequestForNodes(ctx, node1.Name, node2.Name)
			Expect(vr).NotTo(BeNil())
			Expect(nodeCondition(ctx, node1.Name).Status).To(Equal(corev1.ConditionTrue))
			Expect(nodeCondition(ctx, node2.Name).Status).To(Equal(corev1.ConditionTrue))
		})
	})
})

func newEligibleNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.Now(),
			Labels:            map[string]string{"nvidia.com/gpu.present": "true"},
		},
	}
}

func markAlreadyValidated(ctx context.Context, node *corev1.Node) {
	node.Status.Conditions = []corev1.NodeCondition{
		{
			Type: "NewNodeValidated", Status: corev1.ConditionTrue, Reason: "PreExisting",
			LastHeartbeatTime: metav1.Now(), LastTransitionTime: metav1.Now(),
		},
	}
	Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())
}

func removeGPULabel(ctx context.Context, node *corev1.Node) {
	var current corev1.Node
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: node.Name}, &current)).To(Succeed())
	delete(current.Labels, "nvidia.com/gpu.present")
	Expect(k8sClient.Update(ctx, &current)).To(Succeed())
}

func findValidationRequestForNodes(ctx context.Context, nodeNames ...string) *v1alpha1.ValidationRequest {
	var list v1alpha1.ValidationRequestList
	Expect(k8sClient.List(ctx, &list)).To(Succeed())

	for i := range list.Items {
		vr := &list.Items[i]

		got := make([]string, len(vr.Spec.Nodes))
		for j, n := range vr.Spec.Nodes {
			got[j] = n.Name
		}

		if ok, _ := ConsistOf(nodeNames).Match(got); ok {
			return vr
		}
	}

	return nil
}

func nodeCondition(ctx context.Context, name string) *corev1.NodeCondition {
	var node corev1.Node
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &node)).To(Succeed())

	return findNodeCondition(&node, "NewNodeValidated")
}

func findNodeCondition(node *corev1.Node, conditionType string) *corev1.NodeCondition {
	for i := range node.Status.Conditions {
		if string(node.Status.Conditions[i].Type) == conditionType {
			return &node.Status.Conditions[i]
		}
	}

	return nil
}

func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		return 0
	}

	return m.GetCounter().GetValue()
}

func histogramSampleCount(h prometheus.Histogram) uint64 {
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		return 0
	}

	return m.GetHistogram().GetSampleCount()
}
