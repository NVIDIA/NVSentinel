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

package lambda

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

const (
	maintenanceEventsPath = "/api/v1/maintenance_events"
	// maxEventPages caps the pagination loop in ListMaintenanceEvents so a
	// misbehaving API (unbounded token chain or non-advancing page_token)
	// can't hang the poller or grow memory without bound.
	maxEventPages = 1000
)

// Event mirrors the Lambda maintenance API event shape.
type Event struct {
	ID                string     `json:"id"`
	EntityLRNs        []string   `json:"entity_lrns"`
	MaintenanceType   *string    `json:"maintenance_type"` // null in current API
	WorkspaceID       string     `json:"workspace_id"`
	Detail            string     `json:"detail"`
	Urgency           string     `json:"urgency"`
	Status            string     `json:"status"`
	NotBefore         *time.Time `json:"not_before"`
	NotBeforeDeadline *time.Time `json:"not_before_deadline"`
	NotAfter          *time.Time `json:"not_after"`
	LastUpdated       *time.Time `json:"last_updated"`
}

// apiResponse is the top-level structure of the Lambda maintenance events API response.
type apiResponse struct {
	Data      []Event `json:"data"`
	PageToken *string `json:"page_token"`
}

// ListMaintenanceEvents fetches all maintenance events, walking pagination
// via the API's page_token cursor. Retry/backoff is handled by the underlying
// Client.
//
// Events are scoped to the workspace given to WithWorkspaceID, or to the
// default workspace when none was given.
func (c *Client) ListMaintenanceEvents(ctx context.Context) ([]Event, error) {
	var all []Event

	var pageToken *string

	for range maxEventPages {
		q := url.Values{}
		if c.workspaceID != "" {
			q.Set("workspace_id", c.workspaceID)
		}

		if pageToken != nil {
			q.Set("page_token", *pageToken)
		}

		var parsed apiResponse
		if err := c.Get(ctx, maintenanceEventsPath, q, &parsed); err != nil {
			return nil, err
		}

		all = append(all, parsed.Data...)

		if parsed.PageToken == nil {
			return all, nil
		}

		if pageToken != nil && *parsed.PageToken == *pageToken {
			return nil, fmt.Errorf("maintenance events pagination did not advance: repeated page_token")
		}

		pageToken = parsed.PageToken
	}

	return nil, fmt.Errorf("maintenance events pagination exceeded %d pages", maxEventPages)
}
