//go:build amd64_group
// +build amd64_group

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

package tests

import (
	"context"
	"testing"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const (
	truncatedEntityRegressionCheckName = "PodStuckAfterDeletionRegression"
	truncatedEntityRegressionErrorCode = "POD_STUCK_AFTER_DELETION"
	truncatedEntityRegressionType      = "v1/Pod"
	truncatedEntityRegressionValue     = "prod/61f345d08c9a432a-134a464884734f90"
)

// TestPlatformConnectorClearsCompactedLongEntityCondition verifies that platform-connector
// can clear node conditions written by the old compaction behavior. The test seeds a
// node condition directly with a truncated entity token:
//
//	v1/Pod:prod/61f345d08c9a432a-134...
//
// Then it sends a healthy event with the full entity value. Without truncation-aware
// matching, the connector searches for the full token, does not find it in the
// stored condition message, and leaves the condition stuck as unhealthy.
func TestPlatformConnectorClearsCompactedLongEntityCondition(t *testing.T) {
	feature := features.New("Platform Connector - Clears Truncated Long Entity Condition").
		WithLabel("suite", "platform-connector").
		WithLabel("component", "node-condition")

	var nodeName string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName = helpers.AcquireNodeFromPool(ctx, t, client, helpers.DefaultExpiry)
		sendTruncatedEntityClearEvent(ctx, t, nodeName)

		return ctx
	})

	feature.Assess("Healthy event clears a pre-existing condition whose entity was truncated", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		helpers.UpsertNodeCondition(ctx, t, client, nodeName, v1.NodeCondition{
			Type:    v1.NodeConditionType(truncatedEntityRegressionCheckName),
			Status:  v1.ConditionTrue,
			Reason:  truncatedEntityRegressionCheckName + "IsNotHealthy",
			Message: "ErrorCode:POD_STUCK_AFTER_DELETION v1/Pod:prod/61f345d08c9a432a-134... Recommended Action=NONE;",
		})

		healthyEvent := helpers.NewHealthEvent(nodeName).
			WithAgent("platform-connector-regression").
			WithComponentClass("Pod").
			WithCheckName(truncatedEntityRegressionCheckName).
			WithFatal(false).
			WithHealthy(true).
			WithErrorCode(truncatedEntityRegressionErrorCode).
			WithEntitiesImpacted([]helpers.EntityImpacted{{
				EntityType:  truncatedEntityRegressionType,
				EntityValue: truncatedEntityRegressionValue,
			}}).
			WithMessage("pod recovered").
			WithRecommendedAction(int(pb.RecommendedAction_NONE))

		helpers.SendHealthEvent(ctx, t, healthyEvent)

		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
			truncatedEntityRegressionCheckName,
			"No Health Failures",
			truncatedEntityRegressionCheckName+"IsHealthy",
			v1.ConditionFalse)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if nodeName != "" {
			sendTruncatedEntityClearEvent(ctx, t, nodeName)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func sendTruncatedEntityClearEvent(ctx context.Context, t *testing.T, nodeName string) {
	t.Helper()

	clearEvent := helpers.NewHealthEvent(nodeName).
		WithAgent("platform-connector-regression").
		WithComponentClass("Pod").
		WithCheckName(truncatedEntityRegressionCheckName).
		WithFatal(false).
		WithHealthy(true).
		WithErrorCode(truncatedEntityRegressionErrorCode).
		WithEntitiesImpacted(nil).
		WithMessage("No health failures").
		WithRecommendedAction(int(pb.RecommendedAction_NONE))

	helpers.SendHealthEvent(ctx, t, clearEvent)
}
