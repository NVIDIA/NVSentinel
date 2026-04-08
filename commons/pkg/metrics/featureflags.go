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

// Package featureflags exposes runtime feature toggles as a Prometheus gauge
// metric (nvsentinel_feature_flag_enabled) for observability.
package featureflags

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Registry owns a gauge vector for a single NVSentinel service and exposes
// methods to set individual feature flag values.
type Registry struct {
	serviceName string
	gauge       *prometheus.GaugeVec
}

// Option configures how a Registry is created.
type Option func(*registryConfig)

type registryConfig struct {
	registerer prometheus.Registerer
}

// WithRegisterer sets a custom Prometheus registerer. This is required for
// controller-runtime binaries whose metrics must appear on the
// controller-runtime metrics endpoint (crmetrics.Registry).
func WithRegisterer(reg prometheus.Registerer) Option {
	return func(c *registryConfig) {
		c.registerer = reg
	}
}

// NewRegistry creates a Registry that owns the nvsentinel_feature_flag_enabled
// gauge vector for the given service. By default metrics are registered with
// prometheus.DefaultRegisterer; pass WithRegisterer to override.
func NewRegistry(serviceName string, opts ...Option) *Registry {
	cfg := &registryConfig{
		registerer: prometheus.DefaultRegisterer,
	}

	for _, opt := range opts {
		opt(cfg)
	}

	gauge := promauto.With(cfg.registerer).NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nvsentinel_feature_flag_enabled",
			Help: "Reports whether a feature flag is enabled (1) or disabled (0).",
		},
		[]string{"service", "flag"},
	)

	return &Registry{
		serviceName: serviceName,
		gauge:       gauge,
	}
}

// Set sets (or updates) a single flag gauge. Calling Set multiple times for
// the same flag name is idempotent — the gauge value is simply overwritten.
func (r *Registry) Set(flag string, enabled bool) {
	val := float64(0)
	if enabled {
		val = 1
	}

	r.gauge.WithLabelValues(r.serviceName, flag).Set(val)
}

// SetStoreOnlyMode is a convenience helper: it sets the "store_only_mode"
// flag to 1 when strategy == "STORE_ONLY", else 0.
func (r *Registry) SetStoreOnlyMode(strategy string) {
	r.Set("store_only_mode", strategy == "STORE_ONLY")
}
