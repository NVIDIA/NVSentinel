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

package checks

import (
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// FirstPollDeferralLimit bounds how many consecutive polls a check may
// defer its first evaluation because discovery reported unreadable
// devices. A partial first read cannot seed peer-evidence gating or
// trustworthy counter snapshots, so deferring briefly is safer than
// baselining from it — but a device that stays unreadable (e.g.,
// firmware wedged at boot) must not silently disable monitoring of
// every readable NIC forever.
const FirstPollDeferralLimit = 3

// baselineClearSkew backdates the check-scoped baseline clear event.
// The platform-connector sorts each batch by GeneratedTimestamp with a
// non-stable sort before applying it to node conditions, so equal
// timestamps carry no ordering guarantee; the skew makes the clear
// order strictly before the same batch's current-boot events, which it
// must never wipe.
const baselineClearSkew = time.Second

// EligibleDevice is the shared device-scope filter applied by every
// check — state and counter — before any per-port work. Normal
// discovery already excludes SR-IOV VFs and exclusion-regex matches;
// this handles the rest: an explicit inclusion-override match bypasses
// every filter, otherwise unsupported vendors and management-classified
// NICs are out of scope. Keeping all checks on one predicate guarantees
// a management NIC excluded from state monitoring can never emit
// counter-based REPLACE_VM events either.
func EligibleDevice(dev *discovery.IBDevice, classifier *topology.Classifier) bool {
	if dev.IncludedByOverride {
		return true
	}

	if !discovery.IsSupportedVendor(dev) {
		slog.Debug("Skipping unsupported vendor", "device", dev.Name, "vendor", dev.Vendor)
		return false
	}

	return !classifier.IsManagementNIC(dev.Name)
}

// NewBaselineClearEvent builds the healthy event emitted once per check
// on a baseline run (host reboot, or discovery-scope change for state
// checks). Empty EntitiesImpacted means "every entity of this check
// recovered" in both downstream consumers: the platform-connector
// clears the whole node condition and fault-quarantine removes every
// annotation key for the check. This is the only mechanism that clears
// conditions whose entities no longer exist on this boot (renamed
// devices, replaced hardware, removed counters) — the per-entity
// baselines that follow can only clear entities that are still
// observable.
func NewBaselineClearEvent(
	nodeName, checkName, message string,
	processingStrategy pb.ProcessingStrategy,
) *pb.HealthEvent {
	evt := NewHealthEvent(nodeName, checkName, message, nil,
		false, true, pb.RecommendedAction_NONE, processingStrategy)
	evt.GeneratedTimestamp = timestamppb.New(time.Now().Add(-baselineClearSkew))

	return evt
}

// EnsureClearPrecedesBatch backdates the check-scoped clear at
// events[0] so it is strictly older than every other event in the
// batch. The constructor's skew already handles the normal case; this
// pass makes the ordering immune to a backward wall-clock step between
// constructing the clear and the events that follow it (each event
// stamps its own time.Now()). It is a no-op when the batch has no
// following events or events[0] is not a clear.
func EnsureClearPrecedesBatch(events []*pb.HealthEvent) {
	if len(events) < 2 {
		return
	}

	clearEvt := events[0]
	if !clearEvt.IsHealthy || len(clearEvt.EntitiesImpacted) != 0 {
		return
	}

	earliest := events[1].GeneratedTimestamp.AsTime()
	for _, e := range events[2:] {
		if t := e.GeneratedTimestamp.AsTime(); t.Before(earliest) {
			earliest = t
		}
	}

	want := earliest.Add(-baselineClearSkew)
	if clearEvt.GeneratedTimestamp.AsTime().After(want) {
		clearEvt.GeneratedTimestamp = timestamppb.New(want)
	}
}

// NewHealthEvent builds a HealthEvent populated with the fields that are
// constant across all NIC checks (agent name, component class, timestamp).
// The caller supplies the per-event fields.
func NewHealthEvent(
	nodeName, checkName, message string,
	entities []*pb.Entity,
	isFatal, isHealthy bool,
	action pb.RecommendedAction,
	processingStrategy pb.ProcessingStrategy,
) *pb.HealthEvent {
	return &pb.HealthEvent{
		Version:            1,
		Agent:              AgentName,
		CheckName:          checkName,
		ComponentClass:     ComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		Message:            message,
		IsFatal:            isFatal,
		IsHealthy:          isHealthy,
		NodeName:           nodeName,
		RecommendedAction:  action,
		EntitiesImpacted:   entities,
		ProcessingStrategy: processingStrategy,
	}
}

// PortEntities returns the NIC + NICPort entity pair used on port-level
// events so downstream analyzers can pinpoint both the card and the port.
func PortEntities(device string, port int) []*pb.Entity {
	return []*pb.Entity{
		{EntityType: EntityTypeNIC, EntityValue: device},
		{EntityType: EntityTypePort, EntityValue: discovery.PortEntityValue(port)},
	}
}

// DeviceEntities returns a single NIC entity used on device-level events
// (e.g., disappearance).
func DeviceEntities(device string) []*pb.Entity {
	return []*pb.Entity{
		{EntityType: EntityTypeNIC, EntityValue: device},
	}
}
