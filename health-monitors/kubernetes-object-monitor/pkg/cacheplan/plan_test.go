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
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/testutils"
)

func TestBuild_NamedNamespaces_AreSortedAndDeduplicated(t *testing.T) {
	plan := buildPlan(t,
		namespacedPolicy("b", testutils.PodGVK, "monitoring"),
		namespacedPolicy("a", testutils.PodGVK, "gpu-operator"),
		namespacedPolicy("c", testutils.PodGVK, "monitoring"),
	)

	assert.Equal(t, []string{"gpu-operator", "monitoring"}, plan[testutils.PodGVK].Namespaces)
}

func buildPlan(t *testing.T, policies ...config.Policy) map[schema.GroupVersionKind]Entry {
	t.Helper()

	compiler, err := celenv.NewCompilerEnvironment()
	require.NoError(t, err)

	return Build(compiler, policies)
}

func namespacedPolicy(name string, gvk schema.GroupVersionKind, namespace string) config.Policy {
	p := testutils.PolicyWithExpressions(name, gvk, `resource.status.phase != 'Running'`, "")
	p.Resource.Namespace = namespace

	return p
}
