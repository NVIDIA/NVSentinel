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

package cacheplan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

func TestBuild_NamedNamespaces_AreSortedAndDeduplicated(t *testing.T) {
	plan := buildPlan(t,
		namespacedPolicy("b", podGVK, "monitoring"),
		namespacedPolicy("a", podGVK, "gpu-operator"),
		namespacedPolicy("c", podGVK, "monitoring"),
	)

	assert.Equal(t, []string{"gpu-operator", "monitoring"}, plan[podGVK].Namespaces)
}

func TestBuild_PolicyWithoutNamespace_CachesClusterWide(t *testing.T) {
	plan := buildPlan(t,
		namespacedPolicy("a", podGVK, "gpu-operator"),
		namespacedPolicy("b", podGVK, ""),
	)

	assert.Nil(t, plan[podGVK].Namespaces)
}

func TestBuild_LookupOfClusterWideGVK_ReadsTheCache(t *testing.T) {
	plan := buildPlan(t,
		policyWithExpressions("node-owns-pod", nodeGVK,
			`lookup('v1', 'Pod', 'default', 'device-plugin').spec.nodeName == resource.metadata.name`, ""),
	)

	require.Contains(t, plan, podGVK)
	assert.True(t, plan[podGVK].LookupsFromCache)
	assert.NotNil(t, plan[podGVK].Transform)
	assert.False(t, plan[nodeGVK].LookupsFromCache)
}

func TestBuild_LookupOfNamespaceRestrictedGVK_ReadsTheAPI(t *testing.T) {
	plan := buildPlan(t,
		namespacedPolicy("pod-health", podGVK, "gpu-operator"),
		policyWithExpressions("node-owns-pod", nodeGVK,
			`lookup('v1', 'Pod', 'monitoring', 'device-plugin').spec.nodeName == resource.metadata.name`, ""),
	)

	assert.Equal(t, []string{"gpu-operator"}, plan[podGVK].Namespaces)
	assert.False(t, plan[podGVK].LookupsFromCache)
}

func buildPlan(t *testing.T, policies ...config.Policy) map[schema.GroupVersionKind]Entry {
	t.Helper()

	compiler, err := celenv.NewCompilerEnvironment()
	require.NoError(t, err)

	return Build(compiler, policies)
}

func namespacedPolicy(name string, gvk schema.GroupVersionKind, namespace string) config.Policy {
	p := policyWithExpressions(name, gvk, `resource.status.phase != 'Running'`, "")
	p.Resource.Namespace = namespace

	return p
}
