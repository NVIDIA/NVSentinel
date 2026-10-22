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
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/traits"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"

	"github.com/nvidia/nvsentinel/labeler/pkg/metrics"
)

// hardwareKeySeparator joins the sorted values of a grouping expression. It
// cannot appear in a node label value, so it is unambiguous in partition keys.
const hardwareKeySeparator = "+"

// observe evaluates one class for one node.
func (m *Manager) observe(
	ctx context.Context,
	class compiledClass,
	node *corev1.Node,
	resourceSlices []*resourcev1.ResourceSlice,
) countObservation {
	if len(class.sources) == 0 {
		return m.observeClassExpression(ctx, class, node, resourceSlices)
	}

	for i := range class.sources {
		if observation, applies := m.observeSource(ctx, &class.sources[i], node, resourceSlices); applies {
			return observation
		}
	}

	return countObservation{missingSource: true}
}

func (m *Manager) observeClassExpression(
	ctx context.Context,
	class compiledClass,
	node *corev1.Node,
	resourceSlices []*resourcev1.ResourceSlice,
) countObservation {
	observation := countObservation{partitionKey: class.partitionKey(node), hasPartition: true}

	// A ResourceSlice-based expression waits until at least one associated
	// slice exists instead of reporting zero devices.
	if class.referencesResourceSlices() && len(resourceSlices) == 0 {
		observation.missingSource = true
		return observation
	}

	observation.count, observation.err = m.evaluateCurrent(ctx, class.program, node, resourceSlices)

	return observation
}

// observeSource evaluates one source. It reports false when the source does
// not apply to the node, so the next source should be tried.
func (m *Manager) observeSource(
	ctx context.Context,
	source *compiledSource,
	node *corev1.Node,
	resourceSlices []*resourcev1.ResourceSlice,
) (countObservation, bool) {
	if source.ResourceSliceDriver != "" {
		return m.observeDriverSource(ctx, source, node, resourceSlices)
	}

	if source.referencesResourceSlices() && len(resourceSlices) == 0 {
		return countObservation{}, false
	}

	count, err := m.evaluateCurrent(ctx, source.currentProgram, node, resourceSlices)
	if err != nil {
		// For a source without a driver, a failed expression means its input,
		// such as a node label, is absent on this node.
		slog.Debug("Device count source is not available on node",
			"node", node.Name, "class", source.className, "source", source.Name, "error", err)

		return countObservation{}, false
	}

	return m.countedObservation(ctx, source, node, resourceSlices, count), true
}

func (m *Manager) observeDriverSource(
	ctx context.Context,
	source *compiledSource,
	node *corev1.Node,
	resourceSlices []*resourcev1.ResourceSlice,
) (countObservation, bool) {
	driverSlices, found := currentPoolSlices(resourceSlices, source.ResourceSliceDriver)
	if !found {
		return countObservation{}, false
	}

	observation := countObservation{source: source}

	// The driver runs on the node but is still publishing its slices. Wait for
	// the pool to be complete instead of using another, possibly stale, source.
	if !driverSlices.complete {
		slog.Debug("Waiting for a complete ResourceSlice pool before counting devices",
			"node", node.Name, "class", source.className, "source", source.Name,
			"driver", source.ResourceSliceDriver)

		observation.missingSource = true

		return observation, true
	}

	count, err := m.evaluateCurrent(ctx, source.currentProgram, node, driverSlices.slices)
	if err != nil {
		// A broken expression for a driver that is present must not fall back
		// to another, possibly stale, source.
		observation.err = err
		return observation, true
	}

	return m.countedObservation(ctx, source, node, driverSlices.slices, count), true
}

func (m *Manager) countedObservation(
	ctx context.Context,
	source *compiledSource,
	node *corev1.Node,
	resourceSlices []*resourcev1.ResourceSlice,
	count int,
) countObservation {
	observation := countObservation{count: count, source: source}
	observation.partitionKey, observation.hasPartition = sourcePartitionKey(ctx, source, node, resourceSlices)

	return observation
}

// sourcePartitionKey builds the partition key for a node that a source counted.
// It reports false when the grouping expression cannot identify the hardware.
func sourcePartitionKey(
	ctx context.Context,
	source *compiledSource,
	node *corev1.Node,
	resourceSlices []*resourcev1.ResourceSlice,
) (string, bool) {
	parts := make([]string, 0, len(source.GroupingLabels)+2)
	parts = append(parts, "source="+source.Name)

	for _, label := range source.GroupingLabels {
		parts = append(parts, label+"="+node.Labels[label])
	}

	if source.groupingProgram == nil {
		return strings.Join(parts, "|"), true
	}

	hardware, err := evaluateGrouping(ctx, source.groupingProgram, node, resourceSlices)
	if err != nil {
		slog.Warn("Device count grouping failed; node learns its expected count only from itself",
			"node", node.Name, "class", source.className, "source", source.Name, "error", err)

		return "", false
	}

	parts = append(parts, "hardware="+hardware)

	return strings.Join(parts, "|"), true
}

// evaluateGrouping returns the sorted, de-duplicated values of a grouping
// expression joined by hardwareKeySeparator.
func evaluateGrouping(
	ctx context.Context,
	program cel.Program,
	node *corev1.Node,
	resourceSlices []*resourcev1.ResourceSlice,
) (string, error) {
	result, err := evaluateExpression(ctx, program, "groupingExpression", node, resourceSlices)
	if err != nil {
		return "", err
	}

	list, ok := result.(traits.Lister)
	if !ok {
		return "", fmt.Errorf("groupingExpression returned non-list: %T", result.Value())
	}

	values, err := groupingValues(list)
	if err != nil {
		return "", err
	}

	if len(values) == 0 {
		return "", fmt.Errorf("groupingExpression returned no values")
	}

	slices.Sort(values)

	return strings.Join(slices.Compact(values), hardwareKeySeparator), nil
}

func groupingValues(list traits.Lister) ([]string, error) {
	size, ok := list.Size().(types.Int)
	if !ok {
		return nil, fmt.Errorf("groupingExpression result size is not an int")
	}

	values := make([]string, 0, int(size))

	for i := int64(0); i < int64(size); i++ {
		value, ok := list.Get(types.Int(i)).(types.String)
		if !ok {
			return nil, fmt.Errorf("groupingExpression must return only strings")
		}

		if trimmed := strings.TrimSpace(string(value)); trimmed != "" {
			values = append(values, trimmed)
		}
	}

	return values, nil
}

func (source compiledSource) referencesResourceSlices() bool {
	return strings.Contains(source.CurrentExpression, "resourceSlices")
}

// recordDeviceCountSource sets the source gauge for the source that counted
// the node and removes the series of every other source of the class.
func recordDeviceCountSource(class compiledClass, nodeName string, observation countObservation) {
	if len(class.sources) == 0 {
		return
	}

	selected := ""

	if observation.source != nil && !observation.missingSource && observation.err == nil {
		selected = observation.source.Name
	}

	for _, source := range class.sources {
		if source.Name == selected {
			metrics.DeviceCountSource.WithLabelValues(nodeName, class.Name, source.Name).Set(1)
			continue
		}

		metrics.DeviceCountSource.DeleteLabelValues(nodeName, class.Name, source.Name)
	}
}

func logMissingSource(class compiledClass, nodeName string) {
	if len(class.sources) == 0 {
		slog.Warn("Skipping device count label update because no ResourceSlices are associated with the node",
			"node", nodeName, "class", class.Name)

		return
	}

	slog.Warn("Skipping device count label update because no device count source is available on the node",
		"node", nodeName, "class", class.Name)
}
