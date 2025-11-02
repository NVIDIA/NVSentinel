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
	"testing"
	"time"
)

func TestGetNextRequeueDelay(t *testing.T) {
	tests := []struct {
		name                string
		consecutiveFailures int32
		expectedDelay       time.Duration
	}{
		{
			name:                "first retry after initial failure",
			consecutiveFailures: 0,
			expectedDelay:       30 * time.Second,
		},
		{
			name:                "second retry",
			consecutiveFailures: 1,
			expectedDelay:       1 * time.Minute,
		},
		{
			name:                "third retry",
			consecutiveFailures: 2,
			expectedDelay:       2 * time.Minute,
		},
		{
			name:                "fourth retry",
			consecutiveFailures: 3,
			expectedDelay:       5 * time.Minute,
		},
		{
			name:                "fifth retry - capped at max",
			consecutiveFailures: 4,
			expectedDelay:       5 * time.Minute,
		},
		{
			name:                "many retries - still capped at max",
			consecutiveFailures: 10,
			expectedDelay:       5 * time.Minute,
		},
		{
			name:                "very large failure count - still capped",
			consecutiveFailures: 100,
			expectedDelay:       5 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getNextRequeueDelay(tt.consecutiveFailures)
			if got != tt.expectedDelay {
				t.Errorf("getNextRequeueDelay(%d) = %v, want %v",
					tt.consecutiveFailures, got, tt.expectedDelay)
			}
		})
	}
}

func TestGetNextRequeueDelay_BackoffProgression(t *testing.T) {
	// Test that delays increase monotonically until capped
	var prevDelay time.Duration
	for i := int32(0); i < 10; i++ {
		delay := getNextRequeueDelay(i)

		if i > 0 && delay < prevDelay {
			t.Errorf("Backoff delay decreased at failure count %d: prev=%v, current=%v",
				i, prevDelay, delay)
		}

		prevDelay = delay
	}
}
