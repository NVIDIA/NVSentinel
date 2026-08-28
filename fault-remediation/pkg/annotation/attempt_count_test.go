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

package annotation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newAttemptCountTestManager(t *testing.T, nodeName string) NodeAnnotationManager {
	t.Helper()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        nodeName,
			Annotations: map[string]string{},
		},
	}

	return NodeAnnotationManager{client: fake.NewClientBuilder().WithObjects(node).Build()}
}

// TestAttemptCountSurvivesGroupRemoval is the annotation-level contract the attempt cap rests
// on. Groups are removed exactly when their CR failed or vanished, which is the loop the cap
// must stop; if removal also dropped the counter the budget would reset on every failure and
// the cap could never be reached.
func TestAttemptCountSurvivesGroupRemoval(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"
	groupName := "restart"

	m := newAttemptCountTestManager(t, nodeName)

	attempts, err := m.RecordRemediationAttempt(ctx, nodeName, groupName)
	require.NoError(t, err)
	assert.Equal(t, 1, attempts, "first attempt should be counted as 1")

	require.NoError(t, m.UpdateRemediationState(ctx, nodeName, groupName, "cr-1", "RESTART_BM"))

	state, _, err := m.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Equal(t, 1, state.EquivalenceGroups[groupName].AttemptCount,
		"recording the CR must not disturb the attempt count")
	assert.Equal(t, "cr-1", state.EquivalenceGroups[groupName].MaintenanceCR)

	// The CR failed: the reconciler clears the group so a new remediation may start.
	require.NoError(t, m.RemoveGroupsFromState(ctx, nodeName, []string{groupName}))

	state, _, err = m.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Equal(t, 1, state.EquivalenceGroups[groupName].AttemptCount,
		"attempt budget must outlive the failed CR, otherwise the cap never fires")
	assert.Empty(t, state.EquivalenceGroups[groupName].MaintenanceCR,
		"the CR reference must be dropped so a new remediation can be created")

	attempts, err = m.RecordRemediationAttempt(ctx, nodeName, groupName)
	require.NoError(t, err)
	assert.Equal(t, 2, attempts, "the next attempt must continue the count, not restart it")
}

// TestAttemptCountIsPerGroupAndClearedWithSession checks the two remaining boundaries: one
// group's budget cannot consume another's, and ending the quarantine session resets it.
func TestAttemptCountIsPerGroupAndClearedWithSession(t *testing.T) {
	ctx := context.Background()
	nodeName := "test-node"

	m := newAttemptCountTestManager(t, nodeName)

	_, err := m.RecordRemediationAttempt(ctx, nodeName, "restart")
	require.NoError(t, err)
	_, err = m.RecordRemediationAttempt(ctx, nodeName, "restart")
	require.NoError(t, err)
	_, err = m.RecordRemediationAttempt(ctx, nodeName, "terminate")
	require.NoError(t, err)

	state, _, err := m.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Equal(t, 2, state.EquivalenceGroups["restart"].AttemptCount)
	assert.Equal(t, 1, state.EquivalenceGroups["terminate"].AttemptCount)

	// A cancellation ends the session and must hand the node a fresh budget.
	require.NoError(t, m.ClearRemediationState(ctx, nodeName))

	state, _, err = m.GetRemediationState(ctx, nodeName)
	require.NoError(t, err)
	assert.Empty(t, state.EquivalenceGroups, "ending the session must clear the attempt budget")
}
