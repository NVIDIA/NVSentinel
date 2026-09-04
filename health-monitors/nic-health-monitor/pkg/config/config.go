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
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
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
	// linkLayers restricts the counter to ports of the listed link
	// layers (topology.LinkLayer* values). nil means every layer — used
	// for netdev-tree counters, which exist on any adapter with a
	// network interface. The IB-tree counters below are driver specific:
	// mlx5 exposes the IBTA port counters and RoCE hw_counters, while the
	// efa driver exposes its own hw_counters set, and reading one
	// driver's counters on the other's ports is guaranteed to fail.
	linkLayers []string
}

var (
	mlx5LinkLayers = []string{topology.LinkLayerInfiniBand, topology.LinkLayerEthernet}
	efaLinkLayers  = []string{topology.LinkLayerEFA}
)

var counterDefinitions = map[string]counterDefinition{
	// /sys/class/infiniband/<dev>/ports/<port>/counters/
	"excessive_buffer_overrun_errors": {
		path:        "counters/excessive_buffer_overrun_errors",
		isFatal:     true,
		description: "HCA internal buffer overflow - lossless contract violated",
		linkLayers:  mlx5LinkLayers,
	},
	"link_downed": {
		path:        "counters/link_downed",
		isFatal:     true,
		description: "Port Training State Machine failed - QP disconnect",
		linkLayers:  mlx5LinkLayers,
	},
	"link_error_recovery": {
		path:        "counters/link_error_recovery",
		isFatal:     false,
		description: "Link retraining events - micro-flapping",
		linkLayers:  mlx5LinkLayers,
	},
	"local_link_integrity_errors": {
		path:        "counters/local_link_integrity_errors",
		isFatal:     true,
		description: "Physical errors exceed LocalPhyErrors hardware cap",
		linkLayers:  mlx5LinkLayers,
	},
	"port_rcv_discards": {
		path:        "counters/port_rcv_discards",
		isFatal:     false,
		description: "RX discards due to congestion or buffer pressure",
		linkLayers:  mlx5LinkLayers,
	},
	"port_rcv_errors": {
		path:        "counters/port_rcv_errors",
		isFatal:     false,
		description: "Malformed packets received",
		linkLayers:  mlx5LinkLayers,
	},
	"port_rcv_remote_physical_errors": {
		path:        "counters/port_rcv_remote_physical_errors",
		isFatal:     false,
		description: "Remote physical-layer errors received on this port",
		linkLayers:  mlx5LinkLayers,
	},
	"port_rcv_switch_relay_errors": {
		path:        "counters/port_rcv_switch_relay_errors",
		isFatal:     false,
		description: "Packets discarded because switch relay forwarding failed",
		linkLayers:  mlx5LinkLayers,
	},
	"port_xmit_discards": {
		path:        "counters/port_xmit_discards",
		isFatal:     false,
		description: "TX discards due to congestion",
		linkLayers:  mlx5LinkLayers,
	},
	"port_xmit_wait": {
		path:        "counters/port_xmit_wait",
		isFatal:     false,
		description: "TX wait ticks - congestion backpressure",
		linkLayers:  mlx5LinkLayers,
	},
	"symbol_error": {
		path:        "counters/symbol_error",
		isFatal:     false,
		description: "PHY bit errors before FEC - physical layer degradation",
		linkLayers:  mlx5LinkLayers,
	},
	"symbol_error_fatal": {
		path:        "counters/symbol_error",
		isFatal:     true,
		description: "Symbol errors exceed IBTA BER threshold (10E-12) - link outside spec",
		linkLayers:  mlx5LinkLayers,
	},

	// /sys/class/infiniband/<dev>/ports/<port>/hw_counters/
	"implied_nak_seq_err": {
		path:        "hw_counters/implied_nak_seq_err",
		isFatal:     false,
		description: "Implied NAK sequence errors - retransmission pressure",
		linkLayers:  mlx5LinkLayers,
	},
	"local_ack_timeout_err": {
		path:        "hw_counters/local_ack_timeout_err",
		isFatal:     false,
		description: "ACK timeout - potential fabric black hole",
		linkLayers:  mlx5LinkLayers,
	},
	"out_of_sequence": {
		path:        "hw_counters/out_of_sequence",
		isFatal:     false,
		description: "Fabric routing issues - out of sequence packets",
		linkLayers:  mlx5LinkLayers,
	},
	"packet_seq_err": {
		path:        "hw_counters/packet_seq_err",
		isFatal:     false,
		description: "Packet sequence errors - retransmission pressure",
		linkLayers:  mlx5LinkLayers,
	},
	"req_transport_retries_exceeded": {
		path:        "hw_counters/req_transport_retries_exceeded",
		isFatal:     true,
		description: "Requester transport retry limit exceeded",
		linkLayers:  mlx5LinkLayers,
	},
	"rnr_nak_retry_err": {
		path:        "hw_counters/rnr_nak_retry_err",
		isFatal:     true,
		description: "Receiver Not Ready NAK retry exhausted - connection severed",
		linkLayers:  mlx5LinkLayers,
	},
	"roce_slow_restart": {
		path:        "hw_counters/roce_slow_restart",
		isFatal:     false,
		description: "Victim flow oscillation",
		linkLayers:  mlx5LinkLayers,
	},

	// AWS Elastic Fabric Adapter (efa driver). Per-port statistics live
	// under /sys/class/infiniband/<dev>/ports/<port>/hw_counters/ ...
	"efa_rx_drops": {
		path:        "hw_counters/rx_drops",
		isFatal:     false,
		description: "EFA RX packets dropped by the adapter - fabric congestion or receive pressure",
		linkLayers:  efaLinkLayers,
	},
	"efa_rdma_read_wr_err": {
		path:        "hw_counters/rdma_read_wr_err",
		isFatal:     false,
		description: "EFA RDMA read work requests completed with error",
		linkLayers:  efaLinkLayers,
	},
	"efa_rdma_write_wr_err": {
		path:        "hw_counters/rdma_write_wr_err",
		isFatal:     false,
		description: "EFA RDMA write work requests completed with error",
		linkLayers:  efaLinkLayers,
	},
	"efa_unresponsive_remote_err": {
		path:        "hw_counters/unresponsive_remote_err",
		isFatal:     false,
		description: "EFA remote peer unresponsive - potential fabric black hole",
		linkLayers:  efaLinkLayers,
	},
	"efa_impaired_remote_conn_err": {
		path:        "hw_counters/impaired_remote_conn_err",
		isFatal:     false,
		description: "EFA connection to remote peer impaired - excessive packet loss",
		linkLayers:  efaLinkLayers,
	},

	// ... and device-wide admin-queue statistics under
	// /sys/class/infiniband/<dev>/hw_counters/ (the "device_hw_counters/"
	// path class). EFA adapters expose a single port, so these are
	// evaluated and reported against port 1.
	"efa_no_completion_cmds": {
		path:        "device_hw_counters/no_completion_cmds",
		isFatal:     true,
		description: "EFA admin command never completed - device firmware unresponsive",
		linkLayers:  efaLinkLayers,
	},
	"efa_cmds_err": {
		path:        "device_hw_counters/cmds_err",
		isFatal:     false,
		description: "EFA admin commands failed - control plane errors",
		linkLayers:  efaLinkLayers,
	},
	"efa_keep_alive_rcvd": {
		path:        "device_hw_counters/keep_alive_rcvd",
		isFatal:     false,
		description: "EFA keep-alive events received from the device",
		linkLayers:  efaLinkLayers,
	},
	"efa_reg_mr_err": {
		path:        "device_hw_counters/reg_mr_err",
		isFatal:     false,
		description: "EFA memory region registration failures",
		linkLayers:  efaLinkLayers,
	},
	"efa_create_qp_err": {
		path:        "device_hw_counters/create_qp_err",
		isFatal:     false,
		description: "EFA queue pair creation failures",
		linkLayers:  efaLinkLayers,
	},
	"efa_create_cq_err": {
		path:        "device_hw_counters/create_cq_err",
		isFatal:     false,
		description: "EFA completion queue creation failures",
		linkLayers:  efaLinkLayers,
	},
	"efa_create_ah_err": {
		path:        "device_hw_counters/create_ah_err",
		isFatal:     false,
		description: "EFA address handle creation failures",
		linkLayers:  efaLinkLayers,
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

	// CharDeviceCheck tunes the InfiniBandCharDeviceCheck.
	CharDeviceCheck CharDeviceCheckConfig `toml:"charDeviceCheck"`
}

// IssmMode selects whether InfiniBandCharDeviceCheck expects the per-port
// issm character device.
//
// issm presence is architecture- and provider-dependent and, unlike umad, is
// NOT reliably signalled by any port attribute: on a fabric run by an external
// subnet manager every compute port reads SM_DISABLED (cap_mask bit 0x400)
// whether or not issm exists, so that bit cannot gate the check. The
// expectation is therefore an explicit operator opt-in rather than an
// inferred default.
const (
	// IssmModeNever never expects issm. This is the default: umad and uverbs
	// still cover the RDMA-fatal cases, and a platform that legitimately does
	// not create issm nodes (e.g. GB300) is never falsely flagged.
	IssmModeNever = "never"
	// IssmModeAlways expects one issm per InfiniBand-mode port (the pre-1663
	// behaviour). Enable it only where issm is guaranteed to be created.
	IssmModeAlways = "always"
)

// CharDeviceCheckConfig tunes InfiniBandCharDeviceCheck. umad and uverbs are
// always required; only the issm expectation is configurable because its
// presence is architecture-dependent.
type CharDeviceCheckConfig struct {
	// Issm is one of "never" (default) or "always".
	Issm string `toml:"issm"`
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
	// LinkLayers is the set of port link layers the counter exists on
	// (owned by code, see counterDefinition.linkLayers). Empty means all.
	LinkLayers []string `toml:"-"`
}

// AppliesToLinkLayer reports whether the counter should be read on a port
// of the given link layer. Counters without a layer restriction apply
// everywhere.
func (c *CounterConfig) AppliesToLinkLayer(linkLayer string) bool {
	if len(c.LinkLayers) == 0 {
		return true
	}

	for _, l := range c.LinkLayers {
		if strings.EqualFold(l, linkLayer) {
			return true
		}
	}

	return false
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

	if cfg.CharDeviceCheck.Issm == "" {
		cfg.CharDeviceCheck.Issm = IssmModeNever
	}

	if err := validateIssmMode(cfg.CharDeviceCheck.Issm); err != nil {
		return nil, fmt.Errorf("invalid charDeviceCheck: %w", err)
	}

	return cfg, nil
}

// validateIssmMode rejects any charDeviceCheck.issm value outside the
// supported set so a typo fails fast at startup rather than silently
// falling back to a mode the operator did not choose.
func validateIssmMode(mode string) error {
	switch mode {
	case IssmModeNever, IssmModeAlways:
		return nil
	default:
		return fmt.Errorf("issm %q must be one of %q, %q", mode, IssmModeNever, IssmModeAlways)
	}
}

func validateRegexList(commaSeparated string) error {
	if commaSeparated == "" {
		return nil
	}

	for pat := range strings.SplitSeq(commaSeparated, ",") {
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

	for pat := range strings.SplitSeq(commaSeparated, ",") {
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
	c.LinkLayers = def.linkLayers

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
