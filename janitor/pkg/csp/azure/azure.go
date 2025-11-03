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

package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute"
	"github.com/nvidia/nvsentinel/janitor/pkg/model"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	_ model.CSPClient = (*Client)(nil)
)

// VMSSClientInterface defines the interface for VMSS operations we need
type VMSSClientInterface interface {
	GetInstanceView(
		ctx context.Context,
		resourceGroupName string,
		vmScaleSetName string,
		instanceID string,
		options *armcompute.VirtualMachineScaleSetVMsClientGetInstanceViewOptions,
	) (armcompute.VirtualMachineScaleSetVMsClientGetInstanceViewResponse, error)
	BeginRestart(
		ctx context.Context,
		resourceGroupName string,
		vmScaleSetName string,
		instanceID string,
		options *armcompute.VirtualMachineScaleSetVMsClientBeginRestartOptions,
	) (*runtime.Poller[armcompute.VirtualMachineScaleSetVMsClientRestartResponse], error)
}

// Client is the Azure implementation of the CSP Client interface.
type Client struct {
	// Optional client for testing - if nil, uses default Azure client
	vmssClient VMSSClientInterface
}

// NewClient creates a new Azure client.
func NewClient(ctx context.Context) (*Client, error) {
	// Azure client initialization is deferred until first API call
	// This allows validation to happen at construction time in the future
	return &Client{}, nil
}

// SendRebootSignal sends a reboot signal to Azure for the node.
func (c *Client) SendRebootSignal(ctx context.Context, node corev1.Node) (model.ResetSignalRequestRef, error) {
	logger := log.FromContext(ctx)

	// Get the Azure client
	vmssClient, err := c.getVMSSClient(ctx)
	if err != nil {
		logger.Error(err, "Failed to create Azure client")
		return "", err
	}

	// Fetch the node's provider ID
	providerID := node.Spec.ProviderID
	if providerID == "" {
		err := fmt.Errorf("no provider ID found for node %s", node.Name)
		logger.Error(err, "Failed to reboot node")

		return "", err
	}

	// Extract the resource group and VM name from the provider ID
	resourceGroup, vmName, instanceID, err := parseAzureProviderID(providerID)
	if err != nil {
		logger.Error(err, "Failed to parse provider ID")
		return "", err
	}

	// Reboot the VM
	_, err = vmssClient.BeginRestart(ctx, resourceGroup, vmName, instanceID, nil)

	if err != nil {
		logger.Error(err, fmt.Sprintf("Failed to send restart signal to node %s: %s", vmName, err))
		return "", err
	}

	return model.ResetSignalRequestRef(time.Now().Format(time.RFC3339)), nil
}

// IsNodeReady checks if the node is ready after a reboot operation.
func (c *Client) IsNodeReady(ctx context.Context, node corev1.Node, message string) (bool, error) {
	logger := log.FromContext(ctx)

	// don't check too early, wait like 5 minutes before checking, return not ready if too early
	storedTime, err := time.Parse(time.RFC3339, message)
	if err != nil {
		fmt.Println("Error parsing time:", err)
		return false, err
	}

	if time.Since(storedTime) < 5*time.Minute {
		return false, nil
	}

	// Fetch the node's provider ID
	providerID := node.Spec.ProviderID
	if providerID == "" {
		err := fmt.Errorf("no provider ID found for node %s", node.Name)
		logger.Error(err, "Failed to reboot node")

		return false, err
	}

	// Extract the resource group and VM name from the provider ID
	resourceGroup, vmName, instanceID, err := parseAzureProviderID(providerID)

	if err != nil {
		logger.Error(err, "Failed to parse provider ID")
		return false, err
	}

	// Get the Azure client
	vmssClient, err := c.getVMSSClient(ctx)
	if err != nil {
		logger.Error(err, "Failed to create Azure client")
		return false, err
	}

	instanceView, err := vmssClient.GetInstanceView(ctx, resourceGroup, vmName, instanceID, nil)
	if err != nil {
		logger.Error(err, fmt.Sprintf("Failed to get instance view for VM %s: %s", vmName, err))
		return false, err
	}

	if instanceView.Statuses != nil {
		for _, status := range instanceView.Statuses {
			if *status.Code == "ProvisioningState/succeeded" {
				logger.Info(fmt.Sprintf("Node %s is in a healthy state", node.Name))
				return true, nil
			}
		}
	}

	return false, nil
}

// SendTerminateSignal is not implemented for Azure.
func (c *Client) SendTerminateSignal(ctx context.Context, node corev1.Node) (model.TerminateNodeRequestRef, error) {
	return model.TerminateNodeRequestRef(""), fmt.Errorf("SendTerminateSignal not implemented for Azure")
}

// parseProviderID parses the provider ID to extract the resource group and VM name
func parseAzureProviderID(providerID string) (string, string, string, error) {
	// Example provider ID format:
	// azure:///subscriptions/<subscription-id>/resourceGroups/<resource-group>/
	// providers/Microsoft.Compute/virtualMachineScaleSets/<vmss-name>/virtualMachines/<instance-id>
	parts := strings.Split(providerID, "/")
	if len(parts) < 13 {
		return "", "", "", fmt.Errorf("invalid provider ID: %s", providerID)
	}

	resourceGroup := parts[6]
	vmName := parts[10]
	instanceID := parts[12]

	return resourceGroup, vmName, instanceID, nil
}

// getVMSSClient returns a VMSS client, either from the interface (for testing) or default Azure client
func (c *Client) getVMSSClient(ctx context.Context) (VMSSClientInterface, error) {
	if c.vmssClient != nil {
		return c.vmssClient, nil
	}

	// Default production behavior
	return createDefaultVMSSClient(ctx)
}

func createDefaultVMSSClient(ctx context.Context) (VMSSClientInterface, error) {
	logger := log.FromContext(ctx)

	// Get the Azure subscription ID from environment variable or IMDS
	subscriptionID, err := getSubscriptionID(ctx)
	if err != nil {
		return nil, err
	}

	// Create an Azure client
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		logger.Error(err, "Failed to create Azure credential")
		return nil, err
	}

	vmssClient, err := armcompute.NewVirtualMachineScaleSetVMsClient(subscriptionID, cred, nil)
	if err != nil {
		logger.Error(err, "Failed to create Azure client")
		return nil, err
	}

	return vmssClient, nil
}

func getSubscriptionID(ctx context.Context) (string, error) {
	logger := log.FromContext(ctx)

	if os.Getenv("LOCAL") == "true" {
		subscriptionID := os.Getenv("AZURE_SUBSCRIPTION_ID")
		if subscriptionID == "" {
			return "", fmt.Errorf("AZURE_SUBSCRIPTION_ID environment variable is not set")
		}

		return subscriptionID, nil
	}

	// pulled from https://github.com/Microsoft/azureimds/blob/master/imdssample.go
	client := http.Client{Transport: &http.Transport{Proxy: nil}}

	req, err := http.NewRequestWithContext(
		ctx,
		"GET",
		"http://169.254.169.254/metadata/instance",
		nil,
	)
	if err != nil {
		return "", fmt.Errorf("failed to create metadata request: %w", err)
	}

	req.Header.Add("Metadata", "True")

	q := req.URL.Query()
	q.Add("format", "json")
	q.Add("api-version", "2021-02-01")
	req.URL.RawQuery = q.Encode()

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			logger.Error(cerr, "failed to close http client")
		}
	}()

	// now that we have the response get the subscription ID from it
	var result struct {
		Compute struct {
			SubscriptionID string `json:"subscriptionId"`
		} `json:"compute"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode IMDS response: %w", err)
	}

	return result.Compute.SubscriptionID, nil
}
