// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

package informer

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/commons/pkg/quarantinetaints"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
)

// CompleteValidationHandoff clears only the quarantine represented by the created
// request. By default, its validation source remains until lifecycle-manager
// registers it. Passing retainSource=false consumes the source for an adopted
// legacy request that cannot acknowledge it.
func (c *FaultQuarantineClient) CompleteValidationHandoff(ctx context.Context, expected *corev1.Node,
	annotationKeys, labelsToRemove []string, retainSource ...bool) error {
	mu, _ := c.operationMutex.LoadOrStore(expected.Name, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	_, err := c.nodePatcher.Patch(ctx, c.Clientset.CoreV1().Nodes(), expected.Name, nil, func(node *corev1.Node) error {
		if err := matchingHandoffNode(node, expected); err != nil {
			return err
		}

		for _, key := range annotationKeys {
			if key != common.QuarantineValidationHealthEventAnnotationKey && key != cordonownership.AnnotationKey &&
				key != cordonownership.ValidationSessionAnnotationKey && key != quarantinetaints.AnnotationKey {
				delete(node.Annotations, key)
			}
		}

		if len(retainSource) > 0 && !retainSource[0] {
			delete(node.Annotations, common.QuarantineValidationHealthEventAnnotationKey)
		}

		c.removeLabels(ctx, node, labelsToRemove, node.Name)

		_, err := quarantinetaints.Prune(node, true)

		return err
	}, kubeclient.WithOptimisticLock())

	return err
}

func matchingHandoffNode(node, expected *corev1.Node) error {
	if expected.UID == "" || node.UID != expected.UID {
		return fmt.Errorf("node %s identity changed during validation handoff", expected.Name)
	}

	if _, quarantined := expected.Annotations[common.QuarantineHealthEventAnnotationKey]; !quarantined {
		return fmt.Errorf("node %s has no quarantine to hand off", expected.Name)
	}

	for _, key := range common.QuarantineAnnotationKeys {
		// Lifecycle manager can register the request before this handoff completes.
		// Its session is preserved, and it may mark the claim preserved while doing
		// so. The producer's quarantine and the claim identity must still match.
		if key == cordonownership.ValidationSessionAnnotationKey {
			continue
		}

		if key == cordonownership.AnnotationKey {
			if !sameClaimIdentity(node, expected) {
				return fmt.Errorf("node %s cordon claim changed during validation handoff", expected.Name)
			}

			continue
		}

		current, present := node.Annotations[key]
		previous, expectedPresent := expected.Annotations[key]

		if current != previous || present != expectedPresent {
			return fmt.Errorf("node %s quarantine changed during validation handoff", expected.Name)
		}
	}

	return nil
}

func sameClaimIdentity(node, expected *corev1.Node) bool {
	current, currentPresent := node.Annotations[cordonownership.AnnotationKey]
	previous, previousPresent := expected.Annotations[cordonownership.AnnotationKey]

	if currentPresent != previousPresent {
		return false
	}

	if !currentPresent || current == previous {
		return true
	}

	var currentClaim, previousClaim cordonownership.Claim
	if json.Unmarshal([]byte(current), &currentClaim) != nil ||
		json.Unmarshal([]byte(previous), &previousClaim) != nil {
		return false
	}

	return currentClaim.ID != "" && currentClaim.ID == previousClaim.ID &&
		currentClaim.NodeUID == previousClaim.NodeUID
}
