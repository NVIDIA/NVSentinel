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

package server

import (
	"context"
	"testing"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
)

func TestHealthEventOccurredV1_ProcessingStrategyNormalization(t *testing.T) {
	tests := []struct {
		name             string
		inputStrategy    pb.ProcessingStrategy
		expectedStrategy pb.ProcessingStrategy
	}{
		{
			name:             "UNSPECIFIED is normalized to EXECUTE_REMEDIATION",
			inputStrategy:    pb.ProcessingStrategy_UNSPECIFIED,
			expectedStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		},
		{
			name:             "EXECUTE_REMEDIATION remains unchanged",
			inputStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
			expectedStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		},
		{
			name:             "STORE_ONLY remains unchanged",
			inputStrategy:    pb.ProcessingStrategy_STORE_ONLY,
			expectedStrategy: pb.ProcessingStrategy_STORE_ONLY,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &PlatformConnectorServer{}

			healthEvents := &pb.HealthEvents{
				Events: []*pb.HealthEvent{
					{
						NodeName:           "test-node",
						CheckName:          "test-check",
						ProcessingStrategy: tt.inputStrategy,
					},
				},
			}

			_, err := server.HealthEventOccurredV1(context.Background(), healthEvents)

			assert.NoError(t, err)
			assert.Equal(t, tt.expectedStrategy, healthEvents.Events[0].ProcessingStrategy)
		})
	}
}
