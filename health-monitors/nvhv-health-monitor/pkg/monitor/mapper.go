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

// Package monitor turns NVHV verdicts into NVSentinel health events (ADR-076).
//
// Faults are reported as unhealthy events only, with no recovery event: only the
// outage events (NVHVUnreachable, NVHV<kind>CollectionFailure) get a healthy
// one. The processing strategy is the monitor's, STORE_ONLY by default, which
// records faults for observability and never asks NVSentinel to act on them.
package monitor

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdict"
)

const (
	// CheckNamePrefix keeps NVHV check names from colliding with the check
	// names of the monitors NVHV's checks replace.
	CheckNamePrefix = "NVHV"

	entityGPUUUID = "GPU_UUID"
	entityPCI     = "PCI"

	gpuUUIDPrefix = "GPU-"

	// observedGPU is the observed value that names the GPU a result is about.
	observedGPU = "gpu_id"
)

var pciAddress = regexp.MustCompile(`^(?:[0-9a-fA-F]{4,}:)?[0-9a-fA-F]{2}:[01][0-9a-fA-F](?:\.[0-7])?$`)

// fault is one reported non-PASS check result.
type fault struct {
	checkID     string
	entities    []string // sorted affected entities, part of the fault's identity
	fingerprint string   // changes when the result changes in a way worth re-reporting
}

// key identifies a fault across polls.
func (f fault) key() string { return f.checkID + "|" + strings.Join(f.entities, ",") }

// evaluated reports whether a result was really evaluated: not skipped, not a
// collection failure, and not UNKNOWN (which says nothing about the node).
func evaluated(r verdict.Result) bool {
	if r.Skipped || isCollectionFailure(r) {
		return false
	}

	switch r.Severity {
	case verdict.Pass, verdict.Info, verdict.Warning, verdict.Critical:
		return true
	default:
		return false
	}
}

// reportable reports whether a result is a fault: evaluated, and not PASS.
func reportable(r verdict.Result) bool {
	return evaluated(r) && r.Severity != verdict.Pass
}

func newFault(r verdict.Result) fault {
	return fault{
		checkID:  r.CheckID,
		entities: slices.Sorted(slices.Values(r.AffectedEntities)),
		fingerprint: fmt.Sprintf("%s|%s|%s", r.Severity, recommendedAction(r),
			strings.Join(r.Messages, "; ")),
	}
}

// faultEvent is the unhealthy event of a reportable result.
func (m *Monitor) faultEvent(r verdict.Result) *pb.HealthEvent {
	return &pb.HealthEvent{
		Version:            1,
		Agent:              m.cfg.Agent,
		NodeName:           m.cfg.NodeName,
		ComponentClass:     strings.ToUpper(r.Category),
		CheckName:          CheckNamePrefix + r.CheckID,
		IsFatal:            r.Severity == verdict.Critical,
		Message:            strings.Join(r.Messages, "; "),
		RecommendedAction:  recommendedAction(r),
		ErrorCode:          []string{r.CheckID},
		EntitiesImpacted:   entitiesOf(r),
		GeneratedTimestamp: timestamppb.New(m.cfg.Now()),
		ProcessingStrategy: m.cfg.ProcessingStrategy,
	}
}

// recommendedAction follows the ADR: a "monitor" operational state needs no
// action; otherwise the first remediation action decides.
func recommendedAction(r verdict.Result) pb.RecommendedAction {
	if r.OperationalState == "monitor" {
		return pb.RecommendedAction_NONE
	}

	actions := r.RemediationActions
	if len(actions) == 0 {
		return pb.RecommendedAction_NONE
	}

	switch actions[0] {
	case "reset_gpu":
		return pb.RecommendedAction_COMPONENT_RESET
	case "restart_os":
		return pb.RecommendedAction_RESTART_VM
	}

	// service_required, stop_p2p_traffic, hold and every other action need a
	// person: NVSentinel has no automated step for them.
	return pb.RecommendedAction_CONTACT_SUPPORT
}

// entitiesOf returns the GPU UUIDs from the affected entities and the PCI
// addresses found among the observed strings.
func entitiesOf(r verdict.Result) []*pb.Entity {
	var out []*pb.Entity

	for _, e := range r.AffectedEntities {
		if strings.HasPrefix(e, gpuUUIDPrefix) {
			out = append(out, &pb.Entity{EntityType: entityGPUUUID, EntityValue: e})
		}
	}

	for _, k := range slices.Sorted(maps.Keys(r.ObservedStrings)) {
		if v := r.ObservedStrings[k]; pciAddress.MatchString(v) {
			out = append(out, &pb.Entity{EntityType: entityPCI, EntityValue: v})
		}
	}

	return out
}
