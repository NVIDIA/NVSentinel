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

package initializer

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"

	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

const nodeNotReadyPredicate = `resource.status.conditions.filter(c, c.type == "Ready" && c.status == "False").size() > 0`

var (
	nodeGVK = schema.GroupVersionKind{Version: "v1", Kind: "Node"}
	podGVK  = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
)

func TestTransform_ProductionShapedNode_RetainsOnlyPolicyFields(t *testing.T) {
	transform := transformForGVK(t, nodeGVK, []config.Policy{
		policyWithExpressions("node-not-ready", nodeGVK, nodeNotReadyPredicate, ""),
	})

	node := productionShapedNode()
	before := jsonSize(t, node)

	out, err := transform(node)
	require.NoError(t, err)

	pruned, ok := out.(*unstructured.Unstructured)
	require.True(t, ok)

	// The predicate reads status.conditions, so the whole condition list
	// survives with every field of every element.
	conditions, found, err := unstructured.NestedSlice(pruned.Object, "status", "conditions")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, conditions, 38)
	require.Equal(t, map[string]any{
		"type":               "Ready",
		"status":             "True",
		"reason":             "KubeletReady",
		"message":            "kubelet is posting ready status",
		"lastHeartbeatTime":  "2026-09-01T12:00:00Z",
		"lastTransitionTime": "2026-08-01T12:00:00Z",
	}, conditions[0])

	// Informer-critical metadata survives, and so does everything the
	// reconciler reads outside CEL.
	require.Equal(t, "v1", pruned.GetAPIVersion())
	require.Equal(t, "Node", pruned.GetKind())
	require.Equal(t, "gpu-node-0042", pruned.GetName())
	require.Empty(t, pruned.GetNamespace())
	require.Equal(t, "1f0a6c9e-3f5b-4a1d-9c3e-2b7d5a8f4e11", string(pruned.GetUID()))
	require.Equal(t, "918273645", pruned.GetResourceVersion())

	// Everything the policy does not read is gone.
	for _, path := range [][]string{
		{"metadata", "labels"},
		{"metadata", "annotations"},
		{"metadata", "managedFields"},
		{"spec"},
		{"status", "images"},
		{"status", "capacity"},
		{"status", "allocatable"},
		{"status", "nodeInfo"},
		{"status", "addresses"},
	} {
		_, found, err := unstructured.NestedFieldNoCopy(pruned.Object, path...)
		require.NoError(t, err)
		require.False(t, found, "expected %v to be pruned", path)
	}

	after := jsonSize(t, pruned)
	require.Less(t, after*4, before,
		"expected at least a 4x reduction, got %d bytes from %d", after, before)
}

// TestTransform_LabelKeyContainingDots_IsRetained covers a policy that indexes
// a label by a literal key. Label keys routinely contain dots, so a path
// flattened to a dotted string would be read back as five nested fields, none
// of which exist, and the label the policy reads would be pruned away.
func TestTransform_LabelKeyContainingDots_IsRetained(t *testing.T) {
	transform := transformForGVK(t, nodeGVK, []config.Policy{
		policyWithExpressions("gpu-present", nodeGVK,
			`resource.metadata.labels["nvidia.com/gpu.present"] != "true"`, ""),
	})

	node := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata": map[string]any{
			"name": "gpu-node-0042",
			"labels": map[string]any{
				"nvidia.com/gpu.present":      "true",
				"topology.kubernetes.io/zone": "us-west-2a",
			},
		},
	}}

	out, err := transform(node)
	require.NoError(t, err)

	pruned, ok := out.(*unstructured.Unstructured)
	require.True(t, ok)

	labels, found, err := unstructured.NestedStringMap(pruned.Object, "metadata", "labels")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, map[string]string{"nvidia.com/gpu.present": "true"}, labels)
}

func TestTransform_DeletionTimestampPresent_IsRetained(t *testing.T) {
	transform := transformForGVK(t, nodeGVK, []config.Policy{
		policyWithExpressions("node-not-ready", nodeGVK, nodeNotReadyPredicate, ""),
	})

	node := productionShapedNode()
	require.NoError(t, unstructured.SetNestedField(
		node.Object, "2026-09-01T13:00:00Z", "metadata", "deletionTimestamp"))

	out, err := transform(node)
	require.NoError(t, err)

	pruned, ok := out.(*unstructured.Unstructured)
	require.True(t, ok)
	require.NotNil(t, pruned.GetDeletionTimestamp())
}

func TestTransform_NodeAssociationExpression_FieldsAreRetained(t *testing.T) {
	transform := transformForGVK(t, podGVK, []config.Policy{
		policyWithExpressions(
			"gpu-operator-pod-health",
			podGVK,
			`resource.status.phase != 'Running'`,
			`resource.spec.nodeName`,
		),
	})

	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      "nvidia-device-plugin-daemonset-abcde",
			"namespace": "gpu-operator",
			"labels":    map[string]any{"app": "nvidia-device-plugin-daemonset"},
		},
		"spec": map[string]any{
			"nodeName":   "gpu-node-0042",
			"containers": []any{map[string]any{"name": "nvidia-device-plugin-ctr"}},
		},
		"status": map[string]any{
			"phase":    "Pending",
			"hostIP":   "10.0.4.42",
			"podIPs":   []any{map[string]any{"ip": "10.244.4.7"}},
			"qosClass": "BestEffort",
		},
	}}

	out, err := transform(pod)
	require.NoError(t, err)

	pruned, ok := out.(*unstructured.Unstructured)
	require.True(t, ok)

	require.Equal(t, map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      "nvidia-device-plugin-daemonset-abcde",
			"namespace": "gpu-operator",
		},
		"spec":   map[string]any{"nodeName": "gpu-node-0042"},
		"status": map[string]any{"phase": "Pending"},
	}, pruned.Object)
}

func TestTransform_NonUnstructuredInput_PassesThroughUnchanged(t *testing.T) {
	transform := transformForGVK(t, nodeGVK, []config.Policy{
		policyWithExpressions("node-not-ready", nodeGVK, nodeNotReadyPredicate, ""),
	})

	// The same GVK is also served to structured readers and to tombstones from
	// the delta FIFO. Neither can be pruned safely, so both pass through.
	structured := &corev1.Node{}
	out, err := transform(structured)
	require.NoError(t, err)
	require.Same(t, structured, out)

	tombstone := toolscache.DeletedFinalStateUnknown{Key: "gpu-node-0042"}
	out, err = transform(tombstone)
	require.NoError(t, err)
	require.Equal(t, tombstone, out)
}

func TestBuildCacheEntries_OpaquePolicy_CachesGVKInFull(t *testing.T) {
	entries := cacheEntries(t, []config.Policy{
		policyWithExpressions("node-not-ready", nodeGVK, nodeNotReadyPredicate, ""),
		// Reads the object as a whole: the fields it touches are not derivable,
		// so Node must be cached in full.
		policyWithExpressions("node-opaque", nodeGVK, `size(resource) > 3`, ""),
		policyWithExpressions("pod-health", podGVK, `resource.status.phase != 'Running'`, ""),
	})

	require.Contains(t, entries, nodeGVK)
	require.Nil(t, entries[nodeGVK].transform)
	require.NotNil(t, entries[podGVK].transform)
}

// TestBuildCacheEntries_LookupOfWatchedGVK_UnionsBothPathSets is the fail-open
// case. The pod entry is pruned to what the policy watching pods reads, and a
// lookup() from another policy reads a field outside that set. Pruning it away
// would not fail the lookup: CEL cannot tell a field the cache dropped from one
// the object never had, so the predicate would quietly evaluate against an
// absent field.
func TestBuildCacheEntries_LookupOfWatchedGVK_UnionsBothPathSets(t *testing.T) {
	entries := cacheEntries(t, []config.Policy{
		policyWithExpressions("pod-health", podGVK, `resource.status.phase != 'Running'`, ""),
		policyWithExpressions("node-owns-pod", nodeGVK,
			`lookup('v1', 'Pod', 'default', 'device-plugin').spec.nodeName == resource.metadata.name`, ""),
	})

	require.True(t, entries[podGVK].servesLookups)

	pod := prunedObject(t, entries[podGVK].transform, map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": "device-plugin", "namespace": "default"},
		"spec":       map[string]any{"nodeName": "gpu-node-0042", "serviceAccountName": "pruned"},
		"status":     map[string]any{"phase": "Running", "podIP": "10.0.0.1"},
	})

	require.Equal(t, map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": "device-plugin", "namespace": "default"},
		"spec":       map[string]any{"nodeName": "gpu-node-0042"},
		"status":     map[string]any{"phase": "Running"},
	}, pod.Object)
}

// TestBuildCacheEntries_WholeObjectLookup_LeavesWatchTransformInPlace covers a
// lookup() whose result cannot be reduced to fields. The GVK keeps the
// transform its own policy earns, and the lookup reads through the API rather
// than the entry that transform prunes.
func TestBuildCacheEntries_WholeObjectLookup_LeavesWatchTransformInPlace(t *testing.T) {
	entries := cacheEntries(t, []config.Policy{
		policyWithExpressions("pod-health", podGVK, `resource.status.phase != 'Running'`, ""),
		policyWithExpressions("node-owns-pod", nodeGVK,
			`size(lookup('v1', 'Pod', 'default', 'device-plugin')) > 3`, ""),
	})

	require.NotNil(t, entries[podGVK].transform)
	require.False(t, entries[podGVK].servesLookups)
}

// TestBuildCacheEntries_WholeObjectLookupOfUnwatchedGVK_HasNoEntry keeps the
// cache from holding a GVK it can do nothing useful with: no policy watches it,
// and the lookup that names it cannot be served from a pruned entry.
func TestBuildCacheEntries_WholeObjectLookupOfUnwatchedGVK_HasNoEntry(t *testing.T) {
	entries := cacheEntries(t, []config.Policy{
		policyWithExpressions("node-owns-pod", nodeGVK,
			`size(lookup('v1', 'Pod', 'default', 'device-plugin')) > 3`, ""),
	})

	require.NotContains(t, entries, podGVK)
}

func TestBuildCacheEntries_DisabledPolicy_IsIgnored(t *testing.T) {
	opaque := policyWithExpressions("node-opaque", nodeGVK, `size(resource) > 3`, "")
	opaque.Enabled = false

	entries := cacheEntries(t, []config.Policy{
		policyWithExpressions("node-not-ready", nodeGVK, nodeNotReadyPredicate, ""),
		opaque,
	})

	require.NotNil(t, entries[nodeGVK].transform)
}

func TestFieldTree_RedundantPaths_CollapseIntoRetainedSubtree(t *testing.T) {
	tree := newFieldTree([][]string{{"metadata", "name"}, {"status"}})
	tree.insert([]string{"status", "conditions", "type"})
	tree.insert([]string{"metadata", "labels", "example"})

	require.Equal(t, []string{"metadata.labels.example", "metadata.name", "status"}, tree.retainedPaths())
}

func prunedObject(
	t *testing.T,
	transform toolscache.TransformFunc,
	object map[string]any,
) *unstructured.Unstructured {
	t.Helper()

	out, err := transform(&unstructured.Unstructured{Object: object})
	require.NoError(t, err)

	pruned, ok := out.(*unstructured.Unstructured)
	require.True(t, ok)

	return pruned
}

func cacheEntries(t *testing.T, policies []config.Policy) map[schema.GroupVersionKind]gvkCacheEntry {
	t.Helper()

	compiler, err := celenv.NewCompilerEnvironment()
	require.NoError(t, err)

	return buildCacheEntries(compiler, policies)
}

func transformForGVK(
	t *testing.T,
	gvk schema.GroupVersionKind,
	policies []config.Policy,
) toolscache.TransformFunc {
	t.Helper()

	entries := cacheEntries(t, policies)
	require.Contains(t, entries, gvk)
	require.NotNil(t, entries[gvk].transform)

	return entries[gvk].transform
}

func policyWithExpressions(name string, gvk schema.GroupVersionKind, predicate, nodeAssociation string) config.Policy {
	p := config.Policy{
		Name:    name,
		Enabled: true,
		Resource: config.ResourceSpec{
			Group:   gvk.Group,
			Version: gvk.Version,
			Kind:    gvk.Kind,
		},
		Predicate: config.PredicateSpec{Expression: predicate},
	}

	if nodeAssociation != "" {
		p.NodeAssociation = &config.AssociationSpec{Expression: nodeAssociation}
	}

	return p
}

func jsonSize(t *testing.T, obj *unstructured.Unstructured) int {
	t.Helper()

	encoded, err := json.Marshal(obj.Object)
	require.NoError(t, err)

	return len(encoded)
}

// productionShapedNode builds a node with the field counts seen on a large
// production cluster: ~180 labels, 38 status conditions, 50 cached images and a
// managedFields entry per controller that has written to the object.
func productionShapedNode() *unstructured.Unstructured {
	labels := make(map[string]any, 180)
	annotations := make(map[string]any, 24)
	conditions := make([]any, 0, 38)
	images := make([]any, 0, 50)
	managedFields := make([]any, 0, 8)

	for i := range 180 {
		labels[fmt.Sprintf("nvsentinel.nvidia.com/label-%03d", i)] = fmt.Sprintf("value-%03d", i)
	}

	for i := range 24 {
		annotations[fmt.Sprintf("nvsentinel.nvidia.com/annotation-%02d", i)] =
			fmt.Sprintf(`{"key-%02d":"a fairly long annotation value that pads the object out"}`, i)
	}

	conditions = append(conditions, map[string]any{
		"type":               "Ready",
		"status":             "True",
		"reason":             "KubeletReady",
		"message":            "kubelet is posting ready status",
		"lastHeartbeatTime":  "2026-09-01T12:00:00Z",
		"lastTransitionTime": "2026-08-01T12:00:00Z",
	})

	for i := range 37 {
		conditions = append(conditions, map[string]any{
			"type":               fmt.Sprintf("GPU%02dHealthy", i),
			"status":             "False",
			"reason":             "NoFaultDetected",
			"message":            fmt.Sprintf("GPU %02d reported no faults during the last health check", i),
			"lastHeartbeatTime":  "2026-09-01T12:00:00Z",
			"lastTransitionTime": "2026-08-01T12:00:00Z",
		})
	}

	for i := range 50 {
		images = append(images, map[string]any{
			"names": []any{
				fmt.Sprintf("nvcr.io/nvidia/component-%02d@sha256:%064d", i, i),
				fmt.Sprintf("nvcr.io/nvidia/component-%02d:v1.%d.0", i, i),
			},
			"sizeBytes": int64(150_000_000 + i),
		})
	}

	for i := range 8 {
		managedFields = append(managedFields, map[string]any{
			"manager":    fmt.Sprintf("controller-%02d", i),
			"operation":  "Update",
			"apiVersion": "v1",
			"time":       "2026-09-01T12:00:00Z",
			"fieldsType": "FieldsV1",
			"fieldsV1": map[string]any{
				"f:metadata": map[string]any{
					"f:labels":      labels,
					"f:annotations": annotations,
				},
				"f:status": map[string]any{"f:conditions": map[string]any{}},
			},
		})
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata": map[string]any{
			"name":              "gpu-node-0042",
			"uid":               "1f0a6c9e-3f5b-4a1d-9c3e-2b7d5a8f4e11",
			"resourceVersion":   "918273645",
			"creationTimestamp": "2026-06-01T09:00:00Z",
			"generation":        int64(7),
			"labels":            labels,
			"annotations":       annotations,
			"managedFields":     managedFields,
		},
		"spec": map[string]any{
			"podCIDR":    "10.244.42.0/24",
			"podCIDRs":   []any{"10.244.42.0/24"},
			"providerID": "aws:///us-west-2a/i-0abcdef1234567890",
			"taints": []any{
				map[string]any{"key": "nvidia.com/gpu", "value": "present", "effect": "NoSchedule"},
			},
		},
		"status": map[string]any{
			"conditions": conditions,
			"images":     images,
			"capacity": map[string]any{
				"cpu": "224", "memory": "2113929216Ki", "nvidia.com/gpu": "8", "pods": "250",
			},
			"allocatable": map[string]any{
				"cpu": "223500m", "memory": "2113400832Ki", "nvidia.com/gpu": "8", "pods": "250",
			},
			"addresses": []any{
				map[string]any{"type": "InternalIP", "address": "10.0.4.42"},
				map[string]any{"type": "Hostname", "address": "gpu-node-0042"},
			},
			"nodeInfo": map[string]any{
				"machineID":               "ec2f0a6c9e3f5b4a1d9c3e2b7d5a8f4e",
				"systemUUID":              "ec2f0a6c-9e3f-5b4a-1d9c-3e2b7d5a8f4e",
				"bootID":                  "8f4e2b7d-5a8f-4e11-9c3e-1f0a6c9e3f5b",
				"kernelVersion":           "6.8.0-1029-aws",
				"osImage":                 "Ubuntu 24.04.3 LTS",
				"containerRuntimeVersion": "containerd://1.7.28",
				"kubeletVersion":          "v1.33.4",
				"kubeProxyVersion":        "v1.33.4",
				"operatingSystem":         "linux",
				"architecture":            "amd64",
			},
			"daemonEndpoints": map[string]any{
				"kubeletEndpoint": map[string]any{"Port": int64(10250)},
			},
		},
	}}
}
