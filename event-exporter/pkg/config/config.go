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

package config

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

const (
	defaultSinkTimeout    = 30 * time.Second
	defaultBackfillMaxAge = 720 * time.Hour
	defaultInitialBackoff = 1 * time.Second
	defaultMaxBackoff     = 60 * time.Second
)

type Config struct {
	Exporter ExporterConfig `toml:"exporter"`
}

type ExporterConfig struct {
	Metadata        MetadataConfig        `toml:"metadata"`
	Sink            SinkConfig            `toml:"sink"`
	OIDC            OIDCConfig            `toml:"oidc"`
	Backfill        BackfillConfig        `toml:"backfill"`
	ResumeToken     ResumeTokenConfig     `toml:"resume_token"`
	FailureHandling FailureHandlingConfig `toml:"failure_handling"`
}

type MetadataConfig map[string]string

type SinkConfig struct {
	Endpoint string `toml:"endpoint"`
	Timeout  string `toml:"timeout"`
}

type OIDCConfig struct {
	TokenURL string `toml:"token_url"`
	ClientID string `toml:"client_id"`
	Scope    string `toml:"scope"`
}

type BackfillConfig struct {
	Enabled   bool   `toml:"enabled"`
	MaxAge    string `toml:"max_age"`
	MaxEvents int    `toml:"max_events"`
	BatchSize int    `toml:"batch_size"`
	RateLimit int    `toml:"rate_limit"`
}

type ResumeTokenConfig struct {
	Collection string `toml:"collection"`
	Database   string `toml:"database"`
}

type FailureHandlingConfig struct {
	MaxRetries        int     `toml:"max_retries"`
	InitialBackoff    string  `toml:"initial_backoff"`
	MaxBackoff        string  `toml:"max_backoff"`
	BackoffMultiplier float64 `toml:"backoff_multiplier"`
}

func (c *SinkConfig) GetTimeout() time.Duration {
	if c.Timeout == "" {
		slog.Error("Sink timeout not configured, using default 30 seconds")
		return defaultSinkTimeout
	}

	d, err := time.ParseDuration(c.Timeout)
	if err != nil {
		slog.Error("Failed to parse sink timeout, using default 30 seconds", "error", err)
		return defaultSinkTimeout
	}

	return d
}

func (c *BackfillConfig) GetMaxAge() time.Duration {
	if c.MaxAge == "" {
		slog.Error("Backfill max age not configured, using default 720 hours")
		return defaultBackfillMaxAge
	}

	d, err := time.ParseDuration(c.MaxAge)
	if err != nil {
		slog.Error("Failed to parse backfill max age, using default 720 hours", "error", err)
		return defaultBackfillMaxAge
	}

	return d
}

func (c *FailureHandlingConfig) GetInitialBackoff() time.Duration {
	if c.InitialBackoff == "" {
		slog.Error("Initial backoff not configured, using default 1 second")
		return defaultInitialBackoff
	}

	d, err := time.ParseDuration(c.InitialBackoff)
	if err != nil {
		slog.Error("Failed to parse initial backoff, using default 1 second", "error", err)
		return defaultInitialBackoff
	}

	return d
}

func (c *FailureHandlingConfig) GetMaxBackoff() time.Duration {
	if c.MaxBackoff == "" {
		slog.Error("Max backoff not configured, using default 60 seconds")
		return defaultMaxBackoff
	}

	d, err := time.ParseDuration(c.MaxBackoff)
	if err != nil {
		slog.Error("Failed to parse max backoff, using default 60 seconds", "error", err)
		return defaultMaxBackoff
	}

	return d
}

func (c *Config) Validate() error {
	if c.Exporter.Sink.Endpoint == "" {
		return fmt.Errorf("sink endpoint is required")
	}

	if c.Exporter.OIDC.TokenURL == "" {
		return fmt.Errorf("OIDC token_url is required")
	}

	if c.Exporter.OIDC.ClientID == "" {
		return fmt.Errorf("OIDC client_id is required")
	}

	if c.Exporter.OIDC.Scope == "" {
		return fmt.Errorf("OIDC scope is required")
	}

	if c.Exporter.ResumeToken.Collection == "" {
		return fmt.Errorf("resume_token collection is required")
	}

	if c.Exporter.ResumeToken.Database == "" {
		return fmt.Errorf("resume_token database is required")
	}

	return nil
}

func Load(path string) (*Config, error) {
	var cfg Config

	if err := configmanager.LoadTOMLConfig(path, &cfg); err != nil {
		slog.Error("Failed to load config", "error", err)
		return nil, fmt.Errorf("load config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		slog.Error("Config validation failed", "error", err)
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}
