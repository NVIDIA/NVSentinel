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
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/ext"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
	"github.com/nvidia/nvsentinel/labeler/pkg/metrics"
)

// errNoCount reports a negative currentExpression result. A source without a
// ResourceSliceDriver returns it, for example -1, to say that it does not apply.
var errNoCount = errors.New("currentExpression returned a negative count")

// Config controls expected device-count label reconciliation.
type Config struct {
	Enabled bool          `toml:"enabled"`
	Classes []ClassConfig `toml:"classes"`
}

// ClassConfig describes one device-count class, such as GPU or NIC counts.
//
// A class either sets CurrentExpression and GroupingLabels directly, or lists
// Sources. With Sources, each node uses the first source that is available on it.
type ClassConfig struct {
	Name                   string                  `toml:"name"`
	Enabled                bool                    `toml:"enabled"`
	Labels                 Labels                  `toml:"labels"`
	GroupingLabels         []string                `toml:"groupingLabels"`
	ExpectedCountOverrides []ExpectedCountOverride `toml:"expectedCountOverrides"`
	CurrentExpression      string                  `toml:"currentExpression"`
	Sources                []SourceConfig          `toml:"sources"`
}

// SourceConfig describes one way to count a class's devices on a node.
type SourceConfig struct {
	Name string `toml:"name"`
	// ResourceSliceDriver makes the source available only on nodes that have
	// ResourceSlices from this driver, and limits resourceSlices to them.
	ResourceSliceDriver string   `toml:"resourceSliceDriver"`
	GroupingLabels      []string `toml:"groupingLabels"`
	CurrentExpression   string   `toml:"currentExpression"`
	// GroupingExpression returns a list of strings, such as device models, that
	// further partitions nodes for expected-count learning.
	GroupingExpression string `toml:"groupingExpression"`
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
	sources []compiledSource
}

type compiledSource struct {
	SourceConfig
	className       string
	currentProgram  cel.Program
	groupingProgram cel.Program
}

// ReconcileCache caches peer observations while one or more target nodes are reconciled.
// Create a new cache whenever the peer-node or ResourceSlice snapshot may have changed.
// A cache is used sequentially and discarded after its target nodes are reconciled;
// it is not safe for concurrent use.
//
// During a fleet reconciliation, the cache ensures each node's ResourceSlices
// are loaded once, each class/peer CEL expression is evaluated once, and each
// class/partition maximum is learned once. Combined with the node-name informer
// index, ResourceSlice processing scales with the N nodes and their K slices
// (O(N*K)) instead of repeating peer-wide store scans for every target node.
type ReconcileCache struct {
	manager *Manager

	// peerNodes is the cache-scoped node snapshot used to learn the maximum
	// expected count for each class and hardware partition.
	peerNodes []*corev1.Node

	// loadResourceSlicesForNode performs the indexed informer lookup when a node is
	// first observed by this cache.
	loadResourceSlicesForNode func(*corev1.Node) []*resourcev1.ResourceSlice

	// resourceSlices memoizes loadResourceSlicesForNode results by node name. A
	// nil result is cached as well, preventing repeated lookups for a missing source.
	resourceSlices map[string][]*resourcev1.ResourceSlice

	// peerObservations maps a class-and-node key to the observation obtained by
	// evaluating that class for that peer: its current count and partition, or
	// why no count was available. expectedDeviceCountForPartition uses this map
	// to avoid reevaluating the same class/peer pair while the cache lives.
	peerObservations map[peerCurrentCountKey]countObservation

	// partitionExpected memoizes the maximum count learned from all peers in a
	// class/partition pair, allowing every target in that partition to reuse it.
	partitionExpected map[partitionExpectedKey]int
}

// peerCurrentCountKey identifies one peer observation within a ReconcileCache.
//
// classIndex is the position in Manager.classes (the compiled, enabled classes),
// not the position in the original configuration. Including it prevents two
// classes that inspect the same node from sharing results from different CEL
// programs. For example, the GPU class at index 0 for "node-a" is represented
// as peerCurrentCountKey{classIndex: 0, nodeName: "node-a"}.
type peerCurrentCountKey struct {
	classIndex int
	nodeName   string
}

// partitionExpectedKey identifies the learned expected count shared by one
// class and one hardware partition within a ReconcileCache.
//
// partitionKey is countObservation.partitionKey. For a class without sources it
// is "default" when no grouping labels are configured, or a value such as
// "node.kubernetes.io/instance-type=p5|nvidia.com/gpu.product=H100" otherwise.
// For a class with sources it starts with the source name, for example
// "source=dra|node.kubernetes.io/instance-type=p5|hardware=NVIDIA H100 80GB HBM3".
// The class index keeps equal partition strings from different classes separate.
type partitionExpectedKey struct {
	classIndex   int
	partitionKey string
}

// countObservation records the result of evaluating one class for one node.
//
// Exactly one result state is meaningful:
//   - missingSource is true when no count source is available on the node;
//   - err is non-nil when CEL evaluation failed; or
//   - count is valid when missingSource is false and err is nil.
//
// partitionKey is meaningful only when hasPartition is true. A node without a
// partition learns its expected count only from itself and is never a peer.
// For a class without sources, the partition comes from node labels alone and
// is known even when the count is not.
//
// Failed and missing-source observations are cached too, so every peer is
// inspected at most once per class during a ReconcileCache's lifetime.
type countObservation struct {
	count         int
	missingSource bool
	err           error
	source        *compiledSource
	partitionKey  string
	hasPartition  bool
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
		if class.requiresResourceSlices() {
			return true
		}
	}

	return false
}

// ClassCount returns the number of compiled device-count classes.
func (m *Manager) ClassCount() int {
	return len(m.classes)
}

// CalculateAndSetDeviceCountLabels evaluates all enabled classes and writes
// their current and expected device-count labels onto node.
func (m *Manager) CalculateAndSetDeviceCountLabels(
	ctx context.Context,
	node *corev1.Node,
	peerNodes []*corev1.Node,
	loadResourceSlicesForNode func(*corev1.Node) []*resourcev1.ResourceSlice,
) bool {
	return m.NewReconcileCache(peerNodes, loadResourceSlicesForNode).
		CalculateAndSetDeviceCountLabels(ctx, node)
}

// NewReconcileCache creates a scoped cache for peer expected-count learning.
func (m *Manager) NewReconcileCache(
	peerNodes []*corev1.Node,
	loadResourceSlicesForNode func(*corev1.Node) []*resourcev1.ResourceSlice,
) *ReconcileCache {
	return &ReconcileCache{
		manager:                   m,
		peerNodes:                 peerNodes,
		loadResourceSlicesForNode: loadResourceSlicesForNode,
		resourceSlices:            make(map[string][]*resourcev1.ResourceSlice),
		peerObservations:          make(map[peerCurrentCountKey]countObservation),
		partitionExpected:         make(map[partitionExpectedKey]int),
	}
}

// CalculateAndSetDeviceCountLabels evaluates all enabled classes using cached
// peer observations and writes their labels onto node.
func (p *ReconcileCache) CalculateAndSetDeviceCountLabels(ctx context.Context, node *corev1.Node) bool {
	if !p.manager.Enabled() {
		return false
	}

	if node.Labels == nil {
		node.Labels = make(map[string]string)
	}

	needsUpdate := false
	resourceSlices := p.cachedResourceSlicesForNode(node)

	for classIndex, class := range p.manager.classes {
		observation := p.manager.observe(ctx, class, node, resourceSlices)
		recordDeviceCountSource(class, node.Name, observation)

		// Do not turn a missing source into current=0. The labels keep their
		// last values until a source is available again.
		if observation.missingSource {
			metrics.DeviceCountSkippedUpdates.WithLabelValues(class.Name, metrics.SkipReasonMissingSource).Inc()
			slog.Warn("Skipping device count label update because no device count source is available on the node",
				"node", node.Name, "class", class.Name)

			continue
		}

		if observation.err != nil {
			metrics.DeviceCountSkippedUpdates.WithLabelValues(class.Name, metrics.SkipReasonEvaluationError).Inc()
			slog.Warn("Skipping device count label update after current count evaluation failed",
				"node", node.Name, "class", class.Name, "error", observation.err)

			continue
		}

		current := observation.count
		expected := p.expectedDeviceCount(ctx, classIndex, class, node, observation)
		currentValue := strconv.Itoa(current)
		expectedValue := strconv.Itoa(expected)

		metrics.CurrentDeviceCount.WithLabelValues(node.Name, class.Name).Set(float64(current))

		if observation.hasPartition {
			metrics.ExpectedDeviceCount.WithLabelValues(class.Name, observation.partitionKey).Set(float64(expected))
		}

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

// NodeResourcesAffectDeviceCounts reports whether an allocatable or capacity
// change on a node could affect a device-count class whose CEL expression reads
// from node.status.allocatable or node.status.capacity.
func (m *Manager) NodeResourcesAffectDeviceCounts(oldNode, newNode *corev1.Node) bool {
	if !m.Enabled() || oldNode == nil || newNode == nil {
		return false
	}

	resourcesReferenced := false

	for _, class := range m.classes {
		if class.referencesNodeResources() {
			resourcesReferenced = true
			break
		}
	}

	if !resourcesReferenced {
		return false
	}

	oldAllocatable := resourceListToStringMap(oldNode.Status.Allocatable)
	newAllocatable := resourceListToStringMap(newNode.Status.Allocatable)

	if !maps.Equal(oldAllocatable, newAllocatable) {
		return true
	}

	oldCapacity := resourceListToStringMap(oldNode.Status.Capacity)
	newCapacity := resourceListToStringMap(newNode.Status.Capacity)

	return !maps.Equal(oldCapacity, newCapacity)
}

func resourceListToStringMap(rl corev1.ResourceList) map[string]string {
	if len(rl) == 0 {
		return nil
	}

	result := make(map[string]string, len(rl))
	for k, v := range rl {
		result[string(k)] = v.String()
	}

	return result
}

func newDeviceCountCELEnv() (*cel.Env, error) {
	// Keep the CEL surface intentionally small: expressions can only inspect
	// the reconciled node and that node's associated ResourceSlices, sum lists,
	// and use the cel-go list helpers such as flatten.
	return cel.NewEnv(
		cel.Variable("node", cel.DynType),
		cel.Variable("resourceSlices", cel.ListType(cel.DynType)),
		ext.Lists(),
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

	prefix := fmt.Sprintf("expectedDeviceCounts.classes[%d] (%s)", index, classConfig.Name)

	if len(classConfig.Sources) > 0 {
		sources, err := compileDeviceCountSources(env, prefix, classConfig)
		if err != nil {
			return compiledClass{}, false, fmt.Errorf("compile sources: %w", err)
		}

		return compiledClass{
			ClassConfig: classConfig,
			sources:     sources,
		}, true, nil
	}

	program, err := compileExpression(env, prefix, "currentExpression", classConfig.CurrentExpression,
		isCountType, "int")
	if err != nil {
		return compiledClass{}, false, fmt.Errorf("compile class expression: %w", err)
	}

	return compiledClass{
		ClassConfig: classConfig,
		program:     program,
	}, true, nil
}

func compileDeviceCountSources(env *cel.Env, classPrefix string, classConfig ClassConfig) ([]compiledSource, error) {
	sources := make([]compiledSource, 0, len(classConfig.Sources))
	names := map[string]struct{}{}

	for j, sourceConfig := range classConfig.Sources {
		prefix := fmt.Sprintf("%s.sources[%d] (%s)", classPrefix, j, sourceConfig.Name)

		if err := validateDeviceCountSourceConfig(prefix, sourceConfig, names); err != nil {
			return nil, fmt.Errorf("validate source: %w", err)
		}

		names[sourceConfig.Name] = struct{}{}

		currentProgram, err := compileExpression(env, prefix, "currentExpression",
			sourceConfig.CurrentExpression, isCountType, "int")
		if err != nil {
			return nil, fmt.Errorf("compile source current expression: %w", err)
		}

		source := compiledSource{
			SourceConfig:   sourceConfig,
			className:      classConfig.Name,
			currentProgram: currentProgram,
		}

		if strings.TrimSpace(sourceConfig.GroupingExpression) != "" {
			source.groupingProgram, err = compileExpression(env, prefix, "groupingExpression",
				sourceConfig.GroupingExpression, isGroupingType, "list<string>")
			if err != nil {
				return nil, fmt.Errorf("compile source grouping expression: %w", err)
			}
		}

		sources = append(sources, source)
	}

	return sources, nil
}

func validateDeviceCountSourceConfig(prefix string, sourceConfig SourceConfig, names map[string]struct{}) error {
	if sourceConfig.Name == "" {
		return fmt.Errorf("%s: name is required", prefix)
	}

	if _, ok := names[sourceConfig.Name]; ok {
		return fmt.Errorf("%s: source names must be unique within a class", prefix)
	}

	if strings.TrimSpace(sourceConfig.CurrentExpression) == "" {
		return fmt.Errorf("%s: currentExpression is required", prefix)
	}

	return nil
}

func compileExpression(
	env *cel.Env,
	prefix, field, expression string,
	allowed func(*cel.Type) bool,
	want string,
) (cel.Program, error) {
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("%s: compile %s: %w", prefix, field, issues.Err())
	}

	if !allowed(ast.OutputType()) {
		return nil, fmt.Errorf("%s: %s must return %s, got %s", prefix, field, want, ast.OutputType())
	}

	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("%s: create CEL program: %w", prefix, err)
	}

	return program, nil
}

func isCountType(outputType *cel.Type) bool {
	return outputType == cel.IntType || outputType == cel.DynType
}

func isGroupingType(outputType *cel.Type) bool {
	if outputType.Kind() == types.DynKind {
		return true
	}

	if outputType.Kind() != types.ListKind || len(outputType.Parameters()) != 1 {
		return false
	}

	// Element types that CEL cannot infer, such as values read from ResourceSlices, are dyn.
	element := outputType.Parameters()[0].Kind()

	return element == types.StringKind || element == types.DynKind
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

	if err := validateDeviceCountClassForm(index, classConfig); err != nil {
		return fmt.Errorf("validate class form: %w", err)
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

// validateDeviceCountClassForm requires either a class-level currentExpression
// or sources. With sources, counting and grouping are defined only per source.
func validateDeviceCountClassForm(index int, classConfig ClassConfig) error {
	hasClassExpression := strings.TrimSpace(classConfig.CurrentExpression) != ""

	if len(classConfig.Sources) == 0 {
		if !hasClassExpression {
			return fmt.Errorf(
				"expectedDeviceCounts.classes[%d] (%s): currentExpression is required",
				index, classConfig.Name)
		}

		return nil
	}

	if hasClassExpression || len(classConfig.GroupingLabels) > 0 {
		return fmt.Errorf(
			"expectedDeviceCounts.classes[%d] (%s): currentExpression and groupingLabels must be set on each "+
				"source, not on the class, when sources are used",
			index, classConfig.Name)
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
	program cel.Program,
	node *corev1.Node,
	resourceSlices []*resourcev1.ResourceSlice,
) (int, error) {
	result, err := evaluateExpression(ctx, program, "currentExpression", node, resourceSlices)
	if err != nil {
		return 0, fmt.Errorf("evaluate current count: %w", err)
	}

	count, ok := celResultToInt(result)
	if !ok {
		return 0, fmt.Errorf("currentExpression returned non-integer: %T", result.Value())
	}

	if count < 0 {
		return 0, fmt.Errorf("%w: %d", errNoCount, count)
	}

	return count, nil
}

func evaluateExpression(
	ctx context.Context,
	program cel.Program,
	field string,
	node *corev1.Node,
	resourceSlices []*resourcev1.ResourceSlice,
) (ref.Val, error) {
	nodeMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(node)
	if err != nil {
		return nil, fmt.Errorf("convert node to CEL input: %w", err)
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
			return nil, fmt.Errorf("convert ResourceSlice %s to CEL input: %w", resourceSlice.Name, err)
		}

		resourceSliceMaps = append(resourceSliceMaps, resourceSliceMap)
	}

	result, _, err := program.ContextEval(ctx, map[string]any{
		"node":           nodeMap,
		"resourceSlices": resourceSliceMaps,
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate %s: %w", field, err)
	}

	if types.IsUnknownOrError(result) {
		return nil, fmt.Errorf("%s returned unknown/error: %v", field, result)
	}

	return result, nil
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

// cachedResourceSlicesForNode loads a node's indexed ResourceSlices at most
// once. Reusing the result across target and peer evaluations prevents multiple
// classes from repeating the same O(K) informer lookup.
func (p *ReconcileCache) cachedResourceSlicesForNode(node *corev1.Node) []*resourcev1.ResourceSlice {
	if node == nil {
		return nil
	}

	if resourceSlices, ok := p.resourceSlices[node.Name]; ok {
		return resourceSlices
	}

	var resourceSlices []*resourcev1.ResourceSlice
	if p.loadResourceSlicesForNode != nil {
		resourceSlices = p.loadResourceSlicesForNode(node)
	}

	p.resourceSlices[node.Name] = resourceSlices

	return resourceSlices
}

func (p *ReconcileCache) expectedDeviceCount(
	ctx context.Context,
	classIndex int,
	class compiledClass,
	node *corev1.Node,
	observation countObservation,
) int {
	if override, ok := class.expectedOverride(node); ok {
		return override
	}

	// Learned expected counts can rise from current observations or previously
	// written labels, but they must not fall automatically when devices vanish.
	expected := maxCountLabel(observation.count, node.Labels[class.Labels.Expected])

	// Without a partition the node cannot tell which peers have the same
	// hardware, so it learns only from itself rather than risk a higher count.
	if !observation.hasPartition {
		return expected
	}

	partitionExpectedCount := p.expectedDeviceCountForPartition(ctx, classIndex, class, node, observation)
	if partitionExpectedCount > expected {
		expected = partitionExpectedCount
	}

	return expected
}

// expectedDeviceCountForPartition computes a class/partition maximum once and caches it.
// Without this cache, each target in a partition of P peers would repeat the
// same P peer evaluations.
func (p *ReconcileCache) expectedDeviceCountForPartition(
	ctx context.Context,
	classIndex int,
	class compiledClass,
	node *corev1.Node,
	observation countObservation,
) int {
	key := partitionExpectedKey{classIndex: classIndex, partitionKey: observation.partitionKey}
	if expected, ok := p.partitionExpected[key]; ok {
		return expected
	}

	groupingLabels := class.GroupingLabels
	if observation.source != nil {
		groupingLabels = observation.source.GroupingLabels
	}

	expected := 0

	for _, peer := range p.peerNodes {
		// Peers with different grouping-label values cannot share the
		// partition, so skip them before evaluating any CEL for them.
		if !groupingLabelsMatch(groupingLabels, node, peer) {
			continue
		}

		peerObservation := p.peerObservation(ctx, classIndex, class, peer)
		if !peerObservation.hasPartition || peerObservation.partitionKey != observation.partitionKey {
			continue
		}

		expected = maxCountLabel(expected, peer.Labels[class.Labels.Expected])

		peerCurrent, ok := usablePeerCount(class, node.Name, peer.Name, peerObservation)
		if ok && peerCurrent > expected {
			expected = peerCurrent
		}
	}

	p.partitionExpected[key] = expected

	return expected
}

func groupingLabelsMatch(groupingLabels []string, node, peer *corev1.Node) bool {
	for _, label := range groupingLabels {
		if node.Labels[label] != peer.Labels[label] {
			return false
		}
	}

	return true
}

func maxCountLabel(current int, raw string) int {
	existing, ok := parseCountLabel(raw)
	if ok && existing > current {
		return existing
	}

	return current
}

// peerObservation evaluates each class/peer pair at most once per cache
// lifetime, including failed and missing-source results.
func (p *ReconcileCache) peerObservation(
	ctx context.Context,
	classIndex int,
	class compiledClass,
	peer *corev1.Node,
) countObservation {
	key := peerCurrentCountKey{classIndex: classIndex, nodeName: peer.Name}

	if cached, ok := p.peerObservations[key]; ok {
		return cached
	}

	observation := p.manager.observe(ctx, class, peer, p.cachedResourceSlicesForNode(peer))
	p.peerObservations[key] = observation

	return observation
}

// usablePeerCount returns a peer's current count when it may raise the
// partition baseline.
func usablePeerCount(
	class compiledClass,
	reconciledNodeName, peerName string,
	observation countObservation,
) (int, bool) {
	// A peer with no source should not lower or initialize the baseline.
	if observation.missingSource {
		metrics.DeviceCountSkippedUpdates.WithLabelValues(class.Name, metrics.SkipReasonMissingSource).Inc()
		return 0, false
	}

	if observation.err != nil {
		metrics.DeviceCountSkippedUpdates.WithLabelValues(class.Name, metrics.SkipReasonEvaluationError).Inc()
		slog.Warn("Skipping peer device count during expected count learning",
			"node", reconciledNodeName, "peer", peerName, "class", class.Name, "error", observation.err)

		return 0, false
	}

	return observation.count, true
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

// requiresResourceSlices reports whether any expression or source of the class
// reads ResourceSlices, so the labeler must watch them.
func (class compiledClass) requiresResourceSlices() bool {
	for _, source := range class.Sources {
		if source.ResourceSliceDriver != "" {
			return true
		}
	}

	for _, expression := range class.expressions() {
		if strings.Contains(expression, "resourceSlices") {
			return true
		}
	}

	return false
}

// referencesNodeResources is a cheap heuristic mirroring referencesResourceSlices.
// It checks specifically for node.status.allocatable or node.status.capacity
// rather than the broader node.status, so expressions that only reference
// node.status.conditions (noisy with heartbeats) do not trigger unnecessary
// allocatable/capacity comparisons on every node update.
func (class compiledClass) referencesNodeResources() bool {
	for _, expression := range class.expressions() {
		if strings.Contains(expression, "node.status.allocatable") ||
			strings.Contains(expression, "node.status.capacity") {
			return true
		}
	}

	return false
}

// expressions returns the class-level expression and every source expression.
func (class compiledClass) expressions() []string {
	expressions := []string{class.CurrentExpression}
	for _, source := range class.Sources {
		expressions = append(expressions, source.CurrentExpression, source.GroupingExpression)
	}

	return expressions
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
