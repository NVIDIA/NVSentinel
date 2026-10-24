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
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
)

// The controller never reads this annotation itself. The tests write it because the not-under-quarantine readiness
// criterion of the fixture evaluates it, as the default configuration does.
const releaseQuarantineAnnotation = "quarantineHealthEvent"

const releaseQuarantineEvent = `{"new-fatal-event":{}}`

const releaseEvalLabel = "release-eval"

var releaseGateTaint = corev1.Taint{Key: "test.example/quarantine", Effect: corev1.TaintEffectNoSchedule}

// releaseEvalCriteria fails to evaluate, instead of evaluating to false, once the label is removed from the node.
var releaseEvalCriteria = []v1alpha1.CriteriaSpec{
	{
		Name:       "release-eval",
		Expression: `node.metadata.labels["` + releaseEvalLabel + `"] == "true"`,
	},
}

var errReleaseNodeRead = errors.New("injected Node read failure")

// Every operation delegates to envtest's real API server. The hooks schedule another controller's Node update at a
// precise point of the reconcile without changing the reconciler.
type releaseRaceClient struct {
	client.Client
	afterDelete      func(context.Context, client.Object) error
	beforePatch      func(context.Context, client.Object) error
	failNodeReads    bool
	quarantinedReads int
	conflicts        int
}

func (c *releaseRaceClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Node); ok && c.failNodeReads {
		return errReleaseNodeRead
	}
	err := c.Client.Get(ctx, key, obj, opts...)
	if node, ok := obj.(*corev1.Node); err == nil && ok {
		if _, exists := node.Annotations[releaseQuarantineAnnotation]; exists {
			c.quarantinedReads++
		}
	}
	return err
}

func (c *releaseRaceClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if err := c.Client.Delete(ctx, obj, opts...); err != nil {
		return err
	}
	if c.afterDelete != nil {
		return c.afterDelete(ctx, obj)
	}
	return nil
}

func (c *releaseRaceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch,
	opts ...client.PatchOption) error {
	if c.beforePatch != nil {
		if err := c.beforePatch(ctx, obj); err != nil {
			return err
		}
	}
	err := c.Client.Patch(ctx, obj, patch, opts...)
	if apierrors.IsConflict(err) {
		c.conflicts++
	}
	return err
}

// Hold a snapshot read from envtest to model a delayed Node informer update. All writes and other reads still use the
// real API server, including APIReader.
type releaseLaggingNodeClient struct {
	client.Client
	snapshot   *corev1.Node
	staleReads int
}

func (c *releaseLaggingNodeClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if node, ok := obj.(*corev1.Node); ok && c.snapshot != nil && key.Name == c.snapshot.Name {
		c.snapshot.DeepCopyInto(node)
		c.staleReads++
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

type releaseFixtureOptions struct {
	// Readiness criteria evaluated in addition to the default test criterion and not-under-quarantine.
	extraCriteria []v1alpha1.CriteriaSpec
	// Labels applied to every node before the request starts.
	nodeLabels map[string]string
	// Raise the batch minimum above the node count so that the only test is skipped and the request completes
	// from Pending without running a test.
	skipTests bool
}

type releaseFixture struct {
	reconciler *ValidationRequestReconciler
	request    reconcile.Request
	client     *releaseRaceClient
	config     *config.Config
	nodeNames  []string
}

// newReleaseFixture creates cordoned and tainted nodes, a request targeting all of them, and a reconciler whose
// scheduling gate removes both. The provider has no retries, so an attempt failure is terminal.
func newReleaseFixture(name string, opts releaseFixtureOptions, nodeNames ...string) *releaseFixture {
	testConfig := defaultTestConfig()
	testConfig.Validation.Spec.ReadinessCriteria = append(
		testConfig.Validation.Spec.ReadinessCriteria, notUnderQuarantineCriteria...)
	testConfig.Validation.Spec.ReadinessCriteria = append(
		testConfig.Validation.Spec.ReadinessCriteria, opts.extraCriteria...)
	testConfig.Validation.Spec.SchedulingGate = &v1alpha1.SchedulingGateConfig{
		Cordon: v1alpha1.CordonConfig{Remove: true},
		Taints: []v1alpha1.TaintConfig{{Key: releaseGateTaint.Key, Effect: releaseGateTaint.Effect, Remove: true}},
	}
	if opts.skipTests {
		testCfg := testConfig.Validation.Spec.Tests["basic"]
		testCfg.MinimumNodesPerBatch = len(nodeNames) + 1
		testCfg.BatchFailurePolicy = v1alpha1.BatchFailurePolicyIgnore
		testConfig.Validation.Spec.Tests["basic"] = testCfg
	}
	var nodeSpecs []v1alpha1.NodeSpec
	for _, name := range nodeNames {
		nodeSpecs = append(nodeSpecs, v1alpha1.NodeSpec{Name: name})
	}
	raceClient := &releaseRaceClient{Client: k8sClient}
	r, req := newValidationRequestTestSetup(ctx, name, validationRequestTestCase{
		config:    testConfig,
		client:    raceClient,
		nodeNames: nodeNames,
		spec:      v1alpha1.ValidationRequestSpec{Nodes: nodeSpecs},
		beforeInit: func(ctx context.Context, _ *v1alpha1.ValidationRequest) error {
			for _, nodeName := range nodeNames {
				if err := cordonNode(ctx, nodeName); err != nil {
					return err
				}
				if err := addNodeTaint(ctx, nodeName, releaseGateTaint); err != nil {
					return err
				}
				for key, value := range opts.nodeLabels {
					if err := patchNodeLabel(ctx, nodeName, key, value); err != nil {
						return err
					}
				}
			}
			return nil
		},
	})
	DeferCleanup(func() { raceClient.afterDelete = nil; raceClient.beforePatch = nil })
	return &releaseFixture{reconciler: r, request: req, client: raceClient, config: testConfig, nodeNames: nodeNames}
}

func (f *releaseFixture) running() v1alpha1.ValidationRequest {
	return reconcileUntilPhase(ctx, f.reconciler, f.request, v1alpha1.PhaseRunning, maxReconcileIterations)
}

func (f *releaseFixture) reconcile() v1alpha1.ValidationRequest {
	return reconcileForIterations(ctx, f.reconciler, f.request, 1)
}

// afterTestDeleted runs fn once, after the provider resource of a completed test group is deleted. That is after the
// last readiness check of the running group and before the release.
func (f *releaseFixture) afterTestDeleted(fn func(ctx context.Context) error) *bool {
	fired := false
	f.client.afterDelete = func(ctx context.Context, obj client.Object) error {
		if fired || obj.GetObjectKind().GroupVersionKind() != testJobGVK {
			return nil
		}
		fired = true
		return fn(ctx)
	}
	return &fired
}

// beforeReleasePatch runs fn once, immediately before the patch that would uncordon the named node is sent.
func (f *releaseFixture) beforeReleasePatch(nodeName string, fn func(ctx context.Context) error) *bool {
	fired := false
	f.client.beforePatch = func(ctx context.Context, obj client.Object) error {
		node, ok := obj.(*corev1.Node)
		if fired || !ok || node.Name != nodeName || node.Spec.Unschedulable {
			return nil
		}
		fired = true
		return fn(ctx)
	}
	return &fired
}

// beforeSessionPatch runs fn once, immediately before the patch that adds the request to the session of the named
// node is sent. The pending readiness check used the Node read before this patch, so a change made here is first
// observed by the release check.
func (f *releaseFixture) beforeSessionPatch(nodeName string, fn func(ctx context.Context) error) *bool {
	fired := false
	f.client.beforePatch = func(ctx context.Context, obj client.Object) error {
		node, ok := obj.(*corev1.Node)
		if fired || !ok || node.Name != nodeName || len(node.Annotations[annotationValidationSession]) == 0 {
			return nil
		}
		fired = true
		return fn(ctx)
	}
	return &fired
}

func setQuarantine(ctx context.Context, nodeName, value string) error {
	var node corev1.Node
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return err
	}
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[releaseQuarantineAnnotation] = value
	node.Spec.Unschedulable = true
	return k8sClient.Update(ctx, &node)
}

func clearQuarantine(nodeName string) {
	node := getNode(ctx, nodeName)
	delete(node.Annotations, releaseQuarantineAnnotation)
	Expect(k8sClient.Update(ctx, &node)).To(Succeed())
}

func expectGateRetained(nodeName string) {
	GinkgoHelper()
	node := getNode(ctx, nodeName)
	Expect(node.Spec.Unschedulable).To(BeTrue(), "node %q must stay cordoned", nodeName)
	Expect(node.Spec.Taints).To(ContainElement(releaseGateTaint))
}

func expectGateReleased(nodeName string) {
	GinkgoHelper()
	node := getNode(ctx, nodeName)
	Expect(node.Spec.Unschedulable).To(BeFalse(), "node %q must be uncordoned", nodeName)
	Expect(node.Spec.Taints).NotTo(ContainElement(releaseGateTaint))
	Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
}

func releaseBlockedCondition(vr v1alpha1.ValidationRequest) *metav1.Condition {
	return meta.FindStatusCondition(vr.Status.Conditions, conditionTypeReleaseBlocked)
}

// expectReleaseBlocked checks that the request failed at release: every test group succeeded and the ReleaseBlocked
// condition names exactly the given violations, in request node order.
func expectReleaseBlocked(vr v1alpha1.ValidationRequest, violations ...readinessViolation) {
	GinkgoHelper()
	Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
	Expect(vr.Status.CompletionTime).NotTo(BeNil())
	for _, group := range vr.Status.TestGroups {
		Expect(group.Phase).To(Equal(v1alpha1.PhaseSucceeded), "test group %q keeps its result", group.Name)
	}
	cond := releaseBlockedCondition(vr)
	Expect(cond).NotTo(BeNil(), "the ReleaseBlocked condition explains the failed request")
	Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	Expect(cond.Reason).To(Equal(string(v1alpha1.FailureReasonNodeReadinessViolation)))
	Expect(cond.Message).To(Equal((&releaseBlockedError{violations: violations}).message()))
}

func expectAttemptFailedReadiness(vr v1alpha1.ValidationRequest, nodeNames ...string) {
	GinkgoHelper()
	Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
	Expect(vr.Status.TestGroups).To(HaveLen(1))
	group := vr.Status.TestGroups[0]
	Expect(group.Phase).To(Equal(v1alpha1.PhaseFailed))
	Expect(group.Attempts).To(HaveLen(1))
	Expect(group.Attempts[0].Phase).To(Equal(v1alpha1.PhaseFailed))
	Expect(group.Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonNodeReadinessViolation))
	Expect(group.Attempts[0].FailedNodes).To(ConsistOf(nodeNames))
	Expect(releaseBlockedCondition(vr)).To(BeNil())
}

var _ = Describe("Validation release readiness check", func() {
	DescribeTable("retains the gate and fails the request when a quarantine starts before release",
		func(timing, annotation string) {
			nodeName := "release-" + timing
			fixture := newReleaseFixture("validation-"+timing, releaseFixtureOptions{}, nodeName)
			vr := fixture.running()
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			var fired *bool
			switch timing {
			case "during-test":
				Expect(setQuarantine(ctx, nodeName, annotation)).To(Succeed())
			case "after-readiness", "empty-annotation":
				fired = fixture.afterTestDeleted(func(ctx context.Context) error {
					return setQuarantine(ctx, nodeName, annotation)
				})
			case "patch-conflict":
				fired = fixture.beforeReleasePatch(nodeName, func(ctx context.Context) error {
					return setQuarantine(ctx, nodeName, annotation)
				})
			}
			vr = fixture.reconcile()
			if fired != nil {
				Expect(*fired).To(BeTrue())
			}
			node := getNode(ctx, nodeName)
			switch timing {
			case "healthy":
				Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
				Expect(releaseBlockedCondition(vr)).To(BeNil())
				expectGateReleased(nodeName)
				checkNodeValidationState(ctx, vr.Name, []string{nodeName}, v1alpha1.PhaseSucceeded)
				return
			case "during-test":
				// The running group fails its readiness check, which is the existing retry path.
				expectAttemptFailedReadiness(vr, nodeName)
			case "patch-conflict":
				Expect(fixture.client.conflicts).To(BeNumerically(">", 0), "the version-protected patch must conflict")
				expectReleaseBlocked(vr, readinessViolation{node: nodeName, criterion: "not-under-quarantine"})
			default:
				expectReleaseBlocked(vr, readinessViolation{node: nodeName, criterion: "not-under-quarantine"})
			}
			Expect(node.Annotations).To(HaveKeyWithValue(releaseQuarantineAnnotation, annotation))
			Expect(fixture.client.quarantinedReads).To(BeNumerically(">", 0))
			expectGateRetained(nodeName)
			checkNodeValidationState(ctx, vr.Name, []string{nodeName}, v1alpha1.PhaseFailed)
		},
		Entry("healthy control releases normally", "healthy", releaseQuarantineEvent),
		Entry("quarantine during the test fails the attempt", "during-test", releaseQuarantineEvent),
		Entry("quarantine after the last readiness check blocks the release", "after-readiness", releaseQuarantineEvent),
		Entry("quarantine between the read and the patch blocks the release", "patch-conflict", releaseQuarantineEvent),
		Entry("an empty annotation value still blocks the release", "empty-annotation", ""),
	)

	It("evaluates every configured criterion at release, not only the quarantine annotation", func() {
		nodeName := "release-label-node"
		fixture := newReleaseFixture("release-label", releaseFixtureOptions{}, nodeName)
		vr := fixture.running()
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		fired := fixture.afterTestDeleted(func(ctx context.Context) error {
			return removeNodeLabel(ctx, nodeName, "ready")
		})

		vr = fixture.reconcile()
		Expect(*fired).To(BeTrue())
		Expect(getNode(ctx, nodeName).Annotations).NotTo(HaveKey(releaseQuarantineAnnotation))
		expectReleaseBlocked(vr, readinessViolation{node: nodeName, criterion: "test-criterion"})
		expectGateRetained(nodeName)
		checkNodeValidationState(ctx, vr.Name, []string{nodeName}, v1alpha1.PhaseFailed)
	})

	It("fails the request from the live Node when the cached Node lags behind a quarantine", func() {
		nodeName := "release-cache-node"
		fixture := newReleaseFixture("release-cache", releaseFixtureOptions{}, nodeName)
		vr := fixture.running()
		snapshot := getNode(ctx, nodeName)
		Expect(snapshot.Annotations).NotTo(HaveKey(releaseQuarantineAnnotation))
		lagging := &releaseLaggingNodeClient{Client: fixture.client, snapshot: snapshot.DeepCopy()}
		fixture.reconciler.Client = lagging
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		fired := fixture.afterTestDeleted(func(ctx context.Context) error {
			return setQuarantine(ctx, nodeName, releaseQuarantineEvent)
		})

		vr = fixture.reconcile()
		Expect(*fired).To(BeTrue())
		Expect(lagging.staleReads).To(BeNumerically(">", 0), "the running check read the stale Node")
		Expect(fixture.client.quarantinedReads).To(BeNumerically(">", 0), "the release check read the live Node")
		expectReleaseBlocked(vr, readinessViolation{node: nodeName, criterion: "not-under-quarantine"})
		expectGateRetained(nodeName)
	})

	DescribeTable("releases nothing and retries when readiness cannot be evaluated at release",
		func(name string, opts releaseFixtureOptions, inject func(fixture *releaseFixture, nodeName string) *bool,
			restore func(fixture *releaseFixture, nodeName string), matchErr func(error) bool) {
			nodeName := "release-" + name + "-node"
			opts.skipTests = true
			fixture := newReleaseFixture("release-"+name, opts, nodeName)
			vr := reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhasePending,
				maxReconcileIterations)
			Expect(vr.Status.TestGroups).To(BeEmpty())
			fired := inject(fixture, nodeName)

			for range 2 {
				_, err := fixture.reconciler.Reconcile(ctx, fixture.request)
				Expect(err).To(HaveOccurred())
				Expect(matchErr(err)).To(BeTrue(), "unexpected error: %v", err)
				Expect(k8sClient.Get(ctx, fixture.request.NamespacedName, &vr)).To(Succeed())
				Expect(vr.Status.Phase).To(Equal(v1alpha1.PhasePending))
				Expect(releaseBlockedCondition(vr)).To(BeNil())
				expectGateRetained(nodeName)
			}
			Expect(*fired).To(BeTrue())

			restore(fixture, nodeName)
			vr = reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhaseSucceeded,
				maxReconcileIterations)
			Expect(vr.Status.Skipped).NotTo(BeNil())
			Expect(vr.Status.Skipped.Tests).To(ConsistOf("basic"))
			expectGateReleased(nodeName)
		},
		Entry("the live Node read fails", "read-error", releaseFixtureOptions{},
			func(fixture *releaseFixture, nodeName string) *bool {
				return fixture.beforeSessionPatch(nodeName, func(context.Context) error {
					fixture.client.failNodeReads = true
					return nil
				})
			},
			func(fixture *releaseFixture, _ string) { fixture.client.failNodeReads = false },
			func(err error) bool { return errors.Is(err, errReleaseNodeRead) },
		),
		Entry("a criterion fails to evaluate", "eval-error",
			releaseFixtureOptions{extraCriteria: releaseEvalCriteria, nodeLabels: map[string]string{releaseEvalLabel: "true"}},
			func(fixture *releaseFixture, nodeName string) *bool {
				return fixture.beforeSessionPatch(nodeName, func(ctx context.Context) error {
					return removeNodeLabel(ctx, nodeName, releaseEvalLabel)
				})
			},
			func(_ *releaseFixture, nodeName string) {
				Expect(patchNodeLabel(ctx, nodeName, releaseEvalLabel, "true")).To(Succeed())
			},
			func(err error) bool { return strings.Contains(err.Error(), `criterion "release-eval"`) },
		),
	)

	It("fails a request that completes from Pending when the node is not ready at release", func() {
		nodeName := "release-pending-node"
		fixture := newReleaseFixture("release-pending", releaseFixtureOptions{skipTests: true}, nodeName)
		vr := reconcileUntilPhase(ctx, fixture.reconciler, fixture.request, v1alpha1.PhasePending, maxReconcileIterations)
		Expect(vr.Status.TestGroups).To(BeEmpty())
		// The pending readiness check passes on the Node read before the session patch; the quarantine is
		// first observed by the release check.
		fired := fixture.beforeSessionPatch(nodeName, func(ctx context.Context) error {
			return setQuarantine(ctx, nodeName, releaseQuarantineEvent)
		})

		vr = fixture.reconcile()
		Expect(*fired).To(BeTrue())
		Expect(vr.Status.TestGroups).To(BeEmpty(), "the only test is skipped, so no group runs")
		Expect(vr.Status.Skipped).NotTo(BeNil())
		Expect(vr.Status.Skipped.Tests).To(ConsistOf("basic"))
		expectReleaseBlocked(vr, readinessViolation{node: nodeName, criterion: "not-under-quarantine"})
		expectGateRetained(nodeName)
		checkNodeValidationState(ctx, vr.Name, []string{nodeName}, v1alpha1.PhaseFailed)

		// Deleting the failed request once the node is ready again releases the gate.
		clearQuarantine(nodeName)
		Expect(k8sClient.Delete(ctx, &vr)).To(Succeed())
		_, err := fixture.reconciler.Reconcile(ctx, fixture.request)
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, fixture.request.NamespacedName,
			&v1alpha1.ValidationRequest{}))).To(BeTrue())
		expectGateReleased(nodeName)
	})

	It("releases no node of a batch when one member is quarantined before release", func() {
		nodeA, nodeB := "release-batch-a", "release-batch-b"
		fixture := newReleaseFixture("release-batch", releaseFixtureOptions{}, nodeA, nodeB)
		vr := fixture.running()
		Expect(vr.Status.TestGroups).To(HaveLen(1))
		Expect(vr.Status.TestGroups[0].Nodes).To(ConsistOf(nodeA, nodeB))
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		fired := fixture.afterTestDeleted(func(ctx context.Context) error {
			return setQuarantine(ctx, nodeB, releaseQuarantineEvent)
		})

		vr = fixture.reconcile()
		Expect(*fired).To(BeTrue())
		expectReleaseBlocked(vr, readinessViolation{node: nodeB, criterion: "not-under-quarantine"})
		expectGateRetained(nodeA)
		expectGateRetained(nodeB)
		checkNodeValidationState(ctx, vr.Name, []string{nodeA, nodeB}, v1alpha1.PhaseFailed)
	})

	It("keeps an already released node released when a later node fails its check", func() {
		nodeA, nodeB := "release-partial-a", "release-partial-b"
		fixture := newReleaseFixture("release-partial", releaseFixtureOptions{}, nodeA, nodeB)
		vr := fixture.running()
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		fired := fixture.beforeReleasePatch(nodeB, func(ctx context.Context) error {
			Expect(getNode(ctx, nodeA).Spec.Unschedulable).To(BeFalse(), "the first member must already be released")
			return setQuarantine(ctx, nodeB, releaseQuarantineEvent)
		})

		vr = fixture.reconcile()
		Expect(*fired).To(BeTrue())
		Expect(fixture.client.conflicts).To(BeNumerically(">", 0))
		expectReleaseBlocked(vr, readinessViolation{node: nodeB, criterion: "not-under-quarantine"})
		expectGateReleased(nodeA)
		expectGateRetained(nodeB)
		checkNodeValidationState(ctx, vr.Name, []string{nodeA}, v1alpha1.PhaseSucceeded)
		checkNodeValidationState(ctx, vr.Name, []string{nodeB}, v1alpha1.PhaseFailed)
	})

	It("lets a later request with the same tests clear the failed entry and release the node", func() {
		nodeName := "release-takeover-node"
		fixture := newReleaseFixture("release-old", releaseFixtureOptions{}, nodeName)
		vr := fixture.running()
		Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
		fired := fixture.afterTestDeleted(func(ctx context.Context) error {
			return setQuarantine(ctx, nodeName, releaseQuarantineEvent)
		})
		vr = fixture.reconcile()
		Expect(*fired).To(BeTrue())
		expectReleaseBlocked(vr, readinessViolation{node: nodeName, criterion: "not-under-quarantine"})

		r, req := newValidationRequestTestSetup(ctx, "release-new", validationRequestTestCase{
			config: fixture.config,
			spec:   v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
		})
		pending := reconcileForIterations(ctx, r, req, 2)
		Expect(pending.Status.Phase).To(Equal(v1alpha1.PhasePending))
		expectGateRetained(nodeName)
		clearQuarantine(nodeName)
		fresh := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, maxReconcileIterations)
		expectGateRetained(nodeName)
		Expect(updateObjectStatusForAllTestGroups(ctx, &fresh, testSuccessCondType)).To(Succeed())
		fresh = reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, maxReconcileIterations)
		expectGateReleased(nodeName)
		checkNodeValidationState(ctx, fresh.Name, []string{nodeName}, v1alpha1.PhaseSucceeded)
	})

	It("retains the gate when the last request is deleted while the node fails readiness", func() {
		nodeName := "release-delete-node"
		fixture := newReleaseFixture("release-delete", releaseFixtureOptions{}, nodeName)
		vr := fixture.running()
		Expect(setQuarantine(ctx, nodeName, releaseQuarantineEvent)).To(Succeed())
		Expect(k8sClient.Delete(ctx, &vr)).To(Succeed())
		_, err := fixture.reconciler.Reconcile(ctx, fixture.request)
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, fixture.request.NamespacedName,
			&v1alpha1.ValidationRequest{}))).To(BeTrue())
		node := getNode(ctx, nodeName)
		Expect(node.Annotations).To(HaveKeyWithValue(releaseQuarantineAnnotation, releaseQuarantineEvent))
		Expect(node.Annotations).NotTo(HaveKey(annotationValidationSession))
		Expect(node.Annotations).NotTo(HaveKey(annotationActiveValidationRequest))
		Expect(node.Labels).NotTo(HaveKey(validationStateLabelKey))
		expectGateRetained(nodeName)

		// The session is gone, so clearing the quarantine does not release the gate. A later request does.
		clearQuarantine(nodeName)
		_, err = fixture.reconciler.Reconcile(ctx, fixture.request)
		Expect(err).NotTo(HaveOccurred())
		expectGateRetained(nodeName)

		r, req := newValidationRequestTestSetup(ctx, "release-delete-replacement", validationRequestTestCase{
			config: fixture.config,
			spec:   v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
		})
		fresh := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, maxReconcileIterations)
		Expect(updateObjectStatusForAllTestGroups(ctx, &fresh, testSuccessCondType)).To(Succeed())
		reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, maxReconcileIterations)
		expectGateReleased(nodeName)
	})

	It("releases the gate when the last request is deleted and the node is ready", func() {
		nodeName := "release-delete-ready-node"
		fixture := newReleaseFixture("release-delete-ready", releaseFixtureOptions{}, nodeName)
		vr := fixture.running()
		Expect(k8sClient.Delete(ctx, &vr)).To(Succeed())
		_, err := fixture.reconciler.Reconcile(ctx, fixture.request)
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, fixture.request.NamespacedName,
			&v1alpha1.ValidationRequest{}))).To(BeTrue())
		expectGateReleased(nodeName)
	})

	It("keeps the gate when a deleted request was not the last session entry", func() {
		nodeName := "release-delete-shared-node"
		fixture := newReleaseFixture("release-delete-shared", releaseFixtureOptions{}, nodeName)
		vr := fixture.running()
		other, otherReq := newValidationRequestTestSetup(ctx, "release-delete-other", validationRequestTestCase{
			config: fixture.config,
			spec:   v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
		})
		pending := reconcileForIterations(ctx, other, otherReq, 2)
		Expect(pending.Status.Phase).To(Equal(v1alpha1.PhasePending))

		Expect(k8sClient.Delete(ctx, &vr)).To(Succeed())
		_, err := fixture.reconciler.Reconcile(ctx, fixture.request)
		Expect(err).NotTo(HaveOccurred())
		expectGateRetained(nodeName)
		node := getNode(ctx, nodeName)
		Expect(node.Annotations).NotTo(HaveKey(annotationActiveValidationRequest))
		entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(ConsistOf(sessionEntry{Name: pending.Name, Tests: []string{"basic"}}))
	})
})
