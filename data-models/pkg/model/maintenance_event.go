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
)

// MaintenanceEvent represents the normalized structure stored.
type MaintenanceEvent struct {
	EventID                string            `json:"eventId"                      bson:"eventId"`
	CSP                    CSP               `json:"csp"                          bson:"csp"`
	ClusterName            string            `json:"clusterName"                  bson:"clusterName"`
	ResourceType           string            `json:"resourceType"                 bson:"resourceType"`
	ResourceID             string            `json:"resourceId"                   bson:"resourceId"`
	MaintenanceType        MaintenanceType   `json:"maintenanceType"              bson:"maintenanceType"`
	Status                 InternalStatus    `json:"status"                       bson:"status"`
	CSPStatus              ProviderStatus    `json:"cspStatus"                    bson:"cspStatus"`
	ScheduledStartTime     *time.Time        `json:"scheduledStartTime,omitempty" bson:"scheduledStartTime,omitempty"`
	ScheduledEndTime       *time.Time        `json:"scheduledEndTime,omitempty"   bson:"scheduledEndTime,omitempty"`
	ActualStartTime        *time.Time        `json:"actualStartTime,omitempty"    bson:"actualStartTime,omitempty"`
	ActualEndTime          *time.Time        `json:"actualEndTime,omitempty"      bson:"actualEndTime,omitempty"`
	EventReceivedTimestamp time.Time         `json:"eventReceivedTimestamp"       bson:"eventReceivedTimestamp"`
	LastUpdatedTimestamp   time.Time         `json:"lastUpdatedTimestamp"         bson:"lastUpdatedTimestamp"`
	RecommendedAction      string            `json:"recommendedAction"            bson:"recommendedAction"`
	Metadata               map[string]string `json:"metadata,omitempty"           bson:"metadata,omitempty"`
	NodeName               string            `json:"nodeName,omitempty"           bson:"nodeName,omitemtpy"`
}

// CSP represents the Cloud Service Provider identifier as an enum.
type CSP string

// MaintenanceType represents whether an event is scheduled or unscheduled.
type MaintenanceType string

// InternalStatus captures the internal workflow status of a maintenance event.
type InternalStatus string

// ProviderStatus captures the status reported by the CSP provider.
type ProviderStatus string

// Constants for CSP types
const (
	CSPGCP CSP = "gcp"
	CSPAWS CSP = "aws"
)

// Constants for maintenance types
const (
	TypeScheduled   MaintenanceType = "SCHEDULED"
	TypeUnscheduled MaintenanceType = "UNSCHEDULED"
)

// Constants for internal status
const (
	StatusDetected             InternalStatus = "DETECTED"
	StatusQuarantineTriggered  InternalStatus = "QUARANTINE_TRIGGERED"
	StatusMaintenanceOngoing   InternalStatus = "MAINTENANCE_ONGOING"
	StatusMaintenanceComplete  InternalStatus = "MAINTENANCE_COMPLETE"
	StatusHealthyTriggered     InternalStatus = "HEALTHY_TRIGGERED"
	StatusNodeReadinessTimeout InternalStatus = "NODE_READINESS_TIMEOUT"
	StatusCancelled            InternalStatus = "CANCELLED"
	StatusError                InternalStatus = "ERROR"
)

// Constants for CSP provider-reported maintenance statuses
const (
	CSPStatusUnknown   ProviderStatus = "UNKNOWN"
	CSPStatusPending   ProviderStatus = "PENDING"
	CSPStatusOngoing   ProviderStatus = "ONGOING"
	CSPStatusActive    ProviderStatus = "ACTIVE"
	CSPStatusCompleted ProviderStatus = "COMPLETED"
	CSPStatusCancelled ProviderStatus = "CANCELLED"
)