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

package healthevents

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

// SetCondition sets or updates a condition in the conditions slice.
// If the condition already exists, it updates the existing one.
// If the condition doesn't exist, it appends a new one.
func SetCondition(conditions *[]nvsentinelv1alpha1.HealthEventCondition, condition nvsentinelv1alpha1.HealthEventCondition) {
	if conditions == nil {
		return
	}

	// Find existing condition
	for i, existing := range *conditions {
		if existing.Type == condition.Type {
			// Only update if something changed
			if existing.Status != condition.Status ||
				existing.Reason != condition.Reason ||
				existing.Message != condition.Message {
				(*conditions)[i] = condition
			}
			return
		}
	}

	// Condition not found, append
	*conditions = append(*conditions, condition)
}

// GetCondition returns the condition with the specified type, or nil if not found.
func GetCondition(conditions []nvsentinelv1alpha1.HealthEventCondition, conditionType nvsentinelv1alpha1.HealthEventConditionType) *nvsentinelv1alpha1.HealthEventCondition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}

// IsConditionTrue returns true if the condition with the specified type is True.
func IsConditionTrue(conditions []nvsentinelv1alpha1.HealthEventCondition, conditionType nvsentinelv1alpha1.HealthEventConditionType) bool {
	condition := GetCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

// IsConditionFalse returns true if the condition with the specified type is False.
func IsConditionFalse(conditions []nvsentinelv1alpha1.HealthEventCondition, conditionType nvsentinelv1alpha1.HealthEventConditionType) bool {
	condition := GetCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionFalse
}

// RemoveCondition removes the condition with the specified type.
func RemoveCondition(conditions *[]nvsentinelv1alpha1.HealthEventCondition, conditionType nvsentinelv1alpha1.HealthEventConditionType) {
	if conditions == nil {
		return
	}

	newConditions := make([]nvsentinelv1alpha1.HealthEventCondition, 0, len(*conditions))
	for _, c := range *conditions {
		if c.Type != conditionType {
			newConditions = append(newConditions, c)
		}
	}
	*conditions = newConditions
}

// InitializeConditions sets up the initial conditions for a new HealthEvent.
func InitializeConditions(healthEvent *nvsentinelv1alpha1.HealthEvent) {
	now := metav1.Now()

	// Set Detected condition
	SetCondition(&healthEvent.Status.Conditions, nvsentinelv1alpha1.HealthEventCondition{
		Type:               nvsentinelv1alpha1.ConditionDetected,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "EventCreated",
		Message:            "Health event detected and recorded",
		ObservedGeneration: healthEvent.Generation,
	})

	// Initialize other conditions as Unknown
	for _, condType := range []nvsentinelv1alpha1.HealthEventConditionType{
		nvsentinelv1alpha1.ConditionNodeQuarantined,
		nvsentinelv1alpha1.ConditionPodsDrained,
		nvsentinelv1alpha1.ConditionRemediated,
		nvsentinelv1alpha1.ConditionResolved,
	} {
		SetCondition(&healthEvent.Status.Conditions, nvsentinelv1alpha1.HealthEventCondition{
			Type:               condType,
			Status:             metav1.ConditionUnknown,
			LastTransitionTime: now,
			Reason:             "Pending",
			Message:            "Waiting for processing",
			ObservedGeneration: healthEvent.Generation,
		})
	}
}
