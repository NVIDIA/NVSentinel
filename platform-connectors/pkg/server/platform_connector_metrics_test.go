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

package server

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func receivedCount(strategy, healthy string) float64 {
	return testutil.ToFloat64(healthEventsReceived.WithLabelValues(strategy, healthy))
}

func TestHealthEventOccurredV1_CountsReceivedByStrategyAndHealth(t *testing.T) {
	baseExecUnhealthy := receivedCount("EXECUTE_REMEDIATION", "false")
	baseStoreHealthy := receivedCount("STORE_ONLY", "true")

	server := &PlatformConnectorServer{}

	healthEvents := &pb.HealthEvents{
		Events: []*pb.HealthEvent{
			{
				NodeName:           "test-node-1",
				CheckName:          "test-check",
				ProcessingStrategy: pb.ProcessingStrategy_UNSPECIFIED,
				IsHealthy:          false,
			},
			{
				NodeName:           "test-node-2",
				CheckName:          "test-check",
				ProcessingStrategy: pb.ProcessingStrategy_STORE_ONLY,
				IsHealthy:          true,
			},
		},
	}

	_, err := server.HealthEventOccurredV1(context.Background(), healthEvents)
	require.NoError(t, err)

	// Events are counted after defaulting, so UNSPECIFIED lands under the
	// EXECUTE_REMEDIATION label it was normalized to.
	assert.Equal(t, baseExecUnhealthy+1, receivedCount("EXECUTE_REMEDIATION", "false"))
	assert.Equal(t, baseStoreHealthy+1, receivedCount("STORE_ONLY", "true"))
}

func TestHealthEventOccurredV1_RejectedBatchIsNotCounted(t *testing.T) {
	baseExecUnhealthy := receivedCount("EXECUTE_REMEDIATION", "false")

	server := &PlatformConnectorServer{}

	// CUSTOM without a custom action string fails validation, so the whole
	// batch is rejected before anything is counted.
	healthEvents := &pb.HealthEvents{
		Events: []*pb.HealthEvent{
			{
				NodeName:          "test-node",
				CheckName:         "test-check",
				RecommendedAction: pb.RecommendedAction_CUSTOM,
				IsHealthy:         false,
			},
		},
	}

	_, err := server.HealthEventOccurredV1(context.Background(), healthEvents)
	require.Error(t, err)

	assert.Equal(t, baseExecUnhealthy, receivedCount("EXECUTE_REMEDIATION", "false"))
}
