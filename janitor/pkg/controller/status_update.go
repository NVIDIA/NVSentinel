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

package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// NodeActionStatus defines the interface that both RebootNodeStatus and TerminateNodeStatus must implement.
// This allows generic status update handling across different node action types.
type NodeActionStatus interface {
	GetRetryCount() int32
	GetConsecutiveFailures() int32
	GetStartTime() *metav1.Time
	GetCompletionTime() *metav1.Time
	GetConditions() []metav1.Condition
}

// NodeActionObject defines the interface that both RebootNode and TerminateNode must implement.
type NodeActionObject interface {
	client.Object
	GetSpec() NodeActionSpec
	GetStatus() NodeActionStatus
}

// NodeActionSpec defines the common spec fields across node action types.
type NodeActionSpec interface {
	GetNodeName() string
}

// conditionsChanged compares two slices of conditions and returns true if they differ.
// It checks for differences in Type, Status, Reason, and Message fields.
func conditionsChanged(original, updated []metav1.Condition) bool {
	if len(original) != len(updated) {
		return true
	}

	// Build a map of original conditions for quick lookup
	originalMap := make(map[string]metav1.Condition)
	for _, cond := range original {
		originalMap[cond.Type] = cond
	}

	// Check each updated condition against the original
	for _, updatedCond := range updated {
		originalCond, exists := originalMap[updatedCond.Type]
		if !exists {
			return true // New condition type added
		}

		// Compare the fields that matter for status updates
		if originalCond.Status != updatedCond.Status ||
			originalCond.Reason != updatedCond.Reason ||
			originalCond.Message != updatedCond.Message {
			return true
		}
	}

	return false
}

// updateNodeActionStatus is a generic helper function that handles status updates with proper error handling.
// It centralizes the status update logic to avoid code duplication and provides consistent handling
// of status updates across different node action types (RebootNode, TerminateNode, etc.).
//
// This status update is safe because controller-runtime uses leader election to ensure only one
// controller instance is active at a time, even with multiple replicas. The active controller
// has exclusive write access to the resource status.
func updateNodeActionStatus[T client.Object](
	ctx context.Context,
	statusWriter client.SubResourceWriter,
	original T,
	updated T,
	originalStatus NodeActionStatus,
	updatedStatus NodeActionStatus,
	nodeName string,
	resourceType string,
	result ctrl.Result,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Check if status changed by comparing individual fields
	statusChanged := originalStatus.GetRetryCount() != updatedStatus.GetRetryCount() ||
		originalStatus.GetConsecutiveFailures() != updatedStatus.GetConsecutiveFailures() ||
		(originalStatus.GetStartTime() == nil) != (updatedStatus.GetStartTime() == nil) ||
		(originalStatus.GetCompletionTime() == nil) != (updatedStatus.GetCompletionTime() == nil) ||
		conditionsChanged(originalStatus.GetConditions(), updatedStatus.GetConditions())

	if statusChanged {
		if err := statusWriter.Update(ctx, updated); err != nil {
			if apierrors.IsNotFound(err) {
				logger.V(0).Info("post-reconciliation status update: object not found, assumed deleted",
					"type", resourceType)

				return ctrl.Result{}, nil
			}

			logger.Error(err, "failed to update status",
				"node", nodeName,
				"type", resourceType)

			return ctrl.Result{}, err
		}

		logger.Info("status updated",
			"node", nodeName,
			"type", resourceType,
			"retryCount", int(updatedStatus.GetRetryCount()),
			"consecutiveFailures", int(updatedStatus.GetConsecutiveFailures()))
	}

	return result, nil
}
