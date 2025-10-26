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
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// EC2 provides a wrapper around a subset of the AWS EC2 client interface,
// to enable mocking/stubbing for testing.
type EC2 interface {
	RebootInstances(
		ctx context.Context,
		input *ec2.RebootInstancesInput,
		opts ...func(*ec2.Options),
	) (*ec2.RebootInstancesOutput, error)
}

// AWSClient is the AWS implementation of the CSP Client interface.
type AWSClient struct {
	ec2 EC2
}

// AWSClientOptionFunc is a function that configures an AWSClient.
type AWSClientOptionFunc func(*AWSClient) error

// NewAWSClient creates a new AWS client with the provided options.
func NewAWSClient(opts ...AWSClientOptionFunc) (*AWSClient, error) {
	c := &AWSClient{}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// NewAWSClientFromEnv creates a new AWS client based on environment variables.
func NewAWSClientFromEnv() (*AWSClient, error) {
	return NewAWSClient(WithEC2Client())
}

// WithEC2Client returns an option function that configures the AWS EC2 client.
func WithEC2Client() AWSClientOptionFunc {
	return func(c *AWSClient) error {
		if c.ec2 != nil {
			return nil
		}

		cfg, err := config.LoadDefaultConfig(context.TODO(),
			config.WithRegion(os.Getenv("AWS_REGION")),
		)
		if err != nil {
			return fmt.Errorf("failed to load config for EC2 client: %w", err)
		}
		c.ec2 = ec2.NewFromConfig(cfg)
		return nil
	}
}

// SendRebootSignal sends a reboot signal to AWS EC2 for the given node.
func (c *AWSClient) SendRebootSignal(ctx context.Context, node corev1.Node) (ResetSignalRequestRef, error) {
	logger := log.FromContext(ctx)

	// Fetch the node's provider ID
	providerID := node.Spec.ProviderID
	if providerID == "" {
		err := fmt.Errorf("no provider ID found for node %s", node.Name)
		logger.Error(err, "Failed to reboot node")
		return "", err
	}

	// Extract the instance ID from the provider ID
	instanceID, err := parseAWSProviderID(providerID)
	if err != nil {
		logger.Error(err, "Failed to parse provider ID")
		return "", err
	}

	// Reboot the EC2 instance
	logger.Info(fmt.Sprintf("Rebooting node %s (Instance ID: %s)", node.Name, instanceID))

	_, err = c.ec2.RebootInstances(ctx, &ec2.RebootInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		logger.Error(err, fmt.Sprintf("Failed to reboot instance %s: %s", instanceID, err))
		return "", err
	}

	return ResetSignalRequestRef(time.Now().Format(time.RFC3339)), nil
}

// IsNodeReady checks if the node is ready after a reboot signal was sent.
// AWS requires a 5-minute cooldown period before the node status is reliable.
func (c *AWSClient) IsNodeReady(ctx context.Context, node corev1.Node, message string) (bool, error) {
	// Sending a reboot request to AWS doesn't update statuses immediately,
	// the ec2 instance does not report that it isn't in a running state for some time
	// and kubernetes still sees the node as ready. Wait five minutes before checking the status
	storedTime, err := time.Parse(time.RFC3339, message)
	if err != nil {
		fmt.Println("Error parsing time:", err)
		return false, err
	}

	if time.Since(storedTime) < 5*time.Minute {
		return false, nil
	}

	return true, nil
}

// SendTerminateSignal is not implemented for AWS.
func (c *AWSClient) SendTerminateSignal(ctx context.Context, node corev1.Node) (TerminateNodeRequestRef, error) {
	return "", fmt.Errorf("SendTerminateSignal not implemented for AWS")
}

// parseAWSProviderID extracts the EC2 instance ID from an AWS provider ID.
// Example provider ID: aws:///us-west-2/i-1234567890abcdef0
func parseAWSProviderID(providerID string) (string, error) {
	parts := strings.Split(providerID, "/")
	if len(parts) < 5 {
		return "", fmt.Errorf("invalid provider ID: %s", providerID)
	}
	return parts[4], nil
}
