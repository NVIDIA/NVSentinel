// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package informers

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/commons/pkg/celrule"
	"github.com/nvidia/nvsentinel/commons/pkg/nodecache"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExcludedPodTransformUsesRuntimeNamespaceRegexSemantics(t *testing.T) {
	t.Parallel()

	excludeRegex := regexp.MustCompile(`^(kube-.*|nvsentinel)$`)
	transform := excludedPodTransform(excludeRegex)
	informers := &Informers{}

	tests := []struct {
		namespace string
		excluded  bool
	}{
		{namespace: "kube-system", excluded: true},
		{namespace: "kube-public", excluded: true},
		{namespace: "nvsentinel", excluded: true},
		{namespace: "user-kube-system", excluded: false},
		{namespace: "workload", excluded: false},
	}

	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			pod := richDrainEligiblePod(tt.namespace, "pod", "node-a")

			transformed, err := transform(pod)
			require.NoError(t, err)

			included, err := informers.shouldIncludeNamespace(tt.namespace, "*", excludeRegex)
			require.NoError(t, err)
			assert.Equal(t, tt.excluded, !included)

			transformedPod := transformed.(*v1.Pod)
			if tt.excluded {
				assert.Empty(t, transformedPod.Spec.NodeName)
				assert.Empty(t, transformedPod.Spec.Containers)
			} else {
				assert.NotSame(t, pod, transformedPod)
				assert.Equal(t, pod.Spec.NodeName, transformedPod.Spec.NodeName)
				assert.Equal(t, pod.Status.Phase, transformedPod.Status.Phase)
			}
		})
	}
}

func TestExcludedPodTransformDetectsDaemonSetOwners(t *testing.T) {
	t.Parallel()

	transform := excludedPodTransform(regexp.MustCompile(`^kube-system$`))
	pod := richDrainEligiblePod("workload", "daemon", "node-a")
	pod.OwnerReferences = []metav1.OwnerReference{
		{Kind: "ReplicaSet", Name: "replica"},
		{Kind: "DaemonSet", Name: "daemon"},
	}

	transformed, err := transform(pod)
	require.NoError(t, err)

	transformedPod := transformed.(*v1.Pod)
	assert.Equal(t, pod.Name, transformedPod.Name)
	assert.Equal(t, pod.Namespace, transformedPod.Namespace)
	assert.Equal(t, pod.UID, transformedPod.UID)
	assert.Equal(t, pod.ResourceVersion, transformedPod.ResourceVersion)
	assert.Empty(t, transformedPod.Spec.NodeName)
	assert.Empty(t, transformedPod.OwnerReferences)
	assert.True(t, isDaemonSetOwned(pod.OwnerReferences))
}

func TestExcludedPodTransformRetainsDrainFieldsOnly(t *testing.T) {
	t.Parallel()

	pod := richDrainEligiblePod("workload", "eligible", "node-a")
	transform := excludedPodTransform(regexp.MustCompile(`^kube-system$`))

	transformed, err := transform(pod)
	require.NoError(t, err)

	cachedPod := transformed.(*v1.Pod)
	expected := &v1.Pod{
		Name:              "eligible",
		Namespace:         "workload",
		UID:               types.UID("workload-eligible"),
		ResourceVersion:   "11",
		DeletionTimestamp: pod.DeletionTimestamp.DeepCopy(),
		Annotations: map[string]string{
			model.PodDeviceAnnotationName: `{"devices":{"nvidia.com/gpu":["GPU-1"]}}`,
		},
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet"}},
		Spec: v1.PodSpec{
			NodeName:                      "node-a",
			TerminationGracePeriodSeconds: new(int64(60)),
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Limits: v1.ResourceList{v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")},
				},
			}},
			InitContainers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Limits: v1.ResourceList{v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")},
				},
			}},
			ResourceClaims: []v1.PodResourceClaim{{Name: "gpu", ResourceClaimTemplateName: new("gpu-template")}},
		},
		Status: v1.PodStatus{
			Phase:                 v1.PodRunning,
			ResourceClaimStatuses: []v1.PodResourceClaimStatus{{Name: "gpu", ResourceClaimName: new("eligible-gpu")}},
			Conditions: []v1.PodCondition{{
				Type:               v1.PodReady,
				Status:             v1.ConditionTrue,
				LastTransitionTime: pod.Status.Conditions[0].LastTransitionTime,
			}},
		},
	}

	assert.NotSame(t, pod, cachedPod)
	assert.Equal(t, expected, cachedPod)
}

func TestInformerTransformsPassThroughUnknownObjects(t *testing.T) {
	t.Parallel()

	tombstone := cache.DeletedFinalStateUnknown{Key: "workload/pod"}
	podTransform := excludedPodTransform(regexp.MustCompile(`^kube-system$`))

	transformedPodObject, err := podTransform(tombstone)
	require.NoError(t, err)
	assert.Equal(t, tombstone, transformedPodObject)

	transformedNodeObject, err := nodeTransform(NodeFields(nil))(tombstone)
	require.NoError(t, err)
	assert.Equal(t, tombstone, transformedNodeObject)
}

func TestNodeTransformRetainsEventAndEvaluatorFields(t *testing.T) {
	t.Parallel()

	node := &v1.Node{
		Name:            "node-a",
		UID:             types.UID("node-uid"),
		ResourceVersion: "42",
		Labels:          map[string]string{"large": "metadata"},
		Annotations: map[string]string{
			common.QuarantineHealthEventAnnotationKey: `{"events":[]}`,
			"unrelated": "discard",
		},
		Spec:   v1.NodeSpec{Unschedulable: true},
		Status: v1.NodeStatus{Phase: v1.NodeRunning},
	}

	transformed, err := nodeTransform(NodeFields(nil))(node)
	require.NoError(t, err)

	transformedNode := transformed.(*v1.Node)
	assert.Equal(t, node.Name, transformedNode.Name)
	assert.Equal(t, node.UID, transformedNode.UID)
	assert.Equal(t, node.ResourceVersion, transformedNode.ResourceVersion)
	assert.Equal(t, map[string]string{
		common.QuarantineHealthEventAnnotationKey: `{"events":[]}`,
	}, transformedNode.Annotations)
	assert.Empty(t, transformedNode.Labels)
	assert.Empty(t, transformedNode.Spec)
	assert.Empty(t, transformedNode.Status)
}

// TestNodeTransform_RequestedLabels_RetainsOnlyRequestedLabels keeps the node cache small when
// customDrain.nodeSelector needs a label to pick the drain path.
func TestNodeTransform_RequestedLabels_RetainsOnlyRequestedLabels(t *testing.T) {
	t.Parallel()

	node := &v1.Node{
		Name:   "node-a",
		Labels: map[string]string{"scheduler": "slurm", "large": "metadata"},
	}

	transformed, err := nodeTransform(NodeFields([]string{"scheduler", "absent"}))(node)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"scheduler": "slurm"}, transformed.(*v1.Node).Labels)
}

// TestNodeTransform_RuleFields_KeepsOnlyWhatTheRuleReads checks the cache against
// the shapes of Node rule ADR-064 calls out. With no rule, the cache matches today.
func TestNodeTransform_RuleFields_KeepsOnlyWhatTheRuleReads(t *testing.T) {
	t.Parallel()

	node := &v1.Node{
		Name: "node-a",
		Labels: map[string]string{
			"scheduler":        "slurm",
			"example.com/pool": "inference",
			"large":            "metadata",
		},
		Annotations: map[string]string{
			common.QuarantineHealthEventAnnotationKey: `{"events":[]}`,
			"example.com/team":                        "alpha",
			"unrelated":                               "discard",
		},
		Spec:   v1.NodeSpec{Unschedulable: true},
		Status: v1.NodeStatus{Phase: v1.NodeRunning},
	}
	quarantineOnly := map[string]string{
		common.QuarantineHealthEventAnnotationKey: `{"events":[]}`,
	}

	tests := []struct {
		name              string
		labelKeys         []string
		expressions       []string
		wantLabels        map[string]string
		wantAnnotations   map[string]string
		wantUnschedulable bool
	}{
		{
			name:            "no rules",
			wantAnnotations: quarantineOnly,
		},
		{
			name:              "custom drain label and rule label are both kept",
			labelKeys:         []string{"scheduler"},
			expressions:       []string{`node.metadata.labels["example.com/pool"] == "inference"`},
			wantLabels:        map[string]string{"scheduler": "slurm", "example.com/pool": "inference"},
			wantAnnotations:   quarantineOnly,
			wantUnschedulable: true,
		},
		{
			name: "literal label keeps that label and the spec",
			expressions: []string{
				`"scheduler" in node.metadata.labels && node.metadata.labels["scheduler"] == "slurm"`,
			},
			wantLabels:        map[string]string{"scheduler": "slurm"},
			wantAnnotations:   quarantineOnly,
			wantUnschedulable: true,
		},
		{
			name:              "literal annotation keeps that annotation and the quarantine annotation",
			expressions:       []string{`"example.com/team" in node.metadata.annotations`},
			wantLabels:        map[string]string{},
			wantAnnotations:   map[string]string{common.QuarantineHealthEventAnnotationKey: `{"events":[]}`, "example.com/team": "alpha"},
			wantUnschedulable: true,
		},
		{
			name:              "label prefix keeps every label",
			expressions:       []string{`node.metadata.labels.exists(k, k.startsWith("example.com/"))`},
			wantLabels:        node.Labels,
			wantAnnotations:   quarantineOnly,
			wantUnschedulable: true,
		},
		{
			name:              "size of the label map keeps every label",
			expressions:       []string{`size(node.metadata.labels) > 0`},
			wantLabels:        node.Labels,
			wantAnnotations:   quarantineOnly,
			wantUnschedulable: true,
		},
		{
			name:              "computed label key keeps every label",
			expressions:       []string{`node.metadata.labels[node.metadata.name] == "node-a"`},
			wantLabels:        node.Labels,
			wantAnnotations:   quarantineOnly,
			wantUnschedulable: true,
		},
		{
			name:              "status read drops status and keeps spec",
			expressions:       []string{`node.status.phase == "Running"`},
			wantLabels:        map[string]string{},
			wantAnnotations:   quarantineOnly,
			wantUnschedulable: true,
		},
		{
			name:              "unparseable expression keeps every label and annotation",
			expressions:       []string{`node.metadata.labels[`},
			wantLabels:        node.Labels,
			wantAnnotations:   node.Annotations,
			wantUnschedulable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			transformed, err := nodeTransform(pruningFields(tt.labelKeys, tt.expressions...))(node)
			require.NoError(t, err)

			cached := transformed.(*v1.Node)
			assert.Equal(t, tt.wantLabels, cached.Labels)
			assert.Equal(t, tt.wantAnnotations, cached.Annotations)
			assert.Equal(t, tt.wantUnschedulable, cached.Spec.Unschedulable)
			assert.Empty(t, cached.Status)
		})
	}
}

// TestNodeTransform_RuleFields_KeepsMetadataTheRuleReads covers metadata that is
// not a label or an annotation. A rule that reads generation must see the real
// value, not zero. With no rule, the cache keeps only the identity, as today.
func TestNodeTransform_RuleFields_KeepsMetadataTheRuleReads(t *testing.T) {
	t.Parallel()

	created := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	managedFields := []metav1.ManagedFieldsEntry{{Manager: "kubelet", Operation: metav1.ManagedFieldsOperationUpdate}}
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node-a",
			UID:               types.UID("node-uid"),
			ResourceVersion:   "42",
			Generation:        3,
			CreationTimestamp: created,
			ManagedFields:     managedFields,
		},
	}

	tests := []struct {
		name              string
		expressions       []string
		wantGeneration    int64
		wantCreated       metav1.Time
		wantManagedFields []metav1.ManagedFieldsEntry
	}{
		{
			name: "no rules keeps only the identity",
		},
		{
			name:           "generation read keeps generation",
			expressions:    []string{`node.metadata.generation == 3`},
			wantGeneration: 3,
			wantCreated:    created,
		},
		{
			name:           "label read still keeps the other metadata fields",
			expressions:    []string{`"scheduler" in node.metadata.labels`},
			wantGeneration: 3,
			wantCreated:    created,
		},
		{
			name:              "managedFields read keeps managedFields",
			expressions:       []string{`size(node.metadata.managedFields) > 0`},
			wantGeneration:    3,
			wantCreated:       created,
			wantManagedFields: managedFields,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			transformed, err := nodeTransform(pruningFields(nil, tt.expressions...))(node)
			require.NoError(t, err)

			cached := transformed.(*v1.Node)
			assert.Equal(t, node.Name, cached.Name)
			assert.Equal(t, node.UID, cached.UID)
			assert.Equal(t, node.ResourceVersion, cached.ResourceVersion)
			assert.Equal(t, tt.wantGeneration, cached.Generation)
			assert.Equal(t, tt.wantCreated, cached.CreationTimestamp)
			assert.Equal(t, tt.wantManagedFields, cached.ManagedFields)
		})
	}
}

// TestNodeTransform_RuleFields_AbsentKeyEvaluatesFalse runs each rule against
// the cached node. A node that has labels and annotations, but none that the
// rule names, must make a guard evaluate to false. It must not fail with a
// missing-key error. A rule that fails would block the decision for the node.
func TestNodeTransform_RuleFields_AbsentKeyEvaluatesFalse(t *testing.T) {
	t.Parallel()

	node := &v1.Node{
		Name:        "node-a",
		Labels:      map[string]string{"large": "metadata"},
		Annotations: map[string]string{"unrelated": "discard"},
	}

	tests := []struct {
		name       string
		expression string
		want       bool
	}{
		{name: "absent label", expression: `"scheduler" in node.metadata.labels`},
		{name: "absent annotation", expression: `"example.com/team" in node.metadata.annotations`},
		{name: "absent label negated", expression: `!("scheduler" in node.metadata.labels)`, want: true},
		{name: "present label", expression: `"large" in node.metadata.labels`, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			transformed, err := nodeTransform(pruningFields(nil, tt.expression))(node)
			require.NoError(t, err)

			rule, err := celrule.NewNodeRule(tt.expression)
			require.NoError(t, err)

			got, err := rule.Evaluate(transformed.(*v1.Node))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// pruningFields builds the cache set the way startup does, then adds Node expressions.
func pruningFields(labelKeys []string, expressions ...string) nodecache.Keys {
	fields := NodeFields(labelKeys)

	for _, expression := range expressions {
		fields.AddNodeExpression(expression)
	}

	return fields
}

func TestInformerTransformsIntegrateWithIndexes(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	systemPod := richDrainEligiblePod("kube-system", "system", "node-a")
	daemonPod := richDrainEligiblePod("workload", "daemon", "node-a")
	daemonPod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "daemon"}}
	eligiblePod := richDrainEligiblePod("workload", "eligible", "node-a")
	node := &v1.Node{
		Name:            "node-a",
		UID:             types.UID("node-uid"),
		ResourceVersion: "7",
		Annotations: map[string]string{
			common.QuarantineHealthEventAnnotationKey: `{"events":[]}`,
		},
	}

	client := fake.NewSimpleClientset(systemPod, daemonPod, eligiblePod, node)
	informers, err := NewInformers(client, 0, new(5), false, model.DefaultPartialRemediationEntityTypes(), false, `^kube-system$`, NodeFields(nil))
	require.NoError(t, err)
	require.NoError(t, informers.Run(ctx))

	nodeIndexedPods, err := informers.podInformer.GetIndexer().ByIndex(NodeIndex, node.Name)
	require.NoError(t, err)
	require.Len(t, nodeIndexedPods, 1)
	assert.Equal(t, eligiblePod.Name, nodeIndexedPods[0].(*v1.Pod).Name)
	assert.Equal(t, eligiblePod.Spec.NodeName, nodeIndexedPods[0].(*v1.Pod).Spec.NodeName)
	assert.Empty(t, nodeIndexedPods[0].(*v1.Pod).Spec.Containers[0].Image)

	systemCached, exists, err := informers.podInformer.GetIndexer().GetByKey("kube-system/system")
	require.NoError(t, err)
	require.True(t, exists)
	assert.Empty(t, systemCached.(*v1.Pod).Spec.NodeName)

	daemonCached, exists, err := informers.podInformer.GetIndexer().GetByKey("workload/daemon")
	require.NoError(t, err)
	require.True(t, exists)
	assert.Empty(t, daemonCached.(*v1.Pod).Spec.NodeName)

	cachedNode, err := informers.GetNode(node.Name)
	require.NoError(t, err)
	assert.Equal(t, node.UID, cachedNode.UID)
	assert.Equal(t, node.ResourceVersion, cachedNode.ResourceVersion)
	assert.Equal(t, node.Annotations, cachedNode.Annotations)
}

func TestEventRecorderAggregatesNodeEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	testEnv := envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })

	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	node, err := client.CoreV1().Nodes().Create(ctx, &v1.Node{
		Name: "node-a",
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	informers, err := NewInformers(client, 0, new(5), false, model.DefaultPartialRemediationEntityTypes(), false, "", NodeFields(nil))
	require.NoError(t, err)
	require.NoError(t, informers.Run(ctx))

	require.NoError(t, informers.UpdateNodeEvent(ctx, node.Name, "AwaitingPodCompletion", "waiting"))
	require.NoError(t, informers.UpdateNodeEvent(ctx, node.Name, "AwaitingPodCompletion", "waiting"))
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		events, listErr := client.CoreV1().Events(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{})
		if !assert.NoError(collect, listErr) || !assert.Len(collect, events.Items, 1) {
			return
		}

		event := events.Items[0]
		assert.Equal(collect, node.UID, event.InvolvedObject.UID)
		assert.Equal(collect, v1.EventTypeNormal, event.Type)
		assert.Equal(collect, "nvsentinel-node-drainer", event.Source.Component)
		assert.Equal(collect, int32(2), event.Count)
	}, 5*time.Second, 50*time.Millisecond)
}

func TestFindEvictablePodsInNamespaceAndNode_DRAClaims_DetectsGPURequests(t *testing.T) {
	ctx := t.Context()

	testEnv := envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })

	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	const namespace = "workload"

	_, err = client.CoreV1().Namespaces().Create(ctx, &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	for claimName, deviceClassName := range map[string]string{
		"gpu-claim":          "gpu.nvidia.com",
		"gpu-template-abc12": "gpu.nvidia.com",
		"nic-claim":          "nic.example.com",
	} {
		_, err = client.ResourceV1().ResourceClaims(namespace).Create(ctx, &resourcev1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName},
			Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
				Name:    "device",
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: deviceClassName},
			}}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	// Allocated claims: the allocation, not the requested DeviceClasses, decides which driver the pod uses.
	for claimName, alloc := range map[string]struct {
		requests []resourcev1.DeviceRequest
		result   resourcev1.DeviceRequestAllocationResult
	}{
		"fallback-nic-allocated": {
			requests: []resourcev1.DeviceRequest{{Name: "device", FirstAvailable: []resourcev1.DeviceSubRequest{
				{Name: "nic", DeviceClassName: "nic.example.com", AllocationMode: resourcev1.DeviceAllocationModeExactCount, Count: 1},
				{Name: "gpu", DeviceClassName: "gpu.nvidia.com", AllocationMode: resourcev1.DeviceAllocationModeExactCount, Count: 1},
			}}},
			result: resourcev1.DeviceRequestAllocationResult{
				Request: "device/nic", Driver: "nic.example.com", Pool: "node-0", Device: "nic-0",
			},
		},
		"gpu-allocated": {
			requests: []resourcev1.DeviceRequest{{
				Name:    "device",
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: "gpu.nvidia.com"},
			}},
			result: resourcev1.DeviceRequestAllocationResult{
				Request: "device", Driver: "gpu.nvidia.com", Pool: "node-0", Device: "gpu-0",
			},
		},
	} {
		claim, err := client.ResourceV1().ResourceClaims(namespace).Create(ctx, &resourcev1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName},
			Spec:       resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: alloc.requests}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		claim.Status.Allocation = &resourcev1.AllocationResult{
			Devices: resourcev1.DeviceAllocationResult{Results: []resourcev1.DeviceRequestAllocationResult{alloc.result}},
		}
		_, err = client.ResourceV1().ResourceClaims(namespace).UpdateStatus(ctx, claim, metav1.UpdateOptions{})
		require.NoError(t, err)
	}

	const (
		missingAnnotation = "is requesting devices but is missing device annotation; " +
			"partial drains need the metadata-collector pod device mapper (podMapper.enabled)"
		noClaimNeeded     = "-" // bind the claim status entry with a nil ResourceClaimName
	)

	tests := []struct {
		name            string
		annotation      string
		gpuLimit        bool
		claim           v1.PodResourceClaim
		boundClaim      string
		wantPartialErr  string
		wantPartialPods int
		wantGPUOnlyErr  string
		wantGPUOnlyPods int
	}{
		{
			name:            "unannotated pod with GPU claim fails partial drain",
			claim:           v1.PodResourceClaim{Name: "gpu", ResourceClaimName: new("gpu-claim")},
			wantPartialErr:  missingAnnotation,
			wantGPUOnlyPods: 1,
		},
		{
			name:            "unannotated pod with generated GPU claim fails partial drain without reading the template",
			claim:           v1.PodResourceClaim{Name: "gpu", ResourceClaimTemplateName: new("deleted-template")},
			boundClaim:      "gpu-template-abc12",
			wantPartialErr:  missingAnnotation,
			wantGPUOnlyPods: 1,
		},
		{
			name:           "unannotated pod with template claim but no bound claim fails closed",
			claim:          v1.PodResourceClaim{Name: "gpu", ResourceClaimTemplateName: new("gpu-template")},
			wantPartialErr: "no ResourceClaim is bound",
			wantGPUOnlyErr: "no ResourceClaim is bound",
		},
		{
			name:  "unannotated pod with non-GPU claim is not a GPU pod",
			claim: v1.PodResourceClaim{Name: "nic", ResourceClaimName: new("nic-claim")},
		},
		{
			name:           "unannotated pod with missing claim fails closed",
			claim:          v1.PodResourceClaim{Name: "gpu", ResourceClaimName: new("missing-claim")},
			wantPartialErr: "not found",
			wantGPUOnlyErr: "not found",
		},
		{
			name:            "annotated pod with an entry for every DRA driver is drained without reading its claim",
			annotation:      `{"devices":{"gpu.nvidia.com":["GPU-1"]}}`,
			claim:           v1.PodResourceClaim{Name: "gpu", ResourceClaimName: new("missing-claim")},
			wantPartialPods: 1,
			wantGPUOnlyPods: 1,
		},
		{
			// The annotation has no entry for the DRA driver, so the claim decides whether the pod
			// requests a GPU through DRA. A claim that cannot be read fails closed.
			name:            "annotated pod without a DRA driver entry fails closed on a missing claim",
			annotation:      `{"devices":{"nvidia.com/gpu":["GPU-1"]}}`,
			claim:           v1.PodResourceClaim{Name: "gpu", ResourceClaimName: new("missing-claim")},
			wantPartialErr:  "not found",
			wantGPUOnlyPods: 1,
		},
		{
			name:            "annotated pod whose GPU claim has no annotation entry fails partial drain",
			annotation:      `{"devices":{"nvidia.com/gpu":["GPU-0"]}}`,
			claim:           v1.PodResourceClaim{Name: "gpu", ResourceClaimName: new("gpu-claim")},
			wantPartialErr:  "is requesting DeviceClass gpu.nvidia.com of DRA driver gpu.nvidia.com but its device annotation has no entry for it",
			wantGPUOnlyPods: 1,
		},
		{
			name:            "annotated pod whose claim was allocated a non-GPU alternative is drained",
			annotation:      `{"devices":{"nvidia.com/gpu":["GPU-1"]}}`,
			gpuLimit:        true,
			claim:           v1.PodResourceClaim{Name: "fallback", ResourceClaimName: new("fallback-nic-allocated")},
			wantPartialPods: 1,
			wantGPUOnlyPods: 1,
		},
		{
			name:            "annotated pod whose allocated GPU claim has no annotation entry fails partial drain",
			annotation:      `{"devices":{"nvidia.com/gpu":["GPU-0"]}}`,
			claim:           v1.PodResourceClaim{Name: "gpu", ResourceClaimName: new("gpu-allocated")},
			wantPartialErr:  "is requesting a device of DRA driver gpu.nvidia.com but its device annotation has no entry for it",
			wantGPUOnlyPods: 1,
		},
		{
			name:            "annotated pod whose GPU limit has no annotation entry fails partial drain",
			annotation:      `{"devices":{"gpu.nvidia.com":["GPU-0"]}}`,
			gpuLimit:        true,
			wantPartialErr:  "is requesting resource nvidia.com/gpu but its device annotation has no entry for it",
			wantGPUOnlyPods: 1,
		},
		{
			name:            "annotated pod with a non-GPU claim is kept when it uses the entity",
			annotation:      `{"devices":{"nvidia.com/gpu":["GPU-1"]}}`,
			gpuLimit:        true,
			claim:           v1.PodResourceClaim{Name: "nic", ResourceClaimName: new("nic-claim")},
			wantPartialPods: 1,
			wantGPUOnlyPods: 1,
		},
		{
			name:            "unannotated pod with GPU limit fails partial drain and is GPU-only drained",
			gpuLimit:        true,
			wantPartialErr:  missingAnnotation,
			wantGPUOnlyPods: 1,
		},
		{
			name:       "unannotated pod whose claim status says no claim was needed is not a GPU pod",
			claim:      v1.PodResourceClaim{Name: "gpu", ResourceClaimTemplateName: new("gpu-template")},
			boundClaim: noClaimNeeded,
		},
		{
			name: "CPU-only pod is neither drained nor blocks the drain",
		},
	}

	for idx, tt := range tests {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", idx), Namespace: namespace},
			Spec: v1.PodSpec{
				NodeName:   fmt.Sprintf("node-%d", idx),
				Containers: []v1.Container{{Name: "workload", Image: "workload:latest"}},
			},
		}
		if tt.annotation != "" {
			pod.Annotations = map[string]string{model.PodDeviceAnnotationName: tt.annotation}
		}

		if tt.gpuLimit {
			pod.Spec.Containers[0].Resources.Limits = v1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
		}

		if tt.claim.Name != "" {
			pod.Spec.ResourceClaims = []v1.PodResourceClaim{tt.claim}
		}

		created, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
		require.NoError(t, err)

		if tt.boundClaim != "" {
			claimStatus := v1.PodResourceClaimStatus{Name: tt.claim.Name}
			if tt.boundClaim != noClaimNeeded {
				claimStatus.ResourceClaimName = new(tt.boundClaim)
			}

			created.Status.ResourceClaimStatuses = []v1.PodResourceClaimStatus{claimStatus}
			_, err = client.CoreV1().Pods(namespace).UpdateStatus(ctx, created, metav1.UpdateOptions{})
			require.NoError(t, err)
		}
	}

	informers, err := NewInformers(client, 0, new(5), true, model.DefaultPartialRemediationEntityTypes(), false, "", NodeFields(nil))
	require.NoError(t, err)
	require.NoError(t, informers.Run(ctx))

	partialDrainEntity := &protos.Entity{EntityType: "GPU_UUID", EntityValue: "GPU-1"}

	for idx, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeName := fmt.Sprintf("node-%d", idx)

			pods, err := informers.FindEvictablePodsInNamespaceAndNode(ctx, namespace, nodeName, partialDrainEntity)
			if tt.wantPartialErr != "" {
				require.ErrorContains(t, err, tt.wantPartialErr)
			} else {
				require.NoError(t, err)
				assert.Len(t, pods, tt.wantPartialPods)
			}

			pods, err = informers.FindEvictablePodsInNamespaceAndNode(ctx, namespace, nodeName, nil)
			if tt.wantGPUOnlyErr != "" {
				require.ErrorContains(t, err, tt.wantGPUOnlyErr)

				event := &model.HealthEventWithStatus{CreatedAt: time.Now().Add(-time.Hour)}
				err = informers.DeletePodsAfterTimeout(ctx, nodeName, []string{namespace}, 1, event, nil)
				require.ErrorContains(t, err, "could not check every namespace",
					"timeout path must requeue instead of force deleting nothing")
			} else {
				require.NoError(t, err)
				assert.Len(t, pods, tt.wantGPUOnlyPods)
			}
		})
	}
}

func TestFindEvictablePodsInNamespaceAndNode_ConfiguredEntityType_UsesItsDevices(t *testing.T) {
	ctx := t.Context()

	testEnv := envtest.Environment{}
	cfg, err := testEnv.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })

	client, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	const namespace = "workload"

	_, err = client.CoreV1().Namespaces().Create(ctx, &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	for claimName, deviceClassName := range map[string]string{
		"accel-claim":       "accel.example.com",
		"accel-large-claim": "accel-large.example.com",
	} {
		_, err = client.ResourceV1().ResourceClaims(namespace).Create(ctx, &resourcev1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName},
			Spec: resourcev1.ResourceClaimSpec{Devices: resourcev1.DeviceClaim{Requests: []resourcev1.DeviceRequest{{
				Name:    "device",
				Exactly: &resourcev1.ExactDeviceRequest{DeviceClassName: deviceClassName},
			}}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	entityTypes := model.DefaultPartialRemediationEntityTypes()
	entityTypes["ACCEL_SERIAL"] = model.PartialRemediationEntityType{
		Devices: []model.PartialRemediationDevice{
			{ResourceNames: []string{"example.com/accel"}},
			{
				Driver:               "accel.example.com",
				DeviceClasses:        []string{"accel.example.com", "accel-large.example.com"},
				EntityValueAttribute: "serial",
			},
		},
		ScopedActions: []string{"accel-reset"},
	}

	tests := []struct {
		name            string
		annotation      string
		limit           string
		claim           string
		wantPartialErr  string
		wantPartialPods int
		wantFullPods    int
	}{
		{
			name:            "pod using the entity through DRA is drained",
			annotation:      `{"devices":{"accel.example.com":["ACC-1"]}}`,
			claim:           "accel-claim",
			wantPartialPods: 1,
			wantFullPods:    1,
		},
		{
			name:            "pod using the entity through a device plugin is drained",
			annotation:      `{"devices":{"example.com/accel":["ACC-1"]}}`,
			limit:           "example.com/accel",
			wantPartialPods: 1,
			wantFullPods:    1,
		},
		{
			name:         "pod using another device of the entity type keeps running",
			annotation:   `{"devices":{"accel.example.com":["ACC-2"]}}`,
			claim:        "accel-large-claim",
			wantFullPods: 1,
		},
		{
			name:         "the same value under a name of another entity type keeps running",
			annotation:   `{"devices":{"nvidia.com/gpu":["ACC-1"]}}`,
			limit:        "nvidia.com/gpu",
			wantFullPods: 1,
		},
		{
			name:           "pod whose DeviceClass has no annotation entry fails the drain",
			annotation:     `{"devices":{"example.com/accel":["ACC-2"]}}`,
			claim:          "accel-large-claim",
			wantPartialErr: "is requesting DeviceClass accel-large.example.com of DRA driver accel.example.com",
			wantFullPods:   1,
		},
		{
			name:           "unannotated pod with a device-plugin limit fails the drain",
			limit:          "example.com/accel",
			wantPartialErr: "is requesting devices but is missing device annotation",
			wantFullPods:   1,
		},
		{
			name: "CPU-only pod is neither drained nor blocks the drain",
		},
	}

	for idx, tt := range tests {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", idx), Namespace: namespace},
			Spec: v1.PodSpec{
				NodeName:   fmt.Sprintf("node-%d", idx),
				Containers: []v1.Container{{Name: "workload", Image: "workload:latest"}},
			},
		}
		if tt.annotation != "" {
			pod.Annotations = map[string]string{model.PodDeviceAnnotationName: tt.annotation}
		}

		if tt.limit != "" {
			pod.Spec.Containers[0].Resources.Limits = v1.ResourceList{v1.ResourceName(tt.limit): resource.MustParse("1")}
		}

		if tt.claim != "" {
			pod.Spec.ResourceClaims = []v1.PodResourceClaim{{Name: "device", ResourceClaimName: new(tt.claim)}}
		}

		_, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	informers, err := NewInformers(client, 0, new(5), true, entityTypes, false, "", NodeFields(nil))
	require.NoError(t, err)
	require.NoError(t, informers.Run(ctx))

	partialDrainEntity := &protos.Entity{EntityType: "ACCEL_SERIAL", EntityValue: "ACC-1"}

	for idx, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeName := fmt.Sprintf("node-%d", idx)

			pods, err := informers.FindEvictablePodsInNamespaceAndNode(ctx, namespace, nodeName, partialDrainEntity)
			if tt.wantPartialErr != "" {
				require.ErrorContains(t, err, tt.wantPartialErr)
			} else {
				require.NoError(t, err)
				assert.Len(t, pods, tt.wantPartialPods)
			}

			// drainGPUPods keeps the pods that request a device of any configured entity type.
			pods, err = informers.FindEvictablePodsInNamespaceAndNode(ctx, namespace, nodeName, nil)
			require.NoError(t, err)
			assert.Len(t, pods, tt.wantFullPods)
		})
	}
}

func richDrainEligiblePod(namespace, name, nodeName string) *v1.Pod {
	deletionTimestamp := metav1.NewTime(time.Now().Add(-time.Minute))

	return &v1.Pod{
		Name:            name,
		Namespace:       namespace,
		UID:             types.UID(namespace + "-" + name),
		ResourceVersion: "11",
		Labels:          map[string]string{"app": name},
		Annotations: map[string]string{
			model.PodDeviceAnnotationName: `{"devices":{"nvidia.com/gpu":["GPU-1"]}}`,
			"unrelated":                   "discard",
		},
		OwnerReferences:   []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "owner"}},
		Finalizers:        []string{"example.com/finalizer"},
		DeletionTimestamp: &deletionTimestamp,
		Spec: v1.PodSpec{
			NodeName:                      nodeName,
			TerminationGracePeriodSeconds: new(int64(60)),
			InitContainers: []v1.Container{
				{
					Name:  "init",
					Image: "init:latest",
					Resources: v1.ResourceRequirements{
						Limits:   v1.ResourceList{v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")},
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m")},
					},
				},
			},
			Containers: []v1.Container{
				{
					Name:  "workload",
					Image: "workload:latest",
					Resources: v1.ResourceRequirements{
						Limits:   v1.ResourceList{v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")},
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m")},
					},
				},
			},
			ResourceClaims: []v1.PodResourceClaim{{Name: "gpu", ResourceClaimTemplateName: new("gpu-template")}},
		},
		Status: v1.PodStatus{
			Phase:                 v1.PodRunning,
			ResourceClaimStatuses: []v1.PodResourceClaimStatus{{Name: "gpu", ResourceClaimName: new("eligible-gpu")}},
			Conditions: []v1.PodCondition{
				{
					Type:               v1.PodReady,
					Status:             v1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute)),
				},
				{Type: v1.PodScheduled, Status: v1.ConditionTrue},
			},
			ContainerStatuses: []v1.ContainerStatus{{Name: "workload", Ready: true}},
		},
	}
}
