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
	pending   *Evaluator

	// saveFailed records that the last state-file Save failed, so the
	// next commit retries even when nothing changed.
	saveFailed bool
}

var _ checks.TransactionalCheck = (*EthernetDegradationCheck)(nil)

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

// Run executes and commits one poll for direct callers. The production
// monitor uses Prepare/Commit/Discard so publication succeeds before state
// advances.
func (c *EthernetDegradationCheck) Run() ([]*pb.HealthEvent, error) {
	events, err := c.Prepare()
	if err != nil {
		return nil, err
	}

	c.Commit()

	return events, nil
}

// Prepare evaluates one poll against a cloned evaluator and stages the
// resulting counter state without mutating the committed evaluator. The
// discovery completeness policy lives in prepareCounterPoll.
func (c *EthernetDegradationCheck) Prepare() ([]*pb.HealthEvent, error) {
	c.Discard()

	candidate, events, err := prepareCounterPoll(c.reader, c.cfg, c.evaluator, c.evaluateDevices)
	if err != nil {
		return nil, err
	}

	if candidate == nil {
		return nil, nil
	}

	c.pending = candidate

	return events, nil
}

// evaluateDevices runs the enabled IB-tree counters plus the net-statistics
// counters for every Ethernet/RoCE port on supported (or explicitly
// pinned) devices against the candidate evaluator.
func (c *EthernetDegradationCheck) evaluateDevices(
	candidate *Evaluator, devices []discovery.IBDevice,
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for i := range devices {
		dev := &devices[i]

		if !dev.IncludedByOverride && !discovery.IsSupportedVendor(dev) {
			continue
		}

		for j := range dev.Ports {
			port := &dev.Ports[j]

			if !discovery.IsEthernetPort(port) {
				continue
			}

			events = append(events, candidate.EvaluateCounters(
				dev, port, c.cfg.CounterDetection.Counters, c.Name(),
			)...)

			if dev.NetDev != "" {
				events = append(events, candidate.EvaluateNetCounters(
					dev, port, c.cfg.CounterDetection.Counters, c.Name(),
				)...)
			}
		}
	}

	return events
}

// Commit installs and persists the most recently prepared evaluator state.
func (c *EthernetDegradationCheck) Commit() {
	if c.pending == nil {
		return
	}

	c.evaluator = c.pending
	c.pending = nil
	c.persist()
}

// Discard abandons a prepared poll after check or publication failure.
func (c *EthernetDegradationCheck) Discard() {
	c.pending = nil
}

func (c *EthernetDegradationCheck) persist() {
	snapshotsChanged := c.state.UpdateCounterSnapshots(c.evaluator.Snapshots())
	flagsChanged := c.state.UpdateBreachFlags(c.evaluator.BreachFlags())

	if !snapshotsChanged && !flagsChanged && !c.saveFailed {
		return
	}

	if err := c.state.Save(); err != nil {
		c.saveFailed = true

		slog.Warn("Failed to persist counter state to disk",
			"check", c.Name(), "error", err)

		return
	}

	c.saveFailed = false
}
