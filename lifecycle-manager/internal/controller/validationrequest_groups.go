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
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
)

const (
	groupNameSuffix = "-group-"
	// maxGroupBaseLength limits the base portion of a group name, reserving room for the groupNameSuffix,
	// up to a 4-digit index, and the "-<hash>" suffix appended when truncated. This ensures the final name never
	// exceeds the Kubernetes DNS-1123 label length limit.
	maxGroupBaseLength = validation.DNS1123LabelMaxLength - len(groupNameSuffix) - 4 - 1 - 8
)

type groupTestGroupIdentifier struct {
	provider string
	nodeSet  string
	test     string
}

func buildInitialTestGroups(validationRequest *v1alpha1.ValidationRequest, cfg *config.Config,
	existingNodes map[string]bool) (groups []v1alpha1.TestGroupStatus, failedGroupsPresent bool, skippedTests []string) {
	tests := resolveValidationRequestTests(validationRequest, cfg)

	nodes := make([]string, 0, len(existingNodes))
	for n := range existingNodes {
		nodes = append(nodes, n)
	}

	sort.Strings(nodes)

	now := metav1.Now()

	testGroupIdentifierToGroup := map[groupTestGroupIdentifier]*v1alpha1.TestGroupStatus{}

	var testGroupOrdering []groupTestGroupIdentifier

	for _, testName := range tests {
		currentTestConfig := cfg.Validation.Spec.Tests[testName]

		if len(nodes) < currentTestConfig.MinimumNodesPerBatch {
			failedGroup, skipped := batchMinimumNotMetForInitialTestGroup(testName, currentTestConfig, nodes, now)
			if skipped {
				skippedTests = append(skippedTests, testName)
			} else {
				groups = append(groups, *failedGroup)
				failedGroupsPresent = true
			}
		} else {
			currentTestProviderConfig := cfg.Validation.Spec.Providers[currentTestConfig.Provider]
			testGroupOrdering = addTestToGroups(testGroupIdentifierToGroup, testGroupOrdering, testName,
				currentTestConfig, currentTestProviderConfig, nodes,
			)
		}
	}

	uniqueTestSetCount := map[string]int{}

	// Each test group is uniquely identified by the tests and nodes it covers. We could construct test group names by
	// combining the tests and all node names. However, to prevent arbitrarily long names from a large count of node names,
	// we'll only use the test names and include the index of each group covering the same tests as part of each name
	for _, groupIdentifier := range testGroupOrdering {
		currentTestGroup := testGroupIdentifierToGroup[groupIdentifier]
		testSetStr := strings.Join(currentTestGroup.Tests, ",")
		uniqueTestSetCount[testSetStr]++
		currentTestGroup.Name = groupName(currentTestGroup.Tests, uniqueTestSetCount[testSetStr])
		groups = append(groups, *currentTestGroup)
	}

	return groups, failedGroupsPresent, skippedTests
}

func resolveValidationRequestTests(validationRequest *v1alpha1.ValidationRequest, cfg *config.Config) []string {
	if len(validationRequest.Spec.Tests) > 0 {
		return validationRequest.Spec.Tests
	}

	return cfg.Validation.Spec.DefaultTests
}

func batchMinimumNotMetForInitialTestGroup(testName string, testCfg v1alpha1.TestConfig, nodes []string,
	now metav1.Time) (failedGroup *v1alpha1.TestGroupStatus, skipped bool) {
	if testCfg.BatchFailurePolicy == v1alpha1.BatchFailurePolicyIgnore {
		return nil, true
	}

	return &v1alpha1.TestGroupStatus{
		Name:     groupName([]string{testName}, 1),
		Provider: testCfg.Provider,
		Tests:    []string{testName},
		Nodes:    nodes,
		Phase:    v1alpha1.PhaseFailed,
		Attempts: []v1alpha1.AttemptStatus{{
			Phase:         v1alpha1.PhaseFailed,
			FailureReason: v1alpha1.FailureReasonBatchMinimumNotMet,
			EndTime:       &now,
		}},
	}, false
}

func addTestToGroups(testGroupIdentifierToGroup map[groupTestGroupIdentifier]*v1alpha1.TestGroupStatus,
	testGroupOrdering []groupTestGroupIdentifier, testName string, testCfg v1alpha1.TestConfig,
	providerCfg v1alpha1.ProviderConfig, nodes []string) []groupTestGroupIdentifier {
	// If the given test doesn't support batching, we'll need 1 TestGroup per node, otherwise we
	// can batch all nodes into a single TestGroup.
	var nodeSets [][]string
	if testCfg.SupportsBatchingNodes {
		nodeSets = [][]string{nodes}
	} else {
		for _, n := range nodes {
			nodeSets = append(nodeSets, []string{n})
		}
	}

	// If the test provider supports batching, we'll group tests together into the same TestGroup
	// if they target the same set of nodes. If the test provider does not support batching, we'll
	// create a separate test group per test (even if they have a matching set of nodes).
	for _, groupNodes := range nodeSets {
		testGroupIdentifier := groupTestGroupIdentifier{
			provider: testCfg.Provider,
			nodeSet:  strings.Join(groupNodes, ","),
		}
		if !providerCfg.SupportsTestBatching {
			testGroupIdentifier.test = testName
		}

		currentTestGroup, ok := testGroupIdentifierToGroup[testGroupIdentifier]
		if !ok {
			currentTestGroup = &v1alpha1.TestGroupStatus{
				Provider: testCfg.Provider,
				Nodes:    groupNodes,
				Phase:    v1alpha1.PhasePending,
			}
			testGroupIdentifierToGroup[testGroupIdentifier] = currentTestGroup
			testGroupOrdering = append(testGroupOrdering, testGroupIdentifier)
		}

		currentTestGroup.Tests = append(currentTestGroup.Tests, testName)
	}

	return testGroupOrdering
}

func buildSkippedStatus(existing *v1alpha1.SkippedStatus, nodes, tests []string) *v1alpha1.SkippedStatus {
	skipped := appendSkipped(existing, nodes, false)
	return appendSkipped(skipped, tests, true)
}

func newTestGroupPhaseAfterAttempt(numAttempts, maxRetries int) v1alpha1.Phase {
	if numAttempts <= maxRetries {
		return v1alpha1.PhasePending
	}

	return v1alpha1.PhaseFailed
}

func removeDeletedNodesFromTestGroup(validationRequest *v1alpha1.ValidationRequest, g *v1alpha1.TestGroupStatus,
	deletedNodes []string, cfg *config.Config) v1alpha1.Phase {
	remainingNodes := removeFromSlice(g.Nodes, deletedNodes)
	skipped := appendSkipped(validationRequest.Status.Skipped, deletedNodes, false)

	var remainingTests []string

	batchFailed := false

	for i := 0; i < len(g.Tests) && !batchFailed; i++ {
		testName := g.Tests[i]

		testCfg := cfg.Validation.Spec.Tests[testName]
		if len(remainingNodes) < testCfg.MinimumNodesPerBatch {
			if testCfg.BatchFailurePolicy == v1alpha1.BatchFailurePolicyFail {
				batchFailed = true
			} else {
				skipped = appendSkipped(skipped, []string{testName}, true)
			}
		} else {
			remainingTests = append(remainingTests, testName)
		}
	}

	var nextPhase v1alpha1.Phase

	switch {
	case batchFailed:
		remainingTests = nil
		nextPhase = v1alpha1.PhaseFailed
	case len(remainingTests) == 0:
		nextPhase = v1alpha1.PhaseSucceeded
	default:
		nextPhase = newTestGroupPhaseAfterAttempt(len(g.Attempts), cfg.Validation.Spec.Providers[g.Provider].Retries)
	}

	g.Nodes = remainingNodes
	g.Phase = nextPhase
	g.Tests = remainingTests
	validationRequest.Status.Skipped = skipped

	return nextPhase
}

func allNodesSkipped(vr *v1alpha1.ValidationRequest) bool {
	if len(vr.Spec.Nodes) == 0 || vr.Status.Skipped == nil {
		return false
	}

	skipped := make(map[string]bool, len(vr.Status.Skipped.Nodes))
	for _, n := range vr.Status.Skipped.Nodes {
		skipped[n] = true
	}

	for _, ns := range vr.Spec.Nodes {
		if !skipped[ns.Name] {
			return false
		}
	}

	return true
}

func appendSkipped(skipped *v1alpha1.SkippedStatus, items []string, isTests bool) *v1alpha1.SkippedStatus {
	if len(items) == 0 {
		return skipped
	}

	if skipped == nil {
		skipped = &v1alpha1.SkippedStatus{}
	}

	existing := skipped.Nodes
	if isTests {
		existing = skipped.Tests
	}

	seen := make(map[string]bool, len(existing))
	for _, s := range existing {
		seen[s] = true
	}

	for _, s := range items {
		if !seen[s] {
			existing = append(existing, s)
			seen[s] = true
		}
	}

	if isTests {
		skipped.Tests = existing
	} else {
		skipped.Nodes = existing
	}

	return skipped
}

func removeFromSlice(items, toRemove []string) []string {
	remove := make(map[string]bool, len(toRemove))
	for _, s := range toRemove {
		remove[s] = true
	}

	var result []string

	for _, s := range items {
		if !remove[s] {
			result = append(result, s)
		}
	}

	return result
}

func groupName(tests []string, index int) string {
	sorted := append([]string{}, tests...)
	sort.Strings(sorted)

	base := strings.Join(sorted, "-")

	if len(base) > maxGroupBaseLength {
		h := fnv.New32a()
		h.Write([]byte(base))
		base = fmt.Sprintf("%s-%08x", base[:maxGroupBaseLength], h.Sum32())
	}

	return fmt.Sprintf("%s%s%d", base, groupNameSuffix, index)
}
