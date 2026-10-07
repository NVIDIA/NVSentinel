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
	"strconv"
)

// Instance status values NVSentinel branches on. The API's status enum has
// more; everything else is treated as a transient state.
const (
	InstanceStatusRunning  = "running"
	InstanceStatusDeleting = "deleting"
)

// Instance is the subset of the Linode instance object NVSentinel needs.
type Instance struct {
	ID     int    `json:"id"`
	Status string `json:"status"`
	// LKEClusterID is set when the instance is a node of an LKE cluster, and
	// nil otherwise.
	LKEClusterID *int `json:"lke_cluster_id"`
}

// LKENode is a node of an LKE node pool.
type LKENode struct {
	// ID is the LKE node ID, which is not the Linode instance ID.
	ID         string `json:"id"`
	InstanceID int    `json:"instance_id"`
	Status     string `json:"status"`
}

// lkePool is the subset of an LKE node pool needed to find a node.
type lkePool struct {
	Nodes []LKENode `json:"nodes"`
}

func instancePath(instanceID int) string {
	return "/linode/instances/" + strconv.Itoa(instanceID)
}

// GetInstance fetches a single instance by ID. Retry/backoff is handled by the
// underlying Client.
func (c *Client) GetInstance(ctx context.Context, instanceID int) (*Instance, error) {
	if instanceID <= 0 {
		return nil, fmt.Errorf("instance id %d is not valid", instanceID)
	}

	var instance Instance
	if err := c.Get(ctx, instancePath(instanceID), nil, &instance); err != nil {
		return nil, fmt.Errorf("get instance %d: %w", instanceID, err)
	}

	return &instance, nil
}

// RebootInstance reboots an instance, booting the last booted config. It is
// asynchronous: the instance still reports its pre-reboot status when this
// returns, so callers must poll for completion.
//
// The API refuses the request while other actions on the instance are running
// or queued, so an error here is not retried by the Client.
func (c *Client) RebootInstance(ctx context.Context, instanceID int) error {
	if instanceID <= 0 {
		return fmt.Errorf("instance id %d is not valid", instanceID)
	}

	if err := c.Post(ctx, instancePath(instanceID)+"/reboot", struct{}{}, nil); err != nil {
		return fmt.Errorf("reboot instance %d: %w", instanceID, err)
	}

	return nil
}

// DeleteInstance deletes an instance. It is asynchronous: the instance is
// typically still reported as "deleting" when this returns.
//
// Do not use it on an LKE node, the cluster would see the instance vanish out
// from under it. Use RecycleLKENode.
func (c *Client) DeleteInstance(ctx context.Context, instanceID int) error {
	if instanceID <= 0 {
		return fmt.Errorf("instance id %d is not valid", instanceID)
	}

	if err := c.Delete(ctx, instancePath(instanceID), nil); err != nil {
		return fmt.Errorf("delete instance %d: %w", instanceID, err)
	}

	return nil
}

// FindLKENode returns the LKE node backed by instanceID, walking the node pools
// of the cluster. The LKE node endpoints are keyed on the LKE node ID, which
// only the pool listing reports.
func (c *Client) FindLKENode(ctx context.Context, clusterID, instanceID int) (*LKENode, error) {
	path := "/lke/clusters/" + strconv.Itoa(clusterID) + "/pools"

	pools, err := listAll[lkePool](ctx, c, path)
	if err != nil {
		return nil, fmt.Errorf("list node pools of LKE cluster %d: %w", clusterID, err)
	}

	for _, pool := range pools {
		for _, node := range pool.Nodes {
			if node.InstanceID == instanceID {
				return &node, nil
			}
		}
	}

	return nil, fmt.Errorf("instance %d is not a node of LKE cluster %d", instanceID, clusterID)
}

// RecycleLKENode deletes an LKE node and has the cluster provision a
// replacement, so the pool keeps its size. It is asynchronous.
func (c *Client) RecycleLKENode(ctx context.Context, clusterID int, nodeID string) error {
	if clusterID <= 0 || nodeID == "" {
		return fmt.Errorf("LKE cluster id %d / node id %q is not valid", clusterID, nodeID)
	}

	path := "/lke/clusters/" + strconv.Itoa(clusterID) + "/nodes/" + nodeID + "/recycle"
	if err := c.Post(ctx, path, struct{}{}, nil); err != nil {
		return fmt.Errorf("recycle LKE node %s: %w", nodeID, err)
	}

	return nil
}
