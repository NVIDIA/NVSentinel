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

package nodemetadata

import (
	"fmt"
	"time"
)

const (
	DefaultCacheSize        = 50
	DefaultCacheTTL         = 1 * time.Hour
	DefaultAPITimeout       = 2 * time.Second
	DefaultQPS              = 5.0
	DefaultBurst            = 10
	DefaultMaxRetries       = 3
	DefaultDecodeProviderID = true
)

// DefaultAllowedLabels defines the recommended set of node labels to include in health events.
// These labels are carefully selected to provide maximum operational value while minimizing
// metadata bloat and cache churn.
var DefaultAllowedLabels = []string{
	// Topology labels (critical for failure domain analysis)
	"topology.kubernetes.io/zone",   // Current standard (K8s 1.17+)
	"topology.kubernetes.io/region", // Current standard (K8s 1.17+)
	"failure-domain.beta.kubernetes.io/zone",   // Legacy (deprecated but still common)
	"failure-domain.beta.kubernetes.io/region", // Legacy (deprecated but still common)

	// Instance type (critical for cost attribution & capacity planning)
	"node.kubernetes.io/instance-type",   // Current standard
	"beta.kubernetes.io/instance-type",   // Legacy

	// GPU-specific labels (NVSentinel domain)
	"nvidia.com/gpu.present",             // GPU node identification
	"nvidia.com/gpu.deploy.dcgm",         // DCGM deployment topology
	"nvidia.com/gpu.deploy.driver",       // Driver deployment topology
	"nvidia.com/gpu.count",               // Number of GPUs
	"nvsentinel.dgxc.nvidia.com/dcgm.version",     // DCGM version tracking
	"nvsentinel.dgxc.nvidia.com/driver.installed", // Driver installation status

	// Physical topology (if available in your cluster)
	"rack",                               // Physical rack identifier
	"datacenter",                         // Datacenter identifier
	"row",                                // Physical row in datacenter
	"capacity-tranche",                   // Capacity reservation group

	// Workload classification (useful for cost tracking)
	"workload-type",                      // Workload classification

	// NOTE: High-churn labels are intentionally excluded:
	// - dgxc.nvidia.com/nvsentinel-state (changes frequently during remediation)
	// - pod-related labels (not node-level metadata)
	// - temporary labels (annotations are better for these)
}

// Config holds configuration for the node metadata processor.
type Config struct {
	Enabled          bool          `json:"enabled"`
	CacheSize        int           `json:"cacheSize"`
	CacheTTL         time.Duration `json:"cacheTTL"`
	APITimeout       time.Duration `json:"apiTimeout"`
	QPS              float32       `json:"qps"`
	Burst            int           `json:"burst"`
	MaxRetries       int           `json:"maxRetries"`
	DecodeProviderID bool          `json:"decodeProviderID"`
	AllowedLabels    []string      `json:"allowedLabels"`
}

// NewConfigFromMap creates a Config from a map[string]interface{}.
func NewConfigFromMap(cfgMap map[string]interface{}) (*Config, error) {
	cfg := &Config{
		Enabled:          false,
		CacheSize:        DefaultCacheSize,
		CacheTTL:         DefaultCacheTTL,
		APITimeout:       DefaultAPITimeout,
		QPS:              DefaultQPS,
		Burst:            DefaultBurst,
		MaxRetries:       DefaultMaxRetries,
		DecodeProviderID: DefaultDecodeProviderID,
		AllowedLabels:    DefaultAllowedLabels, // Use recommended defaults
	}

	if enabled, ok := cfgMap["nodeMetadataAugmentationEnabled"].(string); ok && enabled == "true" {
		cfg.Enabled = true
	}

	if cacheSize, ok := cfgMap["nodeMetadataCacheSize"].(float64); ok {
		cfg.CacheSize = int(cacheSize)
	}

	if cacheTTLSeconds, ok := cfgMap["nodeMetadataCacheTTLSeconds"].(float64); ok {
		cfg.CacheTTL = time.Duration(cacheTTLSeconds) * time.Second
	}

	if apiTimeoutSeconds, ok := cfgMap["nodeMetadataAPITimeoutSeconds"].(float64); ok {
		cfg.APITimeout = time.Duration(apiTimeoutSeconds) * time.Second
	}

	if qps, ok := cfgMap["nodeMetadataQPS"].(float64); ok {
		cfg.QPS = float32(qps)
	}

	if burst, ok := cfgMap["nodeMetadataBurst"].(float64); ok {
		cfg.Burst = int(burst)
	}

	if maxRetries, ok := cfgMap["nodeMetadataMaxRetries"].(float64); ok {
		cfg.MaxRetries = int(maxRetries)
	}

	if decodeProviderID, ok := cfgMap["nodeMetadataDecodeProviderID"].(string); ok && decodeProviderID == "false" {
		cfg.DecodeProviderID = false
	}

	if allowedLabels, ok := cfgMap["nodeMetadataAllowedLabels"].([]interface{}); ok {
		cfg.AllowedLabels = make([]string, 0, len(allowedLabels))
		for _, label := range allowedLabels {
			if labelStr, ok := label.(string); ok {
				cfg.AllowedLabels = append(cfg.AllowedLabels, labelStr)
			}
		}
	}

	return cfg, nil
}

// Validate checks if the configuration is valid.
func (c *Config) Validate() error {
	if c.CacheSize <= 0 {
		return fmt.Errorf("cacheSize must be positive")
	}

	if c.CacheTTL <= 0 {
		return fmt.Errorf("cacheTTL must be positive")
	}

	if c.APITimeout <= 0 {
		return fmt.Errorf("apiTimeout must be positive")
	}

	if c.QPS <= 0 {
		return fmt.Errorf("qps must be positive")
	}

	if c.Burst <= 0 {
		return fmt.Errorf("burst must be positive")
	}

	if c.MaxRetries < 0 {
		return fmt.Errorf("maxRetries cannot be negative")
	}

	return nil
}

