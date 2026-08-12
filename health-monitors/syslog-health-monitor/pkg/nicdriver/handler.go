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

package nicdriver

import (
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const mlx5CoreDriver = "mlx5_core"

// NewNICDriverHandler creates a handler from the configured pattern file and
// sysfs root. Invalid pattern configuration is treated as a startup error.
func NewNICDriverHandler(
	nodeName, defaultAgentName, checkName, configPath, sysfsRoot string,
	processingStrategy pb.ProcessingStrategy,
) (*NICDriverHandler, error) {
	patterns, err := LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load NIC driver pattern config from %s: %w", configPath, err)
	}

	slog.Info("Loaded NIC driver syslog patterns",
		"configPath", configPath,
		"enabledPatterns", len(patterns))

	resolver := NewSysfsResolver(sysfsRoot)

	PreInitialize(nodeName, patterns)

	return newWithDeps(nodeName, defaultAgentName, checkName, patterns, resolver, processingStrategy), nil
}

// newWithDeps creates a handler with pre-built dependencies for focused tests.
// The mlx5_napi_soft_lockup pattern is routed to the stateful soft-lockup
// detector instead of the single-line matching loop: its evidence spans
// multiple journal entries and must never match a lone stack-frame line.
func newWithDeps(
	nodeName, defaultAgentName, checkName string,
	patterns []CompiledPattern,
	resolver Resolver,
	processingStrategy pb.ProcessingStrategy,
) *NICDriverHandler {
	h := &NICDriverHandler{
		nodeName:           nodeName,
		defaultAgentName:   defaultAgentName,
		checkName:          checkName,
		processingStrategy: processingStrategy,
		resolver:           resolver,
	}

	singleLine := make([]CompiledPattern, 0, len(patterns))

	for _, p := range patterns {
		if p.Name == softLockupPatternName {
			h.lockup = newSoftLockupDetector(p)
			continue
		}

		singleLine = append(singleLine, p)
	}

	h.patterns = singleLine

	return h
}

// ProcessLine evaluates the kernel log message against configured patterns.
// First match wins. Returns nil when no pattern matches.
//
// The soft-lockup detector observes every line (even ones that also match a
// single-line pattern would have counted against its window) and takes
// precedence when it confirms, since its event is fatal.
//
// BDF lookup is best-effort entity enrichment only; it does not gate event
// emission because some mlx5 log lines do not include a PCI address.
func (h *NICDriverHandler) ProcessLine(message string) (*pb.HealthEvents, error) {
	if h.lockup != nil {
		if m := h.lockup.observe(message); m != nil {
			return h.buildLockupEvent(m), nil
		}
	}

	for i := range h.patterns {
		p := &h.patterns[i]
		if !p.Re.MatchString(message) {
			continue
		}

		bdf, hasBDF := extractBDF(message)

		return h.buildEvent(p, message, bdf, hasBDF), nil
	}

	return nil, nil
}

// buildLockupEvent emits the fatal mlx5 NAPI soft-lockup event. The kernel
// dump carries no PCI address, so the event is node-scoped (no NIC entity)
// and the parsed header fields are attached as metadata instead.
func (h *NICDriverHandler) buildLockupEvent(m *softLockupMatch) *pb.HealthEvents {
	events := h.buildEvent(&h.lockup.pattern, m.evidence, "", false)

	event := events.Events[0]
	event.Message = fmt.Sprintf(
		"kernel soft lockup: CPU#%s stuck for %ss in mlx5 NAPI poll loop (evidence: %s)",
		m.cpu, m.duration, m.evidence)
	event.Metadata = map[string]string{
		"cpu":             m.cpu,
		"durationSeconds": m.duration,
	}

	return events
}

func (h *NICDriverHandler) buildEvent(
	p *CompiledPattern, message, bdf string, hasBDF bool,
) *pb.HealthEvents {
	var entities []*pb.Entity

	// Best-effort NIC entity enrichment. Defensive guard: only attach a NIC
	// entity if the BDF actually resolves to mlx5_core.
	if hasBDF {
		if driver, device, ok := h.resolver.Resolve(bdf); ok && driver == mlx5CoreDriver && device != "" {
			entities = append(entities, &pb.Entity{
				EntityType:  "NIC",
				EntityValue: device,
			})
		}
	}

	sev := "non_fatal"
	if p.IsFatal {
		sev = "fatal"
	}

	nicDriverEventCounter.WithLabelValues(h.nodeName, p.Name, sev).Inc()

	processingStrategy := h.processingStrategy
	if p.HasProcessingStrategy {
		processingStrategy = p.ProcessingStrategy
	}

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              h.defaultAgentName,
		CheckName:          h.checkName,
		ComponentClass:     componentClassNIC,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted:   entities,
		Message:            message,
		IsFatal:            p.IsFatal,
		IsHealthy:          false,
		NodeName:           h.nodeName,
		RecommendedAction:  p.RecommendedAction,
		ErrorCode:          []string{p.Name},
		ProcessingStrategy: processingStrategy,
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}
}
