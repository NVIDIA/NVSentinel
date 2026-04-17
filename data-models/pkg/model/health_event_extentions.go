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

package model

import (
	"time"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

type Status string

const (
	StatusNotStarted Status = "NotStarted"
	StatusInProgress Status = "InProgress"
	StatusFailed     Status = "Failed"
	StatusSucceeded  Status = "Succeeded"
	AlreadyDrained   Status = "AlreadyDrained"
)

const (
	UnQuarantined      Status = "UnQuarantined"
	Quarantined        Status = "Quarantined"
	AlreadyQuarantined Status = "AlreadyQuarantined"
	Cancelled          Status = "Cancelled"
)

type HealthEventWithStatus struct {
	CreatedAt         time.Time                 `bson:"createdAt"`
	HealthEvent       *protos.HealthEvent       `bson:"healthevent,omitempty"`
	HealthEventStatus *protos.HealthEventStatus `bson:"healtheventstatus"`
}

// GetEffectiveActionName returns the action name to use for remediation routing.
// For built-in actions, returns the enum string (e.g., "RESTART_BM").
// For CUSTOM actions, returns the customRecommendedAction string.
func GetEffectiveActionName(he *protos.HealthEvent) string {
	if he.RecommendedAction == protos.RecommendedAction_CUSTOM {
		return he.CustomRecommendedAction
	}

	return he.RecommendedAction.String()
}
