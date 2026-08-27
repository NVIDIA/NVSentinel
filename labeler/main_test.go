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
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/labeler/pkg/devicecounts"
)

func TestCLIOptionsInitializationParams_ConfiguredRateLimits_ForwardsValues(t *testing.T) {
	rateLimits := kubeclient.RateLimitConfig{QPS: 40, Burst: 80}

	params := (cliOptions{kubernetesClientRateLimits: rateLimits}).initializationParams(devicecounts.Config{})

	assert.Equal(t, rateLimits, params.KubernetesClientRateLimits)
}
