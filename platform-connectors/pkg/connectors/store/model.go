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

package store

import (
	"time"

	platformconnector "github.com/nvidia/nvsentinel/data-models/pkg/protos"
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
)

type OperationStatus struct {
	Status  Status `bson:"status"`
	Message string `bson:"message,omitempty"`
}

type HealthEventStatus struct {
	NodeQuarantined          *Status         `bson:"nodequarantined"`
	UserPodsEvictionStatus   OperationStatus `bson:"userpodsevictionstatus"`
	FaultRemediated          *bool           `bson:"faultremediated"`
	LastRemediationTimestamp *time.Time      `bson:"lastremediationtimestamp,omitempty"`
}

type HealthEventWithStatus struct {
	CreatedAt         time.Time                      `bson:"createdAt"`
	HealthEvent       *platformconnector.HealthEvent `bson:"healthevent,omitempty"`
	HealthEventStatus HealthEventStatus              `bson:"healtheventstatus"`
}
