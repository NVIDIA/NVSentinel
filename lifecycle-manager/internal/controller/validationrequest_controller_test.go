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
	"encoding/json"
	"fmt"
	"text/template"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
)

const (
	testProviderGroup      = "test.nvsentinel.nvidia.com"
	testProviderVersion    = "v1alpha1"
	testProviderKind       = "TestJob"
	testNamespace          = "default"
	testTemplateFile       = "testjob.yaml"
	testSuccessCondType    = "Complete"
	testFailedCondType     = "Failed"
	maxReconcileIterations = 30
)

var (
	testJobGVK = schema.GroupVersionKind{
		Group:   testProviderGroup,
		Version: testProviderVersion,
		Kind:    testProviderKind,
	}
)

const testJobTemplateText = `apiVersion: test.nvsentinel.nvidia.com/v1alpha1
kind: TestJob
spec:
  validationRequestName: "{{.ValidationRequestName}}"
  testGroupName: "{{.TestGroupName}}"
  namespace: "{{.Namespace}}"
  timeout: {{.TimeoutSeconds}}
  image: "{{.Image}}"
  nodes:
{{- range .Nodes}}
  - name: "{{.NodeName}}"
{{- end}}
  command:
{{- range .Command}}
  - "{{.}}"
{{- end}}
  env:
{{- range .Env}}
  - name: "{{.Name}}"
    value: "{{.Value}}"
{{- end}}
  tolerations:
{{- range .Tolerations}}
  - key: "{{.Key}}"
    value: "{{.Value}}"
    operator: "{{.Operator}}"
    effect: "{{.Effect}}"
{{- end}}
`

const expectedTestJob = `
apiVersion: test.nvsentinel.nvidia.com/v1alpha1
kind: TestJob
spec:
  validationRequestName: %s
  testGroupName: %s
  namespace: %s
  timeout: 300
  image: test-image:latest
  nodes:
  - name: %s
  command:
  - bash
  - -c
  - echo hello
  env:
  - name: ENV_KEY
    value: env-val
  tolerations:
  - key: node.kubernetes.io/unschedulable
    value: ""
    operator: Exists
    effect: NoSchedule
  - key: taint-exists
    value: ""
    operator: Exists
    effect: NoSchedule
  - key: taint-equal
    value: val
    operator: Equal
    effect: NoSchedule
`

/*
We support 3 high-level test runners which all leverage a validationRequestTestCase.
1. Use runValidationRequestTest to reconcile the ValidationRequest to a terminal phase.
- Define a validationRequestTestCase
- Call runValidationRequestTest (which itself calls newValidationRequestTestSetup).
- Modification of test state can be accomplished between reconcile calls by configuring hooks: beforeInit, afterInit,
afterPending, and afterRunning (where afterRunning is a list of hooks that are called in order). For example, these hooks
can modify node state, test provider resources, or the ValidationRequest itself.

2. Use reconcileForIterations to reconcile the ValidationRequest a given number of times.
- Define a validationRequestTestCase
- Call newValidationRequestTestSetup
- Call reconcileForIterations one or more times as part of test case

3. Use reconcileUntilPhase to reconcile the ValidationRequest until it reaches a given phase.
- Define a validationRequestTestCase
- Call newValidationRequestTestSetup
- Call reconcileUntilPhase one or more times as part of test case

Additional details for test runners:
- Depending on the test case, it possible to combine options 2 and 3 within the same test case. Furthermore, modifications
to node state, test provider resources, or the ValidationRequest itself can be made between calls to reconcileForIterations
and reconcileUntilPhase.
- Multiple ValidationRequests can also be driven concurrently within a single test case to exercise cross-request interaction.
This can be accomplished by calling newValidationRequestTestSetup one per request to get independent reconcilers and
requests before interleaving calls to reconcileForIterations/reconcileUntilPhase across them.
*/
type validationRequestTestCase struct {
	// Provide a custom ValidationConfiguration for the given test. If not provided, the test will call defaultTestConfig()
	config *config.Config
	// The set of nodes to create prior to running the given test case
	nodeNames []string
	// The ValidationRequest to use for the given test case
	spec v1alpha1.ValidationRequestSpec

	// These test hooks are executed on either phase transitions during reconciliation or between consecutive reconcile
	// calls after a request has entered the Running phase.
	beforeInit   func(ctx context.Context, vr *v1alpha1.ValidationRequest) error
	afterInit    func(ctx context.Context, vr *v1alpha1.ValidationRequest) error
	afterPending func(ctx context.Context, vr *v1alpha1.ValidationRequest) error
	afterRunning []func(ctx context.Context, vr *v1alpha1.ValidationRequest) error
}

func defaultTestConfig() *config.Config {
	tmpl := template.Must(template.New(testTemplateFile).Parse(testJobTemplateText))
	return &config.Config{
		Validation: &v1alpha1.ValidationConfiguration{
			Spec: v1alpha1.ValidationConfigurationSpec{
				MaxConcurrentGroups: 3,
				TemplateMountPath:   "/unused",
				ReadinessCriteria: []v1alpha1.CriteriaSpec{
					{
						Name:       "test-criterion",
						Expression: `has(node.metadata.labels) && "ready" in node.metadata.labels`,
					},
				},
				Providers: map[string]v1alpha1.ProviderConfig{
					"test-provider": {
						APIGroup:       testProviderGroup,
						Version:        testProviderVersion,
						Kind:           testProviderKind,
						Resource:       "testjobs",
						TemplateFile:   testTemplateFile,
						TimeoutSeconds: 300,
						Retries:        0,
						SuccessfulCondition: v1alpha1.ConditionMatch{
							Type:   testSuccessCondType,
							Status: "True",
						},
						FailedCondition: v1alpha1.ConditionMatch{
							Type:   testFailedCondType,
							Status: "True",
						},
					},
				},
				Tests: map[string]v1alpha1.TestConfig{
					"basic": {
						Provider:              "test-provider",
						MinimumNodesPerBatch:  1,
						SupportsBatchingNodes: true,
						BatchFailurePolicy:    v1alpha1.BatchFailurePolicyFail,
						Image:                 "test-image:latest",
						Command:               []string{"bash", "-c", "echo hello"},
						Env:                   []v1alpha1.EnvVarConfig{{Name: "ENV_KEY", Value: "env-val"}},
					},
				},
				DefaultTests: []string{"basic"},
			},
		},
		Templates: map[string]*template.Template{
			testTemplateFile: tmpl,
		},
	}
}

func twoTestConfig(nodeBatching, testBatching bool) *config.Config {
	tmpl := template.Must(template.New(testTemplateFile).Parse(testJobTemplateText))
	return &config.Config{
		Validation: &v1alpha1.ValidationConfiguration{
			Spec: v1alpha1.ValidationConfigurationSpec{
				MaxConcurrentGroups: 10,
				TemplateMountPath:   "/unused",
				Providers: map[string]v1alpha1.ProviderConfig{
					"test-provider": {
						APIGroup:             testProviderGroup,
						Version:              testProviderVersion,
						Kind:                 testProviderKind,
						Resource:             "testjobs",
						TemplateFile:         testTemplateFile,
						TimeoutSeconds:       300,
						Retries:              0,
						SupportsTestBatching: testBatching,
						SuccessfulCondition:  v1alpha1.ConditionMatch{Type: testSuccessCondType, Status: "True"},
						FailedCondition:      v1alpha1.ConditionMatch{Type: testFailedCondType, Status: "True"},
					},
				},
				Tests: map[string]v1alpha1.TestConfig{
					"test-a": {
						Provider:              "test-provider",
						MinimumNodesPerBatch:  1,
						SupportsBatchingNodes: nodeBatching,
						BatchFailurePolicy:    v1alpha1.BatchFailurePolicyFail,
					},
					"test-b": {
						Provider:              "test-provider",
						MinimumNodesPerBatch:  1,
						SupportsBatchingNodes: nodeBatching,
						BatchFailurePolicy:    v1alpha1.BatchFailurePolicyFail,
					},
				},
				DefaultTests: []string{"test-a", "test-b"},
			},
		},
		Templates: map[string]*template.Template{testTemplateFile: tmpl},
	}
}

func newValidationRequestTestSetup(ctx context.Context, vrName string,
	testCase validationRequestTestCase) (*ValidationRequestReconciler, reconcile.Request) {
	cfg := testCase.config
	if cfg == nil {
		cfg = defaultTestConfig()
	}
	reconciler, err := NewValidationRequestReconciler(k8sClient, k8sClient, k8sClient.Scheme(), cfg, testNamespace)
	Expect(err).NotTo(HaveOccurred())
	for _, name := range testCase.nodeNames {
		Expect(k8sClient.Create(ctx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   name,
				Labels: map[string]string{"ready": "true"}},
		})).To(Succeed())
	}
	validationRequest := &v1alpha1.ValidationRequest{
		ObjectMeta: metav1.ObjectMeta{Name: vrName},
		Spec:       testCase.spec,
	}
	Expect(k8sClient.Create(ctx, validationRequest)).To(Succeed())
	DeferCleanup(func() {
		cleanupValidationRequestTest(ctx, vrName, testCase.nodeNames)
	})
	if testCase.beforeInit != nil {
		Expect(testCase.beforeInit(ctx, validationRequest)).To(Succeed())
	}
	return reconciler, reconcile.Request{NamespacedName: types.NamespacedName{Name: vrName}}
}

func runValidationRequestTest(ctx context.Context, vrName string, testCase validationRequestTestCase) v1alpha1.ValidationRequest {
	reconciler, req := newValidationRequestTestSetup(ctx, vrName, testCase)

	var prevPhase v1alpha1.Phase
	afterRunningIdx := 0
	for i := 0; i < maxReconcileIterations; i++ {
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		var current v1alpha1.ValidationRequest
		if err := k8sClient.Get(ctx, req.NamespacedName, &current); apierrors.IsNotFound(err) {
			checkNodeAnnotations(ctx, vrName, testCase.nodeNames, v1alpha1.PhaseSucceeded)
			return v1alpha1.ValidationRequest{}
		} else {
			Expect(err).NotTo(HaveOccurred())
		}

		phase := current.Status.Phase
		if phase == v1alpha1.PhaseSucceeded || phase == v1alpha1.PhaseFailed {
			checkNodeAnnotations(ctx, vrName, testCase.nodeNames, phase)
			normalizeStatus(&current.Status)
			return current
		}

		if phase != prevPhase {
			prevPhase = phase
			if phase == v1alpha1.PhasePending && testCase.afterInit != nil {
				Expect(testCase.afterInit(ctx, &current)).To(Succeed())
			}
			if phase == v1alpha1.PhaseRunning {
				if testCase.afterPending != nil {
					Expect(testCase.afterPending(ctx, &current)).To(Succeed())
				}
				checkNodeAnnotations(ctx, vrName, testCase.nodeNames, phase)
			}
		}
		if phase == v1alpha1.PhaseRunning && len(testCase.afterRunning) > 0 {
			hook := testCase.afterRunning[min(afterRunningIdx, len(testCase.afterRunning)-1)]
			Expect(hook(ctx, &current)).To(Succeed())
			afterRunningIdx++
		}
	}

	Fail(fmt.Sprintf("VR %q did not reach terminal phase within %d iterations", vrName, maxReconcileIterations))
	return v1alpha1.ValidationRequest{}
}

func reconcileForIterations(ctx context.Context, r *ValidationRequestReconciler, req reconcile.Request, n int) v1alpha1.ValidationRequest {
	for i := 0; i < n; i++ {
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
	}
	var vr v1alpha1.ValidationRequest
	if err := k8sClient.Get(ctx, req.NamespacedName, &vr); err != nil && !apierrors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}
	return vr
}

func reconcileUntilPhase(ctx context.Context, r *ValidationRequestReconciler, req reconcile.Request,
	target v1alpha1.Phase, maxIter int) v1alpha1.ValidationRequest {
	for i := 0; i < maxIter; i++ {
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		var vr v1alpha1.ValidationRequest

		getErr := k8sClient.Get(ctx, req.NamespacedName, &vr)
		if apierrors.IsNotFound(getErr) {
			return v1alpha1.ValidationRequest{}
		}

		Expect(getErr).NotTo(HaveOccurred())

		if vr.Status.Phase == target {
			return vr
		}
	}
	Fail(fmt.Sprintf("VR %q did not reach phase %q within %d iterations", req.Name, target, maxIter))
	return v1alpha1.ValidationRequest{}
}

/*
We verify the state of the active-validation-request and validation-session node annotations during the execution of
runValidationRequestTest. The node annotations are checked on request deletion, when the request reaches a terminal
state, and when the request transitions to running. The following checks are made depending on the phase:
- Running: active-validation-request matches current request and session entry exists and is not failed.
- PhaseSucceeded: active-validation-request is empty and no session entry for current request
- PhaseFailed: active-validation-request is empty and session entry exists and is marked failed.
*/
func checkNodeAnnotations(ctx context.Context, vrName string, nodeNames []string, phase v1alpha1.Phase) {
	for _, name := range nodeNames {
		var node corev1.Node
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
			Expect(client.IgnoreNotFound(err)).To(Succeed())
			continue
		}

		entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
		Expect(err).NotTo(HaveOccurred())
		var found *sessionEntry
		for i := range entries {
			if entries[i].Name == vrName {
				found = &entries[i]
				break
			}
		}

		switch phase {
		case v1alpha1.PhaseRunning:
			Expect(node.Annotations[annotationActiveValidationRequest]).To(Equal(vrName),
				"node %q: active-validation-request should equal VR name while running", name)
			Expect(found).NotTo(BeNil(), "node %q: session entry for %q should exist while running", name, vrName)
			Expect(found.Failed).To(BeFalse(), "node %q: session entry for %q should not be marked failed while running", name, vrName)
		case v1alpha1.PhaseSucceeded:
			Expect(node.Annotations[annotationActiveValidationRequest]).To(BeEmpty(),
				"node %q: active-validation-request should be cleared on success", name)
			Expect(found).To(BeNil(), "node %q: session entry for %q should be removed on success", name, vrName)
		case v1alpha1.PhaseFailed:
			Expect(node.Annotations[annotationActiveValidationRequest]).To(BeEmpty(),
				"node %q: active-validation-request should be cleared on failure", name)
			Expect(found).NotTo(BeNil(), "node %q: session entry for %q should remain on failure", name, vrName)
			Expect(found.Failed).To(BeTrue(), "node %q: session entry for %q should be marked failed", name, vrName)
		}
	}
}

func cleanupValidationRequestTest(ctx context.Context, vrName string, nodeNames []string) {
	var vr v1alpha1.ValidationRequest
	err := k8sClient.Get(ctx, types.NamespacedName{Name: vrName}, &vr)
	Expect(client.IgnoreNotFound(err)).To(Succeed())

	if err == nil {
		for _, currentTestGroup := range vr.Status.TestGroups {
			for _, currentAttempt := range currentTestGroup.Attempts {
				if len(currentAttempt.ObjectName) != 0 {
					u := &unstructured.Unstructured{}
					u.SetGroupVersionKind(testJobGVK)
					u.SetName(currentAttempt.ObjectName)
					u.SetNamespace(testNamespace)
					Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, u))).To(Succeed())
				}
			}
		}
		patch := client.MergeFrom(vr.DeepCopy())
		controllerutil.RemoveFinalizer(&vr, finalizerName)
		Expect(client.IgnoreNotFound(k8sClient.Patch(ctx, &vr, patch))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &vr))).To(Succeed())
	}

	for _, name := range nodeNames {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}))).To(Succeed())
	}
}

func normalizeStatus(s *v1alpha1.ValidationRequestStatus) {
	s.StartTime = nil
	s.CompletionTime = nil
	for i := range s.TestGroups {
		for j := range s.TestGroups[i].Attempts {
			s.TestGroups[i].Attempts[j].StartTime = nil
			s.TestGroups[i].Attempts[j].EndTime = nil
		}
	}
}

func updateObjectStatusForAllTestGroups(ctx context.Context, vr *v1alpha1.ValidationRequest, condType string) error {
	for _, g := range vr.Status.TestGroups {
		if g.Phase != v1alpha1.PhaseRunning || len(g.Attempts) == 0 {
			continue
		}
		attempt := g.Attempts[len(g.Attempts)-1]
		if attempt.Phase != v1alpha1.PhaseRunning || len(attempt.ObjectName) == 0 {
			continue
		}
		if err := updateObjectStatus(ctx, attempt.ObjectName, condType); err != nil {
			return err
		}
	}
	return nil
}

func updateObjectStatus(ctx context.Context, name, condType string) error {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(testJobGVK)
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, u); err != nil {
		return client.IgnoreNotFound(err)
	}
	if conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions"); len(conditions) > 0 {
		return nil
	}
	condition := map[string]any{
		"type":               condType,
		"status":             "True",
		"reason":             "TestComplete",
		"message":            "",
		"lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339),
	}
	if err := unstructured.SetNestedSlice(u.Object, []any{condition}, "status", "conditions"); err != nil {
		return err
	}
	return k8sClient.Status().Update(ctx, u)
}

func removeNodeLabel(ctx context.Context, nodeName, key string) error {
	var node corev1.Node
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	patch := client.MergeFrom(node.DeepCopy())
	delete(node.Labels, key)
	return k8sClient.Patch(ctx, &node, patch)
}

func patchNodeLabel(ctx context.Context, nodeName, key, value string) error {
	var node corev1.Node
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}
	node.Labels[key] = value
	return k8sClient.Patch(ctx, &node, patch)
}

func getNode(ctx context.Context, nodeName string) corev1.Node {
	var node corev1.Node
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &node)).To(Succeed())
	return node
}

func cordonNode(ctx context.Context, nodeName string) error {
	var node corev1.Node
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true
	return k8sClient.Patch(ctx, &node, patch)
}

func addNodeTaint(ctx context.Context, nodeName string, taint corev1.Taint) error {
	var node corev1.Node
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return err
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Taints = append(node.Spec.Taints, taint)
	return k8sClient.Patch(ctx, &node, patch)
}

var _ = Describe("ValidationRequest Controller", func() {
	var (
		ctx    context.Context
		suffix string
	)

	BeforeEach(func() {
		ctx = context.Background()
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	})

	Context("group construction", func() {
		It("request for one node and one test", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status).To(Equal(v1alpha1.ValidationRequestStatus{
				Phase: v1alpha1.PhaseSucceeded,
				TestGroups: []v1alpha1.TestGroupStatus{
					{
						Name:     grp,
						Provider: "test-provider",
						Tests:    []string{"basic"},
						Nodes:    []string{nodeName},
						Phase:    v1alpha1.PhaseSucceeded,
						Attempts: []v1alpha1.AttemptStatus{
							{
								ObjectName: attemptObjectName(vrName, grp, 1),
								Phase:      v1alpha1.PhaseSucceeded,
							},
						},
					},
				},
			}))
		})

		It("batches multiple nodes into one group when node batching is true", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{node1, node2},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}}},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status).To(Equal(v1alpha1.ValidationRequestStatus{
				Phase: v1alpha1.PhaseSucceeded,
				TestGroups: []v1alpha1.TestGroupStatus{
					{
						Name:     grp,
						Provider: "test-provider",
						Tests:    []string{"basic"},
						Nodes:    []string{node1, node2},
						Phase:    v1alpha1.PhaseSucceeded,
						Attempts: []v1alpha1.AttemptStatus{
							{
								ObjectName: attemptObjectName(vrName, grp, 1),
								Phase:      v1alpha1.PhaseSucceeded,
							},
						},
					},
				},
			}))
		})

		It("produces one combined group when both node batching and test batching are enabled", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(true, true)
			grp := groupName([]string{"test-a", "test-b"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.TestGroups).To(HaveLen(1))
			Expect(vr.Status.TestGroups[0].Name).To(Equal(grp))
			Expect(vr.Status.TestGroups[0].Nodes).To(ConsistOf(node1, node2))
			Expect(vr.Status.TestGroups[0].Tests).To(ConsistOf("test-a", "test-b"))
		})

		It("produces four groups when both node batching and test batching are disabled", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(false, false)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.TestGroups).To(HaveLen(4))
			for _, g := range vr.Status.TestGroups {
				Expect(g.Tests).To(HaveLen(1))
				Expect(g.Nodes).To(HaveLen(1))
				Expect(g.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			}
		})

		It("produces one group per node when test batching is enabled but node batching is disabled", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(false, true)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.TestGroups).To(HaveLen(2))
			for _, g := range vr.Status.TestGroups {
				Expect(g.Tests).To(ConsistOf("test-a", "test-b"))
				Expect(g.Nodes).To(HaveLen(1))
				Expect(g.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			}
		})

		It("produces one group per test when node batching is enabled but test batching is disabled", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(true, false)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.TestGroups).To(HaveLen(2))
			for _, g := range vr.Status.TestGroups {
				Expect(g.Tests).To(HaveLen(1))
				Expect(g.Nodes).To(ConsistOf(node1, node2))
				Expect(g.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			}
		})
	})

	Context("pending phase", func() {
		It("succeeds immediately when all nodes are deleted", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
				beforeInit: func(ctx context.Context, _ *v1alpha1.ValidationRequest) error {
					if err := k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node1}}); err != nil {
						return err
					}
					return k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node2}})
				},
			})
			Expect(vr.Status).To(Equal(v1alpha1.ValidationRequestStatus{
				Phase:   v1alpha1.PhaseSucceeded,
				Skipped: &v1alpha1.SkippedStatus{Nodes: []string{node1, node2}},
			}))
		})

		It("skips the deleted node and continues with the remaining node", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
				beforeInit: func(ctx context.Context, _ *v1alpha1.ValidationRequest) error {
					return k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node2}})
				},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.Skipped).NotTo(BeNil())
			Expect(vr.Status.Skipped.Nodes).To(ConsistOf(node2))
			Expect(vr.Status.TestGroups).To(HaveLen(1))
			Expect(vr.Status.TestGroups[0].Nodes).To(Equal([]string{node1}))
			Expect(vr.Status.TestGroups[0].Name).To(Equal(grp))
		})

		It("stays pending until node readiness passes then transitions to running", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})

			// Remove the label so the pending reconcile blocks on readiness
			Expect(removeNodeLabel(ctx, nodeName, "ready")).To(Succeed())

			vr := reconcileForIterations(ctx, r, req, 1)
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhasePending))
			vr = reconcileForIterations(ctx, r, req, 1)
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhasePending))

			// Re-add the label to pass readiness and transition to running
			Expect(patchNodeLabel(ctx, nodeName, "ready", "true")).To(Succeed())
			vr = reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseRunning))
		})

		It("fails immediately when the batch minimum is not met and BatchFailurePolicyFail is fail", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			cfg := defaultTestConfig()
			t := cfg.Validation.Spec.Tests["basic"]
			t.MinimumNodesPerBatch = 2
			t.BatchFailurePolicy = v1alpha1.BatchFailurePolicyFail
			cfg.Validation.Spec.Tests["basic"] = t
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
			Expect(vr.Status.TestGroups).To(HaveLen(1))
			Expect(vr.Status.TestGroups[0].Name).To(Equal(grp))
			Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseFailed))
			Expect(vr.Status.TestGroups[0].Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonBatchMinimumNotMet))
		})

		It("skips the test and succeeds when the batch minimum is not met and BatchFailurePolicyIgnore is ignore", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			cfg := defaultTestConfig()
			t := cfg.Validation.Spec.Tests["basic"]
			t.MinimumNodesPerBatch = 2
			t.BatchFailurePolicy = v1alpha1.BatchFailurePolicyIgnore
			cfg.Validation.Spec.Tests["basic"] = t
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.TestGroups).To(BeEmpty())
			Expect(vr.Status.Skipped).NotTo(BeNil())
			Expect(vr.Status.Skipped.Tests).To(ConsistOf("basic"))
		})

		It("skips one test and proceeds to running when only one test's batch minimum is met", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			cfg := twoTestConfig(true, true)
			tb := cfg.Validation.Spec.Tests["test-b"]
			tb.MinimumNodesPerBatch = 2
			tb.BatchFailurePolicy = v1alpha1.BatchFailurePolicyIgnore
			cfg.Validation.Spec.Tests["test-b"] = tb
			grp := groupName([]string{"test-a"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.TestGroups).To(HaveLen(1))
			Expect(vr.Status.TestGroups[0].Name).To(Equal(grp))
			Expect(vr.Status.TestGroups[0].Tests).To(Equal([]string{"test-a"}))
			Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.Skipped).NotTo(BeNil())
			Expect(vr.Status.Skipped.Tests).To(ConsistOf("test-b"))
		})

		It("writes the session annotation while blocked on readiness", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})

			Expect(removeNodeLabel(ctx, nodeName, "ready")).To(Succeed())
			reconcileForIterations(ctx, r, req, 2)

			node := getNode(ctx, nodeName)
			entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Name).To(Equal(vrName))
			Expect(node.Annotations[annotationActiveValidationRequest]).To(BeEmpty())
		})

		It("adds the session annotation to every node even when an earlier node isn't ready", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
			})

			Expect(removeNodeLabel(ctx, node1, "ready")).To(Succeed())

			vr := reconcileForIterations(ctx, r, req, 2)
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhasePending))

			for _, nodeName := range []string{node1, node2} {
				node := getNode(ctx, nodeName)
				entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].Name).To(Equal(vrName))
			}
		})
	})

	Context("running phase", func() {
		It("runs groups sequentially when they share a node", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			// node batching and test batching both disabled and increase MaxConcurrentGroups to not limit group concurrency
			cfg := twoTestConfig(false, false)
			cfg.Validation.Spec.MaxConcurrentGroups = 10
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(vr.Status.TestGroups).To(HaveLen(2))
			var firstRunningCount, firstPendingCount int
			for _, g := range vr.Status.TestGroups {
				switch g.Phase {
				case v1alpha1.PhaseRunning:
					firstRunningCount++
				case v1alpha1.PhasePending:
					firstPendingCount++
				}
			}
			Expect(firstRunningCount).To(Equal(1), "exactly one group should be running while the other waits for the shared node")
			Expect(firstPendingCount).To(Equal(1), "the second group should be pending while the first holds the node")

			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			vr = reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
			var secondRunningCount, firstSucceededCount int
			for _, g := range vr.Status.TestGroups {
				switch g.Phase {
				case v1alpha1.PhaseRunning:
					secondRunningCount++
				case v1alpha1.PhaseSucceeded:
					firstSucceededCount++
				}
			}
			Expect(firstSucceededCount).To(Equal(1), "first group should be succeeded")
			Expect(secondRunningCount).To(Equal(1), "second group should now be running")

			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			finalVR := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 10)
			Expect(finalVR.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			for _, g := range finalVR.Status.TestGroups {
				Expect(g.Phase).To(Equal(v1alpha1.PhaseSucceeded))
				Expect(g.Attempts).To(HaveLen(1))
			}
		})

		It("runs groups sequentially when MaxConcurrentGroups is 1", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			// node batching disabled, test batching enabled with no node overlap so that MaxConcurrentGroups limits
			// concurrency.
			cfg := twoTestConfig(false, true)
			cfg.Validation.Spec.MaxConcurrentGroups = 1
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
			})

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(vr.Status.TestGroups).To(HaveLen(2))
			var firstRunningCount, firstPendingCount int
			for _, g := range vr.Status.TestGroups {
				switch g.Phase {
				case v1alpha1.PhaseRunning:
					firstRunningCount++
				case v1alpha1.PhasePending:
					firstPendingCount++
				}
			}
			Expect(firstRunningCount).To(Equal(1), "MaxConcurrentGroups=1: only one group should be running")
			Expect(firstPendingCount).To(Equal(1), "MaxConcurrentGroups=1: the second group should be pending")

			vr = reconcileForIterations(ctx, r, req, 1)
			var idleRunningCount, idlePendingCount int
			for _, g := range vr.Status.TestGroups {
				switch g.Phase {
				case v1alpha1.PhaseRunning:
					idleRunningCount++
				case v1alpha1.PhasePending:
					idlePendingCount++
				}
			}
			Expect(idleRunningCount).To(Equal(1), "group 1 should still be running")
			Expect(idlePendingCount).To(Equal(1), "group 2 should remain pending: concurrency cap already reached by the running group")

			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			vr = reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 10)
			var secondRunningCount, firstSucceededCount int
			for _, g := range vr.Status.TestGroups {
				switch g.Phase {
				case v1alpha1.PhaseRunning:
					secondRunningCount++
				case v1alpha1.PhaseSucceeded:
					firstSucceededCount++
				}
			}
			Expect(firstSucceededCount).To(Equal(1), "first group should be succeeded")
			Expect(secondRunningCount).To(Equal(1), "second group should now be running")

			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			finalVR := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 10)
			Expect(finalVR.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			for _, g := range finalVR.Status.TestGroups {
				Expect(g.Phase).To(Equal(v1alpha1.PhaseSucceeded))
				Expect(g.Attempts).To(HaveLen(1))
			}
		})

		It("does not start a pending group with a node readiness failure", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			// node batching disabled, test batching enabled with no node overlap but MaxConcurrentGroups=1
			cfg := twoTestConfig(false, true)
			cfg.Validation.Spec.MaxConcurrentGroups = 1
			cfg.Validation.Spec.ReadinessCriteria = []v1alpha1.CriteriaSpec{
				{Name: "test-criterion", Expression: `has(node.metadata.labels) && "ready" in node.metadata.labels`},
			}
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
			})

			// node1's group is running and node2's group is pending
			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(vr.Status.TestGroups).To(HaveLen(2))

			// Make node2 fail readiness when its group is still pending
			Expect(removeNodeLabel(ctx, node2, "ready")).To(Succeed())
			// Drive node1's group to success
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			vr = reconcileForIterations(ctx, r, req, 1)

			// Ensure node2's groups stays pending until readiness is restored
			var pendingGroup *v1alpha1.TestGroupStatus
			for i := range vr.Status.TestGroups {
				g := &vr.Status.TestGroups[i]
				if g.Phase == v1alpha1.PhasePending {
					pendingGroup = g
				}
			}
			Expect(pendingGroup).NotTo(BeNil())
			Expect(pendingGroup.Attempts).To(BeEmpty())

			Expect(patchNodeLabel(ctx, node2, "ready", "true")).To(Succeed())
			vr = reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			finalVR := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 10)
			Expect(finalVR.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
		})

		It("runs groups concurrently when MaxConcurrentGroups is greater than 1 and groups have no node overlap", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(false, true)
			cfg.Validation.Spec.MaxConcurrentGroups = 2
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
			})

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			runningCount := 0
			for _, g := range vr.Status.TestGroups {
				if g.Phase == v1alpha1.PhaseRunning {
					runningCount++
				}
			}
			Expect(runningCount).To(Equal(2))

			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			finalVR := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 10)
			Expect(finalVR.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
		})

		It("does not start a pending group after another group fails", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(false, true)
			cfg.Validation.Spec.MaxConcurrentGroups = 1
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
			})

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testFailedCondType)).To(Succeed())
			finalVR := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseFailed, 10)

			Expect(finalVR.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
			var pendingGroups []v1alpha1.TestGroupStatus
			for _, g := range finalVR.Status.TestGroups {
				if g.Phase == v1alpha1.PhasePending {
					pendingGroups = append(pendingGroups, g)
				}
			}
			Expect(pendingGroups).To(HaveLen(1), "the second group should remain pending")
			Expect(pendingGroups[0].Attempts).To(BeEmpty())
		})

		It("allows a currently running group to finish when another group fails", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(false, true)
			cfg.Validation.Spec.MaxConcurrentGroups = 2
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
			})

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(vr.Status.TestGroups).To(HaveLen(2))

			var failObjectName string
			for _, g := range vr.Status.TestGroups {
				if g.Phase == v1alpha1.PhaseRunning && len(g.Attempts) > 0 {
					for _, n := range g.Nodes {
						if n == node1 {
							failObjectName = g.Attempts[len(g.Attempts)-1].ObjectName
						}
					}
				}
			}
			Expect(failObjectName).NotTo(BeEmpty())
			Expect(updateObjectStatus(ctx, failObjectName, testFailedCondType)).To(Succeed())
			vr = reconcileForIterations(ctx, r, req, 1)
			var node2GroupPhase v1alpha1.Phase
			for _, g := range vr.Status.TestGroups {
				for _, n := range g.Nodes {
					if n == node2 {
						node2GroupPhase = g.Phase
					}
				}
			}
			Expect(node2GroupPhase).To(Equal(v1alpha1.PhaseRunning))

			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			finalVR := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseFailed, 10)
			Expect(finalVR.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
			var node2FinalPhase v1alpha1.Phase
			for _, g := range finalVR.Status.TestGroups {
				for _, n := range g.Nodes {
					if n == node2 {
						node2FinalPhase = g.Phase
					}
				}
			}
			Expect(node2FinalPhase).To(Equal(v1alpha1.PhaseSucceeded))
		})

		It("fails a pending group without starting it when its nodes are deleted below the batch minimum with BatchFailurePolicyFail is set to fail", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(false, true)
			cfg.Validation.Spec.MaxConcurrentGroups = 1
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
			})

			// node1's group is running and node2's group is pending
			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)

			// Delete node2 while node1's group is running then succeed node1's group
			Expect(k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node2}})).To(Succeed())
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())

			// ensure node2's group is set to failed without any attempts
			finalVR := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseFailed, 10)
			Expect(finalVR.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
			var noAttemptFailedGroups int
			for _, g := range finalVR.Status.TestGroups {
				if g.Phase == v1alpha1.PhaseFailed && len(g.Attempts) == 0 {
					noAttemptFailedGroups++
				}
			}
			Expect(noAttemptFailedGroups).To(Equal(1))
		})

		It("succeeds a pending group without starting it when a deleted node violates the batch minimum and BatchFailurePolicyIgnore is set to ignore", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(false, true)
			cfg.Validation.Spec.MaxConcurrentGroups = 1
			for k, t := range cfg.Validation.Spec.Tests {
				t.BatchFailurePolicy = v1alpha1.BatchFailurePolicyIgnore
				cfg.Validation.Spec.Tests[k] = t
			}
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
			})

			// node1's group is running and node2's group is pending
			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)

			// Delete node2 while node1's group is running then succeed node1's group
			Expect(k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node2}})).To(Succeed())
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())

			// ensure node2's group is set to successful without any attempts
			finalVR := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 10)
			Expect(finalVR.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			var noAttemptSucceededGroups int
			for _, g := range finalVR.Status.TestGroups {
				if g.Phase == v1alpha1.PhaseSucceeded && len(g.Attempts) == 0 {
					noAttemptSucceededGroups++
				}
			}
			Expect(noAttemptSucceededGroups).To(Equal(1))
			Expect(finalVR.Status.Skipped).NotTo(BeNil())
			Expect(finalVR.Status.Skipped.Nodes).To(ConsistOf(node2))
			Expect(finalVR.Status.Skipped.Tests).To(ConsistOf("test-a", "test-b"))
		})

		It("starts a pending group without the deleted node when the batch minimum is still met", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix

			// we have 1 group with both nodes for test-a and 2 groups for test-b with one node per group
			cfg := twoTestConfig(true, false)
			cfg.Validation.Spec.MaxConcurrentGroups = 1
			tb := cfg.Validation.Spec.Tests["test-b"]
			tb.SupportsBatchingNodes = false
			cfg.Validation.Spec.Tests["test-b"] = tb

			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
					// test-b ordered first so its groups use up available concurrency
					Tests: []string{"test-b", "test-a"},
				},
			})

			// test-b/node1's group is running and test-b/node2's group and test-a's group are pending
			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)

			// Delete node2 while node1's group is running then succeed node1's group
			Expect(k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node2}})).To(Succeed())
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())

			// ensure test-a's group starts using only node1
			finalVR := reconcileForIterations(ctx, r, req, 1)

			var testAGroup *v1alpha1.TestGroupStatus
			for i := range finalVR.Status.TestGroups {
				g := &finalVR.Status.TestGroups[i]
				if len(g.Tests) == 1 && g.Tests[0] == "test-a" {
					testAGroup = g
				}
			}
			Expect(testAGroup).NotTo(BeNil())
			Expect(testAGroup.Phase).To(Equal(v1alpha1.PhaseRunning))
			Expect(testAGroup.Nodes).To(Equal([]string{node1}))
			Expect(testAGroup.Attempts).To(HaveLen(1))
		})

		It("does not mutate a VR that is already terminal", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			vr = reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 5)

			after := reconcileForIterations(ctx, r, req, 1)
			Expect(after.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(after.Status.CompletionTime).To(Equal(vr.Status.CompletionTime))
		})
	})

	Context("group failures failures", func() {
		It("marks the request as failed when no retries configured", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testFailedCondType)
					},
				},
			})
			Expect(vr.Status).To(Equal(v1alpha1.ValidationRequestStatus{
				Phase: v1alpha1.PhaseFailed,
				TestGroups: []v1alpha1.TestGroupStatus{
					{
						Name:     grp,
						Provider: "test-provider",
						Tests:    []string{"basic"},
						Nodes:    []string{nodeName},
						Phase:    v1alpha1.PhaseFailed,
						Attempts: []v1alpha1.AttemptStatus{
							{
								ObjectName:    attemptObjectName(vrName, grp, 1),
								Phase:         v1alpha1.PhaseFailed,
								FailureReason: v1alpha1.FailureReasonTestFailed,
							},
						},
					},
				},
			}))
		})

		It("retries and ultimately fails when all retries are exhausted", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			cfg := defaultTestConfig()
			p := cfg.Validation.Spec.Providers["test-provider"]
			p.Retries = 1
			cfg.Validation.Spec.Providers["test-provider"] = p
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testFailedCondType)
					},
				},
			})
			Expect(vr.Status).To(Equal(v1alpha1.ValidationRequestStatus{
				Phase: v1alpha1.PhaseFailed,
				TestGroups: []v1alpha1.TestGroupStatus{
					{
						Name:     grp,
						Provider: "test-provider",
						Tests:    []string{"basic"},
						Nodes:    []string{nodeName},
						Phase:    v1alpha1.PhaseFailed,
						Attempts: []v1alpha1.AttemptStatus{
							{
								ObjectName:    attemptObjectName(vrName, grp, 1),
								Phase:         v1alpha1.PhaseFailed,
								FailureReason: v1alpha1.FailureReasonTestFailed,
							},
							{
								ObjectName:    attemptObjectName(vrName, grp, 2),
								Phase:         v1alpha1.PhaseFailed,
								FailureReason: v1alpha1.FailureReasonTestFailed,
							},
						},
					},
				},
			}))
		})

		It("succeeds after one retry when the first attempt fails and the second succeeds", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			cfg := defaultTestConfig()
			p := cfg.Validation.Spec.Providers["test-provider"]
			p.Retries = 1
			cfg.Validation.Spec.Providers["test-provider"] = p
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testFailedCondType)
					},
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status).To(Equal(v1alpha1.ValidationRequestStatus{
				Phase: v1alpha1.PhaseSucceeded,
				TestGroups: []v1alpha1.TestGroupStatus{
					{
						Name:     grp,
						Provider: "test-provider",
						Tests:    []string{"basic"},
						Nodes:    []string{nodeName},
						Phase:    v1alpha1.PhaseSucceeded,
						Attempts: []v1alpha1.AttemptStatus{
							{
								ObjectName:    attemptObjectName(vrName, grp, 1),
								Phase:         v1alpha1.PhaseFailed,
								FailureReason: v1alpha1.FailureReasonTestFailed,
							},
							{
								ObjectName: attemptObjectName(vrName, grp, 2),
								Phase:      v1alpha1.PhaseSucceeded,
							},
						},
					},
				},
			}))
		})

		It("deletes the failed attempt's provider resource before starting the retry", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			cfg := defaultTestConfig()
			p := cfg.Validation.Spec.Providers["test-provider"]
			p.Retries = 1
			cfg.Validation.Spec.Providers["test-provider"] = p
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			firstObjectName := vr.Status.TestGroups[0].Attempts[0].ObjectName

			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testFailedCondType)).To(Succeed())
			vr = reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(2))

			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(testJobGVK)
			err := k8sClient.Get(ctx, types.NamespacedName{Name: firstObjectName, Namespace: testNamespace}, u)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("fails the attempt with TestTimeout", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			cfg := defaultTestConfig()
			p := cfg.Validation.Spec.Providers["test-provider"]
			p.TimeoutSeconds = 0
			p.Retries = 0
			cfg.Validation.Spec.Providers["test-provider"] = p
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
			Expect(vr.Status.TestGroups[0].Name).To(Equal(grp))
			Expect(vr.Status.TestGroups[0].Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonTestTimeout))
		})

		It("treats an externally deleted provider resource as a test failure", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						objectName := attemptObjectName(vrName, grp, 1)
						u := &unstructured.Unstructured{}
						u.SetGroupVersionKind(testJobGVK)
						u.SetName(objectName)
						u.SetNamespace(testNamespace)
						return client.IgnoreNotFound(k8sClient.Delete(ctx, u))
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
			Expect(vr.Status.TestGroups[0].Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonTestFailed))
		})

		It("fails the attempt and retries when a node readiness violation occurs while running and then succeeds after readiness recovers", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			cfg := defaultTestConfig()
			p := cfg.Validation.Spec.Providers["test-provider"]
			p.Retries = 1
			cfg.Validation.Spec.Providers["test-provider"] = p
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, _ *v1alpha1.ValidationRequest) error {
						return removeNodeLabel(ctx, nodeName, "ready")
					},
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						if err := patchNodeLabel(ctx, nodeName, "ready", "true"); err != nil {
							return err
						}
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.TestGroups[0].Name).To(Equal(grp))
			Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(2))
			Expect(vr.Status.TestGroups[0].Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonNodeReadinessViolation))
			Expect(vr.Status.TestGroups[0].Attempts[1].Phase).To(Equal(v1alpha1.PhaseSucceeded))
		})
	})

	Context("node deletion while running", func() {
		It("fails the attempt with NodeDeleted and retries when a node is deleted but the batch minimum is still met", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := defaultTestConfig()
			t := cfg.Validation.Spec.Tests["basic"]
			t.MinimumNodesPerBatch = 1
			t.SupportsBatchingNodes = true
			cfg.Validation.Spec.Tests["basic"] = t
			p := cfg.Validation.Spec.Providers["test-provider"]
			p.Retries = 1
			cfg.Validation.Spec.Providers["test-provider"] = p
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, _ *v1alpha1.ValidationRequest) error {
						return k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node2}})
					},
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.TestGroups[0].Name).To(Equal(grp))
			Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(2))
			Expect(vr.Status.TestGroups[0].Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonNodeDeleted))
			Expect(vr.Status.TestGroups[0].Attempts[1].Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.Skipped).NotTo(BeNil())
			Expect(vr.Status.Skipped.Nodes).To(ConsistOf(node2))
		})

		It("drops one test and retries with the other when a node deletion violates only one test's batch minimum in a shared group", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(true, true)
			tb := cfg.Validation.Spec.Tests["test-b"]
			tb.MinimumNodesPerBatch = 2
			tb.BatchFailurePolicy = v1alpha1.BatchFailurePolicyIgnore
			cfg.Validation.Spec.Tests["test-b"] = tb
			p := cfg.Validation.Spec.Providers["test-provider"]
			p.Retries = 1
			cfg.Validation.Spec.Providers["test-provider"] = p
			grp := groupName([]string{"test-a", "test-b"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					// Delete one node: test-a's minimum is still met and test-b's minimum is not.
					func(ctx context.Context, _ *v1alpha1.ValidationRequest) error {
						return k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node2}})
					},
					// Succeed the retry attempt, which now only covers test-a.
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return updateObjectStatusForAllTestGroups(ctx, vr, testSuccessCondType)
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.TestGroups).To(HaveLen(1))
			Expect(vr.Status.TestGroups[0].Name).To(Equal(grp))
			Expect(vr.Status.TestGroups[0].Tests).To(Equal([]string{"test-a"}))
			Expect(vr.Status.TestGroups[0].Nodes).To(Equal([]string{node1}))
			Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(2))
			Expect(vr.Status.TestGroups[0].Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonNodeDeleted))
			Expect(vr.Status.TestGroups[0].Attempts[1].Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.Skipped).NotTo(BeNil())
			Expect(vr.Status.Skipped.Nodes).To(ConsistOf(node2))
			Expect(vr.Status.Skipped.Tests).To(ConsistOf("test-b"))
		})

		It("fails the whole group when a node deletion violates one test's batch minimum under BatchFailurePolicyFail in a shared group", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(true, true)
			tb := cfg.Validation.Spec.Tests["test-b"]
			tb.MinimumNodesPerBatch = 2
			tb.BatchFailurePolicy = v1alpha1.BatchFailurePolicyFail
			cfg.Validation.Spec.Tests["test-b"] = tb
			p := cfg.Validation.Spec.Providers["test-provider"]
			p.Retries = 1
			cfg.Validation.Spec.Providers["test-provider"] = p
			grp := groupName([]string{"test-a", "test-b"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, _ *v1alpha1.ValidationRequest) error {
						return k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node2}})
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
			Expect(vr.Status.TestGroups).To(HaveLen(1))
			Expect(vr.Status.TestGroups[0].Name).To(Equal(grp))
			Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseFailed))
			Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(1))
			Expect(vr.Status.TestGroups[0].Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonNodeDeleted))
			Expect(vr.Status.Skipped).NotTo(BeNil())
			Expect(vr.Status.Skipped.Nodes).To(ConsistOf(node2))
		})

		It("fails the group even with retries configured when a deleted node violates the batch minimum (non-retryable)", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := defaultTestConfig()
			t := cfg.Validation.Spec.Tests["basic"]
			t.MinimumNodesPerBatch = 2
			t.SupportsBatchingNodes = true
			t.BatchFailurePolicy = v1alpha1.BatchFailurePolicyFail
			cfg.Validation.Spec.Tests["basic"] = t
			p := cfg.Validation.Spec.Providers["test-provider"]
			p.Retries = 2
			cfg.Validation.Spec.Providers["test-provider"] = p
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					// Delete one node while the group is running: 1 remaining < MinimumNodesPerBatch=2
					func(ctx context.Context, _ *v1alpha1.ValidationRequest) error {
						return k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node2}})
					},
				},
			})
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseFailed))
			Expect(vr.Status.TestGroups[0].Name).To(Equal(grp))
			Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseFailed))
			Expect(vr.Status.TestGroups[0].Attempts).To(HaveLen(1))
			Expect(vr.Status.TestGroups[0].Attempts[0].FailureReason).To(Equal(v1alpha1.FailureReasonNodeDeleted))
			Expect(vr.Status.Skipped).NotTo(BeNil())
			Expect(vr.Status.Skipped.Nodes).To(ConsistOf(node2))
		})

		It("skips the test and marks the group successful when a deleted node violates the batch minimum and BatchFailurePolicyIgnore is set", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			cfg := defaultTestConfig()
			t := cfg.Validation.Spec.Tests["basic"]
			t.MinimumNodesPerBatch = 1
			t.SupportsBatchingNodes = true
			t.BatchFailurePolicy = v1alpha1.BatchFailurePolicyIgnore
			cfg.Validation.Spec.Tests["basic"] = t
			grp := groupName([]string{"basic"}, 1)
			vr := runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, _ *v1alpha1.ValidationRequest) error {
						return k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}})
					},
				},
			})
			Expect(vr.Status.TestGroups[0].Name).To(Equal(grp))
			Expect(vr.Status.TestGroups[0].Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(vr.Status.Skipped).NotTo(BeNil())
			Expect(vr.Status.Skipped.Nodes).To(ConsistOf(nodeName))
			Expect(vr.Status.Skipped.Tests).To(ConsistOf("basic"))
		})

		It("marks the request as successful when all nodes are deleted during running despite a failed group", func() {
			vrName := "vr-" + suffix
			node1, node2 := "node1-"+suffix, "node2-"+suffix
			cfg := twoTestConfig(false, true)
			cfg.Validation.Spec.MaxConcurrentGroups = 2
			for k, t := range cfg.Validation.Spec.Tests {
				t.BatchFailurePolicy = v1alpha1.BatchFailurePolicyFail
				cfg.Validation.Spec.Tests[k] = t
			}
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{node1, node2},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: node1}, {Name: node2}},
				},
			})

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			runningCount := 0
			for _, g := range vr.Status.TestGroups {
				if g.Phase == v1alpha1.PhaseRunning {
					runningCount++
				}
			}
			Expect(runningCount).To(Equal(2))

			Expect(k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node1}})).To(Succeed())
			Expect(k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node2}})).To(Succeed())
			finalVR := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 15)
			Expect(finalVR.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
		})
	})

	Context("multiple requests", func() {
		It("runs two requests concurrently when there's no node overlap", func() {
			nodeA, nodeB := "node-a-"+suffix, "node-b-"+suffix
			vrA, vrB := "vr-a-"+suffix, "vr-b-"+suffix

			rA, reqA := newValidationRequestTestSetup(ctx, vrA, validationRequestTestCase{
				nodeNames: []string{nodeA},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeA}}},
			})
			rB, reqB := newValidationRequestTestSetup(ctx, vrB, validationRequestTestCase{
				nodeNames: []string{nodeB},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeB}}},
			})

			vra := reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseRunning, 5)
			vrb := reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseRunning, 5)

			Expect(updateObjectStatusForAllTestGroups(ctx, &vra, testSuccessCondType)).To(Succeed())
			Expect(updateObjectStatusForAllTestGroups(ctx, &vrb, testSuccessCondType)).To(Succeed())

			finalA := reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseSucceeded, 10)
			finalB := reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseSucceeded, 10)
			Expect(finalA.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
			Expect(finalB.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
		})

		It("blocks a second request in pending from node overlap, unblocks when the first request succeeds", func() {
			nodeName := "node-" + suffix
			vrA, vrB := "vr-a-"+suffix, "vr-b-"+suffix
			spec := v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}}

			rA, reqA := newValidationRequestTestSetup(ctx, vrA, validationRequestTestCase{nodeNames: []string{nodeName}, spec: spec})
			rB, reqB := newValidationRequestTestSetup(ctx, vrB, validationRequestTestCase{nodeNames: nil, spec: spec})

			vra := reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseRunning, 5)

			vrb := reconcileForIterations(ctx, rB, reqB, 2)
			Expect(vrb.Status.Phase).To(Equal(v1alpha1.PhasePending))

			Expect(updateObjectStatusForAllTestGroups(ctx, &vra, testSuccessCondType)).To(Succeed())
			finalA := reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseSucceeded, 10)
			Expect(finalA.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))

			vrb2 := reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseRunning, 10)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vrb2, testSuccessCondType)).To(Succeed())
			finalB := reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseSucceeded, 10)
			Expect(finalB.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
		})

		It("blocks a second request in pending from node overlap, unblocks when the first request fails", func() {
			nodeName := "node-" + suffix
			vrA, vrB := "vr-a-"+suffix, "vr-b-"+suffix
			spec := v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}}

			rA, reqA := newValidationRequestTestSetup(ctx, vrA, validationRequestTestCase{nodeNames: []string{nodeName}, spec: spec})
			rB, reqB := newValidationRequestTestSetup(ctx, vrB, validationRequestTestCase{nodeNames: nil, spec: spec})

			vra := reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseRunning, 5)

			vrb := reconcileForIterations(ctx, rB, reqB, 2)
			Expect(vrb.Status.Phase).To(Equal(v1alpha1.PhasePending))

			Expect(updateObjectStatusForAllTestGroups(ctx, &vra, testFailedCondType)).To(Succeed())
			finalA := reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseFailed, 10)
			Expect(finalA.Status.Phase).To(Equal(v1alpha1.PhaseFailed))

			vrb2 := reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseRunning, 10)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vrb2, testSuccessCondType)).To(Succeed())
			finalB := reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseSucceeded, 10)
			Expect(finalB.Status.Phase).To(Equal(v1alpha1.PhaseSucceeded))
		})

		It("supersedes failed request session entry when a new request succeeds with the same tests", func() {
			nodeName := "node-" + suffix
			vrA, vrB := "vr-a-"+suffix, "vr-b-"+suffix
			spec := v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}}

			rA, reqA := newValidationRequestTestSetup(ctx, vrA, validationRequestTestCase{nodeNames: []string{nodeName}, spec: spec})
			rB, reqB := newValidationRequestTestSetup(ctx, vrB, validationRequestTestCase{nodeNames: nil, spec: spec})

			vra := reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseRunning, 5)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vra, testFailedCondType)).To(Succeed())
			reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseFailed, 10)

			vrb := reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseRunning, 10)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vrb, testSuccessCondType)).To(Succeed())
			reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseSucceeded, 10)

			node := getNode(ctx, nodeName)
			Expect(node.Annotations[annotationValidationSession]).To(BeEmpty())
		})

		It("leaves the failed request session entry when the succeeding request targets different tests", func() {
			nodeName := "node-" + suffix
			vrA, vrB := "vr-a-"+suffix, "vr-b-"+suffix
			cfg := twoTestConfig(true, true)

			rA, reqA := newValidationRequestTestSetup(ctx, vrA, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}, Tests: []string{"test-a"}},
			})
			rB, reqB := newValidationRequestTestSetup(ctx, vrB, validationRequestTestCase{
				config:    cfg,
				nodeNames: nil,
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}, Tests: []string{"test-b"}},
			})

			vra := reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseRunning, 5)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vra, testFailedCondType)).To(Succeed())
			reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseFailed, 10)

			vrb := reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseRunning, 10)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vrb, testSuccessCondType)).To(Succeed())
			reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseSucceeded, 10)

			node := getNode(ctx, nodeName)
			entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
			Expect(err).NotTo(HaveOccurred())
			var vrAEntry *sessionEntry
			for i := range entries {
				if entries[i].Name == vrA {
					vrAEntry = &entries[i]
				}
			}
			Expect(vrAEntry).NotTo(BeNil())
			Expect(vrAEntry.Failed).To(BeTrue())
		})

		It("leaves the failed request session entry when the succeeding request targets a subset of the failed tests", func() {
			nodeName := "node-" + suffix
			vrA, vrB := "vr-a-"+suffix, "vr-b-"+suffix
			cfg := twoTestConfig(true, true)

			rA, reqA := newValidationRequestTestSetup(ctx, vrA, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}, Tests: []string{"test-a", "test-b"},
				},
			})
			rB, reqB := newValidationRequestTestSetup(ctx, vrB, validationRequestTestCase{
				config:    cfg,
				nodeNames: nil,
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}, Tests: []string{"test-a"},
				},
			})

			vra := reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseRunning, 5)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vra, testFailedCondType)).To(Succeed())
			reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseFailed, 10)

			vrb := reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseRunning, 10)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vrb, testSuccessCondType)).To(Succeed())
			reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseSucceeded, 10)

			node := getNode(ctx, nodeName)
			entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
			Expect(err).NotTo(HaveOccurred())
			var vrAEntry *sessionEntry
			for i := range entries {
				if entries[i].Name == vrA {
					vrAEntry = &entries[i]
				}
			}
			Expect(vrAEntry).NotTo(BeNil())
			Expect(vrAEntry.Failed).To(BeTrue())
		})

		It("adds concurrent requests to the session annotation", func() {
			nodeName := "node-" + suffix
			vrA, vrB := "vr-a-"+suffix, "vr-b-"+suffix
			spec := v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}}

			rA, reqA := newValidationRequestTestSetup(ctx, vrA, validationRequestTestCase{nodeNames: []string{nodeName}, spec: spec})
			rB, reqB := newValidationRequestTestSetup(ctx, vrB, validationRequestTestCase{nodeNames: nil, spec: spec})

			reconcileForIterations(ctx, rA, reqA, 2)
			reconcileForIterations(ctx, rB, reqB, 2)

			node := getNode(ctx, nodeName)
			entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
			Expect(err).NotTo(HaveOccurred())
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name
			}
			Expect(names).To(ConsistOf(vrA, vrB))
		})
	})

	Context("request deletion", func() {
		It("releases node annotations and removes the finalizer", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			runValidationRequestTest(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
				afterRunning: []func(context.Context, *v1alpha1.ValidationRequest) error{
					func(ctx context.Context, vr *v1alpha1.ValidationRequest) error {
						return k8sClient.Delete(ctx, vr)
					},
				},
			})
		})

		It("removes failed session entry when a failed request is deleted", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testFailedCondType)).To(Succeed())
			reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseFailed, 10)

			node := getNode(ctx, nodeName)
			entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Failed).To(BeTrue())

			Expect(k8sClient.Get(ctx, req.NamespacedName, &vr)).To(Succeed())
			Expect(k8sClient.Delete(ctx, &vr)).To(Succeed())
			reconcileForIterations(ctx, r, req, 1)

			node = getNode(ctx, nodeName)
			Expect(node.Annotations[annotationValidationSession]).To(BeEmpty())
		})
	})

	Context("scheduling gate", func() {
		It("releases the cordon and removes configured taints after success", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			taintCfg := v1alpha1.TaintConfig{Key: "nvsentinel", Value: "", Effect: "NoSchedule", Remove: true}
			cfg := defaultTestConfig()
			cfg.Validation.Spec.SchedulingGate = &v1alpha1.SchedulingGateConfig{
				Cordon: v1alpha1.CordonConfig{Remove: true},
				Taints: []v1alpha1.TaintConfig{taintCfg},
			}
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})
			Expect(cordonNode(ctx, nodeName)).To(Succeed())
			Expect(addNodeTaint(ctx, nodeName, corev1.Taint{Key: "nvsentinel", Effect: corev1.TaintEffectNoSchedule})).To(Succeed())

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 10)

			node := getNode(ctx, nodeName)
			Expect(node.Spec.Unschedulable).To(BeFalse())
			Expect(node.Spec.Taints).NotTo(ContainElement(HaveField("Key", Equal("nvsentinel"))))
		})

		It("leaves the cordon and taints after failure", func() {
			nodeName := "node-" + suffix
			vrA, vrB := "vr-a-"+suffix, "vr-b-"+suffix
			taintCfg := v1alpha1.TaintConfig{Key: "nvsentinel", Value: "", Effect: "NoSchedule", Remove: true}
			gate := &v1alpha1.SchedulingGateConfig{
				Cordon: v1alpha1.CordonConfig{Remove: true},
				Taints: []v1alpha1.TaintConfig{taintCfg},
			}
			cfgA := defaultTestConfig()
			cfgA.Validation.Spec.SchedulingGate = gate
			cfgB := twoTestConfig(true, true)
			cfgB.Validation.Spec.SchedulingGate = gate

			rA, reqA := newValidationRequestTestSetup(ctx, vrA, validationRequestTestCase{
				config:    cfgA,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})
			Expect(cordonNode(ctx, nodeName)).To(Succeed())
			Expect(addNodeTaint(ctx, nodeName, corev1.Taint{Key: "nvsentinel", Effect: corev1.TaintEffectNoSchedule})).To(Succeed())
			rB, reqB := newValidationRequestTestSetup(ctx, vrB, validationRequestTestCase{
				config:    cfgB,
				nodeNames: nil,
				spec: v1alpha1.ValidationRequestSpec{
					Nodes: []v1alpha1.NodeSpec{{Name: nodeName}},
					Tests: []string{"test-b"},
				},
			})

			vra := reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseRunning, 5)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vra, testFailedCondType)).To(Succeed())
			reconcileUntilPhase(ctx, rA, reqA, v1alpha1.PhaseFailed, 10)

			vrb := reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseRunning, 10)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vrb, testSuccessCondType)).To(Succeed())
			reconcileUntilPhase(ctx, rB, reqB, v1alpha1.PhaseSucceeded, 10)

			node := getNode(ctx, nodeName)
			Expect(node.Spec.Unschedulable).To(BeTrue())
			Expect(node.Spec.Taints).To(ContainElement(HaveField("Key", Equal("nvsentinel"))))
		})

		It("leaves taints non-matching taints", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			taintCfg := v1alpha1.TaintConfig{Key: "nvsentinel", Value: "", Effect: "NoSchedule", Remove: true}
			cfg := defaultTestConfig()
			cfg.Validation.Spec.SchedulingGate = &v1alpha1.SchedulingGateConfig{
				Taints: []v1alpha1.TaintConfig{taintCfg},
			}
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})
			Expect(addNodeTaint(ctx, nodeName, corev1.Taint{Key: "nvsentinel", Effect: corev1.TaintEffectNoExecute})).To(Succeed())

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 10)

			node := getNode(ctx, nodeName)
			Expect(node.Spec.Taints).To(ContainElement(
				And(HaveField("Key", Equal("nvsentinel")), HaveField("Effect", Equal(corev1.TaintEffectNoExecute))),
			))
		})

		It("leaves a taint in place when remove is set to false", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			taintCfg := v1alpha1.TaintConfig{Key: "nvsentinel", Value: "", Effect: "NoSchedule", Remove: false}
			cfg := defaultTestConfig()
			cfg.Validation.Spec.SchedulingGate = &v1alpha1.SchedulingGateConfig{
				Taints: []v1alpha1.TaintConfig{taintCfg},
			}
			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})
			Expect(addNodeTaint(ctx, nodeName, corev1.Taint{Key: "nvsentinel", Effect: corev1.TaintEffectNoSchedule})).To(Succeed())

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(updateObjectStatusForAllTestGroups(ctx, &vr, testSuccessCondType)).To(Succeed())
			reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseSucceeded, 10)

			node := getNode(ctx, nodeName)
			Expect(node.Spec.Taints).To(ContainElement(HaveField("Key", Equal("nvsentinel"))))
		})

		It("does not release the scheduling gate on a node with no session entries for this request", func() {
			nodeName := "node-" + suffix
			taintCfg := v1alpha1.TaintConfig{Key: "nvsentinel", Value: "", Effect: "NoSchedule", Remove: true}
			cfg := defaultTestConfig()
			cfg.Validation.Spec.SchedulingGate = &v1alpha1.SchedulingGateConfig{
				Cordon: v1alpha1.CordonConfig{Remove: true},
				Taints: []v1alpha1.TaintConfig{taintCfg},
			}

			// The node has no validation-session annotation at all but it's cordoned and tainted for an external reason
			Expect(k8sClient.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}})).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(
					k8sClient.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}))).To(Succeed())
			})
			Expect(cordonNode(ctx, nodeName)).To(Succeed())
			Expect(addNodeTaint(ctx, nodeName, corev1.Taint{Key: "nvsentinel", Effect: corev1.TaintEffectNoSchedule})).To(Succeed())

			reconciler, err := NewValidationRequestReconciler(k8sClient, k8sClient, k8sClient.Scheme(), cfg, testNamespace)
			Expect(err).NotTo(HaveOccurred())

			vr := &v1alpha1.ValidationRequest{
				ObjectMeta: metav1.ObjectMeta{Name: "vr-" + suffix},
				Spec:       v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			}
			Expect(reconciler.releaseNodesFromSuccessfulRequest(ctx, vr, nil)).To(Succeed())

			node := getNode(ctx, nodeName)
			Expect(node.Spec.Unschedulable).To(BeTrue())
			Expect(node.Spec.Taints).To(ContainElement(HaveField("Key", Equal("nvsentinel"))))
		})

		It("renders all template context fields into the provider resource", func() {
			vrName, nodeName := "vr-"+suffix, "node-"+suffix
			cfg := defaultTestConfig()
			cfg.Validation.Spec.SchedulingGate = &v1alpha1.SchedulingGateConfig{
				Taints: []v1alpha1.TaintConfig{
					{Key: "taint-exists", Value: "", Effect: "NoSchedule"},
					{Key: "taint-equal", Value: "val", Effect: "NoSchedule"},
				},
			}
			grp := groupName([]string{"basic"}, 1)

			r, req := newValidationRequestTestSetup(ctx, vrName, validationRequestTestCase{
				config:    cfg,
				nodeNames: []string{nodeName},
				spec:      v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: nodeName}}},
			})

			vr := reconcileUntilPhase(ctx, r, req, v1alpha1.PhaseRunning, 5)
			Expect(vr.Status.TestGroups).To(HaveLen(1))
			objectName := vr.Status.TestGroups[0].Attempts[0].ObjectName

			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(testJobGVK)
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: objectName, Namespace: testNamespace}, u)).To(Succeed())

			expectedObjectYAML := fmt.Sprintf(expectedTestJob, vrName, grp, testNamespace, nodeName)
			actualObject := map[string]any{
				"apiVersion": u.Object["apiVersion"],
				"kind":       u.Object["kind"],
				"spec":       u.Object["spec"],
			}
			actualObjectJSON, err := json.Marshal(actualObject)
			Expect(err).NotTo(HaveOccurred())
			expectedObjectJSON, err := sigsyaml.YAMLToJSON([]byte(expectedObjectYAML))
			Expect(err).NotTo(HaveOccurred())
			Expect(actualObjectJSON).To(MatchJSON(expectedObjectJSON))
		})
	})
})
