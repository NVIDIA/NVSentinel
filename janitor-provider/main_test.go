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

package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTranslateCSPError_VariousErrors_ReturnsExpectedStatus(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode codes.Code
		wantText string
	}{
		{
			name:     "plain provider error",
			err:      errors.New("permission denied"),
			wantCode: codes.Internal,
			wantText: "permission denied",
		},
		{
			name:     "retryable provider error",
			err:      status.Error(codes.Unavailable, "instance busy"),
			wantCode: codes.Unavailable,
			wantText: "instance busy",
		},
		{
			name:     "provider deadline",
			err:      status.Error(codes.DeadlineExceeded, "request timed out"),
			wantCode: codes.DeadlineExceeded,
			wantText: "request timed out",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := translateCSPError(test.err, "perform CSP operation")

			require.Error(t, err)
			assert.Equal(t, test.wantCode, status.Code(err))
			assert.ErrorContains(t, err, "perform CSP operation")
			assert.ErrorContains(t, err, test.wantText)
		})
	}
}

func TestTranslateCSPError_NilError_ReturnsNil(t *testing.T) {
	require.NoError(t, translateCSPError(nil, "perform CSP operation"))
}
