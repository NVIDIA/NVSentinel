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

// Package nebius implements the Nebius Cloud (MK8s) CSP client for NVSentinel Janitor node operations.
// It provides node reboot functionality using the Nebius Compute API via the official Nebius Go SDK.
// Authentication is supported via IAM token (NEBIUS_IAM_TOKEN) or service account key file (NEBIUS_SA_KEY_FILE).
// Explicit credentials are required - the Nebius SDK does not support automatic credential discovery.
//
// Nebius does not provide a direct reboot API, so this implementation uses an async stop/start pattern:
// 1. SendRebootSignal initiates a stop and returns the instance ID
// 2. IsNodeReady polls the instance state and handles the state machine:
//   - STOPPING -> wait
//   - STOPPED -> initiate start
//   - STARTING -> wait
//   - RUNNING -> done
package nebius

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/auth"
	"github.com/nebius/gosdk/operations"
	compute "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computev1 "github.com/nebius/gosdk/services/nebius/compute/v1"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/janitor-provider/pkg/model"
)

var (
	_ model.CSPClient = (*Client)(nil)
)

// InstanceService provides a wrapper around Nebius Instance operations,
// to enable mocking/stubbing for testing.
type InstanceService interface {
	Stop(ctx context.Context, req *compute.StopInstanceRequest, opts ...grpc.CallOption) (operations.Operation, error)
	Start(ctx context.Context, req *compute.StartInstanceRequest, opts ...grpc.CallOption) (operations.Operation, error)
	Get(ctx context.Context, req *compute.GetInstanceRequest, opts ...grpc.CallOption) (*compute.Instance, error)
}

// Client is the Nebius implementation of the CSP Client interface.
type Client struct {
	// Optional client for testing - if nil, uses default SDK client
	instanceService InstanceService
}

// nebiusNodeFields contains the extracted fields from a Nebius node's provider ID.
type nebiusNodeFields struct {
	instanceID string
}

// NewClient creates a new Nebius client.
func NewClient(ctx context.Context) (*Client, error) {
	// Nebius client initialization is deferred until first API call
	// This allows validation to happen at construction time in the future
	return &Client{}, nil
}

// NewClientFromEnv creates a new Nebius client based on environment variables.
func NewClientFromEnv(ctx context.Context) (*Client, error) {
	return NewClient(ctx)
}

// getSDK creates a new Nebius SDK instance with appropriate credentials.
// Supports two authentication methods (similar to OCI):
// 1. NEBIUS_IAM_TOKEN environment variable (direct token)
// 2. NEBIUS_SA_KEY_FILE service account credentials file (recommended for production)
func getSDK(ctx context.Context) (*gosdk.SDK, error) {
	var creds gosdk.Credentials

	// Check for direct IAM token first
	if token := os.Getenv("NEBIUS_IAM_TOKEN"); token != "" {
		creds = gosdk.IAMToken(token)
	} else if keyFile := os.Getenv("NEBIUS_SA_KEY_FILE"); keyFile != "" {
		// Use service account credentials file (recommended for production)
		creds = gosdk.ServiceAccountReader(
			auth.NewServiceAccountCredentialsFileParser(nil, keyFile),
		)
	} else {
		return nil, fmt.Errorf("no Nebius credentials found: set NEBIUS_IAM_TOKEN or NEBIUS_SA_KEY_FILE")
	}

	sdk, err := gosdk.New(ctx, gosdk.WithCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("failed to create Nebius SDK: %w", err)
	}

	return sdk, nil
}

// getInstanceService returns the instance service, either from mock (for testing) or real SDK.
func (c *Client) getInstanceService(ctx context.Context) (InstanceService, func(), error) {
	// Use mock client if available (for testing)
	if c.instanceService != nil {
		return c.instanceService, func() {}, nil
	}

	// Production: create SDK and return instance service
	sdk, err := getSDK(ctx)
	if err != nil {
		return nil, nil, err
	}

	// Return cleanup function to close SDK
	cleanup := func() { sdk.Close() }

	return sdk.Services().Compute().V1().Instance(), cleanup, nil
}

// SendRebootSignal sends a reboot signal to Nebius for the given node.
// Nebius doesn't have a direct reboot API, so we stop the instance first.
// The instance will be started in IsNodeReady after the stop completes.
// This is async - we don't wait for the stop to complete, just initiate it.
func (c *Client) SendRebootSignal(ctx context.Context, node corev1.Node) (model.ResetSignalRequestRef, error) {
	// Fetch the node's provider ID
	providerID := node.Spec.ProviderID
	if providerID == "" {
		return "", fmt.Errorf("no provider ID found for node %s", node.Name)
	}

	// Extract the instance ID from the provider ID
	nodeFields, err := getNodeFields(node)
	if err != nil {
		return "", fmt.Errorf("failed to parse provider ID: %w", err)
	}

	slog.Info("Stopping Nebius instance for reboot", "instanceID", nodeFields.instanceID)

	instanceService, cleanup, err := c.getInstanceService(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get Nebius instance service: %w", err)
	}
	defer cleanup()

	_, err = instanceService.Stop(ctx, &compute.StopInstanceRequest{
		Id: nodeFields.instanceID,
	})
	if err != nil {
		return "", fmt.Errorf("failed to stop instance %s: %w", nodeFields.instanceID, err)
	}

	// Return instance ID - IsNodeReady will poll the instance state
	return model.ResetSignalRequestRef(nodeFields.instanceID), nil
}

// IsNodeReady checks if the node is ready after a reboot operation.
// For Nebius, this polls the instance state and handles the stop/start cycle:
// - STOPPING -> wait for stop to complete
// - STOPPED -> initiate start
// - STARTING -> wait for start to complete
// - RUNNING -> node is ready
func (c *Client) IsNodeReady(ctx context.Context, node corev1.Node, requestID string) (bool, error) {
	// requestID contains the instance ID from SendRebootSignal
	instanceID := requestID

	instanceService, cleanup, err := c.getInstanceService(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to get Nebius instance service: %w", err)
	}
	defer cleanup()

	// Get current instance status
	instance, err := instanceService.Get(ctx, &compute.GetInstanceRequest{
		Id: instanceID,
	})
	if err != nil {
		return false, fmt.Errorf("failed to get instance status: %w", err)
	}

	state := instance.GetStatus().GetState()

	switch state {
	case compute.InstanceStatus_RUNNING:
		slog.Info("Nebius instance is running", "instanceID", instanceID)

		return true, nil

	case compute.InstanceStatus_STOPPED:
		slog.Info("Starting Nebius instance", "instanceID", instanceID)

		_, err := instanceService.Start(ctx, &compute.StartInstanceRequest{
			Id: instanceID,
		})
		if err != nil {
			return false, fmt.Errorf("failed to start instance %s: %w", instanceID, err)
		}

		// Start initiated, will check again on next poll
		return false, nil

	case compute.InstanceStatus_STOPPING, compute.InstanceStatus_STARTING:
		slog.Info("Nebius instance is in transitional state, waiting",
			"instanceID", instanceID, "state", state.String())

		return false, nil

	case compute.InstanceStatus_UNSPECIFIED,
		compute.InstanceStatus_CREATING,
		compute.InstanceStatus_UPDATING,
		compute.InstanceStatus_DELETING,
		compute.InstanceStatus_ERROR:
		slog.Info("Nebius instance is in unexpected state",
			"instanceID", instanceID, "state", state.String())

		return false, nil
	}

	// Fallback for any unhandled states (future-proofing)
	slog.Info("Nebius instance is in unknown state",
		"instanceID", instanceID, "state", state.String())

	return false, nil
}

// SendTerminateSignal is not implemented for Nebius.
func (c *Client) SendTerminateSignal(ctx context.Context, node corev1.Node) (model.TerminateNodeRequestRef, error) {
	return model.TerminateNodeRequestRef(""), fmt.Errorf("SendTerminateSignal not implemented for Nebius")
}

// getNodeFields extracts instance information from the node's provider ID.
// Nebius MK8s provider ID format: nebius://computeinstance-{id}
// Example: nebius://computeinstance-e00a2rcz1xvbqgbvp5
func getNodeFields(node corev1.Node) (*nebiusNodeFields, error) {
	providerID := node.Spec.ProviderID
	if providerID == "" {
		return nil, fmt.Errorf("no provider ID found for node %s", node.Name)
	}

	// Primary format: nebius://instance-id (actual MK8s format)
	// Handles both nebius:// and nebius:/// prefixes
	re := regexp.MustCompile(`^nebius:///?([a-zA-Z0-9-]+)$`)
	match := re.FindStringSubmatch(providerID)

	if len(match) > 1 {
		return &nebiusNodeFields{
			instanceID: match[1],
		}, nil
	}

	return nil, fmt.Errorf("invalid Nebius provider ID format: %s", providerID)
}

// WithInstanceService sets a custom instance service for testing.
func WithInstanceService(svc InstanceService) func(*Client) {
	return func(c *Client) {
		c.instanceService = svc
	}
}

// Ensure computev1.InstanceService satisfies our interface at compile time.
var _ InstanceService = (computev1.InstanceService)(nil)
