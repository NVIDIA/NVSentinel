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

// Package cancellation defines per-check rules of the form
// "observing onErrorCode emits synthetic healthy events for cancelErrorCodes".
package cancellation

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

type CancellationRule struct {
	OnErrorCode      string   `toml:"onErrorCode"`
	CancelErrorCodes []string `toml:"cancelErrorCodes"`
}

type CheckCancellations struct {
	Name    string             `toml:"name"`
	Enabled bool               `toml:"enabled"`
	Rules   []CancellationRule `toml:"cancellations"`
}

type Config struct {
	Checks []CheckCancellations `toml:"checks"`
}

// LoadConfig reads and validates a cancellations TOML file. A missing file
// returns an empty Config with no error.
func LoadConfig(path string) (*Config, error) {
	var cfg Config

	if err := configmanager.LoadTOMLConfig(path, &cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}

		return nil, err
	}

	if err := Validate(&cfg); err != nil {
		return nil, fmt.Errorf("cancellations config %s validation failed: %w", path, err)
	}

	return &cfg, nil
}

// Validate rejects empty/padded names and codes, empty cancelErrorCodes,
// duplicate onErrorCode within a check, self-cancel, duplicate cancel target,
// and duplicate check names.
func Validate(cfg *Config) error {
	if cfg == nil {
		return nil
	}

	seenChecks := make(map[string]struct{}, len(cfg.Checks))

	for i := range cfg.Checks {
		check := &cfg.Checks[i]
		if err := validateNonEmptyTrimmed(check.Name); err != nil {
			return fmt.Errorf("checks[%d]: name: %w", i, err)
		}

		if _, dup := seenChecks[check.Name]; dup {
			return fmt.Errorf("checks[%d]: duplicate check name %q", i, check.Name)
		}

		seenChecks[check.Name] = struct{}{}

		if err := validateCheckRules(check); err != nil {
			return fmt.Errorf("checks[%d] (%s): %w", i, check.Name, err)
		}
	}

	return nil
}

func validateCheckRules(check *CheckCancellations) error {
	seenOn := make(map[string]struct{}, len(check.Rules))

	for j := range check.Rules {
		rule := &check.Rules[j]
		if err := validateNonEmptyTrimmed(rule.OnErrorCode); err != nil {
			return fmt.Errorf("cancellations[%d]: onErrorCode: %w", j, err)
		}

		if _, dup := seenOn[rule.OnErrorCode]; dup {
			return fmt.Errorf("cancellations[%d]: duplicate onErrorCode %q", j, rule.OnErrorCode)
		}

		seenOn[rule.OnErrorCode] = struct{}{}

		if len(rule.CancelErrorCodes) == 0 {
			return fmt.Errorf("cancellations[%d] (onErrorCode=%s): cancelErrorCodes must be non-empty",
				j, rule.OnErrorCode)
		}

		seenTargets := make(map[string]struct{}, len(rule.CancelErrorCodes))

		for k, target := range rule.CancelErrorCodes {
			if err := validateNonEmptyTrimmed(target); err != nil {
				return fmt.Errorf("cancellations[%d] (onErrorCode=%s): cancelErrorCodes[%d]: %w",
					j, rule.OnErrorCode, k, err)
			}

			if target == rule.OnErrorCode {
				return fmt.Errorf("cancellations[%d] (onErrorCode=%s): rule cancels its own source error code",
					j, rule.OnErrorCode)
			}

			if _, dup := seenTargets[target]; dup {
				return fmt.Errorf("cancellations[%d] (onErrorCode=%s): duplicate cancelErrorCode %q",
					j, rule.OnErrorCode, target)
			}

			seenTargets[target] = struct{}{}
		}
	}

	return nil
}

// validateNonEmptyTrimmed rejects "" and any value with surrounding whitespace.
func validateNonEmptyTrimmed(value string) error {
	if value == "" {
		return fmt.Errorf("must be set")
	}

	if strings.TrimSpace(value) != value {
		return fmt.Errorf("must not have leading or trailing whitespace (got %q)", value)
	}

	return nil
}

// ValidateSupportedChecks rejects rules for checks whose handlers do not
// attach a Resolver. Prevents silent-no-op rules from loading.
func ValidateSupportedChecks(cfg *Config, supported []string) error {
	return validateAgainstSet(cfg, supported,
		"does not have cancellation support wired in this monitor (supported: %v)")
}

// ValidateAgainstEnabledChecks rejects rules for checks not present in the
// process's --checks list. Catches the "supported but not enabled" gap that
// ValidateSupportedChecks does not cover.
func ValidateAgainstEnabledChecks(cfg *Config, enabled []string) error {
	return validateAgainstSet(cfg, enabled,
		"is not enabled in this monitor (--checks: %v)")
}

func validateAgainstSet(cfg *Config, allowed []string, suffixFmt string) error {
	if cfg == nil {
		return nil
	}

	set := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		set[name] = struct{}{}
	}

	for i, check := range cfg.Checks {
		if _, ok := set[check.Name]; !ok {
			return fmt.Errorf("checks[%d]: name %q "+suffixFmt, i, check.Name, allowed)
		}
	}

	return nil
}

// FindCheck returns the named check, or nil if absent or disabled.
func (c *Config) FindCheck(name string) *CheckCancellations {
	if c == nil {
		return nil
	}

	for i := range c.Checks {
		check := &c.Checks[i]
		if check.Name != name {
			continue
		}

		if !check.Enabled {
			return nil
		}

		return check
	}

	return nil
}
