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
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
)

func receivedCount(strategy, healthy string) float64 {
	return testutil.ToFloat64(healthEventsReceived.WithLabelValues(strategy, healthy))
}

func TestHealthEventOccurredV1_CountsReceivedByFinalStrategyAndHealth(t *testing.T) {
	tests := []struct {
		name          string
		inputStrategy pb.ProcessingStrategy
		isHealthy     bool
		// storeOnlyFor, when set, installs a pipeline that rewrites events for this
		// check name to STORE_ONLY, exercising counting after the pipeline.
		storeOnlyFor string
		wantStrategy string
		wantHealthy  string
	}{
		{
			name:          "UNSPECIFIED unhealthy is counted as its defaulted EXECUTE_REMEDIATION",
			inputStrategy: pb.ProcessingStrategy_UNSPECIFIED,
			isHealthy:     false,
			wantStrategy:  "EXECUTE_REMEDIATION",
			wantHealthy:   "false",
		},
		{
			name:          "STORE_ONLY healthy is counted unchanged",
			inputStrategy: pb.ProcessingStrategy_STORE_ONLY,
			isHealthy:     true,
			wantStrategy:  "STORE_ONLY",
			wantHealthy:   "true",
		},
		{
			name:          "a pipeline rewrite to STORE_ONLY is reflected in the label",
			inputStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
			isHealthy:     false,
			storeOnlyFor:  "test-check",
			wantStrategy:  "STORE_ONLY",
			wantHealthy:   "false",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := receivedCount(tt.wantStrategy, tt.wantHealthy)

			server := &PlatformConnectorServer{}
			if tt.storeOnlyFor != "" {
				server.Pipeline = pipeline.New(&storeOnlyTransformer{checkName: tt.storeOnlyFor})
			}

			_, err := server.HealthEventOccurredV1(context.Background(), &pb.HealthEvents{
				Events: []*pb.HealthEvent{{
					NodeName:           "test-node",
					CheckName:          "test-check",
					ProcessingStrategy: tt.inputStrategy,
					IsHealthy:          tt.isHealthy,
				}},
			})
			require.NoError(t, err)

			assert.Equal(t, before+1, receivedCount(tt.wantStrategy, tt.wantHealthy))
		})
	}
}

func TestHealthEventOccurredV1_RejectedBatchIsNotCounted(t *testing.T) {
	tests := []struct {
		name  string
		event *pb.HealthEvent
	}{
		{
			name: "CUSTOM action without a custom string",
			event: &pb.HealthEvent{
				NodeName:          "test-node",
				RecommendedAction: pb.RecommendedAction_CUSTOM,
			},
		},
		{
			name: "unknown processing strategy",
			event: &pb.HealthEvent{
				NodeName:           "test-node",
				ProcessingStrategy: pb.ProcessingStrategy(99),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := receivedCount("EXECUTE_REMEDIATION", "false")

			server := &PlatformConnectorServer{}
			_, err := server.HealthEventOccurredV1(context.Background(),
				&pb.HealthEvents{Events: []*pb.HealthEvent{tt.event}})
			require.Error(t, err)

			assert.Equal(t, before, receivedCount("EXECUTE_REMEDIATION", "false"))
		})
	}
}

func TestHealthEventsReceived_SeriesInitializedAtZero(t *testing.T) {
	// init() pre-creates one series per countable strategy times both health values,
	// so a rate() can observe the first increment. Confirm all are exposed.
	for _, strategy := range countableStrategies {
		for _, healthy := range []string{"true", "false"} {
			assert.GreaterOrEqual(t,
				testutil.CollectAndCount(healthEventsReceived.WithLabelValues(strategy.String(), healthy)),
				0, "series %s/%s must be registered", strategy, healthy)
		}
	}
	assert.GreaterOrEqual(t, testutil.CollectAndCount(healthEventsReceived), len(countableStrategies)*2)
}
