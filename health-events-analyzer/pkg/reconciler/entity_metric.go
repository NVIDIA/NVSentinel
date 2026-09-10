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
	"strings"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	config "github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
)

const (
	entityTypeGPUUUID         = "GPU_UUID"
	entitiesImpactedFieldName = "entitiesimpacted"
)

// metricSafeEntityTypes are bounded slot identities. GPU UUID is excluded: it is
// unbounded from Prometheus's point of view, and a replaced GPU changes it.
var metricSafeEntityTypes = map[string]struct{}{
	"gpu":     {},
	"pci":     {},
	"gpc":     {},
	"tpc":     {},
	"nvlink":  {},
	"nic":     {},
	"nicport": {},
}

// ruleSelectsOnEntity reports whether the rule's aggregation keys on an
// impacted entity. Node-scoped rules do not mention entitiesimpacted, so they
// do not emit rule_matched_entity_total even when the triggering event happens
// to carry GPU or NIC entities.
func ruleSelectsOnEntity(rule config.HealthEventsAnalyzerRule) bool {
	for _, stage := range rule.Stage {
		if strings.Contains(strings.ToLower(stage), entitiesImpactedFieldName) {
			return true
		}
	}

	return false
}

func isMetricSafeEntityType(entityType string) bool {
	_, ok := metricSafeEntityTypes[strings.ToLower(entityType)]

	return ok
}

// metricSafeEntities returns the triggering event's impacted entities that are
// safe Prometheus labels: stable slot identity, not GPU UUID, SM, or register
// values. Duplicates are dropped.
func metricSafeEntities(event *protos.HealthEvent) []*protos.Entity {
	if event == nil {
		return nil
	}

	seen := make(map[string]struct{}, len(event.GetEntitiesImpacted()))
	out := make([]*protos.Entity, 0, len(event.GetEntitiesImpacted()))

	for _, entity := range event.GetEntitiesImpacted() {
		if entity == nil {
			continue
		}

		entityType := entity.GetEntityType()
		entityValue := entity.GetEntityValue()

		if entityType == "" || entityValue == "" {
			continue
		}

		if strings.EqualFold(entityType, entityTypeGPUUUID) {
			continue
		}

		if !isMetricSafeEntityType(entityType) {
			continue
		}

		key := strings.ToLower(entityType) + "\x00" + entityValue
		if _, ok := seen[key]; ok {
			continue
		}

		seen[key] = struct{}{}

		out = append(out, entity)
	}

	return out
}

func recordMatchedEntityMetric(ruleName, nodeName string, event *protos.HealthEvent) {
	for _, entity := range metricSafeEntities(event) {
		recordRuleMatchedEntity(ruleName, nodeName, entity.GetEntityType(), entity.GetEntityValue())
	}
}

func recordMatchedEntityMetricForRule(rule config.HealthEventsAnalyzerRule, event *protos.HealthEvent) {
	if event == nil || !ruleSelectsOnEntity(rule) {
		return
	}

	recordMatchedEntityMetric(rule.Name, event.GetNodeName(), event)
}
