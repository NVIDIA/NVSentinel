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

package devicecounts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/nvidia/nvsentinel/labeler/pkg/metrics"
)

const (
	testGPUDRADriver    = "gpu.nvidia.com"
	testInstanceType    = "node.kubernetes.io/instance-type"
	testGFDGPUCount     = "nvidia.com/gpu.count"
	testGFDGPUProduct   = "nvidia.com/gpu.product"
	testH100ProductName = "NVIDIA H100 80GB HBM3"
	testL40SProductName = "NVIDIA L40S"

	// These match the GPU class sources in the labeler chart values.yaml.
	testDRAGPUCurrentExpression = `
sum(resourceSlices
  .filter(rs, has(rs.spec.devices))
  .map(rs, rs.spec.devices
    .filter(d,
      has(d.attributes) &&
      'type' in d.attributes &&
      has(d.attributes['type'].string) &&
      d.attributes['type'].string == 'gpu'
    )
    .size()
  ))`
	testDRAGPUGroupingExpression = `
resourceSlices
  .map(rs, has(rs.spec.devices) ? rs.spec.devices : [])
  .flatten()
  .filter(d,
    has(d.attributes) &&
    'type' in d.attributes &&
    has(d.attributes['type'].string) &&
    d.attributes['type'].string == 'gpu' &&
    'productName' in d.attributes &&
    has(d.attributes['productName'].string)
  )
  .map(d, d.attributes['productName'].string)`
	testGFDGPUCurrentExpression = `
has(node.status.allocatable) &&
node.status.allocatable.exists(r,
  (r == 'nvidia.com/gpu' || r.startsWith('nvidia.com/mig-')) &&
  int(node.status.allocatable[r]) > 0)
  ? int(node.metadata.labels['nvidia.com/gpu.count'])
  : int('device plugin does not advertise GPUs')`
)

func testGPUSourcesClass() ClassConfig {
	return ClassConfig{
		Name:    "gpu",
		Enabled: true,
		Labels: Labels{
			Current:  testGPUCountCurrentLabel,
			Expected: testGPUCountExpectedLabel,
		},
		Sources: []SourceConfig{
			{
				Name:                "dra",
				ResourceSliceDriver: testGPUDRADriver,
				GroupingLabels:      []string{testInstanceType},
				CurrentExpression:   testDRAGPUCurrentExpression,
				GroupingExpression:  testDRAGPUGroupingExpression,
			},
			{
				Name:              "gfd",
				GroupingLabels:    []string{testInstanceType, testGFDGPUProduct},
				CurrentExpression: testGFDGPUCurrentExpression,
			},
		},
	}
}

func testGPUSourcesConfig() Config {
	return Config{Enabled: true, Classes: []ClassConfig{testGPUSourcesClass()}}
}

// testDevicePluginGPUs is the allocatable of a node whose NVIDIA device plugin
// advertises count GPUs, as in ClusterPolicy mode.
func testDevicePluginGPUs(count string) corev1.ResourceList {
	return corev1.ResourceList{"nvidia.com/gpu": resource.MustParse(count)}
}

func TestDeviceCountSources_SourceSelection_UsesFirstAvailableSource(t *testing.T) {
	tests := []struct {
		name           string
		nodeLabels     map[string]string
		allocatable    corev1.ResourceList
		resourceSlices func(nodeName string) []*resourcev1.ResourceSlice
		wantUpdated    bool
		wantCurrent    string
	}{
		{
			name: "GPUCluster node counts DRA devices",
			resourceSlices: func(nodeName string) []*resourcev1.ResourceSlice {
				return []*resourcev1.ResourceSlice{testGPUSlice("gpu", nodeName, 1, 1, testGPUDevices(8, testH100ProductName)...)}
			},
			wantUpdated: true,
			wantCurrent: "8",
		},
		{
			name:        "ClusterPolicy node counts the GFD label",
			nodeLabels:  map[string]string{testGFDGPUCount: "8"},
			allocatable: testDevicePluginGPUs("8"),
			wantUpdated: true,
			wantCurrent: "8",
		},
		{
			name:       "ClusterPolicy node with only MIG devices counts the GFD label",
			nodeLabels: map[string]string{testGFDGPUCount: "8"},
			allocatable: corev1.ResourceList{
				"nvidia.com/gpu":         resource.MustParse("0"),
				"nvidia.com/mig-1g.10gb": resource.MustParse("7"),
			},
			wantUpdated: true,
			wantCurrent: "8",
		},
		{
			name:        "GFD label with zeroed device plugin GPUs is not counted",
			nodeLabels:  map[string]string{testGFDGPUCount: "8"},
			allocatable: testDevicePluginGPUs("0"),
		},
		{
			name:       "DRA has priority over a stale GFD label",
			nodeLabels: map[string]string{testGFDGPUCount: "8"},
			resourceSlices: func(nodeName string) []*resourcev1.ResourceSlice {
				return []*resourcev1.ResourceSlice{testGPUSlice("gpu", nodeName, 1, 1, testGPUDevices(7, testH100ProductName)...)}
			},
			wantUpdated: true,
			wantCurrent: "7",
		},
		{
			name:        "slices of another driver do not make the DRA source report zero",
			nodeLabels:  map[string]string{testGFDGPUCount: "8"},
			allocatable: testDevicePluginGPUs("8"),
			resourceSlices: func(nodeName string) []*resourcev1.ResourceSlice {
				return []*resourcev1.ResourceSlice{testResourceSlice("nic", nodeName, testDevice("roce", stringAttribute("roce")))}
			},
			wantUpdated: true,
			wantCurrent: "8",
		},
		{
			name: "node without any source keeps its labels unset",
			resourceSlices: func(nodeName string) []*resourcev1.ResourceSlice {
				return []*resourcev1.ResourceSlice{testResourceSlice("nic", nodeName, testDevice("roce", stringAttribute("roce")))}
			},
		},
		{
			name:       "incomplete DRA pool skips the class instead of using the GFD label",
			nodeLabels: map[string]string{testGFDGPUCount: "8"},
			resourceSlices: func(nodeName string) []*resourcev1.ResourceSlice {
				return []*resourcev1.ResourceSlice{testGPUSlice("gpu-1", nodeName, 1, 2, testGPUDevices(4, testH100ProductName)...)}
			},
		},
		{
			name: "old pool generation is ignored",
			resourceSlices: func(nodeName string) []*resourcev1.ResourceSlice {
				return []*resourcev1.ResourceSlice{
					testGPUSlice("gpu-old", nodeName, 1, 1, testGPUDevices(8, testH100ProductName)...),
					testGPUSlice("gpu-new", nodeName, 2, 1, testGPUDevices(7, testH100ProductName)...),
				}
			},
			wantUpdated: true,
			wantCurrent: "7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := testNode("node-a", tt.nodeLabels)
			if node.Labels == nil {
				node.Labels = map[string]string{}
			}

			node.Status.Allocatable = tt.allocatable

			var resourceSlices []*resourcev1.ResourceSlice
			if tt.resourceSlices != nil {
				resourceSlices = tt.resourceSlices(node.Name)
			}

			manager := newTestManager(t, testGPUSourcesConfig())
			updated := manager.CalculateAndSetDeviceCountLabels(
				context.Background(),
				node,
				[]*corev1.Node{node},
				func(*corev1.Node) []*resourcev1.ResourceSlice { return resourceSlices },
			)

			require.Equal(t, tt.wantUpdated, updated)

			current, ok := node.Labels[testGPUCountCurrentLabel]
			if tt.wantCurrent == "" {
				require.False(t, ok, "current count label must not be written without a usable source")
				require.NotContains(t, node.Labels, testGPUCountExpectedLabel)

				return
			}

			require.Equal(t, tt.wantCurrent, current)
		})
	}
}

func TestDeviceCountSources_DriverExpressionError_DoesNotFallBack(t *testing.T) {
	class := testGPUSourcesClass()
	class.Sources[0].CurrentExpression = "int(resourceSlices[0].spec.driver)"

	node := testNode("node-a", map[string]string{testGFDGPUCount: "8"})
	slice := testGPUSlice("gpu", node.Name, 1, 1, testGPUDevices(8, testH100ProductName)...)

	manager := newTestManager(t, Config{Enabled: true, Classes: []ClassConfig{class}})
	updated := manager.CalculateAndSetDeviceCountLabels(
		context.Background(),
		node,
		[]*corev1.Node{node},
		func(*corev1.Node) []*resourcev1.ResourceSlice { return []*resourcev1.ResourceSlice{slice} },
	)

	require.False(t, updated)
	require.NotContains(t, node.Labels, testGPUCountCurrentLabel)
}

func TestDeviceCountSources_GroupingWithoutInstanceType_SeparatesGPUModels(t *testing.T) {
	h100 := testNode("h100", map[string]string{})
	h100MissingGPU := testNode("h100-missing-gpu", map[string]string{})
	l40s := testNode("l40s", map[string]string{})
	nodes := []*corev1.Node{h100, h100MissingGPU, l40s}

	resourceSlicesByNode := map[string][]*resourcev1.ResourceSlice{
		h100.Name: {testGPUSlice("h100", h100.Name, 1, 1, testGPUDevices(8, testH100ProductName)...)},
		h100MissingGPU.Name: {
			testGPUSlice("h100-missing-gpu", h100MissingGPU.Name, 1, 1, testGPUDevices(7, testH100ProductName)...),
		},
		l40s.Name: {testGPUSlice("l40s", l40s.Name, 1, 1, testGPUDevices(4, testL40SProductName)...)},
	}

	reconcileCache := newTestManager(t, testGPUSourcesConfig()).NewReconcileCache(
		nodes,
		func(node *corev1.Node) []*resourcev1.ResourceSlice { return resourceSlicesByNode[node.Name] },
	)

	for _, node := range nodes {
		require.True(t, reconcileCache.CalculateAndSetDeviceCountLabels(context.Background(), node))
	}

	require.Equal(t, "8", h100.Labels[testGPUCountExpectedLabel])
	require.Equal(t, "7", h100MissingGPU.Labels[testGPUCountCurrentLabel])
	require.Equal(t, "8", h100MissingGPU.Labels[testGPUCountExpectedLabel],
		"a node that loses a GPU must stay in the partition of its GPU model")
	require.Equal(t, "4", l40s.Labels[testGPUCountCurrentLabel])
	require.Equal(t, "4", l40s.Labels[testGPUCountExpectedLabel],
		"a node with a different GPU model must not learn a higher count")
}

func TestDeviceCountSources_GroupingWithSameGPUModel_SeparatesInstanceTypes(t *testing.T) {
	oneGPU := testNode("a3-1g", map[string]string{testInstanceType: "a3-highgpu-1g"})
	eightGPU := testNode("a3-8g", map[string]string{testInstanceType: "a3-highgpu-8g"})
	nodes := []*corev1.Node{oneGPU, eightGPU}

	resourceSlicesByNode := map[string][]*resourcev1.ResourceSlice{
		oneGPU.Name:   {testGPUSlice("a3-1g", oneGPU.Name, 1, 1, testGPUDevices(1, testH100ProductName)...)},
		eightGPU.Name: {testGPUSlice("a3-8g", eightGPU.Name, 1, 1, testGPUDevices(8, testH100ProductName)...)},
	}

	reconcileCache := newTestManager(t, testGPUSourcesConfig()).NewReconcileCache(
		nodes,
		func(node *corev1.Node) []*resourcev1.ResourceSlice { return resourceSlicesByNode[node.Name] },
	)

	for _, node := range nodes {
		require.True(t, reconcileCache.CalculateAndSetDeviceCountLabels(context.Background(), node))
	}

	require.Equal(t, "1", oneGPU.Labels[testGPUCountExpectedLabel])
	require.Equal(t, "8", eightGPU.Labels[testGPUCountExpectedLabel])
}

func TestDeviceCountSources_GroupingExpressionWithoutValues_LearnsOnlyFromSelf(t *testing.T) {
	class := testGPUSourcesClass()
	class.Sources[0].GroupingExpression = "resourceSlices.filter(rs, false).map(rs, rs.spec.driver)"

	peer := testNode("peer", map[string]string{})
	target := testNode("target", map[string]string{})
	nodes := []*corev1.Node{peer, target}

	resourceSlicesByNode := map[string][]*resourcev1.ResourceSlice{
		peer.Name:   {testGPUSlice("peer", peer.Name, 1, 1, testGPUDevices(8, testH100ProductName)...)},
		target.Name: {testGPUSlice("target", target.Name, 1, 1, testGPUDevices(4, testL40SProductName)...)},
	}

	reconcileCache := newTestManager(t, Config{Enabled: true, Classes: []ClassConfig{class}}).NewReconcileCache(
		nodes,
		func(node *corev1.Node) []*resourcev1.ResourceSlice { return resourceSlicesByNode[node.Name] },
	)

	require.True(t, reconcileCache.CalculateAndSetDeviceCountLabels(context.Background(), target))
	require.Equal(t, "4", target.Labels[testGPUCountCurrentLabel])
	require.Equal(t, "4", target.Labels[testGPUCountExpectedLabel])
}

func TestDeviceCountSources_ModeChange_KeepsExpectedCount(t *testing.T) {
	node := testNode("node-a", map[string]string{
		testGPUCountCurrentLabel:  "8",
		testGPUCountExpectedLabel: "8",
	})
	manager := newTestManager(t, testGPUSourcesConfig())
	draSlices := []*resourcev1.ResourceSlice{
		testGPUSlice("gpu", node.Name, 1, 1, testGPUDevices(8, testH100ProductName)...),
	}

	reconcile := func(resourceSlices []*resourcev1.ResourceSlice) bool {
		return manager.CalculateAndSetDeviceCountLabels(
			context.Background(),
			node,
			[]*corev1.Node{node},
			func(*corev1.Node) []*resourcev1.ResourceSlice { return resourceSlices },
		)
	}

	require.False(t, reconcile(draSlices), "GPUCluster mode keeps the existing labels")

	require.False(t, reconcile(nil), "no source is available while the operators switch mode")
	require.Equal(t, "8", node.Labels[testGPUCountCurrentLabel])
	require.Equal(t, "8", node.Labels[testGPUCountExpectedLabel])

	node.Labels[testGFDGPUCount] = "8"
	node.Labels[testGFDGPUProduct] = "NVIDIA-H100-80GB-HBM3"

	node.Status.Allocatable = testDevicePluginGPUs("8")

	require.False(t, reconcile(nil), "ClusterPolicy mode reports the same count")

	node.Labels[testGFDGPUCount] = "7"

	require.True(t, reconcile(nil))
	require.Equal(t, "7", node.Labels[testGPUCountCurrentLabel])
	require.Equal(t, "8", node.Labels[testGPUCountExpectedLabel],
		"the expected count learned in the other mode must carry over")
}

// A node that moved to GPUCluster mode can keep a stale GFD count. When the DRA
// plugin stops, for example because the GPU driver is unhealthy, kubelet deletes
// its slices. The stale count must not replace the DRA count, because GPUCluster
// mode has no device plugin that advertises GPUs.
func TestDeviceCountSources_DRASlicesRemoved_IgnoresStaleGFDLabel(t *testing.T) {
	node := testNode("node-a", map[string]string{testGFDGPUCount: "8"})
	manager := newTestManager(t, testGPUSourcesConfig())

	reconcile := func(resourceSlices []*resourcev1.ResourceSlice) bool {
		return manager.CalculateAndSetDeviceCountLabels(
			context.Background(),
			node,
			[]*corev1.Node{node},
			func(*corev1.Node) []*resourcev1.ResourceSlice { return resourceSlices },
		)
	}

	require.True(t, reconcile([]*resourcev1.ResourceSlice{
		testGPUSlice("gpu", node.Name, 1, 1, testGPUDevices(7, testH100ProductName)...),
	}))
	require.Equal(t, "7", node.Labels[testGPUCountCurrentLabel])

	node.Labels[testGPUCountExpectedLabel] = "8"

	require.False(t, reconcile(nil), "the stale GFD label must not be counted")
	require.Equal(t, "7", node.Labels[testGPUCountCurrentLabel])
	require.Equal(t, "8", node.Labels[testGPUCountExpectedLabel])
}

// The GFD source reads the device plugin allocatable, so a device plugin that
// starts or stops advertising GPUs must trigger a reconciliation.
func TestDeviceCountSources_DevicePluginAllocatableChange_TriggersReconcile(t *testing.T) {
	manager := newTestManager(t, testGPUSourcesConfig())

	oldNode := testNode("node-a", nil)
	newNode := testNode("node-a", nil)
	newNode.Status.Allocatable = testDevicePluginGPUs("8")

	require.True(t, manager.NodeResourcesAffectDeviceCounts(oldNode, newNode))
}

func TestDeviceCountSources_SourceMetric_TracksSelectedSource(t *testing.T) {
	node := testNode("metric-node", map[string]string{})
	manager := newTestManager(t, testGPUSourcesConfig())

	reconcile := func(resourceSlices []*resourcev1.ResourceSlice) {
		manager.CalculateAndSetDeviceCountLabels(
			context.Background(),
			node,
			[]*corev1.Node{node},
			func(*corev1.Node) []*resourcev1.ResourceSlice { return resourceSlices },
		)
	}

	reconcile([]*resourcev1.ResourceSlice{
		testGPUSlice("gpu", node.Name, 1, 1, testGPUDevices(8, testH100ProductName)...),
	})

	node.Labels[testGFDGPUCount] = "8"
	node.Status.Allocatable = testDevicePluginGPUs("8")
	reconcile(nil)

	require.False(t, metrics.DeviceCountSource.DeleteLabelValues(node.Name, "gpu", "dra"),
		"the series of a source that no longer counts the node must be removed")
	require.True(t, metrics.DeviceCountSource.DeleteLabelValues(node.Name, "gpu", "gfd"))
}

func TestDeviceCountSources_Config_Validation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ClassConfig)
		wantErr string
	}{
		{
			name:   "valid sources",
			mutate: func(*ClassConfig) {},
		},
		{
			name:    "class currentExpression with sources",
			mutate:  func(c *ClassConfig) { c.CurrentExpression = testGFDGPUCurrentExpression },
			wantErr: "must be set on each source",
		},
		{
			name:    "class groupingLabels with sources",
			mutate:  func(c *ClassConfig) { c.GroupingLabels = []string{testInstanceType} },
			wantErr: "must be set on each source",
		},
		{
			name:    "source without name",
			mutate:  func(c *ClassConfig) { c.Sources[1].Name = "" },
			wantErr: "name is required",
		},
		{
			name:    "duplicate source names",
			mutate:  func(c *ClassConfig) { c.Sources[1].Name = c.Sources[0].Name },
			wantErr: "source names must be unique",
		},
		{
			name:    "source without currentExpression",
			mutate:  func(c *ClassConfig) { c.Sources[1].CurrentExpression = " " },
			wantErr: "currentExpression is required",
		},
		{
			name:    "groupingExpression that does not return a list",
			mutate:  func(c *ClassConfig) { c.Sources[0].GroupingExpression = "1" },
			wantErr: "groupingExpression must return list<string>",
		},
		{
			name:    "groupingExpression that returns a list of integers",
			mutate:  func(c *ClassConfig) { c.Sources[0].GroupingExpression = "resourceSlices.map(rs, 1)" },
			wantErr: "groupingExpression must return list<string>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class := testGPUSourcesClass()
			tt.mutate(&class)

			_, err := NewManager(Config{Enabled: true, Classes: []ClassConfig{class}})
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestDeviceCountSources_LoadConfig_ParsesSources(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "expected-device-counts.toml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
enabled = true

[[classes]]
name = "gpu"
enabled = true

[classes.labels]
current = "nvsentinel.dgxc.nvidia.com/gpu.count.current"
expected = "nvsentinel.dgxc.nvidia.com/gpu.count.expected"

[[classes.sources]]
name = "dra"
resourceSliceDriver = "gpu.nvidia.com"
groupingLabels = ["node.kubernetes.io/instance-type"]
currentExpression = '''
resourceSlices.size()
'''
groupingExpression = '''
['hardware']
'''

[[classes.sources]]
name = "gfd"
groupingLabels = ["node.kubernetes.io/instance-type", "nvidia.com/gpu.product"]
currentExpression = '''
int(node.metadata.labels['nvidia.com/gpu.count'])
'''
`), 0600))

	config, err := LoadConfig(configPath)
	require.NoError(t, err)
	require.Len(t, config.Classes, 1)
	require.Len(t, config.Classes[0].Sources, 2)
	require.Equal(t, "dra", config.Classes[0].Sources[0].Name)
	require.Equal(t, testGPUDRADriver, config.Classes[0].Sources[0].ResourceSliceDriver)
	require.Contains(t, config.Classes[0].Sources[0].GroupingExpression, "hardware")
	require.Equal(t, []string{testInstanceType, testGFDGPUProduct}, config.Classes[0].Sources[1].GroupingLabels)

	manager := newTestManager(t, config)
	require.True(t, manager.RequiresResourceSlices())
}

func TestDeviceCountSources_RequiresResourceSlices_ForDriverSource(t *testing.T) {
	class := testGPUSourcesClass()
	class.Sources = []SourceConfig{{
		Name:                "dra",
		ResourceSliceDriver: testGPUDRADriver,
		CurrentExpression:   "8",
	}}

	manager := newTestManager(t, Config{Enabled: true, Classes: []ClassConfig{class}})
	require.True(t, manager.RequiresResourceSlices())
}

func TestCurrentPoolSlices(t *testing.T) {
	const nodeName = "node-a"

	tests := []struct {
		name           string
		resourceSlices []*resourcev1.ResourceSlice
		wantFound      bool
		wantComplete   bool
		wantSlices     []string
	}{
		{
			name:           "no slice from the driver",
			resourceSlices: []*resourcev1.ResourceSlice{testResourceSlice("nic", nodeName)},
		},
		{
			name: "complete pool with several slices",
			resourceSlices: []*resourcev1.ResourceSlice{
				testGPUSlice("gpu-a", nodeName, 1, 2),
				testGPUSlice("gpu-b", nodeName, 1, 2),
			},
			wantFound:    true,
			wantComplete: true,
			wantSlices:   []string{"gpu-a", "gpu-b"},
		},
		{
			name:           "incomplete pool",
			resourceSlices: []*resourcev1.ResourceSlice{testGPUSlice("gpu-a", nodeName, 1, 2)},
			wantFound:      true,
			wantSlices:     []string{"gpu-a"},
		},
		{
			name: "older generation is ignored",
			resourceSlices: []*resourcev1.ResourceSlice{
				testGPUSlice("gpu-new", nodeName, 3, 1),
				testGPUSlice("gpu-old", nodeName, 2, 1),
			},
			wantFound:    true,
			wantComplete: true,
			wantSlices:   []string{"gpu-new"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current, found := currentPoolSlices(tt.resourceSlices, testGPUDRADriver)
			require.Equal(t, tt.wantFound, found)
			require.Equal(t, tt.wantComplete, current.complete)

			names := make([]string, 0, len(current.slices))
			for _, resourceSlice := range current.slices {
				names = append(names, resourceSlice.Name)
			}

			require.ElementsMatch(t, tt.wantSlices, names)
		})
	}
}

func testGPUSlice(
	name, nodeName string,
	generation, sliceCount int64,
	devices ...resourcev1.Device,
) *resourcev1.ResourceSlice {
	return &resourcev1.ResourceSlice{
		Name: name,
		Spec: resourcev1.ResourceSliceSpec{
			Driver: testGPUDRADriver,
			Pool: resourcev1.ResourcePool{
				Name:               nodeName,
				Generation:         generation,
				ResourceSliceCount: sliceCount,
			},
			NodeName: &nodeName,
			Devices:  devices,
		},
	}
}

func testGPUDevices(count int, productName string) []resourcev1.Device {
	devices := make([]resourcev1.Device, 0, count)

	for i := range count {
		deviceType := "gpu"
		product := productName
		devices = append(devices, resourcev1.Device{
			Name: "gpu-" + string(rune('0'+i)),
			Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
				"type":        {StringValue: &deviceType},
				"productName": {StringValue: &product},
			},
		})
	}

	return devices
}
