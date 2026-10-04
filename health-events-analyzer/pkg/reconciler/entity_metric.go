// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package reconciler

import (
	"fmt"
	"strconv"
	"strings"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
)

func parseBoundedHex(value string, minLen, maxLen int) (uint64, bool) {
	if len(value) < minLen || len(value) > maxLen {
		return 0, false
	}

	parsed, err := strconv.ParseUint(value, 16, 64)
	if err != nil {
		return 0, false
	}

	return parsed, true
}

// canonicalPCIValue accepts PCI addresses in the forms normalizePCI emits
// (optional 8-hex domain, optional .function) and rewrites them to
// domain:bus:device so mixed padding cannot split a series.
func canonicalPCIValue(value string) (string, bool) {
	parts := strings.Split(strings.ToLower(value), ":")
	if len(parts) != 3 {
		return "", false
	}

	domain, ok := parseBoundedHex(parts[0], 1, 8)
	if !ok || domain > 0xffff {
		return "", false
	}

	bus, ok := parseBoundedHex(parts[1], 1, 2)
	if !ok {
		return "", false
	}

	devicePart, function, hasFunction := strings.Cut(parts[2], ".")
	parsedFunction, validFunction := parseBoundedHex(function, 1, 2)

	if hasFunction && (!validFunction || parsedFunction > 7) {
		return "", false
	}

	device, ok := parseBoundedHex(devicePart, 1, 2)
	if !ok {
		return "", false
	}

	return fmt.Sprintf("%04x:%02x:%02x", domain, bus, device), true
}

func canonicalMetricEntityValue(entityType, entityValue string) (string, bool) {
	switch strings.ToLower(entityType) {
	case "gpu", "gpc", "tpc", "sm", "nvlink", "nicport":
		return entityValue, true
	case "pci":
		return canonicalPCIValue(entityValue)
	case "nic", "nvswitch":
		if len(entityValue) == 0 || len(entityValue) > 64 {
			return "", false
		}

		return entityValue, true
	default:
		return "", false
	}
}

func ruleSelectsOnEntity(rule config.HealthEventsAnalyzerRule) bool {
	for _, stage := range rule.Stage {
		if strings.Contains(strings.ToLower(stage), "entitiesimpacted") {
			return true
		}
	}

	return false
}

// metricSafeEntities returns copies of the triggering event's impacted
// entities that are safe Prometheus labels. Only PCI, GPU, GPC, TPC, SM,
// NVLINK, NIC, NICPort, and NVSwitch are kept, using the producer spelling.
// GPU UUID is omitted. PCI is rewritten to domain:bus:device; other values
// are used as received. Malformed PCI and overlong NIC/NVSwitch values are
// dropped.
func metricSafeEntities(event *protos.HealthEvent) []*protos.Entity {
	out := make([]*protos.Entity, 0, len(event.GetEntitiesImpacted()))

	for _, entity := range event.GetEntitiesImpacted() {
		entityType := entity.GetEntityType()
		entityValue := entity.GetEntityValue()

		if entityType == "" || entityValue == "" || strings.EqualFold(entityType, "GPU_UUID") {
			continue
		}

		canonicalValue, ok := canonicalMetricEntityValue(entityType, entityValue)
		if !ok {
			continue
		}

		out = append(out, &protos.Entity{
			EntityType:  entityType,
			EntityValue: canonicalValue,
		})
	}

	return out
}

func (r *Reconciler) recordMatchedEntityMetric(ruleName, nodeName string, event *protos.HealthEvent, entityKeyed bool) {
	if !entityKeyed ||
		r.config.HealthEventsAnalyzerRules == nil ||
		!r.config.HealthEventsAnalyzerRules.RuleMatchedEntityMetricEnabled {
		return
	}

	for _, entity := range metricSafeEntities(event) {
		ruleMatchedEntityTotal.WithLabelValues(
			ruleName, nodeName, entity.GetEntityType(), entity.GetEntityValue()).Inc()
	}
}
