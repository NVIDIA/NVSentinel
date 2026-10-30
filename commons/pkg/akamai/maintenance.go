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

package akamai

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const maintenancePath = "/account/maintenance"

// MaintenanceEntityLinode is the entity type of a Linode instance, the only
// entity the API reports maintenance for today.
const MaintenanceEntityLinode = "linode"

// Maintenance description values, as the API reports them in
// Maintenance.Description.
const (
	MaintenanceDescriptionScheduled = "scheduled"
	MaintenanceDescriptionEmergency = "emergency"
)

// MaintenanceEntity is the resource to which a maintenance event applies.
type MaintenanceEntity struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
	Type  string `json:"type"`
}

// Maintenance mirrors the Akamai Cloud maintenance API event shape.
//
// Type is one of reboot, cold_migration, live_migration, migrate or
// power_off_on, and Status one of pending, scheduled, started, in-progress,
// completed or canceled. Both are kept as strings so a value the API adds later
// still decodes.
type Maintenance struct {
	ID          int               `json:"id"`
	Entity      MaintenanceEntity `json:"entity"`
	Type        string            `json:"type"`
	Status      string            `json:"status"`
	Reason      string            `json:"reason"`
	Description string            `json:"description"`
	Source      string            `json:"source"`
	// MaintenancePolicySet is the policy that decided how the instance is
	// maintained, for example "linode/migrate" or "linode/power_off_on".
	MaintenancePolicySet string     `json:"maintenance_policy_set"`
	When                 *Timestamp `json:"when"`
	NotBefore            *Timestamp `json:"not_before"`
	StartTime            *Timestamp `json:"start_time"`
	CompleteTime         *Timestamp `json:"complete_time"`
}

// timestampLayout is the form the API documents for date-time fields: ISO 8601
// in UTC with no zone designator.
const timestampLayout = "2006-01-02T15:04:05"

// Timestamp is a time.Time that decodes both the API's zone-less UTC form and
// RFC 3339. A plain time.Time rejects the former, which would fail the whole
// response over one field.
type Timestamp struct {
	time.Time
}

// UnmarshalJSON decodes a quoted ISO 8601 timestamp, treating one without a
// zone as UTC. JSON null leaves the zero value, as time.Time does.
func (t *Timestamp) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "null" || s == "" {
		return nil
	}

	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		parsed, err = time.ParseInLocation(timestampLayout, s, time.UTC)
		if err != nil {
			return fmt.Errorf("parse timestamp %q: %w", s, err)
		}
	}

	t.Time = parsed.UTC()

	return nil
}

// ListMaintenance fetches every maintenance event on the account, walking
// pagination. Retry/backoff is handled by the underlying Client.
func (c *Client) ListMaintenance(ctx context.Context) ([]Maintenance, error) {
	events, err := listAll[Maintenance](ctx, c, maintenancePath)
	if err != nil {
		return nil, fmt.Errorf("list maintenance events: %w", err)
	}

	return events, nil
}
