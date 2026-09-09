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

// Package lambda implements the Lambda Cloud CSP client for NVSentinel janitor
// node operations, using the shared Lambda REST client in commons/pkg/lambda.
// Reboot maps to the Lambda power-cycle operation, terminate to the Lambda
// terminate operation.
//
// Both are asynchronous. IsNodeReady reports the reboot done only once the
// kubelet-reported bootID differs from the one recorded before the power cycle;
// instance status alone still reads "active" for a while after the API accepts
// the request.
package lambda

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	commonslambda "github.com/nvidia/nvsentinel/commons/pkg/lambda"
	"github.com/nvidia/nvsentinel/janitor-provider/pkg/model"
)

const (
	// APIEndpointEnvVar overrides the Lambda Cloud API base URL.
	APIEndpointEnvVar = "LAMBDA_API_ENDPOINT"

	providerIDPrefix = "lambda://"

	// requestRefSeparator joins the requestRef fields below.
	requestRefSeparator = "|"

	// requestRefFields is instanceID, preRebootBootID, startedAt.
	requestRefFields = 3

	// powerCycleStatusFloor absorbs the eventual consistency of the instance
	// status, which still reads "active" for a while after the API accepts
	// the power-cycle request.
	powerCycleStatusFloor = 120 * time.Second

	// apiTimeout bounds a single Lambda API call. Longer than the shared
	// client's default: a power cycle reaches through to the hypervisor, and a
	// timeout on a request that already took effect costs a second one.
	apiTimeout = 120 * time.Second
)

var _ model.CSPClient = (*Client)(nil)

// InstanceAPI is the subset of the shared Lambda client this package needs.
type InstanceAPI interface {
	GetInstance(ctx context.Context, instanceID string) (*commonslambda.Instance, error)
	PowerCycleInstance(ctx context.Context, instanceID string) error
	TerminateInstance(ctx context.Context, instanceID string) error
}

// Client is the Lambda implementation of the CSP Client interface.
type Client struct {
	api InstanceAPI
}

// NewClient creates a Lambda client backed by the given API.
func NewClient(api InstanceAPI) *Client {
	return &Client{api: api}
}

// NewClientFromEnv creates a Lambda client from LAMBDA_API_ENDPOINT (optional)
// and LAMBDA_API_KEY.
func NewClientFromEnv(_ context.Context) (*Client, error) {
	endpoint, err := endpointFromEnv()
	if err != nil {
		return nil, err
	}

	mode := commonslambda.DetectAuthMode()
	if mode == commonslambda.AuthNone {
		return nil, fmt.Errorf(
			"no Lambda credential: set %s, or annotate the ServiceAccount with lambda.ai/identity-lrn so the "+
				"pod-identity webhook injects %s",
			commonslambda.APIKeyEnvVar, commonslambda.IdentityLRNEnvVar)
	}

	httpClient := &http.Client{
		Timeout:   apiTimeout,
		Transport: auditlogger.NewAuditingRoundTripper(http.DefaultTransport),
	}

	slog.Info("Using Lambda Cloud API", "authMode", mode)

	return NewClient(commonslambda.NewClient(endpoint, commonslambda.WithHTTPClient(httpClient))), nil
}

// endpointFromEnv resolves the API base URL from the environment. The policy
// itself lives in the shared client, so both Lambda callers bound where a
// credential can be sent the same way.
func endpointFromEnv() (string, error) {
	return commonslambda.NormalizeEndpoint(APIEndpointEnvVar, os.Getenv(APIEndpointEnvVar))
}

// SendRebootSignal power-cycles the node's Lambda instance and returns
// immediately, the janitor controller polls IsNodeReady for completion.
func (c *Client) SendRebootSignal(ctx context.Context, node corev1.Node, _ string) (model.ResetSignalRequestRef, error) {
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
		return "", fmt.Errorf("power cycle node %s: %w", node.Name, err)
	}

	// Lambda's power-cycle endpoint does no in-flight validation, so this is the
	// only thing stopping a second power cycle landing on a booting host.
	if err := actionBlocked(instance.PowerCycleAction(), "power cycle", instanceID); err != nil {
		return "", fmt.Errorf("node %s: %w", node.Name, err)
	}

	slog.InfoContext(ctx, "Power cycling Lambda instance",
		"node", node.Name, "instanceID", instanceID, "bootID", preRebootBootID)

	if err := c.api.PowerCycleInstance(ctx, instanceID); err != nil {
		return "", fmt.Errorf("power cycle node %s: %w", node.Name, err)
	}

	return model.ResetSignalRequestRef(strings.Join([]string{
		instanceID,
		preRebootBootID,
		time.Now().UTC().Format(time.RFC3339),
	}, requestRefSeparator)), nil
}

// IsNodeReady reports whether the node came back from the power cycle.
func (c *Client) IsNodeReady(ctx context.Context, node corev1.Node, requestID string) (bool, error) {
	ref, err := parseRequestRef(requestID)
	if err != nil {
		return false, fmt.Errorf("node %s: %w", node.Name, err)
	}

	instanceID := ref.instanceID

	if elapsed := time.Since(ref.startedAt); elapsed < powerCycleStatusFloor {
		slog.InfoContext(ctx, "Within power cycle startup floor, not ready yet",
			"node", node.Name, "instanceID", instanceID, "elapsed", elapsed.String())

		return false, nil
	}

	instance, err := c.api.GetInstance(ctx, instanceID)
	if err != nil {
		slog.WarnContext(ctx, "Failed to read Lambda instance, treating node as not ready yet",
			"node", node.Name, "instanceID", instanceID, "error", err)

		return false, nil
	}

	switch instance.Status {
	case commonslambda.InstanceStatusTerminated,
		commonslambda.InstanceStatusTerminating,
		commonslambda.InstanceStatusPreempted:
		return false, fmt.Errorf("node %s: instance %s is %s, it will not come back from the power cycle",
			node.Name, instanceID, instance.Status)
	case commonslambda.InstanceStatusActive:
		// Checked against the bootID below.
	default:
		slog.InfoContext(ctx, "Lambda instance not active yet",
			"node", node.Name, "instanceID", instanceID, "status", instance.Status)

		return false, nil
	}

	// An empty bootID means kubelet has not reported yet, not that the node
	// came back on a new boot.
	currentBootID := node.Status.NodeInfo.BootID
	if currentBootID == "" || currentBootID == ref.preRebootBootID {
		slog.InfoContext(ctx, "Node has not yet rebooted",
			"node", node.Name, "instanceID", instanceID, "bootID", currentBootID)

		return false, nil
	}

	slog.InfoContext(ctx, "Node rebooted",
		"node", node.Name, "instanceID", instanceID,
		"oldBootID", ref.preRebootBootID, "newBootID", currentBootID)

	return true, nil
}

// SendTerminateSignal terminates the node's Lambda instance. The janitor
// controller then waits for the Kubernetes Node to go NotReady and deletes it.
func (c *Client) SendTerminateSignal(ctx context.Context, node corev1.Node) (model.TerminateNodeRequestRef, error) {
	instanceID, err := parseProviderID(node.Spec.ProviderID)
	if err != nil {
		return "", fmt.Errorf("node %s: %w", node.Name, err)
	}

	instance, err := c.api.GetInstance(ctx, instanceID)
	if err != nil {
		return "", fmt.Errorf("terminate node %s: %w", node.Name, err)
	}

	if err := actionBlocked(instance.Actions.Terminate, "terminate", instanceID); err != nil {
		return "", fmt.Errorf("node %s: %w", node.Name, err)
	}

	slog.InfoContext(ctx, "Terminating Lambda instance", "node", node.Name, "instanceID", instanceID)

	if err := c.api.TerminateInstance(ctx, instanceID); err != nil {
		return "", fmt.Errorf("terminate node %s: %w", node.Name, err)
	}

	return model.TerminateNodeRequestRef(instanceID), nil
}

// actionBlocked errors only if the API explicitly marks the action unavailable.
// A nil action (not reported) is treated as available, not blocked.
func actionBlocked(action *commonslambda.InstanceAction, op, instanceID string) error {
	if action == nil || action.Available {
		return nil
	}

	return fmt.Errorf("%s is blocked for instance %s: %s [reason_code=%s]",
		op, instanceID,
		cmp.Or(action.ReasonDescription, "no reason given"),
		cmp.Or(action.ReasonCode, "unknown"))
}

// parseProviderID extracts the instance ID from lambda://<instanceID>.
func parseProviderID(providerID string) (string, error) {
	if providerID == "" {
		return "", fmt.Errorf("no provider ID set")
	}

	// Instance IDs are opaque hex, so a slash means this is not our shape.
	instanceID, ok := strings.CutPrefix(providerID, providerIDPrefix)
	if !ok || instanceID == "" || strings.Contains(instanceID, "/") {
		return "", fmt.Errorf("provider ID %q is not %s<instanceID>", providerID, providerIDPrefix)
	}

	return instanceID, nil
}

// requestRef is the opaque ref SendRebootSignal returns and IsNodeReady gets
// back verbatim.
type requestRef struct {
	instanceID      string
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
	if len(parts) != requestRefFields || parts[0] == "" || parts[1] == "" {
		return requestRef{}, malformed
	}

	startedAt, err := time.Parse(time.RFC3339, parts[2])
	if err != nil {
		return requestRef{}, malformed
	}

	return requestRef{instanceID: parts[0], preRebootBootID: parts[1], startedAt: startedAt}, nil
}
