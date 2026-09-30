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

package reconciler

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
)

type recoveryIdentity struct {
	nodeName string
	key      string
	entities []*protos.Entity
}

func recoveryIdentityForEvent(
	rule config.HealthEventsAnalyzerRule,
	event *protos.HealthEvent,
) (recoveryIdentity, bool) {
	if rule.Recovery == nil || event == nil || event.NodeName == "" {
		return recoveryIdentity{}, false
	}

	identity := recoveryIdentity{
		nodeName: event.NodeName,
		key:      event.NodeName,
	}

	if rule.Recovery.Scope == config.RecoveryScopeNode {
		return identity, true
	}

	entities, foundAllTypes := recoveryEntities(event.EntitiesImpacted, rule.Recovery.EntityTypes)
	if !foundAllTypes {
		return recoveryIdentity{}, false
	}

	identity.entities = entities
	identity.key = recoveryEntityKey(event.NodeName, entities)

	return identity, true
}

func recoveryEntities(entities []*protos.Entity, entityTypes []string) ([]*protos.Entity, bool) {
	allowedTypes := make(map[string]struct{}, len(entityTypes))
	for _, entityType := range entityTypes {
		allowedTypes[entityType] = struct{}{}
	}

	selectedByType := make(map[string]*protos.Entity, len(entityTypes))

	for _, entity := range entities {
		if !selectRecoveryEntity(selectedByType, allowedTypes, entity) {
			return nil, false
		}
	}

	if len(selectedByType) != len(allowedTypes) {
		return nil, false
	}

	selected := make([]*protos.Entity, 0, len(selectedByType))
	for _, entity := range selectedByType {
		selected = append(selected, entity)
	}

	slices.SortFunc(selected, func(a, b *protos.Entity) int {
		if result := strings.Compare(a.EntityType, b.EntityType); result != 0 {
			return result
		}

		return strings.Compare(a.EntityValue, b.EntityValue)
	})

	return selected, true
}

func selectRecoveryEntity(
	selected map[string]*protos.Entity,
	allowed map[string]struct{},
	entity *protos.Entity,
) bool {
	if entity == nil || entity.EntityValue == "" {
		return true
	}

	if _, ok := allowed[entity.EntityType]; !ok {
		return true
	}

	existing, found := selected[entity.EntityType]
	if found {
		return existing.EntityValue == entity.EntityValue
	}

	selected[entity.EntityType] = proto.Clone(entity).(*protos.Entity)

	return true
}

func recoveryEntityKey(nodeName string, entities []*protos.Entity) string {
	var key strings.Builder
	key.WriteString(nodeName)

	for _, entity := range entities {
		fmt.Fprintf(&key, "|%d:%s=%d:%s", len(entity.EntityType), entity.EntityType,
			len(entity.EntityValue), entity.EntityValue)
	}

	return key.String()
}
