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

package eventutil

import (
	"fmt"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// FirstImpactedEntity returns the first entity of entityType with a non-empty value,
// in event order, as specified by ADR-071. The caller selects the scope type;
// this helper does not infer a device identity from another entity type.
func FirstImpactedEntity(healthEvent *protos.HealthEvent, entityType string) (*protos.Entity, error) {
	for _, entity := range healthEvent.GetEntitiesImpacted() {
		if entity.GetEntityType() == entityType && entity.GetEntityValue() != "" {
			return entity, nil
		}
	}

	return nil, fmt.Errorf("health event for node %s has no non-empty impacted entity of type %s",
		healthEvent.GetNodeName(), entityType)
}
