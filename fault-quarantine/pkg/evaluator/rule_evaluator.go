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

package evaluator

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corelisters "k8s.io/client-go/listers/core/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/celrule"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/coldstart"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
)

type RuleEvaluator interface {
	Evaluate(context.Context, *protos.HealthEvent) (common.RuleEvaluationResult, error)
}

// celRule is a rule from commons/pkg/celrule.
type celRule interface {
	Evaluate(context.Context, *protos.HealthEvent) (bool, error)
}

type HealthEventRuleEvaluator struct {
	rule *celrule.HealthEventRule
}

type NodeRuleEvaluator struct {
	rule *celrule.NodeRule
}

type nodeReader interface {
	GetNodeDirect(context.Context, string) (*corev1.Node, error)
}

// NewHealthEventRuleEvaluator creates a new HealthEventRuleEvaluator with dynamic declarations
func NewHealthEventRuleEvaluator(expression string) (*HealthEventRuleEvaluator, error) {
	rule, err := celrule.NewHealthEventRule(expression)
	if err != nil {
		return nil, err
	}

	return &HealthEventRuleEvaluator{rule: rule}, nil
}

// evaluates the CEL expression against the provided HealthEvent
func (he *HealthEventRuleEvaluator) Evaluate(
	ctx context.Context,
	event *protos.HealthEvent) (common.RuleEvaluationResult, error) {
	return evaluate(ctx, he.rule, event)
}

// NewNodeRuleEvaluator creates a new NodeRuleEvaluator
func NewNodeRuleEvaluator(expression string, nodeLister corelisters.NodeLister) (*NodeRuleEvaluator, error) {
	return newNodeRuleEvaluator(expression, nodeLister, nil)
}

func newNodeRuleEvaluator(
	expression string,
	nodeLister corelisters.NodeLister,
	nodeReader nodeReader,
) (*NodeRuleEvaluator, error) {
	rule, err := celrule.NewNodeRule(expression, &nodeGetter{
		nodeLister: nodeLister,
		nodeReader: nodeReader,
	})
	if err != nil {
		return nil, err
	}

	return &NodeRuleEvaluator{rule: rule}, nil
}

// Evaluate the CEL expression against node metadata and spec. Recovery reads
// directly from the API server; live processing uses the informer cache.
func (nm *NodeRuleEvaluator) Evaluate(
	ctx context.Context,
	event *protos.HealthEvent,
) (common.RuleEvaluationResult, error) {
	return evaluate(ctx, nm.rule, event)
}

// evaluate converts a celrule result. An error in the expression is permanent for
// the event, so cold start does not retry it.
func evaluate(
	ctx context.Context,
	rule celRule,
	event *protos.HealthEvent,
) (common.RuleEvaluationResult, error) {
	matched, err := rule.Evaluate(ctx, event)
	if err != nil {
		if celrule.IsEvaluationError(err) {
			return common.RuleEvaluationFailed, coldstart.PermanentError(err)
		}

		return common.RuleEvaluationFailed, err
	}

	if matched {
		return common.RuleEvaluationSuccess, nil
	}

	return common.RuleEvaluationFailed, nil
}

type nodeGetter struct {
	nodeLister corelisters.NodeLister
	nodeReader nodeReader
}

// GetNode bypasses the informer during recovery so rules see current node state.
// A node deleted before replay is a permanent event error.
func (g *nodeGetter) GetNode(ctx context.Context, nodeName string) (*corev1.Node, error) {
	var (
		node *corev1.Node
		err  error
	)

	isRecoveryRead := coldstart.IsRecoveryContext(ctx) && g.nodeReader != nil
	if isRecoveryRead {
		node, err = coldstart.GetRecoveryNode(ctx, nodeName, func() (*corev1.Node, error) {
			return g.nodeReader.GetNodeDirect(ctx, nodeName)
		})
	} else {
		node, err = g.nodeLister.Get(nodeName)
	}

	if err != nil {
		wrappedErr := fmt.Errorf("failed to get node %s: %w", nodeName, err)
		if isRecoveryRead && apierrors.IsNotFound(err) {
			return nil, coldstart.PermanentError(wrappedErr)
		}

		return nil, wrappedErr
	}

	return node, nil
}
