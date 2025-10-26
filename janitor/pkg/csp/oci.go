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

// nolint:wsl // CSP client code migrated from old code
package csp

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/core"
	corev1 "k8s.io/api/core/v1"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// Compute provides a wrapper around a subset of the OCI Compute client interface,
// to enable mocking/stubbing for testing.
type Compute interface {
	InstanceAction(
		ctx context.Context,
		request core.InstanceActionRequest,
	) (response core.InstanceActionResponse, err error)
}

type OCIClient struct {
	compute Compute
}

type OCIClientOptionFunc func(*OCIClient) error

func WithComputeClient() OCIClientOptionFunc {
	return func(c *OCIClient) error {
		if c.compute != nil {
			return nil
		}

		var cfgProvider common.ConfigurationProvider
		var err error

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

func NewOCIClient(opts ...OCIClientOptionFunc) (*OCIClient, error) {
	c := &OCIClient{}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func NewOCIClientFromEnv() (*OCIClient, error) {
	return NewOCIClient(WithComputeClient())
}

func (c *OCIClient) SendRebootSignal(ctx context.Context, node corev1.Node) (ResetSignalRequestRef, error) {
	_, err := c.compute.InstanceAction(ctx, core.InstanceActionRequest{
		InstanceId: &node.Spec.ProviderID,
		Action:     core.InstanceActionActionSoftreset,
	})
	if err != nil {
		return "", err
	}

	return ResetSignalRequestRef(time.Now().UTC().Format(time.RFC3339)), nil
}

func (c *OCIClient) IsNodeReady(ctx context.Context, node corev1.Node, message string) (bool, error) {
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

func (c *OCIClient) SendTerminateSignal(
	ctx context.Context,
	node corev1.Node,
) (TerminateNodeRequestRef, error) {
	return "", fmt.Errorf("SendTerminateSignal not implemented for OCI")
}
