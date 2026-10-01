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

package central

import (
	"fmt"
	"time"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/configfile"
)

// Options are the deployment role's settings, from flags.
type Options struct {
	// ListenAddr is the TCP address the gRPC server binds.
	ListenAddr string
	// TLSCertDir holds tls.crt and tls.key, reloaded when cert-manager
	// rotates them. Empty is refused unless InsecureDevelopmentMode is set:
	// the caller token crosses the pod network in gRPC metadata.
	TLSCertDir              string
	InsecureDevelopmentMode bool
}

// settings are the deployment role's tunables, from the "deployment" object
// of the shared config.json. The chart writes every key; a missing or
// non-positive value refuses to start and names the key.
type settings struct {
	// The TokenReview client and its verdict cache are sized for a call on
	// the path of every batch of the fleet, not for one node's callers.
	tokenReviewQPS   float32
	tokenReviewBurst int
	tokenCacheSize   int
	// conditionUpdateTimeout bounds the node condition update (and the
	// Kubernetes Event write) and the gRPC sink call of one request; the
	// datastore write alone decides the reply, so this only bounds how long
	// a slow API server or sink can delay it.
	conditionUpdateTimeout time.Duration
	// Connections are closed after maxConnAge or maxConnIdle so a rollout
	// spreads the monitors over the new replicas.
	maxConnAge  time.Duration
	maxConnIdle time.Duration
	// The two buffers gRPC keeps per connection: the main lever on the memory
	// each connected monitor pod costs.
	grpcReadBufferBytes  int
	grpcWriteBufferBytes int
}

func settingsFromConfig(raw map[string]any) (settings, error) {
	dep, err := configfile.Object(raw, "deployment")
	if err != nil {
		return settings{}, err
	}

	var s settings

	ints := []struct {
		dst *int
		key string
	}{
		{&s.tokenReviewBurst, "TokenReviewBurst"},
		{&s.tokenCacheSize, "TokenCacheSize"},
		{&s.grpcReadBufferBytes, "GrpcReadBufferBytes"},
		{&s.grpcWriteBufferBytes, "GrpcWriteBufferBytes"},
	}
	for _, v := range ints {
		if *v.dst, err = positiveInt(dep, v.key); err != nil {
			return settings{}, err
		}
	}

	qps, err := positiveInt(dep, "TokenReviewQps")
	if err != nil {
		return settings{}, err
	}

	s.tokenReviewQPS = float32(qps)

	durations := []struct {
		dst *time.Duration
		key string
	}{
		{&s.conditionUpdateTimeout, "ConditionUpdateTimeout"},
		{&s.maxConnAge, "MaxConnectionAge"},
		{&s.maxConnIdle, "MaxConnectionIdle"},
	}
	for _, v := range durations {
		if *v.dst, err = positiveDuration(dep, v.key); err != nil {
			return settings{}, err
		}
	}

	return s, nil
}

func positiveInt(m map[string]any, key string) (int, error) {
	v, err := configfile.Int64(m, key)
	if err != nil {
		return 0, fmt.Errorf("deployment settings: %w", err)
	}

	if v <= 0 {
		return 0, fmt.Errorf("deployment settings: %s must be positive, got %d", key, v)
	}

	return int(v), nil
}

func positiveDuration(m map[string]any, key string) (time.Duration, error) {
	v, err := configfile.Duration(m, key)
	if err != nil {
		return 0, fmt.Errorf("deployment settings: %w", err)
	}

	if v <= 0 {
		return 0, fmt.Errorf("deployment settings: %s must be positive, got %s", key, v)
	}

	return v, nil
}
