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

package helpers

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"

	nvsentinelv1alpha1 "github.com/nvidia/nvsentinel/api/nvsentinel/v1alpha1"
)

// HealthEventCRDBuilder provides a fluent interface for building HealthEvent CRDs.
type HealthEventCRDBuilder struct {
	event *nvsentinelv1alpha1.HealthEvent
}

// NewHealthEventCRD creates a new HealthEvent CRD builder with sensible defaults.
func NewHealthEventCRD(nodeName string) *HealthEventCRDBuilder {
	return &HealthEventCRDBuilder{
		event: &nvsentinelv1alpha1.HealthEvent{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: fmt.Sprintf("test-%s-", nodeName),
			},
			Spec: nvsentinelv1alpha1.HealthEventSpec{
				Source:         "e2e-test",
				NodeName:       nodeName,
				ComponentClass: "GPU",
				CheckName:      "GpuXidError",
				IsFatal:        true,
				IsHealthy:      false,
				DetectedAt:     metav1.Now(),
			},
		},
	}
}

// WithName sets the exact name (instead of GenerateName).
func (b *HealthEventCRDBuilder) WithName(name string) *HealthEventCRDBuilder {
	b.event.ObjectMeta.Name = name
	b.event.ObjectMeta.GenerateName = ""
	return b
}

// WithSource sets the source of the health event.
func (b *HealthEventCRDBuilder) WithSource(source string) *HealthEventCRDBuilder {
	b.event.Spec.Source = source
	return b
}

// WithCheckName sets the check name.
func (b *HealthEventCRDBuilder) WithCheckName(checkName string) *HealthEventCRDBuilder {
	b.event.Spec.CheckName = checkName
	return b
}

// WithComponentClass sets the component class.
func (b *HealthEventCRDBuilder) WithComponentClass(class string) *HealthEventCRDBuilder {
	b.event.Spec.ComponentClass = class
	return b
}

// WithFatal sets the isFatal flag.
func (b *HealthEventCRDBuilder) WithFatal(isFatal bool) *HealthEventCRDBuilder {
	b.event.Spec.IsFatal = isFatal
	return b
}

// WithHealthy sets the isHealthy flag.
func (b *HealthEventCRDBuilder) WithHealthy(isHealthy bool) *HealthEventCRDBuilder {
	b.event.Spec.IsHealthy = isHealthy
	return b
}

// WithMessage sets the message.
func (b *HealthEventCRDBuilder) WithMessage(message string) *HealthEventCRDBuilder {
	b.event.Spec.Message = message
	return b
}

// WithRecommendedAction sets the recommended action.
func (b *HealthEventCRDBuilder) WithRecommendedAction(action nvsentinelv1alpha1.RecommendedAction) *HealthEventCRDBuilder {
	b.event.Spec.RecommendedAction = action
	return b
}

// WithErrorCodes sets the error codes.
func (b *HealthEventCRDBuilder) WithErrorCodes(codes ...string) *HealthEventCRDBuilder {
	b.event.Spec.ErrorCodes = codes
	return b
}

// WithEntity adds an entity to the entities impacted list.
func (b *HealthEventCRDBuilder) WithEntity(entityType, entityValue string) *HealthEventCRDBuilder {
	b.event.Spec.EntitiesImpacted = append(b.event.Spec.EntitiesImpacted, nvsentinelv1alpha1.Entity{
		Type:  entityType,
		Value: entityValue,
	})
	return b
}

// WithMetadata adds a metadata key-value pair.
func (b *HealthEventCRDBuilder) WithMetadata(key, value string) *HealthEventCRDBuilder {
	if b.event.Spec.Metadata == nil {
		b.event.Spec.Metadata = make(map[string]string)
	}
	b.event.Spec.Metadata[key] = value
	return b
}

// WithSkipQuarantine sets the quarantine skip override.
func (b *HealthEventCRDBuilder) WithSkipQuarantine(skip bool) *HealthEventCRDBuilder {
	if b.event.Spec.Overrides == nil {
		b.event.Spec.Overrides = &nvsentinelv1alpha1.BehaviourOverrides{}
	}
	if b.event.Spec.Overrides.Quarantine == nil {
		b.event.Spec.Overrides.Quarantine = &nvsentinelv1alpha1.ActionOverride{}
	}
	b.event.Spec.Overrides.Quarantine.Skip = skip
	return b
}

// WithSkipDrain sets the drain skip override.
func (b *HealthEventCRDBuilder) WithSkipDrain(skip bool) *HealthEventCRDBuilder {
	if b.event.Spec.Overrides == nil {
		b.event.Spec.Overrides = &nvsentinelv1alpha1.BehaviourOverrides{}
	}
	if b.event.Spec.Overrides.Drain == nil {
		b.event.Spec.Overrides.Drain = &nvsentinelv1alpha1.ActionOverride{}
	}
	b.event.Spec.Overrides.Drain.Skip = skip
	return b
}

// Build returns the constructed HealthEvent.
func (b *HealthEventCRDBuilder) Build() *nvsentinelv1alpha1.HealthEvent {
	return b.event
}

// =============================================================================
// CRD Operations
// =============================================================================

// CreateHealthEventCRD creates a HealthEvent CRD in the cluster.
func CreateHealthEventCRD(
	ctx context.Context, t *testing.T, c klient.Client, event *nvsentinelv1alpha1.HealthEvent,
) *nvsentinelv1alpha1.HealthEvent {
	t.Helper()
	t.Logf("Creating HealthEvent CRD for node %s: checkName=%s, isFatal=%v",
		event.Spec.NodeName, event.Spec.CheckName, event.Spec.IsFatal)

	err := c.Resources().Create(ctx, event)
	require.NoError(t, err, "failed to create HealthEvent CRD")

	t.Logf("Created HealthEvent CRD: %s", event.Name)
	return event
}

// GetHealthEventCRD retrieves a HealthEvent CRD by name.
func GetHealthEventCRD(
	ctx context.Context, c klient.Client, name string,
) (*nvsentinelv1alpha1.HealthEvent, error) {
	event := &nvsentinelv1alpha1.HealthEvent{}
	err := c.Resources().Get(ctx, name, "", event)
	if err != nil {
		return nil, fmt.Errorf("failed to get HealthEvent %s: %w", name, err)
	}
	return event, nil
}

// DeleteHealthEventCRD deletes a HealthEvent CRD by name.
func DeleteHealthEventCRD(ctx context.Context, t *testing.T, c klient.Client, name string) {
	t.Helper()
	t.Logf("Deleting HealthEvent CRD: %s", name)

	event := &nvsentinelv1alpha1.HealthEvent{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	err := c.Resources().Delete(ctx, event)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Logf("Warning: failed to delete HealthEvent %s: %v", name, err)
	}
}

// ListHealthEventCRDs lists all HealthEvent CRDs in the cluster.
func ListHealthEventCRDs(
	ctx context.Context, c klient.Client,
) (*nvsentinelv1alpha1.HealthEventList, error) {
	list := &nvsentinelv1alpha1.HealthEventList{}
	err := c.Resources().List(ctx, list)
	if err != nil {
		return nil, fmt.Errorf("failed to list HealthEvents: %w", err)
	}
	return list, nil
}

// ListHealthEventCRDsForNode lists all HealthEvent CRDs for a specific node.
func ListHealthEventCRDsForNode(
	ctx context.Context, c klient.Client, nodeName string,
) ([]nvsentinelv1alpha1.HealthEvent, error) {
	list, err := ListHealthEventCRDs(ctx, c)
	if err != nil {
		return nil, err
	}

	var result []nvsentinelv1alpha1.HealthEvent
	for _, event := range list.Items {
		if event.Spec.NodeName == nodeName {
			result = append(result, event)
		}
	}
	return result, nil
}

// DeleteAllHealthEventCRDs deletes all HealthEvent CRDs in the cluster.
func DeleteAllHealthEventCRDs(ctx context.Context, t *testing.T, c klient.Client) {
	t.Helper()
	t.Log("Deleting all HealthEvent CRDs")

	list, err := ListHealthEventCRDs(ctx, c)
	if err != nil {
		t.Logf("Warning: failed to list HealthEvents: %v", err)
		return
	}

	for _, event := range list.Items {
		DeleteHealthEventCRD(ctx, t, c, event.Name)
	}

	t.Logf("Deleted %d HealthEvent CRD(s)", len(list.Items))
}

// =============================================================================
// Phase Waiting
// =============================================================================

// WaitForHealthEventPhase waits for a HealthEvent to reach the specified phase.
func WaitForHealthEventPhase(
	ctx context.Context, t *testing.T, c klient.Client, name string, expectedPhase nvsentinelv1alpha1.HealthEventPhase,
) *nvsentinelv1alpha1.HealthEvent {
	t.Helper()
	t.Logf("Waiting for HealthEvent %s to reach phase %s", name, expectedPhase)

	var result *nvsentinelv1alpha1.HealthEvent

	require.Eventually(t, func() bool {
		event, err := GetHealthEventCRD(ctx, c, name)
		if err != nil {
			t.Logf("Failed to get HealthEvent %s: %v", name, err)
			return false
		}

		currentPhase := event.Status.Phase
		// Handle empty phase as "New" (status not yet set)
		if currentPhase == "" {
			currentPhase = nvsentinelv1alpha1.PhaseNew
		}

		t.Logf("HealthEvent %s: current phase=%s, expected=%s", name, currentPhase, expectedPhase)

		if currentPhase == expectedPhase {
			result = event
			return true
		}
		return false
	}, EventuallyWaitTimeout, WaitInterval,
		"HealthEvent %s should reach phase %s", name, expectedPhase)

	return result
}

// WaitForHealthEventPhaseNotEqual waits for a HealthEvent to NOT be in the specified phase.
func WaitForHealthEventPhaseNotEqual(
	ctx context.Context, t *testing.T, c klient.Client, name string, notExpectedPhase nvsentinelv1alpha1.HealthEventPhase,
) *nvsentinelv1alpha1.HealthEvent {
	t.Helper()
	t.Logf("Waiting for HealthEvent %s to leave phase %s", name, notExpectedPhase)

	var result *nvsentinelv1alpha1.HealthEvent

	require.Eventually(t, func() bool {
		event, err := GetHealthEventCRD(ctx, c, name)
		if err != nil {
			t.Logf("Failed to get HealthEvent %s: %v", name, err)
			return false
		}

		currentPhase := event.Status.Phase
		if currentPhase == "" {
			currentPhase = nvsentinelv1alpha1.PhaseNew
		}

		if currentPhase != notExpectedPhase {
			t.Logf("HealthEvent %s: phase changed from %s to %s", name, notExpectedPhase, currentPhase)
			result = event
			return true
		}
		return false
	}, EventuallyWaitTimeout, WaitInterval,
		"HealthEvent %s should leave phase %s", name, notExpectedPhase)

	return result
}

// WaitForHealthEventCondition waits for a HealthEvent to have a specific condition.
func WaitForHealthEventCondition(
	ctx context.Context, t *testing.T, c klient.Client, name string,
	conditionType nvsentinelv1alpha1.HealthEventConditionType, expectedStatus metav1.ConditionStatus,
) *nvsentinelv1alpha1.HealthEvent {
	t.Helper()
	t.Logf("Waiting for HealthEvent %s to have condition %s=%s", name, conditionType, expectedStatus)

	var result *nvsentinelv1alpha1.HealthEvent

	require.Eventually(t, func() bool {
		event, err := GetHealthEventCRD(ctx, c, name)
		if err != nil {
			t.Logf("Failed to get HealthEvent %s: %v", name, err)
			return false
		}

		for _, condition := range event.Status.Conditions {
			if condition.Type == conditionType && condition.Status == expectedStatus {
				t.Logf("HealthEvent %s has condition %s=%s", name, conditionType, expectedStatus)
				result = event
				return true
			}
		}

		t.Logf("HealthEvent %s: condition %s not yet %s", name, conditionType, expectedStatus)
		return false
	}, EventuallyWaitTimeout, WaitInterval,
		"HealthEvent %s should have condition %s=%s", name, conditionType, expectedStatus)

	return result
}

// =============================================================================
// Assertions
// =============================================================================

// AssertHealthEventPhase asserts that a HealthEvent is in the expected phase.
func AssertHealthEventPhase(
	t *testing.T, event *nvsentinelv1alpha1.HealthEvent, expectedPhase nvsentinelv1alpha1.HealthEventPhase,
) {
	t.Helper()

	actualPhase := event.Status.Phase
	if actualPhase == "" {
		actualPhase = nvsentinelv1alpha1.PhaseNew
	}

	require.Equal(t, expectedPhase, actualPhase,
		"HealthEvent %s should be in phase %s, but is in %s", event.Name, expectedPhase, actualPhase)
}

// AssertHealthEventNotExists asserts that a HealthEvent does not exist.
func AssertHealthEventNotExists(ctx context.Context, t *testing.T, c klient.Client, name string) {
	t.Helper()

	_, err := GetHealthEventCRD(ctx, c, name)
	require.True(t, apierrors.IsNotFound(err),
		"HealthEvent %s should not exist, but it does", name)
}

// AssertHealthEventHasCondition asserts that a HealthEvent has a specific condition.
func AssertHealthEventHasCondition(
	t *testing.T, event *nvsentinelv1alpha1.HealthEvent,
	conditionType nvsentinelv1alpha1.HealthEventConditionType, expectedStatus metav1.ConditionStatus,
) {
	t.Helper()

	for _, condition := range event.Status.Conditions {
		if condition.Type == conditionType {
			require.Equal(t, expectedStatus, condition.Status,
				"HealthEvent %s condition %s should be %s, but is %s",
				event.Name, conditionType, expectedStatus, condition.Status)
			return
		}
	}

	t.Fatalf("HealthEvent %s does not have condition %s", event.Name, conditionType)
}

// AssertHealthEventNeverReachesPhase asserts that a HealthEvent never reaches a specific phase.
func AssertHealthEventNeverReachesPhase(
	ctx context.Context, t *testing.T, c klient.Client, name string, phase nvsentinelv1alpha1.HealthEventPhase,
) {
	t.Helper()
	t.Logf("Asserting HealthEvent %s never reaches phase %s", name, phase)

	require.Never(t, func() bool {
		event, err := GetHealthEventCRD(ctx, c, name)
		if err != nil {
			return false
		}

		currentPhase := event.Status.Phase
		if currentPhase == "" {
			currentPhase = nvsentinelv1alpha1.PhaseNew
		}

		if currentPhase == phase {
			t.Logf("ERROR: HealthEvent %s reached phase %s (should not happen)", name, phase)
			return true
		}
		return false
	}, NeverWaitTimeout, WaitInterval,
		"HealthEvent %s should never reach phase %s", name, phase)
}

// =============================================================================
// Phase Sequence Tracking
// =============================================================================

// ExpectedPhaseSequence defines an expected sequence of phase transitions.
type ExpectedPhaseSequence []nvsentinelv1alpha1.HealthEventPhase

// WaitForHealthEventPhaseSequence waits for a HealthEvent to progress through a sequence of phases.
// Returns the final HealthEvent once all phases have been observed.
func WaitForHealthEventPhaseSequence(
	ctx context.Context, t *testing.T, c klient.Client, name string, sequence ExpectedPhaseSequence,
) *nvsentinelv1alpha1.HealthEvent {
	t.Helper()
	t.Logf("Waiting for HealthEvent %s to progress through phases: %v", name, sequence)

	var result *nvsentinelv1alpha1.HealthEvent

	for i, expectedPhase := range sequence {
		t.Logf("Waiting for phase %d/%d: %s", i+1, len(sequence), expectedPhase)
		result = WaitForHealthEventPhase(ctx, t, c, name, expectedPhase)
		t.Logf("✓ Phase %d/%d reached: %s", i+1, len(sequence), expectedPhase)
	}

	return result
}

// =============================================================================
// Cleanup Helpers
// =============================================================================

// CleanupHealthEventAndNode cleans up a HealthEvent and uncordons the associated node.
func CleanupHealthEventAndNode(ctx context.Context, t *testing.T, c klient.Client, eventName, nodeName string) {
	t.Helper()
	t.Logf("Cleaning up HealthEvent %s and node %s", eventName, nodeName)

	// Delete the HealthEvent
	DeleteHealthEventCRD(ctx, t, c, eventName)

	// Uncordon the node
	node, err := GetNodeByName(ctx, c, nodeName)
	if err != nil {
		t.Logf("Warning: failed to get node %s: %v", nodeName, err)
		return
	}

	if node.Spec.Unschedulable {
		node.Spec.Unschedulable = false
		err = c.Resources().Update(ctx, node)
		if err != nil {
			t.Logf("Warning: failed to uncordon node %s: %v", nodeName, err)
		} else {
			t.Logf("Uncordoned node %s", nodeName)
		}
	}
}

// =============================================================================
// Condition Assertions
// =============================================================================

// AssertNodeQuarantinedCondition asserts that the NodeQuarantined condition is set.
func AssertNodeQuarantinedCondition(t *testing.T, event *nvsentinelv1alpha1.HealthEvent) {
	t.Helper()
	AssertHealthEventHasCondition(t, event, nvsentinelv1alpha1.ConditionNodeQuarantined, metav1.ConditionTrue)
}

// AssertPodsDrainedCondition asserts that the PodsDrained condition is set.
func AssertPodsDrainedCondition(t *testing.T, event *nvsentinelv1alpha1.HealthEvent) {
	t.Helper()
	AssertHealthEventHasCondition(t, event, nvsentinelv1alpha1.ConditionPodsDrained, metav1.ConditionTrue)
}

// AssertRemediatedCondition asserts that the Remediated condition is set.
func AssertRemediatedCondition(t *testing.T, event *nvsentinelv1alpha1.HealthEvent) {
	t.Helper()
	AssertHealthEventHasCondition(t, event, nvsentinelv1alpha1.ConditionRemediated, metav1.ConditionTrue)
}

// AssertResolvedAtSet asserts that the resolvedAt timestamp is set.
func AssertResolvedAtSet(t *testing.T, event *nvsentinelv1alpha1.HealthEvent) {
	t.Helper()
	require.NotNil(t, event.Status.ResolvedAt,
		"HealthEvent %s should have resolvedAt set", event.Name)
	require.False(t, event.Status.ResolvedAt.IsZero(),
		"HealthEvent %s should have non-zero resolvedAt", event.Name)
}

// GetConditionLastTransitionTime returns the last transition time for a condition.
func GetConditionLastTransitionTime(
	event *nvsentinelv1alpha1.HealthEvent, conditionType nvsentinelv1alpha1.HealthEventConditionType,
) *metav1.Time {
	for _, condition := range event.Status.Conditions {
		if condition.Type == conditionType {
			return &condition.LastTransitionTime
		}
	}
	return nil
}

// =============================================================================
// Integration with Old Test Patterns
// =============================================================================

// SendHealthEventViaCRD creates a HealthEvent CRD equivalent to the old SendHealthEvent helper.
// This provides a migration path from HTTP-based event sending to CRD-based.
func SendHealthEventViaCRD(
	ctx context.Context, t *testing.T, c klient.Client, template *HealthEventTemplate,
) *nvsentinelv1alpha1.HealthEvent {
	t.Helper()

	builder := NewHealthEventCRD(template.NodeName).
		WithSource(template.Agent).
		WithCheckName(template.CheckName).
		WithComponentClass(template.ComponentClass).
		WithFatal(template.IsFatal).
		WithHealthy(template.IsHealthy).
		WithMessage(template.Message)

	// Add error codes
	if len(template.ErrorCode) > 0 {
		builder.WithErrorCodes(template.ErrorCode...)
	}

	// Add entities
	for _, entity := range template.EntitiesImpacted {
		builder.WithEntity(entity.EntityType, entity.EntityValue)
	}

	// Add metadata
	for k, v := range template.Metadata {
		builder.WithMetadata(k, v)
	}

	event := builder.Build()
	return CreateHealthEventCRD(ctx, t, c, event)
}

// SendHealthyEventViaCRD creates a healthy HealthEvent CRD equivalent to the old SendHealthyEvent helper.
func SendHealthyEventViaCRD(ctx context.Context, t *testing.T, c klient.Client, nodeName string) *nvsentinelv1alpha1.HealthEvent {
	t.Helper()
	t.Logf("Creating healthy HealthEvent CRD for node %s", nodeName)

	event := NewHealthEventCRD(nodeName).
		WithHealthy(true).
		WithFatal(false).
		WithMessage("No health failures").
		Build()

	return CreateHealthEventCRD(ctx, t, c, event)
}

// TriggerFullRemediationFlowViaCRD creates a fatal HealthEvent and waits for quarantine.
// This is the CRD equivalent of TriggerFullRemediationFlow.
func TriggerFullRemediationFlowViaCRD(
	ctx context.Context, t *testing.T, c klient.Client, nodeName string,
) *nvsentinelv1alpha1.HealthEvent {
	t.Helper()
	t.Logf("Triggering full remediation flow via CRD for node %s", nodeName)

	event := NewHealthEventCRD(nodeName).
		WithFatal(true).
		WithCheckName("GpuXidError").
		WithErrorCodes("79").
		WithRecommendedAction(nvsentinelv1alpha1.ActionRestartVM).
		Build()

	created := CreateHealthEventCRD(ctx, t, c, event)

	// Wait for quarantine
	WaitForHealthEventPhase(ctx, t, c, created.Name, nvsentinelv1alpha1.PhaseQuarantined)
	WaitForNodesCordonState(ctx, t, c, []string{nodeName}, true)

	return created
}
