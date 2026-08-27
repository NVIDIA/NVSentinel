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

package kubeclient

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestRateLimitConfigApply_ExplicitValues_AppliesToRESTConfig(t *testing.T) {
	config := &rest.Config{}

	err := (RateLimitConfig{QPS: 50, Burst: 100}).Apply(config)

	require.NoError(t, err)
	assert.Equal(t, float32(50), config.QPS)
	assert.Equal(t, 100, config.Burst)
}

func TestRateLimitConfigApply_ZeroValues_UsesClientGoDefaults(t *testing.T) {
	config := &rest.Config{}

	err := (RateLimitConfig{}).Apply(config)

	require.NoError(t, err)
	assert.Equal(t, rest.DefaultQPS, config.QPS)
	assert.Equal(t, rest.DefaultBurst, config.Burst)
}

func TestRateLimitConfigApply_InvalidValues_ReturnsError(t *testing.T) {
	tests := []RateLimitConfig{
		{QPS: 5, Burst: -1},
		{QPS: math.NaN(), Burst: 10},
		{QPS: math.Inf(1), Burst: 10},
		{QPS: math.Inf(-1), Burst: 10},
		{QPS: float64(math.MaxFloat32) * 2, Burst: 10},
		{QPS: -float64(math.MaxFloat32) * 2, Burst: 10},
	}

	for _, config := range tests {
		assert.Error(t, config.Apply(&rest.Config{}))
	}
}

func TestRateLimitConfigApply_NegativeQPS_DisablesClientGoRateLimiter(t *testing.T) {
	config := &rest.Config{Host: "https://example.invalid"}
	err := (RateLimitConfig{QPS: -1, Burst: 10}).Apply(config)
	require.NoError(t, err)

	clientset, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	assert.Nil(t, clientset.CoreV1().RESTClient().GetRateLimiter())
}

func TestRateLimitConfigApply_ConfiguredRateLimits_EnforcedByClientGo(t *testing.T) {
	config := &rest.Config{Host: "https://example.invalid"}
	err := (RateLimitConfig{QPS: 0.01, Burst: 3}).Apply(config)
	require.NoError(t, err)

	clientset, err := kubernetes.NewForConfig(config)
	require.NoError(t, err)

	limiter := clientset.CoreV1().RESTClient().GetRateLimiter()
	require.NotNil(t, limiter)
	assert.InDelta(t, 0.01, limiter.QPS(), 0.0001)

	assert.True(t, limiter.TryAccept())
	assert.True(t, limiter.TryAccept())
	assert.True(t, limiter.TryAccept())
	assert.False(t, limiter.TryAccept(), "request beyond the configured burst must be throttled")
}
