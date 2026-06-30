// Copyright 2026 k8s-gpu-mcp-server contributors
// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tools

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// Pattern categories ported from donor's pkg/incidents.
const (
	CategoryThermalCascade = "thermal_cascade"
	CategoryECCFailure     = "ecc_failure"
	CategorySoftwareOOM    = "software_oom"
	CategoryNVLinkFailure  = "nvlink_failure"
	CategoryXID79BusError  = "xid_79_bus_error"
	CategoryUnknown        = "unknown"
)

// Recommendation priorities.
const (
	PriorityHigh   = "high"
	PriorityMedium = "medium"
	PriorityLow    = "low"
)

// drainNodeCommand is the canonical kubectl drain invocation embedded in every
// pattern recommendation that asks for node evacuation. Hoisted to a constant
// because it repeats four times across pattern recommendations and the
// goconst linter (rightly) flags the repetition.
const drainNodeCommand = "kubectl drain {{.Node}} --ignore-daemonsets --delete-emptydir-data"

// Recommendation is an actionable step a pattern attaches to its matches.
// Templates may use {{.Node}}, {{.PodName}}, {{.Namespace}}, {{.GPUUUID}}
// placeholders; substitution is the responsibility of callers (Task 12's
// explain_failure and Task 13's get_incident_report).
type Recommendation struct {
	Action   string `json:"action"`
	Command  string `json:"command,omitempty"`
	Priority string `json:"priority"`
}

// FailurePattern is the simplified, NVSentinel-event-adapted form of the
// donor's pattern definition. NVSentinel's store carries only HealthEvent
// objects (no snapshot-time temperature, ECC count, throttle reasons, or mem
// utilisation), so the indicator types are narrowed to what event fields can
// support: XID codes via ErrorCode and substring matches against Message.
type FailurePattern struct {
	Name string

	Category string

	// XIDCodes is the set of XID error codes that match this pattern. An
	// event with any matching code in ErrorCode contributes one indicator
	// to the score.
	XIDCodes []int

	// MessagePhrases is the set of substrings to search in event Message.
	// Any matching phrase contributes one indicator to the score. Match is
	// case-sensitive — phrases should be authored to match real monitor
	// output (e.g., "OOMKilled", "NVLink").
	MessagePhrases []string

	NotYourCode bool

	Recommendations []Recommendation
}

// Incident is one matched pattern. Confidence is the fraction of the
// pattern's indicators that matched (0..1).
type Incident struct {
	PatternName     string           `json:"pattern_name"`
	Category        string           `json:"category"`
	Confidence      float64          `json:"confidence"`
	Evidence        []string         `json:"evidence"`
	NotYourCode     bool             `json:"not_your_code"`
	Recommendations []Recommendation `json:"recommendations"`
}

// KnownIncidentPatterns is the registry of failure patterns ported from
// donor's pkg/incidents/patterns.go, adapted to NVSentinel's event-only
// indicators. Recommendations are kept verbatim; future monitor extensions
// (e.g., persisting GPU temperature into the store) would let us reintroduce
// the donor's richer indicator set.
var KnownIncidentPatterns = []FailurePattern{
	{
		Name:     "xid_79_bus_error",
		Category: CategoryXID79BusError,
		XIDCodes: []int{79},
		MessagePhrases: []string{
			"bus fall-off",
			"GPU has fallen off the bus",
		},
		NotYourCode: true,
		Recommendations: []Recommendation{
			{
				Action:   "DRAIN NODE IMMEDIATELY - GPU hardware failure",
				Command:  drainNodeCommand,
				Priority: PriorityHigh,
			},
			{
				Action:   "Check PCIe bus status",
				Command:  "lspci -vvv | grep -A20 'NVIDIA'",
				Priority: PriorityHigh,
			},
			{
				Action:   "Check dmesg for PCIe errors",
				Command:  "dmesg | grep -i 'pci\\|nvidia' | tail -50",
				Priority: PriorityHigh,
			},
			{
				Action:   "Schedule GPU replacement - hardware failure confirmed",
				Priority: PriorityHigh,
			},
		},
	},
	{
		Name:           "nvlink_failure",
		Category:       CategoryNVLinkFailure,
		XIDCodes:       []int{74},
		MessagePhrases: []string{"NVLink", "interconnect"},
		NotYourCode:    true,
		Recommendations: []Recommendation{
			{
				Action:   "Check NVLink topology and status",
				Command:  "nvidia-smi nvlink -s",
				Priority: PriorityHigh,
			},
			{
				Action:   "Inspect NVLink error counters",
				Command:  "nvidia-smi nvlink -e",
				Priority: PriorityHigh,
			},
			{
				Action:   "Drain node if multi-GPU workload affected",
				Command:  drainNodeCommand,
				Priority: PriorityMedium,
			},
			{
				Action:   "Check physical NVLink cable connections",
				Priority: PriorityMedium,
			},
		},
	},
	{
		Name:        "ecc_failure",
		Category:    CategoryECCFailure,
		XIDCodes:    []int{48, 63, 64, 68, 69, 8, 92},
		NotYourCode: true,
		Recommendations: []Recommendation{
			{
				Action:   "DRAIN NODE IMMEDIATELY - Memory corruption detected",
				Command:  drainNodeCommand,
				Priority: PriorityHigh,
			},
			{
				Action:   "Check ECC error counts",
				Command:  "nvidia-smi -q -d ECC",
				Priority: PriorityHigh,
			},
			{
				Action:   "Schedule GPU replacement",
				Command:  "kubectl label node {{.Node}} gpu-health=degraded",
				Priority: PriorityMedium,
			},
		},
	},
	{
		Name:           "software_oom",
		Category:       CategorySoftwareOOM,
		MessagePhrases: []string{"OOMKilled", "out of memory", "CUDA out of memory"},
		NotYourCode:    false,
		Recommendations: []Recommendation{
			{
				Action:   "Review pod memory limits and GPU memory usage",
				Command:  "kubectl describe pod {{.PodName}} -n {{.Namespace}}",
				Priority: PriorityHigh,
			},
			{
				Action:   "Check application memory allocation patterns",
				Command:  "kubectl logs {{.PodName}} -n {{.Namespace}} --previous | tail -100",
				Priority: PriorityMedium,
			},
			{
				Action:   "Consider increasing GPU memory request or optimizing model",
				Priority: PriorityMedium,
			},
		},
	},
	{
		Name:           "thermal_cascade",
		Category:       CategoryThermalCascade,
		XIDCodes:       []int{79, 43},
		MessagePhrases: []string{"thermal", "overheating", "temperature"},
		NotYourCode:    true,
		Recommendations: []Recommendation{
			{
				Action:   "Check node cooling and airflow",
				Command:  "kubectl describe node {{.Node}} | grep -A5 'Conditions'",
				Priority: PriorityHigh,
			},
			{
				Action:   "Drain node for cooling investigation",
				Command:  drainNodeCommand,
				Priority: PriorityHigh,
			},
			{
				Action:   "Check GPU temperature and throttling",
				Command:  "nvidia-smi -q -d TEMPERATURE,PERFORMANCE",
				Priority: PriorityMedium,
			},
		},
	},
}

// MatchIncidents runs the pattern registry over a slice of stored health
// events and returns every pattern that scored above zero, sorted by
// confidence descending. Callers can take incidents[0] as the primary
// diagnosis or iterate to surface secondary signals.
func MatchIncidents(events []datastore.HealthEventWithStatus) []Incident {
	xidCodes, messages := extractSignals(events)

	incidents := make([]Incident, 0, len(KnownIncidentPatterns))

	for _, p := range KnownIncidentPatterns {
		confidence, evidence := scorePattern(p, xidCodes, messages)
		if confidence == 0 {
			continue
		}

		incidents = append(incidents, Incident{
			PatternName:     p.Name,
			Category:        p.Category,
			Confidence:      confidence,
			Evidence:        evidence,
			NotYourCode:     p.NotYourCode,
			Recommendations: append([]Recommendation{}, p.Recommendations...),
		})
	}

	sort.SliceStable(incidents, func(i, j int) bool {
		return incidents[i].Confidence > incidents[j].Confidence
	})

	return incidents
}

// extractSignals collapses a slice of events into the two indicator
// dimensions the matcher uses: numeric XID codes (parsed from ErrorCode
// entries) and a list of message strings.
func extractSignals(events []datastore.HealthEventWithStatus) (map[int]struct{}, []string) {
	xidCodes := map[int]struct{}{}

	var messages []string

	for i := range events {
		he, ok := events[i].HealthEvent.(*protos.HealthEvent)
		if !ok || he == nil {
			continue
		}

		for _, code := range he.GetErrorCode() {
			if n, err := strconv.Atoi(strings.TrimSpace(code)); err == nil {
				xidCodes[n] = struct{}{}
			}
		}

		if msg := he.GetMessage(); msg != "" {
			messages = append(messages, msg)
		}
	}

	return xidCodes, messages
}

// scorePattern returns the confidence (fraction of indicators that fired)
// and the evidence strings backing that score. Patterns with zero declared
// indicators are skipped (returns 0).
func scorePattern(p FailurePattern, xidCodes map[int]struct{}, messages []string) (float64, []string) {
	indicators := 0

	if len(p.XIDCodes) > 0 {
		indicators++
	}

	if len(p.MessagePhrases) > 0 {
		indicators++
	}

	if indicators == 0 {
		return 0, nil
	}

	matched := 0

	var evidence []string

	if xidEvidence := matchXIDCode(p.XIDCodes, xidCodes); xidEvidence != "" {
		matched++

		evidence = append(evidence, xidEvidence)
	}

	if msgEvidence := matchMessagePhrase(p.MessagePhrases, messages); msgEvidence != "" {
		matched++

		evidence = append(evidence, msgEvidence)
	}

	if matched == 0 {
		return 0, nil
	}

	return float64(matched) / float64(indicators), evidence
}

func matchXIDCode(want []int, have map[int]struct{}) string {
	for _, code := range want {
		if _, ok := have[code]; ok {
			return fmt.Sprintf("XID %d present in error codes", code)
		}
	}

	return ""
}

func matchMessagePhrase(phrases []string, messages []string) string {
	for _, phrase := range phrases {
		for _, msg := range messages {
			if strings.Contains(msg, phrase) {
				return fmt.Sprintf("Message contains %q", phrase)
			}
		}
	}

	return ""
}
