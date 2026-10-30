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

package helpers

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
)

const (
	NodeUsedByAnnotation   = "nvsentinel.nvidia.com/test-used-by"
	NodeUsedFromAnnotation = "nvsentinel.nvidia.com/test-used-from"
	DefaultExpiry          = 10 * time.Minute
)

var (
	nodePoolMutex sync.Mutex
)

// AcquireNodeFromPool selects an available node from the cluster pool.
// An available node is one that is either:
// 1. Not labeled as used by a test.
// 2. Labeled as used, but its expiry timestamp has passed.
// If no node is immediately available, it waits until one becomes free or expires.
func AcquireNodeFromPool(ctx context.Context, t *testing.T, client klient.Client, expiryDuration time.Duration) string {
	t.Helper()

	nodePoolMutex.Lock()
	defer nodePoolMutex.Unlock()

	var selectedNodeName string

	require.Eventually(t, func() bool {
		nodes, err := GetAllNodesNames(ctx, client)
		if err != nil {
			t.Logf("failed to get nodes from cluster: %v", err)
			return false
		}

		if len(nodes) == 0 {
			t.Log("no nodes found in cluster")
			return false
		}

		for _, nodeName := range nodes {
			if isNodeAvailable(ctx, t, client, nodeName, expiryDuration) {
				selectedNodeName = nodeName
				return true
			}
		}

		t.Logf(
			"No available nodes found, waiting for one to expire or be released. Current time: %s",
			time.Now().Format(time.RFC3339),
		)

		return false
	}, EventuallyWaitTimeout, WaitInterval, "timed out waiting to acquire a node from the pool")

	require.NotEmpty(t, selectedNodeName, "failed to acquire a node")

	err := annotateNodeAsUsed(ctx, client, selectedNodeName, t.Name())
	require.NoError(t, err, "failed to annotate node %s as used", selectedNodeName)

	t.Logf("Acquired node '%s' for test '%s'", selectedNodeName, t.Name())

	return selectedNodeName
}

// isNodeAvailable checks if a node is currently available for use by a test.
// A node is available if it does not have the NodeUsedByAnnotation, or if the annotation
// indicates it was used by an expired test.
func isNodeAvailable(
	ctx context.Context,
	t *testing.T,
	client klient.Client,
	nodeName string,
	expiryDuration time.Duration,
) bool {
	node, err := GetNodeByName(ctx, client, nodeName)
	if err != nil {
		return false
	}

	if node.Spec.Unschedulable {
		return false
	}

	usedBy, hasUsedBy := node.Annotations[NodeUsedByAnnotation]
	usedFromStr, hasUsedFrom := node.Annotations[NodeUsedFromAnnotation]

	if !hasUsedBy || !hasUsedFrom {
		return true
	}

	var usedFromUnix int64

	_, parseErr := fmt.Sscanf(usedFromStr, "%d", &usedFromUnix)
	if parseErr != nil {
		t.Logf(
			"Warning: Malformed timestamp for node '%s' annotation '%s': %v. Treating as expired.",
			nodeName,
			NodeUsedFromAnnotation,
			parseErr,
		)

		return true
	}

	usedFrom := time.Unix(usedFromUnix, 0)
	if time.Since(usedFrom) > expiryDuration {
		t.Logf(
			"Node '%s' previously used by '%s' has expired (used %s ago). Releasing.",
			nodeName,
			usedBy,
			time.Since(usedFrom).Round(time.Second),
		)

		return true
	}

	return false
}

// annotateNodeAsUsed applies the usage annotations to a node.
func annotateNodeAsUsed(ctx context.Context, client klient.Client, nodeName, testName string) error {
	return retry.RetryOnConflict(ConflictBackoff, func() error {
		node, err := GetNodeByName(ctx, client, nodeName)
		if err != nil {
			return err
		}

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		node.Annotations[NodeUsedByAnnotation] = testName
		node.Annotations[NodeUsedFromAnnotation] = fmt.Sprintf("%d", time.Now().Unix())

		return client.Resources().Update(ctx, node)
	})
}
