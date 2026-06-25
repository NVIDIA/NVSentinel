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
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
	"github.com/nvidia/nvsentinel/labeler/pkg/metrics"
)

// Config controls expected device-count label reconciliation.
type Config struct {
	Enabled bool          `toml:"enabled"`
	Classes []ClassConfig `toml:"classes"`
}

// ClassConfig describes one device-count class, such as GPU or NIC counts.
type ClassConfig struct {
	Name                   string                  `toml:"name"`
	Enabled                bool                    `toml:"enabled"`
	Labels                 Labels                  `toml:"labels"`
	GroupingLabels         []string                `toml:"groupingLabels"`
	ExpectedCountOverrides []ExpectedCountOverride `toml:"expectedCountOverrides"`
	CurrentExpression      string                  `toml:"currentExpression"`
}

// Labels contains the current and expected node labels managed for a class.
type Labels struct {
	Current  string `toml:"current"`
	Expected string `toml:"expected"`
}

// ExpectedCountOverride pins an expected count when a node's labels match.
type ExpectedCountOverride struct {
	MatchLabels map[string]string `toml:"matchLabels"`
	Count       int               `toml:"count"`
}

// Manager evaluates device-count classes and applies their derived node labels.
type Manager struct {
	classes          []compiledClass
	managedLabelKeys map[string]struct{}
}

type compiledClass struct {
	ClassConfig
	program cel.Program
}

// LoadConfig loads expected device-count TOML configuration from a file.
func LoadConfig(path string) (Config, error) {
	var config Config

	if err := configmanager.LoadTOMLConfig(path, &config); err != nil {
		return Config{}, fmt.Errorf("parse expected device counts config: %w", err)
	}

	return config, nil
}

// NewManager compiles enabled device-count classes and returns a ready manager.
func NewManager(config Config) (*Manager, error) {
	if !config.Enabled {
		return nil, nil
	}

	env, err := newDeviceCountCELEnv()
	if err != nil {
		return nil, fmt.Errorf("create device-count CEL environment: %w", err)
	}

	manager := &Manager{
		managedLabelKeys: map[string]struct{}{},
	}

	for i, classConfig := range config.Classes {
		compiledClass, ok, err := compileDeviceCountClass(env, i, classConfig)
		if err != nil {
			return nil, fmt.Errorf("compile device-count class at index %d: %w", i, err)
		}

		if !ok {
			continue
		}

		manager.addClass(compiledClass)
	}

	if len(manager.classes) == 0 {
		return nil, nil
	}

	return manager, nil
}

// Enabled reports whether the manager has any compiled device-count classes.
func (m *Manager) Enabled() bool {
	return m != nil && len(m.classes) > 0
}

// RequiresResourceSlices reports whether any enabled class reads ResourceSlices.
func (m *Manager) RequiresResourceSlices() bool {
	if !m.Enabled() {
		return false
	}

	for _, class := range m.classes {
		if class.referencesResourceSlices() {
			return true
		}
	}

	return false
}

// ClassCount returns the number of compiled device-count classes.
func (m *Manager) ClassCount() int {
	return len(m.classes)
}

// ReconcileNodeLabelsInPlace evaluates all enabled device-count classes for a node.
func (m *Manager) ReconcileNodeLabelsInPlace(
	ctx context.Context,
	node *corev1.Node,
	peerNodes []*corev1.Node,
	resourceSlicesForNode func(*corev1.Node) []*resourcev1.ResourceSlice,
) bool {
	if !m.Enabled() {
		return false
	}

	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}

	needsUpdate := false
	resourceSlices := resourceSlicesForNode(node)

	for _, class := range m.classes {
		// Do not turn a missing DRA source into current=0. A ResourceSlice-based
		// expression should wait until at least one associated slice exists.
		if class.referencesResourceSlices() && len(resourceSlices) == 0 {
			metrics.DeviceCountSkippedUpdates.WithLabelValues(class.Name, metrics.SkipReasonMissingSource).Inc()
			slog.Warn("Skipping device count label update because no ResourceSlices are associated with the node",
				"node", node.Name, "class", class.Name)

			continue
		}

		current, err := m.evaluateCurrent(ctx, class, node, resourceSlices)
		if err != nil {
			metrics.DeviceCountSkippedUpdates.WithLabelValues(class.Name, metrics.SkipReasonEvaluationError).Inc()
			slog.Warn("Skipping device count label update after current count evaluation failed",
				"node", node.Name, "class", class.Name, "error", err)

			continue
		}

		expected := m.expectedDeviceCount(ctx, class, node, current, peerNodes, resourceSlicesForNode)
		currentValue := strconv.Itoa(current)
		expectedValue := strconv.Itoa(expected)
		partitionKey := class.partitionKey(node)

		metrics.CurrentDeviceCount.WithLabelValues(node.Name, class.Name).Set(float64(current))
		metrics.ExpectedDeviceCount.WithLabelValues(class.Name, partitionKey).Set(float64(expected))

		if node.Labels[class.Labels.Current] != currentValue {
			node.Labels[class.Labels.Current] = currentValue
			needsUpdate = true
		}

		if node.Labels[class.Labels.Expected] != expectedValue {
			node.Labels[class.Labels.Expected] = expectedValue
			needsUpdate = true
		}
	}

	return needsUpdate
}

// NodeLabelsAffectDeviceCounts reports whether a node label change affects device-count inputs.
func (m *Manager) NodeLabelsAffectDeviceCounts(oldLabels, newLabels map[string]string) bool {
	if !m.Enabled() {
		return false
	}

	oldInputLabels := maps.Clone(oldLabels)
	newInputLabels := maps.Clone(newLabels)

	for key := range m.managedLabelKeys {
		// Ignore labels owned by this feature to avoid self-triggered updates.
		delete(oldInputLabels, key)
		delete(newInputLabels, key)
	}

	return !maps.Equal(oldInputLabels, newInputLabels)
}

func newDeviceCountCELEnv() (*cel.Env, error) {
	// Keep the CEL surface intentionally small: expressions can only inspect
	// the reconciled node, that node's associated ResourceSlices, and sum lists.
	return cel.NewEnv(
		cel.Variable("node", cel.DynType),
		cel.Variable("resourceSlices", cel.ListType(cel.DynType)),
		cel.Function("sum",
			cel.Overload("sum_list_int",
				[]*cel.Type{cel.ListType(cel.IntType)},
				cel.IntType,
				cel.FunctionBinding(sumIntList),
			),
		),
	)
}

func compileDeviceCountClass(
	env *cel.Env,
	index int,
	classConfig ClassConfig,
) (compiledClass, bool, error) {
	if !classConfig.Enabled {
		return compiledClass{}, false, nil
	}

	if err := validateDeviceCountClassConfig(index, classConfig); err != nil {
		return compiledClass{}, false, fmt.Errorf("validate device-count class: %w", err)
	}

	ast, issues := env.Compile(classConfig.CurrentExpression)
	if issues != nil && issues.Err() != nil {
		return compiledClass{}, false, fmt.Errorf(
			"expectedDeviceCounts.classes[%d] (%s): compile currentExpression: %w",
			index, classConfig.Name, issues.Err())
	}

	if ast.OutputType() != cel.IntType && ast.OutputType() != cel.DynType {
		return compiledClass{}, false, fmt.Errorf(
			"expectedDeviceCounts.classes[%d] (%s): currentExpression must return int, got %s",
			index, classConfig.Name, ast.OutputType())
	}

	program, err := env.Program(ast)
	if err != nil {
		return compiledClass{}, false, fmt.Errorf(
			"expectedDeviceCounts.classes[%d] (%s): create CEL program: %w",
			index, classConfig.Name, err)
	}

	return compiledClass{
		ClassConfig: classConfig,
		program:     program,
	}, true, nil
}

func validateDeviceCountClassConfig(index int, classConfig ClassConfig) error {
	if classConfig.Name == "" {
		return fmt.Errorf("expectedDeviceCounts.classes[%d]: name is required", index)
	}

	if classConfig.Labels.Current == "" || classConfig.Labels.Expected == "" {
		return fmt.Errorf(
			"expectedDeviceCounts.classes[%d] (%s): current and expected labels are required",
			index, classConfig.Name)
	}

	if strings.TrimSpace(classConfig.CurrentExpression) == "" {
		return fmt.Errorf(
			"expectedDeviceCounts.classes[%d] (%s): currentExpression is required",
			index, classConfig.Name)
	}

	for j, override := range classConfig.ExpectedCountOverrides {
		if override.Count < 0 {
			return fmt.Errorf(
				"expectedDeviceCounts.classes[%d] (%s).expectedCountOverrides[%d]: count must be non-negative",
				index, classConfig.Name, j)
		}
	}

	return nil
}

func (m *Manager) addClass(compiledClass compiledClass) {
	m.classes = append(m.classes, compiledClass)

	// Node label updates caused by these derived labels should not trigger
	// another device-count reconciliation loop.
	m.managedLabelKeys[compiledClass.Labels.Current] = struct{}{}
	m.managedLabelKeys[compiledClass.Labels.Expected] = struct{}{}
}

func sumIntList(args ...ref.Val) ref.Val {
	if len(args) != 1 {
		return types.NewErr("sum requires exactly one argument")
	}

	list, ok := args[0].(traits.Lister)
	if !ok {
		return types.NewErr("sum argument must be list<int>")
	}

	size, ok := list.Size().(types.Int)
	if !ok {
		return types.NewErr("sum argument size is not an int")
	}

	var total int64

	for i := int64(0); i < int64(size); i++ {
		value := list.Get(types.Int(i))
		intValue, ok := value.(types.Int)

		if !ok {
			return types.NewErr("sum argument must contain only ints")
		}

		total += int64(intValue)
	}

	return types.Int(total)
}

func (m *Manager) evaluateCurrent(
	ctx context.Context,
	class compiledClass,
	node *corev1.Node,
	resourceSlices []*resourcev1.ResourceSlice,
) (int, error) {
	nodeMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(node)
	if err != nil {
		return 0, fmt.Errorf("convert node to CEL input: %w", err)
	}

	// Kubernetes labels are strings, but the documented GPU expression uses
	// int(node.metadata.labels['nvidia.com/gpu.count']). Normalizing numeric
	// strings avoids CEL runtime conversion gaps for dynamic map values. Do the
	// same for resource quantities so device-plugin allocatable counts can use
	// int(node.status.allocatable['...']).
	normalizeNumericNodeInputs(nodeMap)

	resourceSliceMaps := make([]map[string]any, 0, len(resourceSlices))
	for _, resourceSlice := range resourceSlices {
		resourceSliceMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(resourceSlice)
		if err != nil {
			return 0, fmt.Errorf("convert ResourceSlice %s to CEL input: %w", resourceSlice.Name, err)
		}

		resourceSliceMaps = append(resourceSliceMaps, resourceSliceMap)
	}

	result, _, err := class.program.ContextEval(ctx, map[string]any{
		"node":           nodeMap,
		"resourceSlices": resourceSliceMaps,
	})
	if err != nil {
		return 0, fmt.Errorf("evaluate currentExpression: %w", err)
	}

	if types.IsUnknownOrError(result) {
		return 0, fmt.Errorf("currentExpression returned unknown/error: %v", result)
	}

	count, ok := celResultToInt(result)
	if !ok {
		return 0, fmt.Errorf("currentExpression returned non-integer: %T", result.Value())
	}

	if count < 0 {
		return 0, fmt.Errorf("currentExpression returned negative count: %d", count)
	}

	return count, nil
}

func celResultToInt(result ref.Val) (int, bool) {
	if intValue, ok := result.(types.Int); ok {
		return int(int64(intValue)), true
	}

	switch value := result.Value().(type) {
	case int:
		return value, true
	case int32:
		return int(value), true
	case int64:
		return int(value), true
	default:
		return 0, false
	}
}

func normalizeNumericNodeInputs(nodeMap map[string]any) {
	metadata, ok := nodeMap["metadata"].(map[string]any)
	if ok {
		if nodeLabels, ok := metadata["labels"].(map[string]any); ok {
			normalizeNumericMapValues(nodeLabels)
		}
	}

	status, ok := nodeMap["status"].(map[string]any)
	if !ok {
		return
	}

	if allocatable, ok := status["allocatable"].(map[string]any); ok {
		normalizeNumericMapValues(allocatable)
	}

	if capacity, ok := status["capacity"].(map[string]any); ok {
		normalizeNumericMapValues(capacity)
	}
}

func normalizeNumericMapValues(values map[string]any) {
	for key, value := range values {
		raw, ok := value.(string)
		if !ok {
			continue
		}

		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err == nil {
			values[key] = parsed
		}
	}
}

func (m *Manager) expectedDeviceCount(
	ctx context.Context,
	class compiledClass,
	node *corev1.Node,
	current int,
	peerNodes []*corev1.Node,
	resourceSlicesForNode func(*corev1.Node) []*resourcev1.ResourceSlice,
) int {
	if override, ok := class.expectedOverride(node); ok {
		return override
	}

	// Learned expected counts can rise from current observations or previously
	// written labels, but they must not fall automatically when devices vanish.
	expected := maxCountLabel(current, node.Labels[class.Labels.Expected])

	partitionKey := class.partitionKey(node)
	for _, peer := range peerNodesForPartition(class, partitionKey, peerNodes) {
		expected = m.learnExpectedFromPeer(ctx, class, node, peer, expected, resourceSlicesForNode)
	}

	return expected
}

func maxCountLabel(current int, raw string) int {
	existing, ok := parseCountLabel(raw)
	if ok && existing > current {
		return existing
	}

	return current
}

func peerNodesForPartition(
	class compiledClass,
	partitionKey string,
	peerNodes []*corev1.Node,
) []*corev1.Node {
	nodes := []*corev1.Node{}

	for _, peer := range peerNodes {
		if class.partitionKey(peer) != partitionKey {
			continue
		}

		nodes = append(nodes, peer)
	}

	return nodes
}

func (m *Manager) learnExpectedFromPeer(
	ctx context.Context,
	class compiledClass,
	node *corev1.Node,
	peer *corev1.Node,
	expected int,
	resourceSlicesForNode func(*corev1.Node) []*resourcev1.ResourceSlice,
) int {
	expected = maxCountLabel(expected, peer.Labels[class.Labels.Expected])

	peerCurrent, ok := m.currentDeviceCountForPeer(ctx, class, node, peer, resourceSlicesForNode)
	if ok && peerCurrent > expected {
		return peerCurrent
	}

	return expected
}

func (m *Manager) currentDeviceCountForPeer(
	ctx context.Context,
	class compiledClass,
	node *corev1.Node,
	peer *corev1.Node,
	resourceSlicesForNode func(*corev1.Node) []*resourcev1.ResourceSlice,
) (int, bool) {
	peerResourceSlices := resourceSlicesForNode(peer)

	// A peer with no DRA source should not lower or initialize the baseline
	// for ResourceSlice-backed classes.
	if class.referencesResourceSlices() && len(peerResourceSlices) == 0 {
		metrics.DeviceCountSkippedUpdates.WithLabelValues(class.Name, metrics.SkipReasonMissingSource).Inc()
		return 0, false
	}

	peerCurrent, err := m.evaluateCurrent(ctx, class, peer, peerResourceSlices)
	if err != nil {
		metrics.DeviceCountSkippedUpdates.WithLabelValues(class.Name, metrics.SkipReasonEvaluationError).Inc()
		slog.Warn("Skipping peer device count during expected count learning",
			"node", node.Name, "peer", peer.Name, "class", class.Name, "error", err)

		return 0, false
	}

	return peerCurrent, true
}

func parseCountLabel(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, false
	}

	return value, true
}

func (class compiledClass) expectedOverride(node *corev1.Node) (int, bool) {
	for _, override := range class.ExpectedCountOverrides {
		if matchLabels(node.Labels, override.MatchLabels) {
			return override.Count, true
		}
	}

	return 0, false
}

func (class compiledClass) referencesResourceSlices() bool {
	// This cheap check is only used to distinguish "missing DRA source" from a
	// legitimate zero count. Expressions that do not reference ResourceSlices can
	// still evaluate from node labels alone.
	return strings.Contains(class.CurrentExpression, "resourceSlices")
}

func matchLabels(actual, expected map[string]string) bool {
	for key, expectedValue := range expected {
		if actual[key] != expectedValue {
			return false
		}
	}

	return true
}

func (class compiledClass) partitionKey(node *corev1.Node) string {
	if len(class.GroupingLabels) == 0 {
		return "default"
	}

	// The class name is not included here because callers already track class
	// separately; the partition is just the configured hardware grouping.
	parts := make([]string, 0, len(class.GroupingLabels))
	for _, label := range class.GroupingLabels {
		parts = append(parts, label+"="+node.Labels[label])
	}

	return strings.Join(parts, "|")
}
