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

// Package akamai implements the Akamai Cloud (Linode) CSP client for NVSentinel
// janitor node operations, using the shared Akamai client in
// commons/pkg/akamai. Reboot maps to the Linode reboot operation. Terminate
// recycles the node through LKE when the instance belongs to a cluster, so the
// pool gets a replacement, and deletes the instance otherwise.
//
// Both are asynchronous. IsNodeReady reports the reboot done only once the
// kubelet-reported bootID differs from the one recorded before the reboot:
// instance status alone still reads "running" for a while after the API
// accepts the request.
package akamai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/akamai"
	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/janitor-provider/pkg/model"
)

const (
	// APIEndpointEnvVar overrides the Akamai Cloud API base URL.
	APIEndpointEnvVar = "AKAMAI_API_ENDPOINT"

	providerIDPrefix = "linode://"

	// requestRefSeparator joins the requestRef fields below.
	requestRefSeparator = "|"

	// requestRefFields is instanceID, preRebootBootID, startedAt.
	requestRefFields = 3

	// apiTimeout bounds a single Akamai API call. Longer than the shared
	// client's default: a timeout on a request that already took effect costs
	// a second one.
	apiTimeout = 120 * time.Second
)

var _ model.CSPClient = (*Client)(nil)

// InstanceAPI is the subset of the shared Akamai client this package needs.
type InstanceAPI interface {
	GetInstance(ctx context.Context, instanceID int) (*akamai.Instance, error)
	RebootInstance(ctx context.Context, instanceID int) error
	DeleteInstance(ctx context.Context, instanceID int) error
	FindLKENode(ctx context.Context, clusterID, instanceID int) (*akamai.LKENode, error)
	RecycleLKENode(ctx context.Context, clusterID int, nodeID string) error
}

// Client is the Akamai implementation of the CSP Client interface.
type Client struct {
	api InstanceAPI
}

// NewClient creates an Akamai client backed by the given API.
func NewClient(api InstanceAPI) *Client {
	return &Client{api: api}
}

// NewClientFromEnv creates an Akamai client from AKAMAI_API_ENDPOINT (optional)
// and LINODE_TOKEN.
func NewClientFromEnv(_ context.Context) (*Client, error) {
	endpoint, err := akamai.NormalizeEndpoint(APIEndpointEnvVar, os.Getenv(APIEndpointEnvVar))
	if err != nil {
		return nil, err
	}

	if os.Getenv(akamai.APITokenEnvVar) == "" {
		return nil, fmt.Errorf("no Akamai API token: set %s", akamai.APITokenEnvVar)
	}

	httpClient := &http.Client{
		Timeout:   apiTimeout,
		Transport: auditlogger.NewAuditingRoundTripper(http.DefaultTransport),
	}

	slog.Info("Using Akamai Cloud API", "endpoint", endpoint)

	return NewClient(akamai.NewClient(endpoint, akamai.WithHTTPClient(httpClient))), nil
}

// SendRebootSignal reboots the node's Linode instance and returns immediately,
// the janitor controller polls IsNodeReady for completion.
func (c *Client) SendRebootSignal(
	ctx context.Context, node corev1.Node, _ string,
) (model.ResetSignalRequestRef, error) {
	instanceID, err := parseProviderID(node.Spec.ProviderID)
	if err != nil {
		return "", fmt.Errorf("node %s: %w", node.Name, err)
	}

	preRebootBootID := node.Status.NodeInfo.BootID
	if preRebootBootID == "" {
		return "", fmt.Errorf("node %s has no bootID", node.Name)
	}

	instance, err := c.api.GetInstance(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("reboot node %s: %w", node.Name, err)
	}

	if instance.Status == akamai.InstanceStatusDeleting {
		return "", fmt.Errorf("node %s: instance %d is %s, it cannot be rebooted",
			node.Name, instanceID, instance.Status)
	}

	slog.InfoContext(ctx, "Rebooting Akamai instance",
		"node", node.Name, "instanceID", instanceID, "status", instance.Status, "bootID", preRebootBootID)

	if err := c.api.RebootInstance(ctx, instanceID); err != nil {
		return "", fmt.Errorf("reboot node %s: %w", node.Name, err)
	}

	return model.ResetSignalRequestRef(strings.Join([]string{
		strconv.Itoa(instanceID),
		preRebootBootID,
		time.Now().UTC().Format(time.RFC3339),
	}, requestRefSeparator)), nil
}

// IsNodeReady reports whether the node came back from the reboot.
func (c *Client) IsNodeReady(ctx context.Context, node corev1.Node, requestID string) (bool, error) {
	ref, err := parseRequestRef(requestID)
	if err != nil {
		return false, fmt.Errorf("node %s: %w", node.Name, err)
	}

	instance, err := c.api.GetInstance(ctx, ref.instanceID)
	if err != nil {
		// A refusal that waiting cannot change would only run the janitor into its
		// timeout, so it is returned. Anything else may pass, so the node is not
		// ready yet.
		if refusalWillNotClear(err) {
			return false, fmt.Errorf("node %s: instance %d: %w", node.Name, ref.instanceID, err)
		}

		slog.WarnContext(ctx, "Failed to read Akamai instance, treating node as not ready yet",
			"node", node.Name, "instanceID", ref.instanceID, "error", err)

		return false, nil
	}

	switch instance.Status {
	case akamai.InstanceStatusDeleting:
		return false, fmt.Errorf("node %s: instance %d is %s, it will not come back from the reboot",
			node.Name, ref.instanceID, instance.Status)
	case akamai.InstanceStatusRunning:
		// Checked against the bootID below.
	default:
		slog.InfoContext(ctx, "Akamai instance not running yet",
			"node", node.Name, "instanceID", ref.instanceID, "status", instance.Status)

		return false, nil
	}

	// An empty bootID means kubelet has not reported yet, not that the node
	// came back on a new boot. The instance still reads "running" for a while
	// after the API accepts the reboot, so the bootID is the real signal.
	currentBootID := node.Status.NodeInfo.BootID
	if currentBootID == "" || currentBootID == ref.preRebootBootID {
		slog.InfoContext(ctx, "Node has not yet rebooted",
			"node", node.Name, "instanceID", ref.instanceID, "bootID", currentBootID)

		return false, nil
	}

	slog.InfoContext(ctx, "Node rebooted",
		"node", node.Name, "instanceID", ref.instanceID,
		"oldBootID", ref.preRebootBootID, "newBootID", currentBootID,
		"elapsed", time.Since(ref.startedAt).String())

	return true, nil
}

// refusalWillNotClear reports whether the API refused a request in a way that
// waiting does not change: a rejected token, a missing permission, or an
// instance that does not exist. Rate limits, server errors and transport
// failures can pass, so they are not included.
func refusalWillNotClear(err error) bool {
	var statusErr *akamai.StatusError
	if !errors.As(err, &statusErr) {
		return false
	}

	switch statusErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	default:
		return false
	}
}

// SendTerminateSignal removes the node's Linode instance. The janitor
// controller then waits for the Kubernetes Node to go NotReady and deletes it.
//
// An instance that belongs to an LKE cluster is recycled through LKE, which
// deletes it and provisions a replacement so the pool keeps its size. Deleting
// it directly would leave LKE to notice the loss on its own. Any other instance
// is deleted.
func (c *Client) SendTerminateSignal(ctx context.Context, node corev1.Node) (model.TerminateNodeRequestRef, error) {
	instanceID, err := parseProviderID(node.Spec.ProviderID)
	if err != nil {
		return "", fmt.Errorf("node %s: %w", node.Name, err)
	}

	instance, err := c.api.GetInstance(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("terminate node %s: %w", node.Name, err)
	}

	if instance.LKEClusterID != nil {
		if err := c.recycleLKENode(ctx, node.Name, *instance.LKEClusterID, instanceID); err != nil {
			return "", err
		}

		return model.TerminateNodeRequestRef(strconv.Itoa(instanceID)), nil
	}

	slog.InfoContext(ctx, "Deleting Akamai instance", "node", node.Name, "instanceID", instanceID)

	if err := c.api.DeleteInstance(ctx, instanceID); err != nil {
		return "", fmt.Errorf("terminate node %s: %w", node.Name, err)
	}

	return model.TerminateNodeRequestRef(strconv.Itoa(instanceID)), nil
}

// recycleLKENode recycles the LKE node backed by instanceID.
func (c *Client) recycleLKENode(ctx context.Context, nodeName string, clusterID, instanceID int) error {
	lkeNode, err := c.api.FindLKENode(ctx, clusterID, instanceID)
	if err != nil {
		return fmt.Errorf("terminate node %s: %w", nodeName, err)
	}

	slog.InfoContext(ctx, "Recycling LKE node",
		"node", nodeName, "instanceID", instanceID, "clusterID", clusterID, "lkeNodeID", lkeNode.ID)

	if err := c.api.RecycleLKENode(ctx, clusterID, lkeNode.ID); err != nil {
		return fmt.Errorf("terminate node %s: %w", nodeName, err)
	}

	return nil
}

// parseProviderID extracts the instance ID from linode://<instanceID>.
func parseProviderID(providerID string) (int, error) {
	if providerID == "" {
		return 0, fmt.Errorf("no provider ID set")
	}

	raw, ok := strings.CutPrefix(providerID, providerIDPrefix)
	if !ok {
		return 0, fmt.Errorf("provider ID %q is not %s<instanceID>", providerID, providerIDPrefix)
	}

	instanceID, err := strconv.Atoi(raw)
	if err != nil || instanceID <= 0 {
		return 0, fmt.Errorf("provider ID %q is not %s<instanceID>", providerID, providerIDPrefix)
	}

	return instanceID, nil
}

// requestRef is the opaque ref SendRebootSignal returns and IsNodeReady gets
// back verbatim.
type requestRef struct {
	instanceID      int
	preRebootBootID string
	startedAt       time.Time
}

// parseRequestRef splits a ref back into its fields.
func parseRequestRef(requestID string) (requestRef, error) {
	malformed := fmt.Errorf(
		"malformed request ref %q, want <instanceID>%s<bootID>%s<RFC3339 time>",
		requestID, requestRefSeparator, requestRefSeparator,
	)

	parts := strings.Split(requestID, requestRefSeparator)
	if len(parts) != requestRefFields || parts[1] == "" {
		return requestRef{}, malformed
	}

	instanceID, err := strconv.Atoi(parts[0])
	if err != nil || instanceID <= 0 {
		return requestRef{}, malformed
	}

	startedAt, err := time.Parse(time.RFC3339, parts[2])
	if err != nil {
		return requestRef{}, malformed
	}

	return requestRef{instanceID: instanceID, preRebootBootID: parts[1], startedAt: startedAt}, nil
}
