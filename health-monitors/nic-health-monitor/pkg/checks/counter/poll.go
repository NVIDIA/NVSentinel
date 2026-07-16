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

package counter

import (
	"fmt"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
)

// counterDiscoveryGate applies the shared completeness policy for a
// counter poll. done=true means Prepare must return early with err
// (nil err for the quiet no-op cases).
//
// An incomplete enumeration with committed counter history is a loud
// error: discarding the poll preserves snapshots, latches, and reset
// detection. Without history it is a quiet no-op so nodes without an IB
// tree stay silent and the post-reboot baseline is not consumed before
// the first complete enumeration. A partial first read likewise defers
// the baseline — it cannot establish trustworthy snapshots for every
// counter.
func counterDiscoveryGate(result *discovery.DiscoveryResult, hasState bool) (bool, error) {
	if !result.Complete {
		if hasState {
			return true, fmt.Errorf("failed to discover devices: InfiniBand sysfs tree unavailable")
		}

		return true, nil
	}

	if len(result.UnreadableDevices) > 0 && !hasState {
		return true, nil
	}

	return false, nil
}

// prepareCounterPoll runs the shared transactional counter poll:
// discover devices, apply the completeness gate, and evaluate against a
// cloned candidate evaluator. It returns a nil candidate when the gate
// defers the poll; on success the caller stages the candidate as its
// pending state.
func prepareCounterPoll(
	reader sysfs.Reader,
	cfg *config.Config,
	committed *Evaluator,
	evaluate func(candidate *Evaluator, devices []discovery.IBDevice) []*pb.HealthEvent,
) (*Evaluator, []*pb.HealthEvent, error) {
	result, err := discovery.DiscoverDevicesWithOverride(
		reader, cfg.NicExclusionRegex, cfg.NicInclusionRegexOverride,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to discover devices: %w", err)
	}

	if done, gateErr := counterDiscoveryGate(result, committed.HasState()); done {
		return nil, nil, gateErr
	}

	candidate := committed.Clone()
	events := evaluate(candidate, result.Devices)

	candidate.ClearBootIDFlag()

	return candidate, events, nil
}
