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

package informer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/nvidia/nvsentinel/commons/pkg/celrule"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/nodecache"
)

// newRuleNodeInformer returns a NodeInformer whose cache holds cached and whose
// API server holds live.
func newRuleNodeInformer(t *testing.T, cached []*v1.Node, live ...*v1.Node) (*NodeInformer, *fake.Clientset) {
	t.Helper()

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, node := range cached {
		require.NoError(t, indexer.Add(node))
	}

	objects := make([]runtime.Object, 0, len(live))
	for _, node := range live {
		objects = append(objects, node)
	}

	clientset := fake.NewSimpleClientset(objects...)

	return &NodeInformer{clientset: clientset, lister: corelisters.NewNodeLister(indexer)}, clientset
}

func countNodeGets(clientset *fake.Clientset) int {
	count := 0

	for _, action := range clientset.Actions() {
		if action.Matches("get", "nodes") {
			count++
		}
	}

	return count
}

func newOptOutRule(t *testing.T, ni *NodeInformer, expression string) *celrule.NodeRule {
	t.Helper()

	rule, err := celrule.NewNodeRule(expression, celrule.NodeGetterFunc(ni.GetRuleNode))
	require.NoError(t, err)

	return rule
}

func retainedForRule(expression string, operational nodecache.Operational) nodecache.Keys {
	return nodecache.Derive(config.TomlConfig{
		LabelPrefix: "k8saas.nvidia.com/",
		RuleSets: []config.QuarantineRuleSet{{
			RuleSetMeta: config.RuleSetMeta{
				Enabled: true,
				Name:    "shipped",
				Match: config.Match{
					All: []config.Rule{{Kind: "Node", Expression: expression}},
				},
			},
		}},
	}, operational)
}

// A healthy node that has never been quarantined carries none of the keys the
// cache retains, so it prunes to empty label and annotation maps. Converting it
// for CEL drops an empty map, and an absent map raises "no such key" rather than
// evaluating the guard as false, which would fail every rule and stop
// fault-quarantine cordoning the node at all.
func TestGetRuleNode_PrunedNodeKeepsNoRetainedKeys_EvaluatesOptOutGuards(t *testing.T) {
	const optOutGuards = `!('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels &&
		 node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false") &&
	!('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels &&
	  node.metadata.labels['nvsentinel.dgxc.nvidia.com/managed'] == "false") &&
	!('quarantinedNodeUncordonedManually' in node.metadata.annotations)`

	transformed, err := retainedForRule(optOutGuards, nodecache.Operational{}).Transform()(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "healthy-node",
			Labels:      map[string]string{"kubernetes.io/hostname": "healthy-node"},
			Annotations: map[string]string{"unrelated/annotation": "value"},
		},
	})
	require.NoError(t, err)

	pruned, ok := transformed.(*v1.Node)
	require.True(t, ok, "Transform() returned %T", transformed)
	require.Empty(t, pruned.Labels, "node under test must retain no labels")
	require.Empty(t, pruned.Annotations, "node under test must retain no annotations")

	ni, _ := newRuleNodeInformer(t, []*v1.Node{pruned})

	matched, err := newOptOutRule(t, ni, optOutGuards).Evaluate(
		context.Background(), &protos.HealthEvent{NodeName: "healthy-node"})
	require.NoError(t, err)
	require.True(t, matched, "a node that opted out of nothing must match the opt-out guards")
}

// Recovery reads the node from the API server rather than the cache, so it has
// to see an opt-out the cache does not hold. Pruning narrows the cache as well
// as ageing it, so this drives both paths of the same rule against a cache
// pruned by the shipped rules.
func TestGetRuleNode_RecoveryRead_PrunedCacheDoesNotHideOptOut(t *testing.T) {
	const optOutGuard = `!('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && ` +
		`node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false")`

	retained := retainedForRule(optOutGuard, nodecache.Operational{GPUNodeLabelKey: "nvidia.com/gpu.present"})

	// The cached entry predates the opt-out, and pruning has stripped the rest.
	cached, err := retained.Transform()(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "recovering-node",
			Labels: map[string]string{"kubernetes.io/hostname": "recovering-node"},
		},
	})
	require.NoError(t, err)

	// The live node carries the opt-out its owner has since set.
	ni, clientset := newRuleNodeInformer(t, []*v1.Node{cached.(*v1.Node)}, &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "recovering-node",
			Labels: map[string]string{
				"kubernetes.io/hostname":                "recovering-node",
				"k8saas.nvidia.com/ManagedByNVSentinel": "false",
			},
		},
	})
	rule := newOptOutRule(t, ni, optOutGuard)

	matched, err := rule.Evaluate(context.Background(), &protos.HealthEvent{NodeName: "recovering-node"})
	require.NoError(t, err, "the pruned cache must still evaluate, not error")
	require.True(t, matched, "the cache does not hold the opt-out, so the steady-state path matches")
	require.Zero(t, countNodeGets(clientset), "the steady-state path must not read the API server")

	matched, err = rule.Evaluate(
		coldstart.WithRecoveryContext(context.Background()),
		&protos.HealthEvent{NodeName: "recovering-node"},
	)
	require.NoError(t, err)
	require.False(t, matched, "recovery must honour the opt-out on the live node, whatever the pruned cache holds")
	require.Equal(t, 1, countNodeGets(clientset), "recovery must read the API server")
}

func TestGetRuleNode_RecoveryRead_UsesCurrentNode(t *testing.T) {
	ni, clientset := newRuleNodeInformer(t,
		[]*v1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"state": "stale"}}}},
		&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"state": "current"}}},
	)
	rule := newOptOutRule(t, ni, `node.metadata.labels["state"] == "current"`)

	matched, err := rule.Evaluate(context.Background(), &protos.HealthEvent{NodeName: "node-a"})
	require.NoError(t, err)
	require.False(t, matched, "normal evaluation must read the cache")
	require.Zero(t, countNodeGets(clientset))

	recoveryCtx := coldstart.WithRecoveryContext(context.Background())

	matched, err = rule.Evaluate(recoveryCtx, &protos.HealthEvent{NodeName: "node-a"})
	require.NoError(t, err)
	require.True(t, matched, "recovery must read the current node")
	require.Equal(t, 1, countNodeGets(clientset))

	matched, err = rule.Evaluate(recoveryCtx, &protos.HealthEvent{NodeName: "node-a"})
	require.NoError(t, err)
	require.True(t, matched)
	require.Equal(t, 1, countNodeGets(clientset), "recovery must reuse the snapshot for the event")
}

func TestGetRuleNode_DeletedNodeDuringRecovery_ReturnsPermanentError(t *testing.T) {
	ni, clientset := newRuleNodeInformer(t, nil)

	_, err := ni.GetRuleNode(coldstart.WithRecoveryContext(context.Background()), "deleted-node")
	require.Error(t, err)
	require.True(t, coldstart.IsPermanentError(err))
	require.Equal(t, 1, countNodeGets(clientset))

	_, err = ni.GetRuleNode(context.Background(), "deleted-node")
	require.Error(t, err)
	require.False(t, coldstart.IsPermanentError(err), "a cache miss outside recovery is not permanent")
}
