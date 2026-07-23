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

package nicdriver

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// Config is the top-level TOML structure for NIC driver syslog patterns.
type Config struct {
	NICDriverDetection PatternDetectionConfig `toml:"nicDriverDetection"`
}

// PatternDetectionConfig wraps the configurable pattern list.
type PatternDetectionConfig struct {
	Patterns []PatternConfig `toml:"patterns"`
}

// PatternConfig defines a single kernel log pattern to match.
type PatternConfig struct {
	Name               string `toml:"name"`
	Enabled            bool   `toml:"enabled"`
	ProcessingStrategy string `toml:"processingStrategy"`
}

// CompiledPattern is a validated, ready-to-evaluate pattern produced by LoadConfig.
type CompiledPattern struct {
	Name                  string
	Re                    *regexp.Regexp
	IsFatal               bool
	RecommendedAction     pb.RecommendedAction
	ProcessingStrategy    pb.ProcessingStrategy
	HasProcessingStrategy bool
	Description           string
}

type patternDefinition struct {
	re                *regexp.Regexp
	isFatal           bool
	recommendedAction pb.RecommendedAction
	description       string
}

var patternDefinitions = map[string]patternDefinition{
	"cmd_exec_timeout": {
		re:                regexp.MustCompile(`mlx5_core.*timeout\. Will cause a leak of a command resource`),
		isFatal:           true,
		recommendedAction: pb.RecommendedAction_REPLACE_VM,
		description:       "Firmware/driver command timeout - control plane broken",
	},
	"health_poll_failed": {
		re:                regexp.MustCompile(`mlx5_core.*device's health compromised.*reached miss count`),
		isFatal:           true,
		recommendedAction: pb.RecommendedAction_REPLACE_VM,
		description:       "Firmware heartbeat lost - device health compromised",
	},
	"unrecoverable_err": {
		re:                regexp.MustCompile(`mlx5_core.*unrecoverable hardware error`),
		isFatal:           true,
		recommendedAction: pb.RecommendedAction_REPLACE_VM,
		description:       "Unrecoverable hardware error (syndrome 0x8)",
	},
	"netdev_watchdog": {
		// Kernel commit e316dd1cf135 (v6.8, backported to 6.6.47/6.1.107) moved the
		// driver name in front of "NETDEV WATCHDOG", so both orders must match:
		//   old: NETDEV WATCHDOG: eth0 (mlx5_core): transmit queue 0 timed out
		//   new: mlx5_core 0000:65:00.0 eth0: NETDEV WATCHDOG: CPU: 5: transmit queue 0 timed out 5032 ms
		re: regexp.MustCompile(
			`(NETDEV WATCHDOG.*mlx5_core|mlx5_core.*NETDEV WATCHDOG).*transmit queue.*timed out`),
		isFatal:           false,
		recommendedAction: pb.RecommendedAction_NONE,
		description:       "TX queue timeout - mlx5 driver has auto-recovery",
	},
	"mlx5_tx_timeout_detected": {
		re:                regexp.MustCompile(`mlx5_core.*TX timeout detected`),
		isFatal:           false,
		recommendedAction: pb.RecommendedAction_NONE,
		description:       "mlx5 driver TX timeout - driver attempts auto-recovery",
	},
	"mlx5_rx_timeout_detected": {
		re:                regexp.MustCompile(`mlx5_core.*RX timeout on channel`),
		isFatal:           false,
		recommendedAction: pb.RecommendedAction_NONE,
		description:       "mlx5 driver RX timeout - driver attempts auto-recovery",
	},
	softLockupPatternName: {
		// Confirmation regex for the stateful soft-lockup detector, not a
		// standalone single-line pattern: it only fires on a non-speculative
		// mlx5 NAPI stack frame observed shortly after a "BUG: soft lockup"
		// header line (see softlockup.go).
		re:                regexp.MustCompile(`mlx5e_(poll_ico_cq|napi_poll)\+0x`),
		isFatal:           true,
		recommendedAction: pb.RecommendedAction_REPLACE_VM,
		description:       "CPU soft lockup in mlx5 NAPI poll loop - NIC driver wedged",
	},
	"pci_power_insufficient": {
		re:                regexp.MustCompile(`mlx5_core.*Detected insufficient power on the PCIe slot`),
		isFatal:           false,
		recommendedAction: pb.RecommendedAction_NONE,
		description:       "PCIe slot power negotiation issue",
	},
	"port_module_high_temp": {
		re:                regexp.MustCompile(`mlx5_core.*Port module event.*High Temperature`),
		isFatal:           false,
		recommendedAction: pb.RecommendedAction_NONE,
		description:       "Port module high temperature warning",
	},
	"access_reg_failed": {
		re:                regexp.MustCompile(`mlx5_cmd_out_err.*ACCESS_REG.*failed`),
		isFatal:           false,
		recommendedAction: pb.RecommendedAction_NONE,
		description:       "ACCESS_REG command failed - restricted PF access noise",
	},
	"module_unplugged": {
		re:                regexp.MustCompile(`mlx5_core.*Port module event.*Cable unplugged`),
		isFatal:           false,
		recommendedAction: pb.RecommendedAction_NONE,
		description:       "SFP/transceiver cable unplugged",
	},
}

// LoadConfig reads and validates the TOML configuration, returning only the
// enabled pattern definitions requested by name.
func LoadConfig(path string) ([]CompiledPattern, error) {
	cfg := &Config{}
	if err := configmanager.LoadTOMLConfig(path, cfg); err != nil {
		return nil, fmt.Errorf("failed to load NIC driver config: %w", err)
	}

	return compilePatterns(cfg.NICDriverDetection.Patterns)
}

func compilePatterns(raw []PatternConfig) ([]CompiledPattern, error) {
	seen := make(map[string]struct{})

	var compiled []CompiledPattern

	for i, p := range raw {
		if !p.Enabled {
			continue
		}

		if err := validatePattern(p); err != nil {
			return nil, fmt.Errorf("patterns[%d] (%q): %w", i, p.Name, err)
		}

		if _, exists := seen[p.Name]; exists {
			return nil, fmt.Errorf("patterns[%d]: duplicate pattern name %q", i, p.Name)
		}

		seen[p.Name] = struct{}{}

		def, ok := patternDefinitions[p.Name]
		if !ok {
			return nil, fmt.Errorf("patterns[%d]: pattern name %q is not supported", i, p.Name)
		}

		processingStrategy, hasProcessingStrategy, err := resolveProcessingStrategy(p.ProcessingStrategy)
		if err != nil {
			return nil, fmt.Errorf("patterns[%d] (%q): %w", i, p.Name, err)
		}

		compiled = append(compiled, CompiledPattern{
			Name:                  p.Name,
			Re:                    def.re,
			IsFatal:               def.isFatal,
			RecommendedAction:     def.recommendedAction,
			ProcessingStrategy:    processingStrategy,
			HasProcessingStrategy: hasProcessingStrategy,
			Description:           def.description,
		})
	}

	return compiled, nil
}

func validatePattern(p PatternConfig) error {
	if p.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	return nil
}

func resolveProcessingStrategy(strategy string) (pb.ProcessingStrategy, bool, error) {
	if strings.TrimSpace(strategy) == "" {
		return 0, false, nil
	}

	upper := strings.ToUpper(strings.TrimSpace(strategy))

	val, ok := pb.ProcessingStrategy_value[upper]
	if !ok {
		return 0, false, fmt.Errorf("processingStrategy %q is not a known ProcessingStrategy enum value", strategy)
	}

	return pb.ProcessingStrategy(val), true, nil
}
