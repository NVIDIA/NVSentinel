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

// Package celrule evaluates the HealthEvent and Node CEL rules of a rule set.
//
// A HealthEvent rule reads "event", which holds every field of the event under its
// JSON name, with enums as numbers. A Node rule reads "node", the Node as unstructured
// data. This is the vocabulary that fault-quarantine rules have always used, so a rule
// works the same in every component that evaluates it. It differs from celevent, which
// binds a fixed set of event fields and names the enums.
package celrule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/ext"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const (
	// EventVar is the CEL variable the HealthEvent rules are evaluated against.
	EventVar = "event"
	// NodeVar is the CEL variable the Node rules are evaluated against.
	NodeVar = "node"
)

// EvaluationError reports that the expression itself failed for the input: the event
// did not convert, evaluation failed, or the result was not a boolean. Evaluating the
// same input again gives the same error.
type EvaluationError struct {
	err error
}

func (e *EvaluationError) Error() string { return e.err.Error() }
func (e *EvaluationError) Unwrap() error { return e.err }

// IsEvaluationError reports whether err is, or wraps, an EvaluationError.
func IsEvaluationError(err error) bool {
	var evalErr *EvaluationError

	return errors.As(err, &evalErr)
}

// NodeGetter returns the Node a Node rule is evaluated against.
type NodeGetter interface {
	GetNode(ctx context.Context, name string) (*corev1.Node, error)
}

// NodeGetterFunc adapts a function to a NodeGetter.
type NodeGetterFunc func(ctx context.Context, name string) (*corev1.Node, error)

func (f NodeGetterFunc) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	return f(ctx, name)
}

type HealthEventRule struct {
	expression string
	program    cel.Program
}

type NodeRule struct {
	expression string
	program    cel.Program
	nodeGetter NodeGetter
}

// NewHealthEventRule compiles a HealthEvent rule.
func NewHealthEventRule(expression string) (*HealthEventRule, error) {
	slog.Info("Creating HealthEventRuleEvaluator", "expression", expression)

	program, err := compile(EventVar, expression)
	if err != nil {
		return nil, err
	}

	return &HealthEventRule{
		expression: expression,
		program:    program,
	}, nil
}

// Evaluate evaluates the CEL expression against the provided HealthEvent.
func (he *HealthEventRule) Evaluate(_ context.Context, event *protos.HealthEvent) (bool, error) {
	obj, err := RoundTrip(event)
	if err != nil {
		return false, &EvaluationError{err: fmt.Errorf("error roundtripping event: %w", err)}
	}

	return evaluate(he.program, map[string]any{
		EventVar: obj,
	})
}

// NewNodeRule compiles a Node rule that reads its Node from nodeGetter.
func NewNodeRule(expression string, nodeGetter NodeGetter) (*NodeRule, error) {
	slog.Info("Creating NodeRuleEvaluator", "expression", expression)

	program, err := compile(NodeVar, expression)
	if err != nil {
		return nil, err
	}

	return &NodeRule{
		expression: expression,
		program:    program,
		nodeGetter: nodeGetter,
	}, nil
}

// Evaluate evaluates the CEL expression against the metadata and spec of the node
// of the event.
func (nm *NodeRule) Evaluate(ctx context.Context, event *protos.HealthEvent) (bool, error) {
	slog.Info("Evaluating NodeRuleEvaluator for node", "node", event.NodeName)

	node, err := nm.nodeGetter.GetNode(ctx, event.NodeName)
	if err != nil {
		return false, fmt.Errorf("failed to get node metadata: %w", err)
	}

	nodeInfo, err := NodeToUnstructured(node)
	if err != nil {
		return false, fmt.Errorf("failed to get node metadata: %w", err)
	}

	return evaluate(nm.program, nodeInfo)
}

// NodeToUnstructured converts a Node to the CEL activation of a Node rule.
func NodeToUnstructured(node *corev1.Node) (map[string]any, error) {
	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(node)
	if err != nil {
		return nil, fmt.Errorf("failed to convert node %s to unstructured: %w", node.Name, err)
	}

	restorePrunedMetadataMaps(unstructuredObj, node)

	return map[string]any{
		NodeVar: unstructuredObj,
	}, nil
}

func compile(variable, expression string) (cel.Program, error) {
	env, err := cel.NewEnv(
		cel.Variable(variable, cel.AnyType),
		ext.Strings(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	ast, issues := env.Parse(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("failed to parse expression: %w", issues.Err())
	}

	checkedAst, issues := env.Check(ast)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("failed to check expression: %w", issues.Err())
	}

	program, err := env.Program(checkedAst)
	if err != nil {
		return nil, fmt.Errorf("failed to compile expression: %w", err)
	}

	return program, nil
}

func evaluate(program cel.Program, activation map[string]any) (bool, error) {
	out, _, err := program.Eval(activation)
	if err != nil {
		return false, &EvaluationError{err: fmt.Errorf("failed to evaluate expression: %w", err)}
	}

	result, ok := out.Value().(bool)
	if !ok {
		return false, &EvaluationError{err: fmt.Errorf("expression did not return a boolean: %v", out)}
	}

	return result, nil
}

// restorePrunedMetadataMaps puts metadata.labels and metadata.annotations back
// as empty maps when pruning is what emptied them.
//
// The rules read these maps by key, and CEL raises "no such key: labels" for an
// absent map rather than evaluating the guard as false. Both fields carry
// omitempty, so the conversion drops whichever one is empty, and the informer
// cache prunes them to the keys the rules read: a node holding none of those
// keys converts with no maps at all, which fails every rule that guards on one.
//
// node is the source the conversion ran over, and it still tells the two cases
// apart because pruning keeps a nil map nil. A map the node never had stays
// absent, so a rule asking whether the node carries any labels at all reads the
// node rather than the cache.
func restorePrunedMetadataMaps(unstructuredNode map[string]any, node *corev1.Node) {
	metadata, ok := unstructuredNode["metadata"].(map[string]any)
	if !ok {
		return
	}

	restore := func(field string, source map[string]string) {
		if source == nil {
			return
		}

		if _, present := metadata[field]; !present {
			metadata[field] = map[string]any{}
		}
	}

	restore("labels", node.Labels)
	restore("annotations", node.Annotations)
}
