// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package config

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nvidia/nvsentinel/commons/pkg/configmanager"
)

const (
	thresholdTypeDelta    = "delta"
	thresholdTypeVelocity = "velocity"

	velocityUnitSecond = "second"
	velocityUnitMinute = "minute"
	velocityUnitHour   = "hour"

	recommendedActionMarker = "Recommended Action="
)

type counterDefinition struct {
	path        string
	isFatal     bool
	description string
}

var counterDefinitions = map[string]counterDefinition{
	// /sys/class/infiniband/<dev>/ports/<port>/counters/
	"excessive_buffer_overrun_errors": {
		path:        "counters/excessive_buffer_overrun_errors",
		isFatal:     true,
		description: "HCA internal buffer overflow - lossless contract violated",
	},
	"link_downed": {
		path:        "counters/link_downed",
		isFatal:     true,
		description: "Port Training State Machine failed - QP disconnect",
	},
	"link_error_recovery": {
		path:        "counters/link_error_recovery",
		isFatal:     false,
		description: "Link retraining events - micro-flapping",
	},
	"local_link_integrity_errors": {
		path:        "counters/local_link_integrity_errors",
		isFatal:     true,
		description: "Physical errors exceed LocalPhyErrors hardware cap",
	},
	"port_rcv_discards": {
		path:        "counters/port_rcv_discards",
		isFatal:     false,
		description: "RX discards due to congestion or buffer pressure",
	},
	"port_rcv_errors": {
		path:        "counters/port_rcv_errors",
		isFatal:     false,
		description: "Malformed packets received",
	},
	"port_rcv_remote_physical_errors": {
		path:        "counters/port_rcv_remote_physical_errors",
		isFatal:     false,
		description: "Remote physical-layer errors received on this port",
	},
	"port_rcv_switch_relay_errors": {
		path:        "counters/port_rcv_switch_relay_errors",
		isFatal:     false,
		description: "Packets discarded because switch relay forwarding failed",
	},
	"port_xmit_discards": {
		path:        "counters/port_xmit_discards",
		isFatal:     false,
		description: "TX discards due to congestion",
	},
	"port_xmit_wait": {
		path:        "counters/port_xmit_wait",
		isFatal:     false,
		description: "TX wait ticks - congestion backpressure",
	},
	"symbol_error": {
		path:        "counters/symbol_error",
		isFatal:     false,
		description: "PHY bit errors before FEC - physical layer degradation",
	},
	"symbol_error_fatal": {
		path:        "counters/symbol_error",
		isFatal:     true,
		description: "Symbol errors exceed IBTA BER threshold (10E-12) - link outside spec",
	},

	// /sys/class/infiniband/<dev>/ports/<port>/hw_counters/
	"implied_nak_seq_err": {
		path:        "hw_counters/implied_nak_seq_err",
		isFatal:     false,
		description: "Implied NAK sequence errors - retransmission pressure",
	},
	"local_ack_timeout_err": {
		path:        "hw_counters/local_ack_timeout_err",
		isFatal:     false,
		description: "ACK timeout - potential fabric black hole",
	},
	"out_of_sequence": {
		path:        "hw_counters/out_of_sequence",
		isFatal:     false,
		description: "Fabric routing issues - out of sequence packets",
	},
	"packet_seq_err": {
		path:        "hw_counters/packet_seq_err",
		isFatal:     false,
		description: "Packet sequence errors - retransmission pressure",
	},
	"req_transport_retries_exceeded": {
		path:        "hw_counters/req_transport_retries_exceeded",
		isFatal:     true,
		description: "Requester transport retry limit exceeded",
	},
	"rnr_nak_retry_err": {
		path:        "hw_counters/rnr_nak_retry_err",
		isFatal:     true,
		description: "Receiver Not Ready NAK retry exhausted - connection severed",
	},
	"roce_slow_restart": {
		path:        "hw_counters/roce_slow_restart",
		isFatal:     false,
		description: "Victim flow oscillation",
	},

	// /sys/class/net/<iface>/ (netdev-root attribute — carrier_changes
	// lives beside operstate, NOT under statistics/; the old
	// "statistics/carrier_changes" path never existed on any kernel, so
	// this counter silently never worked before the netdev/ path class).
	"carrier_changes": {
		path:        "netdev/carrier_changes",
		isFatal:     false,
		description: "Link instability - carrier state changes",
	},

	// /sys/class/net/<iface>/statistics/
	"rx_crc_errors": {
		path:        "statistics/rx_crc_errors",
		isFatal:     false,
		description: "RX packets with CRC/FCS errors",
	},
	"rx_errors": {
		path:        "statistics/rx_errors",
		isFatal:     false,
		description: "Aggregate RX packet errors",
	},
	"rx_missed_errors": {
		path:        "statistics/rx_missed_errors",
		isFatal:     false,
		description: "RX packets missed due to receive-side capacity pressure",
	},
	"tx_carrier_errors": {
		path:        "statistics/tx_carrier_errors",
		isFatal:     false,
		description: "TX carrier-sense errors",
	},
	"tx_errors": {
		path:        "statistics/tx_errors",
		isFatal:     false,
		description: "Aggregate TX packet errors",
	},
}

// Config represents the NIC Health Monitor configuration loaded from TOML.
type Config struct {
	// NicExclusionRegex contains comma-separated regex patterns for NICs to exclude
	// during normal discovery. NicInclusionRegexOverride takes precedence when set.
	NicExclusionRegex string `toml:"nicExclusionRegex"`

	// NicInclusionRegexOverride, when non-empty, monitors only NIC devices whose
	// names match these comma-separated regex patterns and bypasses all automatic
	// device filters for those matches.
	NicInclusionRegexOverride string `toml:"nicInclusionRegexOverride"`

	// SysClassNetPath is the sysfs path for network interfaces (container mount point)
	SysClassNetPath string `toml:"sysClassNetPath"`

	// SysClassInfinibandPath is the sysfs path for InfiniBand devices (container mount point)
	SysClassInfinibandPath string `toml:"sysClassInfinibandPath"`

	// CounterDetection contains counter monitoring configuration
	CounterDetection CounterDetectionConfig `toml:"counterDetection"`
}

// CounterDetectionConfig contains the configuration for counter-based monitoring.
type CounterDetectionConfig struct {
	Enabled  bool            `toml:"enabled"`
	Counters []CounterConfig `toml:"counters"`
}

// CounterConfig defines a single counter to monitor.
type CounterConfig struct {
	Name          string  `toml:"name"`
	Path          string  `toml:"-"`
	Enabled       bool    `toml:"enabled"`
	IsFatal       bool    `toml:"-"`
	ThresholdType string  `toml:"thresholdType"`
	Threshold     float64 `toml:"threshold"`
	VelocityUnit  string  `toml:"velocityUnit,omitempty"`
	Description   string  `toml:"-"`
}

// LoadConfig reads and parses the TOML configuration file.
func LoadConfig(path string) (*Config, error) {
	cfg := &Config{}
	if err := configmanager.LoadTOMLConfig(path, cfg); err != nil {
		return nil, err
	}

	if cfg.SysClassNetPath == "" {
		cfg.SysClassNetPath = "/nvsentinel/sys/class/net"
	}

	if cfg.SysClassInfinibandPath == "" {
		cfg.SysClassInfinibandPath = "/nvsentinel/sys/class/infiniband"
	}

	if err := validateRegexList(cfg.NicExclusionRegex); err != nil {
		return nil, fmt.Errorf("invalid nicExclusionRegex: %w", err)
	}

	if err := validateInclusionRegexList(cfg.NicInclusionRegexOverride); err != nil {
		return nil, fmt.Errorf("invalid nicInclusionRegexOverride: %w", err)
	}

	if err := validateCounterDetection(&cfg.CounterDetection); err != nil {
		return nil, fmt.Errorf("invalid counterDetection: %w", err)
	}

	return cfg, nil
}

func validateRegexList(commaSeparated string) error {
	if commaSeparated == "" {
		return nil
	}

	for _, pat := range strings.Split(commaSeparated, ",") {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}

		if _, err := regexp.Compile(pat); err != nil {
			return fmt.Errorf("pattern %q: %w", pat, err)
		}
	}

	return nil
}

// validateInclusionRegexList additionally requires a configured override
// to contain at least one non-empty pattern. Without this guard values such
// as "," or ",," would enable an exclusive override that cannot match any
// device.
func validateInclusionRegexList(commaSeparated string) error {
	if err := validateRegexList(commaSeparated); err != nil {
		return err
	}

	if strings.TrimSpace(commaSeparated) == "" {
		return nil
	}

	for _, pat := range strings.Split(commaSeparated, ",") {
		if strings.TrimSpace(pat) != "" {
			return nil
		}
	}

	return fmt.Errorf("must contain at least one non-empty pattern")
}

func validateCounterDetection(cd *CounterDetectionConfig) error {
	if !cd.Enabled {
		return nil
	}

	seen := make(map[string]struct{})

	for i, c := range cd.Counters {
		if !c.Enabled {
			continue
		}

		if err := validateCounter(&cd.Counters[i]); err != nil {
			return fmt.Errorf("counters[%d] (%q): %w", i, c.Name, err)
		}

		if _, exists := seen[c.Name]; exists {
			return fmt.Errorf("counters[%d]: duplicate counter name %q", i, c.Name)
		}

		seen[c.Name] = struct{}{}
	}

	return nil
}

var validVelocityUnits = map[string]struct{}{
	velocityUnitSecond: {},
	velocityUnitMinute: {},
	velocityUnitHour:   {},
}

func validateCounter(c *CounterConfig) error {
	if c.Name == "" {
		return fmt.Errorf("name must not be empty")
	}

	if err := applyCounterDefinition(c); err != nil {
		return err
	}

	if err := validateThreshold(c.Threshold); err != nil {
		return err
	}

	switch c.ThresholdType {
	case thresholdTypeDelta:
		// velocityUnit is ignored for delta counters
	case thresholdTypeVelocity:
		if _, ok := validVelocityUnits[c.VelocityUnit]; !ok {
			return fmt.Errorf("velocityUnit %q is invalid; must be one of: second, minute, hour", c.VelocityUnit)
		}
	default:
		return fmt.Errorf("thresholdType %q is invalid; must be one of: delta, velocity", c.ThresholdType)
	}

	if err := validateDescription(c.Description); err != nil {
		return fmt.Errorf("description: %w", err)
	}

	return nil
}

func validateThreshold(threshold float64) error {
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 {
		return fmt.Errorf("threshold %v is invalid; must be a finite value >= 0", threshold)
	}

	return nil
}

func applyCounterDefinition(c *CounterConfig) error {
	def, ok := counterDefinitions[c.Name]
	if !ok {
		return fmt.Errorf(
			"counter name %q is not allowed; allowed counters: %s",
			c.Name, strings.Join(allowedCounterNames(), ", "),
		)
	}

	c.Path = def.path
	c.IsFatal = def.isFatal
	c.Description = def.description

	return nil
}

func allowedCounterNames() []string {
	names := make([]string, 0, len(counterDefinitions))
	for name := range counterDefinitions {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

func validateDescription(desc string) error {
	if desc == "" {
		return fmt.Errorf("must not be empty")
	}

	if !utf8.ValidString(desc) {
		return fmt.Errorf("contains invalid UTF-8")
	}

	if strings.Contains(desc, ";") {
		return fmt.Errorf("must not contain %q (used as message delimiter by platform-connectors)", ";")
	}

	if strings.Contains(desc, recommendedActionMarker) {
		return fmt.Errorf(
			"must not contain %q (used as message parser marker by platform-connectors)",
			recommendedActionMarker,
		)
	}

	return nil
}
