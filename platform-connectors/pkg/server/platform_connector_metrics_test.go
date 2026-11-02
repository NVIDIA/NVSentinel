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

func receivedCount(strategy, isHealthy string) float64 {
	return testutil.ToFloat64(healthEventsReceived.WithLabelValues(strategy, isHealthy))
}

// This runs first, and no other test in the package creates an UNKNOWN series, so the
// vec holds exactly the series init() created: the countable strategies times two
// health values. Exact equality also catches a leaked series, e.g. UNSPECIFIED.
func TestHealthEventsReceived_SeriesInitializedAtZero(t *testing.T) {
	assert.Equal(t, len(countableStrategies)*2, testutil.CollectAndCount(healthEventsReceived))
}

func TestStrategyLabel(t *testing.T) {
	tests := []struct {
		name     string
		strategy pb.ProcessingStrategy
		want     string
	}{
		{"known EXECUTE_REMEDIATION", pb.ProcessingStrategy_EXECUTE_REMEDIATION, "EXECUTE_REMEDIATION"},
		{"known STORE_ONLY", pb.ProcessingStrategy_STORE_ONLY, "STORE_ONLY"},
		{"unknown value folds to UNKNOWN", pb.ProcessingStrategy(99), "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, strategyLabel(tt.strategy))
		})
	}
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
	// The strategy is a valid, init-created one, so a regression that counted before
	// validation would bump this exact series' value; assert the value, not just the
	// series count, to catch it. CollectAndCount also guards against a stray series.
	valueBefore := receivedCount("EXECUTE_REMEDIATION", "false")
	seriesBefore := testutil.CollectAndCount(healthEventsReceived)

	server := &PlatformConnectorServer{}

	// CUSTOM without a custom action string fails validation, so the whole batch is
	// rejected before anything is counted.
	_, err := server.HealthEventOccurredV1(context.Background(), &pb.HealthEvents{
		Events: []*pb.HealthEvent{{
			NodeName:           "test-node",
			ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
			RecommendedAction:  pb.RecommendedAction_CUSTOM,
			IsHealthy:          false,
		}},
	})
	require.Error(t, err)

	assert.Equal(t, valueBefore, receivedCount("EXECUTE_REMEDIATION", "false"),
		"a rejected batch must not be counted")
	assert.Equal(t, seriesBefore, testutil.CollectAndCount(healthEventsReceived),
		"a rejected batch must not create a series")
}
