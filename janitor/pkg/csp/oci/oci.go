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

package oci

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nvidia/nvsentinel/janitor/pkg/model"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/core"
	corev1 "k8s.io/api/core/v1"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	_ model.CSPClient = (*Client)(nil)
)

// Compute provides a wrapper around a subset of the OCI Compute client interface,
// to enable mocking/stubbing for testing.
type Compute interface {
	InstanceAction(
		ctx context.Context,
		request core.InstanceActionRequest,
	) (response core.InstanceActionResponse, err error)
}

// Client is the OCI implementation of the CSP Client interface.
type Client struct {
	compute Compute
}

// ClientOptionFunc is a function that configures a Client.
type ClientOptionFunc func(*Client) error

// WithComputeClient returns an option function that configures the OCI Compute client.
func WithComputeClient() ClientOptionFunc {
	return func(c *Client) error {
		if c.compute != nil {
			return nil
		}

		var (
			cfgProvider common.ConfigurationProvider
			err         error
		)

		if os.Getenv("OCI_CREDENTIALS_FILE") != "" {
			cfgProvider = common.CustomProfileConfigProvider(os.Getenv("OCI_CREDENTIALS_FILE"), os.Getenv("OCI_PROFILE"))
		} else {
			cfgProvider, err = auth.OkeWorkloadIdentityConfigurationProvider()
			if err != nil {
				return err
			}
		}

		computeClient, err := core.NewComputeClientWithConfigurationProvider(cfgProvider)
		if err != nil {
			return err
		}

		c.compute = computeClient

		return nil
	}
}

// NewClient creates a new OCI client with the provided options.
func NewClient(opts ...ClientOptionFunc) (*Client, error) {
	c := &Client{}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	return c, nil
}

// NewClientFromEnv creates a new OCI client based on environment variables.
func NewClientFromEnv(ctx context.Context) (*Client, error) {
	// Context is accepted for consistency but OCI SDK doesn't use it during client creation
	// Configuration loading happens synchronously without I/O
	return NewClient(WithComputeClient())
}

// SendRebootSignal sends a reboot signal to OCI for the given node.
func (c *Client) SendRebootSignal(ctx context.Context, node corev1.Node) (model.ResetSignalRequestRef, error) {
	_, err := c.compute.InstanceAction(ctx, core.InstanceActionRequest{
		InstanceId: &node.Spec.ProviderID,
		Action:     core.InstanceActionActionSoftreset,
	})
	if err != nil {
		return "", err
	}

	return model.ResetSignalRequestRef(time.Now().UTC().Format(time.RFC3339)), nil
}

// IsNodeReady checks if the node is ready after a reboot operation.
func (c *Client) IsNodeReady(ctx context.Context, node corev1.Node, message string) (bool, error) {
	logger := ctrllog.FromContext(ctx)

	// Sending a reboot request to OCI doesn't update statuses immediately,
	// the instance does not report that it isn't in a running state for some time
	// and kubernetes still sees the node as ready. Wait five minutes before checking the status
	storedTime, err := time.Parse(time.RFC3339, message)
	if err != nil {
		logger.Error(err, "error parsing time")
		return false, err
	}

	if time.Since(storedTime) < 5*time.Minute {
		return false, nil
	}

	return true, nil
}

// SendTerminateSignal is not implemented for OCI.
func (c *Client) SendTerminateSignal(
	ctx context.Context,
	node corev1.Node,
) (model.TerminateNodeRequestRef, error) {
	return model.TerminateNodeRequestRef(""), fmt.Errorf("SendTerminateSignal not implemented for OCI")
}
