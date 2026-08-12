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
)

const (
	instancesPath          = "/api/v1/instances"
	powerCycleInstancePath = "/api/v1/instance-operations/power-cycle"
	terminateInstancePath  = "/api/v1/instance-operations/terminate"
)

// Instance status values NVSentinel branches on. The API's InstanceStatus enum
// has more; everything else is treated as a transient state.
const (
	InstanceStatusActive      = "active"
	InstanceStatusTerminated  = "terminated"
	InstanceStatusTerminating = "terminating"
	InstanceStatusPreempted   = "preempted"
)

// Instance is the subset of the Lambda instance object NVSentinel needs.
type Instance struct {
	ID      string          `json:"id"`
	Status  string          `json:"status"`
	Actions InstanceActions `json:"actions"`
}

// InstanceAction reports whether an operation can be performed on an instance
// right now, and why not when it cannot.
type InstanceAction struct {
	Available         bool   `json:"available"`
	ReasonCode        string `json:"reason_code"`
	ReasonDescription string `json:"reason_description"`
}

// InstanceActions is the subset of the API's action-availability block
// NVSentinel checks. Fields are pointers so callers can tell "the API did not
// report on this action" from "the action is unavailable".
type InstanceActions struct {
	PowerCycle *InstanceAction `json:"power_cycle"`
	ColdReboot *InstanceAction `json:"cold_reboot"`
	Terminate  *InstanceAction `json:"terminate"`
}

// PowerCycleAction returns the reported availability of the power-cycle action,
// or nil when the API did not report on it.
//
// The API reports it under cold_reboot: internally the action type is
// COLD_REBOOT and it maps to the public power_cycle operation. power_cycle is
// read first so this keeps working once the field is renamed to match the
// surviving endpoint.
func (i *Instance) PowerCycleAction() *InstanceAction {
	if i.Actions.PowerCycle != nil {
		return i.Actions.PowerCycle
	}

	return i.Actions.ColdReboot
}

// instanceIDsRequest is the body shared by the instance-operations endpoints,
// which are batch APIs even when acting on a single instance.
type instanceIDsRequest struct {
	InstanceIDs []string `json:"instance_ids"`
}

// instanceResponse is the envelope GET /instances/{id} returns.
type instanceResponse struct {
	Data Instance `json:"data"`
}

// powerCycleResponse is the envelope the power-cycle endpoint returns. The
// instances it echoes back are what acknowledged checks.
type powerCycleResponse struct {
	Data struct {
		PowerCycledInstances []Instance `json:"power_cycled_instances"`
	} `json:"data"`
}

// terminateResponse is the envelope the terminate endpoint returns, differing
// from powerCycleResponse only in the key holding the echoed instances.
type terminateResponse struct {
	Data struct {
		TerminatedInstances []Instance `json:"terminated_instances"`
	} `json:"data"`
}

// GetInstance fetches a single instance by ID. Retry/backoff is handled by the
// underlying Client.
func (c *Client) GetInstance(ctx context.Context, instanceID string) (*Instance, error) {
	if instanceID == "" {
		return nil, fmt.Errorf("instance id is empty")
	}

	var parsed instanceResponse
	if err := c.Get(ctx, instancesPath+"/"+url.PathEscape(instanceID), nil, &parsed); err != nil {
		return nil, fmt.Errorf("get instance %s: %w", instanceID, err)
	}

	return &parsed.Data, nil
}

// PowerCycleInstance hard power-cycles an instance at the host level. It is
// asynchronous: the instance still reports its pre-power-cycle status when this
// returns, so callers must poll for completion.
func (c *Client) PowerCycleInstance(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("instance id is empty")
	}

	var parsed powerCycleResponse

	req := instanceIDsRequest{InstanceIDs: []string{instanceID}}
	if err := c.Post(ctx, powerCycleInstancePath, req, &parsed); err != nil {
		return fmt.Errorf("power cycle instance %s: %w", instanceID, err)
	}

	if !acknowledged(parsed.Data.PowerCycledInstances, instanceID) {
		return fmt.Errorf("power cycle instance %s: not acknowledged by the API", instanceID)
	}

	return nil
}

// TerminateInstance terminates an instance. It is asynchronous: the instance is
// typically still reported as "terminating" when this returns.
func (c *Client) TerminateInstance(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return fmt.Errorf("instance id is empty")
	}

	var parsed terminateResponse

	req := instanceIDsRequest{InstanceIDs: []string{instanceID}}
	if err := c.Post(ctx, terminateInstancePath, req, &parsed); err != nil {
		return fmt.Errorf("terminate instance %s: %w", instanceID, err)
	}

	if !acknowledged(parsed.Data.TerminatedInstances, instanceID) {
		return fmt.Errorf("terminate instance %s: not acknowledged by the API", instanceID)
	}

	return nil
}

// acknowledged reports whether the API echoed the instance back. These
// endpoints re-read the IDs they were given, so a missing one means the
// operation did not apply to our instance.
func acknowledged(instances []Instance, instanceID string) bool {
	for _, i := range instances {
		if i.ID == instanceID {
			return true
		}
	}

	return false
}
