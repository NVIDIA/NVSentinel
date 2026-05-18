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
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/klient"
)

// UpsertNodeCondition adds or replaces a node condition through the status subresource.
func UpsertNodeCondition(
	ctx context.Context,
	t *testing.T,
	client klient.Client,
	nodeName string,
	condition v1.NodeCondition,
) {
	t.Helper()

	require.Eventually(t, func() bool {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			node, nodeErr := GetNodeByName(ctx, client, nodeName)
			if nodeErr != nil {
				return nodeErr
			}

			now := metav1.Now()
			if condition.LastHeartbeatTime.IsZero() {
				condition.LastHeartbeatTime = now
			}

			if condition.LastTransitionTime.IsZero() {
				condition.LastTransitionTime = now
			}

			replaced := false

			for i := range node.Status.Conditions {
				if node.Status.Conditions[i].Type == condition.Type {
					node.Status.Conditions[i] = condition
					replaced = true

					break
				}
			}

			if !replaced {
				node.Status.Conditions = append(node.Status.Conditions, condition)
			}

			return client.Resources().UpdateStatus(ctx, node)
		})
		if err != nil {
			t.Logf("Failed to upsert node condition: %v", err)

			return false
		}

		return true
	}, EventuallyWaitTimeout, WaitInterval, "failed to upsert node condition")
}
