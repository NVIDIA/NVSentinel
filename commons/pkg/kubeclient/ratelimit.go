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
	"flag"
	"fmt"
	"math"

	"k8s.io/client-go/rest"
)

// RateLimitConfig configures client-go's token-bucket rate limiter.
type RateLimitConfig struct {
	QPS   float64
	Burst int
}

// RegisterRateLimitFlags registers the shared Kubernetes API client rate-limit flags.
func RegisterRateLimitFlags() *RateLimitConfig {
	config := &RateLimitConfig{}
	flag.Float64Var(&config.QPS, "kube-api-qps", float64(rest.DefaultQPS),
		"maximum average Kubernetes API requests per second; a negative value disables client-side throttling")
	flag.IntVar(&config.Burst, "kube-api-burst", rest.DefaultBurst,
		"maximum burst of Kubernetes API requests")

	return config
}

// Apply validates and applies the configured limits to a REST config.
func (c RateLimitConfig) Apply(config *rest.Config) error {
	if math.IsNaN(c.QPS) || math.IsInf(c.QPS, 0) {
		return fmt.Errorf("kube API QPS must be finite")
	}

	if math.Abs(c.QPS) > math.MaxFloat32 {
		return fmt.Errorf("kube API QPS exceeds float32 range")
	}

	if c.Burst < 0 {
		return fmt.Errorf("kube API burst must not be negative")
	}

	if c.QPS == 0 {
		c.QPS = float64(rest.DefaultQPS)
	}

	if c.Burst == 0 {
		c.Burst = rest.DefaultBurst
	}

	config.QPS = float32(c.QPS)
	config.Burst = c.Burst

	return nil
}
