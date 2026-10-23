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
	"log/slog"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const providerIDPrefix = "linode://"

// NodeResolver maps Linode instance IDs to the names of the nodes they back,
// keyed on the ID extracted from node.Spec.ProviderID (format:
// linode://<instanceID>).
//
// It reads the nodes with a List on demand instead of watching them. The
// monitor polls on a minutes-scale interval, so one List per poll costs far less
// than a watch held open for the life of the process, and each poll sees the
// current providerIDs, including one set after the node registered.
type NodeResolver struct {
	client kubernetes.Interface
}

// NewNodeResolver builds a NodeResolver over every node the client can see.
func NewNodeResolver(k8sClient kubernetes.Interface) *NodeResolver {
	return &NodeResolver{client: k8sClient}
}

// InstanceNodeNames returns the node name for each Linode instance ID found in
// the cluster's nodes.
func (nr *NodeResolver) InstanceNodeNames(ctx context.Context) (map[int]string, error) {
	// ResourceVersion "0" lets the API server answer from its watch cache rather
	// than a quorum read of etcd. A result a moment stale only delays an event to
	// the next poll.
	nodes, err := nr.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	names := make(map[int]string, len(nodes.Items))

	for i := range nodes.Items {
		node := &nodes.Items[i]

		if instanceID := extractInstanceID(node); instanceID != 0 {
			names[instanceID] = node.Name
		}
	}

	return names, nil
}

// extractInstanceID parses the Linode providerID format "linode://<instanceID>"
// and returns the ID. Returns 0 if the node has no providerID, the prefix does
// not match, or the ID is not a positive integer.
func extractInstanceID(node *v1.Node) int {
	raw, ok := strings.CutPrefix(node.Spec.ProviderID, providerIDPrefix)
	if !ok {
		return 0
	}

	instanceID, err := strconv.Atoi(raw)
	if err != nil || instanceID <= 0 {
		slog.Debug("Invalid Akamai providerID", "node", node.Name, "providerID", node.Spec.ProviderID)

		return 0
	}

	return instanceID
}
