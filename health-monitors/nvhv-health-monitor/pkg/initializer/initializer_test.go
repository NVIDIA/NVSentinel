// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package initializer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestValidate(t *testing.T) {
	ok := Params{NodeName: "n", PollInterval: 15 * time.Second, ProcessingStrategy: "STORE_ONLY"}

	timeout, strategy, err := validate(ok)
	require.NoError(t, err)
	assert.Equal(t, 15*time.Second, timeout, "the timeout defaults to the poll interval")
	assert.Equal(t, pb.ProcessingStrategy_STORE_ONLY, strategy)

	ok.RequestTimeout = 5 * time.Second
	timeout, _, err = validate(ok)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, timeout)

	_, _, err = validate(Params{PollInterval: time.Second})
	assert.ErrorContains(t, err, "node name is required")

	_, _, err = validate(Params{NodeName: "n"})
	assert.ErrorContains(t, err, "poll-interval must be positive")

	ok.RequestTimeout = time.Minute
	_, _, err = validate(ok)
	assert.ErrorContains(t, err, "at most the poll interval")

	ok.RequestTimeout = 0
	ok.ProcessingStrategy = "NOT_A_STRATEGY"
	_, _, err = validate(ok)
	assert.ErrorContains(t, err, "unexpected processingStrategy value")
}
