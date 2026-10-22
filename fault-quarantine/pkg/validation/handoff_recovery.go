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

package validation

import (
	"context"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/informer"
)

// EnsurePendingHandoff recreates a request deleted before lifecycle-manager
// registered it. The retained source already passed the initial drain check.
// pending remains true until lifecycle-manager consumes that source; checking
// again also catches deletion after this call observes an existing request.
func (c *ValidationClient) EnsurePendingHandoff(ctx context.Context, nodeName string) (pending bool, err error) {
	if !c.persistHandoff || c.k8sClient.DryRunMode {
		return false, nil
	}

	node, err := c.k8sClient.Clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("read pending handoff node %s: %w", nodeName, err)
	}

	if !pendingHandoff(node) {
		return false, nil
	}

	source := node.Annotations[common.QuarantineValidationHealthEventAnnotationKey]

	_, tests, id, err := parseValidationSource(source)
	if err != nil {
		return true, fmt.Errorf("read pending handoff for node %s: %w", nodeName, err)
	}

	if len(tests) == 0 {
		return true, fmt.Errorf("pending validation handoff for node %s contains no tests", nodeName)
	}

	return true, c.restoreHandoffRequest(ctx, node, tests, id, source)
}

func (c *ValidationClient) restoreHandoffRequest(ctx context.Context, node *corev1.Node,
	tests []string, id, source string) error {
	desired, err := c.renderValidationRequest(node, tests, id, "", "", source)
	if err != nil {
		return err
	}

	requests := c.k8sClient.DynamicClient.Resource(c.resourceGVR)

	existing, err := requests.Get(ctx, desired.GetName(), metav1.GetOptions{})
	if err == nil {
		// Completed requests are left intact as well: an existing object never
		// starts another test merely because the producer rechecks its handoff.
		return informer.ValidateRecoveredValidationRequest(existing, desired)
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("read validation handoff request %s: %w", desired.GetName(), err)
	}

	// Recheck after discovering the missing request. The node may have been
	// replaced, quarantined again, or registered while that GET was in flight.
	current, err := c.currentPendingHandoff(ctx, node, source)
	if err != nil || current == nil {
		return err
	}

	// Ownership is derived again from this latest Node, including a preservation
	// decision made while the request lookup was in flight.
	desired, err = c.renderValidationRequest(current, tests, id, "", "", source)
	if err != nil {
		return err
	}

	_, err = requests.Create(ctx, desired, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, err = requests.Get(ctx, desired.GetName(), metav1.GetOptions{})
		if err == nil {
			return informer.ValidateRecoveredValidationRequest(existing, desired)
		}
	}

	if err != nil {
		return fmt.Errorf("restore validation handoff request %s: %w", desired.GetName(), err)
	}

	slog.InfoContext(ctx, "Restored missing validation handoff request",
		"node", node.Name, "validationRequest", desired.GetName())

	return nil
}

func (c *ValidationClient) currentPendingHandoff(ctx context.Context, expected *corev1.Node,
	source string) (*corev1.Node, error) {
	current, err := c.k8sClient.Clientset.CoreV1().Nodes().Get(ctx, expected.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("recheck pending handoff node %s: %w", expected.Name, err)
	}

	if current.UID != expected.UID || !pendingHandoff(current) ||
		current.Annotations[common.QuarantineValidationHealthEventAnnotationKey] != source {
		return nil, nil
	}

	return current, nil
}

func pendingHandoff(node *corev1.Node) bool {
	_, quarantined := node.Annotations[common.QuarantineHealthEventAnnotationKey]
	_, pending := node.Annotations[common.QuarantineValidationHealthEventAnnotationKey]
	_, dryRun := node.Annotations[common.QuarantineHealthEventDryRunAnnotationKey]

	return pending && !quarantined && !dryRun
}
