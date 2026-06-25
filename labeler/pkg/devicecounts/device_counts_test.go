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

package devicecounts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testGPUCountCurrentLabel  = "nvsentinel.dgxc.nvidia.com/gpu.count.current"
	testGPUCountExpectedLabel = "nvsentinel.dgxc.nvidia.com/gpu.count.expected"
	testNICCountCurrentLabel  = "nvsentinel.dgxc.nvidia.com/nic.count.current"
	testNICCountExpectedLabel = "nvsentinel.dgxc.nvidia.com/nic.count.expected"
)

func TestParseConfigTOML(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "expected-device-counts.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
enabled = true

[[classes]]
name = "gpu"
enabled = true
groupingLabels = ["node.kubernetes.io/instance-type", "nvidia.com/gpu.product"]
currentExpression = '''
int(node.metadata.labels['nvidia.com/gpu.count'])
'''

[classes.labels]
current = "nvsentinel.dgxc.nvidia.com/gpu.count.current"
expected = "nvsentinel.dgxc.nvidia.com/gpu.count.expected"

[[classes.expectedCountOverrides]]
count = 8

[classes.expectedCountOverrides.matchLabels]
"nvidia.com/gpu.product" = "NVIDIA-GB200"
`), 0600))

	config, err := LoadConfig(configPath)
	require.NoError(t, err)
	require.True(t, config.Enabled)
	require.Len(t, config.Classes, 1)
	require.Equal(t, "gpu", config.Classes[0].Name)
	require.Equal(t, []string{"node.kubernetes.io/instance-type", "nvidia.com/gpu.product"},
		config.Classes[0].GroupingLabels)
	require.Equal(t, testGPUCountCurrentLabel, config.Classes[0].Labels.Current)
	require.Equal(t, "NVIDIA-GB200",
		config.Classes[0].ExpectedCountOverrides[0].MatchLabels["nvidia.com/gpu.product"])
}

func TestExpectedDeviceCountsLearnFromPeerNodeLabels(t *testing.T) {
	config := Config{
		Enabled: true,
		Classes: []ClassConfig{
			{
				Name:    "gpu",
				Enabled: true,
				Labels: Labels{
					Current:  testGPUCountCurrentLabel,
					Expected: testGPUCountExpectedLabel,
				},
				GroupingLabels:    []string{"node.kubernetes.io/instance-type"},
				CurrentExpression: "int(node.metadata.labels['nvidia.com/gpu.count'])",
			},
		},
	}

	nodeA := testNode("node-a", map[string]string{
		"node.kubernetes.io/instance-type": "p5",
		"nvidia.com/gpu.count":             "8",
	})
	nodeB := testNode("node-b", map[string]string{
		"node.kubernetes.io/instance-type": "p5",
		"nvidia.com/gpu.count":             "4",
	})

	manager := newTestManager(t, config)
	updated := manager.ReconcileNodeLabelsInPlace(
		context.Background(),
		nodeB,
		[]*corev1.Node{nodeA, nodeB},
		func(*corev1.Node) []*resourcev1.ResourceSlice { return nil },
	)

	require.True(t, updated)
	require.Equal(t, "4", nodeB.Labels[testGPUCountCurrentLabel])
	require.Equal(t, "8", nodeB.Labels[testGPUCountExpectedLabel])
}

func TestExpectedDeviceCountsOverridePrecedence(t *testing.T) {
	config := Config{
		Enabled: true,
		Classes: []ClassConfig{
			{
				Name:    "gpu",
				Enabled: true,
				Labels: Labels{
					Current:  testGPUCountCurrentLabel,
					Expected: testGPUCountExpectedLabel,
				},
				ExpectedCountOverrides: []ExpectedCountOverride{
					{
						MatchLabels: map[string]string{"nvidia.com/gpu.product": "NVIDIA-GB200"},
						Count:       8,
					},
				},
				CurrentExpression: "int(node.metadata.labels['nvidia.com/gpu.count'])",
			},
		},
	}

	node := testNode("node-a", map[string]string{
		"nvidia.com/gpu.product": "NVIDIA-GB200",
		"nvidia.com/gpu.count":   "4",
	})

	manager := newTestManager(t, config)
	updated := manager.ReconcileNodeLabelsInPlace(
		context.Background(),
		node,
		[]*corev1.Node{node},
		func(*corev1.Node) []*resourcev1.ResourceSlice { return nil },
	)

	require.True(t, updated)
	require.Equal(t, "4", node.Labels[testGPUCountCurrentLabel])
	require.Equal(t, "8", node.Labels[testGPUCountExpectedLabel])
}

func newTestManager(t *testing.T, config Config) *Manager {
	t.Helper()

	manager, err := NewManager(config)
	require.NoError(t, err)
	require.NotNil(t, manager)

	return manager
}

func testNode(name string, nodeLabels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: nodeLabels,
		},
	}
}
