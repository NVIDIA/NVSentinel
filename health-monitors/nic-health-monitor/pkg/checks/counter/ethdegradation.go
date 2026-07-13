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

package counter

import (
	"fmt"
	"log/slog"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
)

// EthernetDegradationCheck monitors Ethernet/RoCE counter thresholds.
// It evaluates both the InfiniBand-style hw_counters/counters that the
// mlx5 driver exposes for RoCE devices and the interface-level
// /sys/class/net/<iface>/statistics/carrier_changes counter.
type EthernetDegradationCheck struct {
	nodeName  string
	reader    sysfs.Reader
	cfg       *config.Config
	state     *statefile.Manager
	evaluator *Evaluator
}

// NewEthernetDegradationCheck creates a new EthernetDegradationCheck.
func NewEthernetDegradationCheck(
	nodeName string,
	reader sysfs.Reader,
	cfg *config.Config,
	processingStrategy pb.ProcessingStrategy,
	state *statefile.Manager,
	bootIDChanged bool,
) *EthernetDegradationCheck {
	evaluator := NewEvaluator(
		nodeName, reader, processingStrategy,
		state.CounterSnapshots(), state.BreachFlags(), bootIDChanged,
	)

	return &EthernetDegradationCheck{
		nodeName:  nodeName,
		reader:    reader,
		cfg:       cfg,
		state:     state,
		evaluator: evaluator,
	}
}

// Name returns the check identifier.
func (c *EthernetDegradationCheck) Name() string {
	return checks.EthernetDegradationCheckName
}

// Run executes a single Ethernet/RoCE counter degradation poll.
func (c *EthernetDegradationCheck) Run() ([]*pb.HealthEvent, error) {
	result, err := discovery.DiscoverDevicesWithOverride(
		c.reader, c.cfg.NicExclusionRegex, c.cfg.NicInclusionRegexOverride,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to discover devices: %w", err)
	}

	var events []*pb.HealthEvent

	for i := range result.Devices {
		dev := &result.Devices[i]

		if !dev.IncludedByOverride && !discovery.IsSupportedVendor(dev) {
			continue
		}

		for j := range dev.Ports {
			port := &dev.Ports[j]

			if !discovery.IsEthernetPort(port) {
				continue
			}

			ibEvents := c.evaluator.EvaluateCounters(
				dev, port, c.cfg.CounterDetection.Counters, c.Name(),
			)
			events = append(events, ibEvents...)

			if dev.NetDev != "" {
				netEvents := c.evaluator.EvaluateNetCounters(
					dev, port, c.cfg.CounterDetection.Counters, c.Name(),
				)
				events = append(events, netEvents...)
			}
		}
	}

	c.evaluator.ClearBootIDFlag()
	c.persist()

	return events, nil
}

func (c *EthernetDegradationCheck) persist() {
	snapshotsChanged := c.state.UpdateCounterSnapshots(c.evaluator.Snapshots())
	flagsChanged := c.state.UpdateBreachFlags(c.evaluator.BreachFlags())

	if !snapshotsChanged && !flagsChanged {
		return
	}

	if err := c.state.Save(); err != nil {
		slog.Warn("Failed to persist counter state to disk",
			"check", c.Name(), "error", err)
	}
}
