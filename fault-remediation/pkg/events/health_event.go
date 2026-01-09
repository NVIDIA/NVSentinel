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

package events

import "github.com/nvidia/nvsentinel/data-models/pkg/model"

// HealthEventDoc represents health event data with JSON "_id" tag for document-based storage.
type HealthEventDoc struct {
	ID                          string `json:"_id"`
	model.HealthEventWithStatus `json:",inline"`
}

// HealthEventData represents health event data with string ID for compatibility
type HealthEventData struct {
	ID                          string `bson:"_id,omitempty"`
	model.HealthEventWithStatus `bson:",inline"`
}
