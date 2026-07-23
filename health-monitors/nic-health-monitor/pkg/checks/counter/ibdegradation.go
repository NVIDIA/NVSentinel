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

// InfiniBandDegradationCheck monitors InfiniBand counter thresholds for
// both fatal and non-fatal degradation detection. The check owns its own
// Evaluator but shares the persistent state file with all sibling
// checks (state and degradation, IB and Ethernet).
type InfiniBandDegradationCheck struct {
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

var _ checks.TransactionalCheck = (*InfiniBandDegradationCheck)(nil)

// NewInfiniBandDegradationCheck creates a new InfiniBandDegradationCheck.
// bootIDChanged is forwarded to the Evaluator so the first poll after a
// host reboot emits the check-scoped clear plus healthy baselines. The
// classifier scopes the check to the same devices the state checks
// monitor (management NICs excluded).
func NewInfiniBandDegradationCheck(
	nodeName string,
	reader sysfs.Reader,
	cfg *config.Config,
	classifier *topology.Classifier,
	processingStrategy pb.ProcessingStrategy,
	state *statefile.Manager,
	bootIDChanged bool,
) *InfiniBandDegradationCheck {
	// A baseline owed by a previous pod (deferred/partial window, then
	// restart) is picked up from the persisted flag; a fresh trigger is
	// registered so it survives partial-window commits.
	pendingBaseline := bootIDChanged || state.PendingBaseline(checks.InfiniBandDegradationCheckName)
	if pendingBaseline {
		state.SetPendingBaseline(checks.InfiniBandDegradationCheckName)
	}

	evaluator := NewEvaluator(
		nodeName, reader, processingStrategy,
		state.CounterSnapshots(), state.BreachFlags(), pendingBaseline,
	)

	return &InfiniBandDegradationCheck{
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
func (c *InfiniBandDegradationCheck) Name() string {
	return checks.InfiniBandDegradationCheckName
}

// Run executes and commits one poll for direct callers. The production
// monitor uses Prepare/Commit/Discard so publication succeeds before state
// advances.
func (c *InfiniBandDegradationCheck) Run() ([]*pb.HealthEvent, error) {
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
func (c *InfiniBandDegradationCheck) Prepare() ([]*pb.HealthEvent, error) {
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
func (c *InfiniBandDegradationCheck) pollDeps() counterPollDeps {
	return counterPollDeps{
		reader:             c.reader,
		cfg:                c.cfg,
		classifier:         c.classifier,
		nodeName:           c.nodeName,
		checkName:          c.Name(),
		processingStrategy: c.processingStrategy,
	}
}

// evaluateDevices runs the enabled IB-tree counters for every InfiniBand
// port on eligible (or explicitly pinned) devices against the candidate
// evaluator. Eligibility matches the state checks exactly — see
// checks.EligibleDevice.
func (c *InfiniBandDegradationCheck) evaluateDevices(
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

			if !discovery.IsIBPort(port) {
				continue
			}

			events = append(events, candidate.EvaluateCounters(
				dev, port, c.cfg.CounterDetection.Counters, c.Name(),
			)...)
		}
	}

	return events
}

// Commit installs and persists the most recently prepared evaluator state.
func (c *InfiniBandDegradationCheck) Commit() {
	if c.pending == nil {
		return
	}

	// The prepared poll ran the baseline reconciliation when it consumed
	// the pending flag the committed evaluator still carries.
	if c.evaluator.BootBaselinePending() && !c.pending.BootBaselinePending() {
		c.state.ClearPendingBaseline(checks.InfiniBandDegradationCheckName)
	}

	c.evaluator = c.pending
	c.pending = nil
	c.persist()
}

// Discard abandons a prepared poll after check or publication failure.
func (c *InfiniBandDegradationCheck) Discard() {
	c.pending = nil
}

// persist writes any changes to the shared state file. Errors are
// logged and swallowed — the design explicitly chooses not to halt
// monitoring on persistence failures.
func (c *InfiniBandDegradationCheck) persist() {
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
