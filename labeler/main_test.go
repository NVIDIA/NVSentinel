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
	"flag"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/labeler/pkg/devicecounts"
)

func TestCLIOptionsInitializationParams_ConfiguredRateLimits_ForwardsValues(t *testing.T) {
	rateLimits := kubeclient.RateLimitConfig{QPS: 40, Burst: 80}

	params := (cliOptions{kubernetesClientRateLimits: rateLimits}).initializationParams(devicecounts.Config{})

	assert.Equal(t, rateLimits, params.KubernetesClientRateLimits)
}

// TestRegisterMovedFlags_ArgsFromOlderChart_ParseAndAreReported parses the arguments
// that an older chart passes to the labeler. The flags moved to nvidia-labeler, and
// the labeler must still start with them.
func TestRegisterMovedFlags_ArgsFromOlderChart_ParseAndAreReported(t *testing.T) {
	fs := flag.NewFlagSet("labeler", flag.ContinueOnError)
	registerMovedFlags(fs)

	require.NoError(t, fs.Parse([]string{
		"--assume-driver-installed",
		"--dcgm-app-label=nvidia-dcgm,nvidia-dcgm-dra",
		"--driver-app-label=nvidia-driver-daemonset",
		"--gke-installer-app-label=nvidia-driver-installer",
		"--require-dcgm-ready-for-bootstrap=true",
		"--assume-dcgm-available",
	}))

	assert.ElementsMatch(t, append(append([]string{}, movedStringFlags...), movedBoolFlags...), ignoredFlags(fs))
}
