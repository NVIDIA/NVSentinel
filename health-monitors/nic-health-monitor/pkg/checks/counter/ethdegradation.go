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
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// netdevDegradationCheck is the shared implementation behind
// EthernetDegradationCheck and EFADegradationCheck: a per-port counter
// poll over the IB-tree counters scoped to the check's link layer plus
// the interface-level /sys/class/net/<iface>/ counters when the device
// exposes a network interface. The two checks differ only in which
// ports they own and the check name they report under.
type netdevDegradationCheck struct {
	checkName    string
	isTargetPort func(port *discovery.IBPort) bool

	nodeName           string
	reader             sysfs.Reader
	cfg                *config.Config
	classifier         *topology.Classifier
	processingStrategy pb.ProcessingStrategy
	state              *statefile.Manager
	evaluator          *Evaluator
	pending            *Evaluator

	// saveFailed records that the last state-file Save failed, so the
	// next commit retries even when nothing changed.
	saveFailed bool

	// firstPollDeferrals counts consecutive polls deferred because the
	// first enumeration included unreadable devices. Poll bookkeeping,
	// deliberately outside the transactional commit (like saveFailed).
	firstPollDeferrals int
}

// EthernetDegradationCheck monitors Ethernet/RoCE counter thresholds.
// It evaluates both the InfiniBand-style hw_counters/counters that the
// mlx5 driver exposes for RoCE devices and the interface-level
// /sys/class/net/<iface>/statistics/carrier_changes counter.
type EthernetDegradationCheck struct {
	netdevDegradationCheck
}

// EFADegradationCheck monitors AWS Elastic Fabric Adapter counter
// thresholds: the efa driver's per-port hw_counters (rx_drops, RDMA
// work-request errors, remote-peer errors), its device-level
// admin-queue statistics (/sys/class/infiniband/<dev>/hw_counters/ —
// command failures and timeouts, read through the device_hw_counters/
// path class) and, when the adapter exposes a network interface, the
// interface-level netdev counters. Counters scoped to other link layers
// (the mlx5 IBTA and RoCE sets) are skipped by the evaluator, so one
// shared counter list serves every degradation check.
type EFADegradationCheck struct {
	netdevDegradationCheck
}

var _ checks.TransactionalCheck = (*EthernetDegradationCheck)(nil)
var _ checks.TransactionalCheck = (*EFADegradationCheck)(nil)

// NewEthernetDegradationCheck creates a new EthernetDegradationCheck.
// The classifier scopes the check to the same devices the state checks
// monitor (management NICs excluded).
func NewEthernetDegradationCheck(
	nodeName string,
	reader sysfs.Reader,
	cfg *config.Config,
	classifier *topology.Classifier,
	processingStrategy pb.ProcessingStrategy,
	state *statefile.Manager,
	bootIDChanged bool,
) *EthernetDegradationCheck {
	return &EthernetDegradationCheck{
		netdevDegradationCheck: newNetdevDegradationCheck(
			checks.EthernetDegradationCheckName, discovery.IsEthernetPort,
			nodeName, reader, cfg, classifier, processingStrategy, state, bootIDChanged,
		),
	}
}

// NewEFADegradationCheck creates a new EFADegradationCheck with the same
// wiring and baseline contract as the Ethernet check.
func NewEFADegradationCheck(
	nodeName string,
	reader sysfs.Reader,
	cfg *config.Config,
	classifier *topology.Classifier,
	processingStrategy pb.ProcessingStrategy,
	state *statefile.Manager,
	bootIDChanged bool,
) *EFADegradationCheck {
	return &EFADegradationCheck{
		netdevDegradationCheck: newNetdevDegradationCheck(
			checks.EFADegradationCheckName, discovery.IsEFAPort,
			nodeName, reader, cfg, classifier, processingStrategy, state, bootIDChanged,
		),
	}
}

// newNetdevDegradationCheck wires the shared implementation for one
// check name / port predicate pair.
func newNetdevDegradationCheck(
	checkName string,
	isTargetPort func(port *discovery.IBPort) bool,
	nodeName string,
	reader sysfs.Reader,
	cfg *config.Config,
	classifier *topology.Classifier,
	processingStrategy pb.ProcessingStrategy,
	state *statefile.Manager,
	bootIDChanged bool,
) netdevDegradationCheck {
	// A baseline owed by a previous pod (deferred/partial window, then
	// restart) is picked up from the persisted flag; a fresh trigger is
	// registered so it survives partial-window commits.
	pendingBaseline := bootIDChanged || state.PendingBaseline(checkName)
	if pendingBaseline {
		state.SetPendingBaseline(checkName)
	}

	evaluator := NewEvaluator(
		nodeName, reader, processingStrategy,
		state.CounterSnapshots(), state.BreachFlags(), pendingBaseline,
	)

	return netdevDegradationCheck{
		checkName:          checkName,
		isTargetPort:       isTargetPort,
		nodeName:           nodeName,
		reader:             reader,
		cfg:                cfg,
		classifier:         classifier,
		processingStrategy: processingStrategy,
		state:              state,
		evaluator:          evaluator,
	}
}

// Name returns the check identifier.
func (c *netdevDegradationCheck) Name() string {
	return c.checkName
}

// Run executes and commits one poll for direct callers. The production
// monitor uses Prepare/Commit/Discard so publication succeeds before state
// advances.
func (c *netdevDegradationCheck) Run() ([]*pb.HealthEvent, error) {
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
func (c *netdevDegradationCheck) Prepare() ([]*pb.HealthEvent, error) {
	c.Discard()

	candidate, events, err := prepareCounterPoll(c.pollDeps(), c.evaluator, &c.firstPollDeferrals, c.evaluateDevices)
	if err != nil {
		return nil, err
	}

	if candidate == nil {
		return nil, nil
	}

	c.pending = candidate

	return events, nil
}

// pollDeps assembles the per-check wiring the shared counter poll needs.
func (c *netdevDegradationCheck) pollDeps() counterPollDeps {
	return counterPollDeps{
		reader:             c.reader,
		cfg:                c.cfg,
		classifier:         c.classifier,
		nodeName:           c.nodeName,
		checkName:          c.Name(),
		processingStrategy: c.processingStrategy,
	}
}

// evaluateDevices runs the enabled IB-tree counters plus the net-statistics
// counters for every port this check owns on eligible (or explicitly
// pinned) devices against the candidate evaluator. Eligibility matches
// the state checks exactly — see checks.EligibleDevice.
func (c *netdevDegradationCheck) evaluateDevices(
	candidate *Evaluator, devices []discovery.IBDevice,
) []*pb.HealthEvent {
	var events []*pb.HealthEvent

	for i := range devices {
		dev := &devices[i]

		if !checks.EligibleDevice(dev, c.classifier) {
			continue
		}

		for j := range dev.Ports {
			port := &dev.Ports[j]

			if !c.isTargetPort(port) {
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
func (c *netdevDegradationCheck) Commit() {
	if c.pending == nil {
		return
	}

	// The prepared poll ran the baseline reconciliation when it consumed
	// the pending flag the committed evaluator still carries.
	if c.evaluator.BootBaselinePending() && !c.pending.BootBaselinePending() {
		c.state.ClearPendingBaseline(c.checkName)
	}

	c.evaluator = c.pending
	c.pending = nil
	c.persist()
}

// Discard abandons a prepared poll after check or publication failure.
func (c *netdevDegradationCheck) Discard() {
	c.pending = nil
}

func (c *netdevDegradationCheck) persist() {
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
