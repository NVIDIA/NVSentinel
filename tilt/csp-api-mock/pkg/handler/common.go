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

package handler

import "csp-api-mock/pkg/store"

// mergeEvent updates dst with non-empty fields from src
func mergeEvent(dst, src *store.MaintenanceEvent) {
	if src.Status != "" {
		dst.Status = src.Status
	}
	if src.InstanceID != "" {
		dst.InstanceID = src.InstanceID
	}
	if src.NodeName != "" {
		dst.NodeName = src.NodeName
	}
	if src.Zone != "" {
		dst.Zone = src.Zone
	}
	if src.ProjectID != "" {
		dst.ProjectID = src.ProjectID
	}
	if src.Region != "" {
		dst.Region = src.Region
	}
	if src.AccountID != "" {
		dst.AccountID = src.AccountID
	}
	if src.EventTypeCode != "" {
		dst.EventTypeCode = src.EventTypeCode
	}
	if src.MaintenanceType != "" {
		dst.MaintenanceType = src.MaintenanceType
	}
	if src.ScheduledStart != nil {
		dst.ScheduledStart = src.ScheduledStart
	}
	if src.ScheduledEnd != nil {
		dst.ScheduledEnd = src.ScheduledEnd
	}
	if src.Description != "" {
		dst.Description = src.Description
	}
}
