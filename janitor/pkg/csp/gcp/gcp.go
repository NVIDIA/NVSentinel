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

package gcp

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"github.com/nvidia/nvsentinel/janitor/pkg/model"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	_ model.CSPClient = (*Client)(nil)
)

// Client is the GCP implementation of the CSP Client interface.
type Client struct{}

type gcpNodeFields struct {
	project  string
	zone     string
	instance string
}

// NewClient creates a new GCP client.
func NewClient(ctx context.Context) (*Client, error) {
	// GCP client initialization is deferred until first API call
	// This allows validation to happen at construction time in the future
	return &Client{}, nil
}

func getNodeFields(node corev1.Node) (*gcpNodeFields, error) {
	// probably a better way to find these fields but this is what we did
	// in the shoreline script
	reqInfo := &gcpNodeFields{}
	providerID := node.Spec.ProviderID
	re := regexp.MustCompile(`^gce://(?P<project>[^/]+)/(?P<zone>[^/]+)/(?P<instance>[^/]+)`)

	match := re.FindStringSubmatch(providerID)
	result := make(map[string]string)

	if len(match) > 0 {
		// Get the names of the capture groups
		groupNames := re.SubexpNames()
		// Map the names to their matched values
		for i, name := range groupNames {
			// groupNames[0] is always an empty string
			if i == 0 {
				continue
			}

			if name != "" {
				result[name] = match[i]
			} else {
				return nil, fmt.Errorf("failed to extract required field %s from node.Spec.ProviderID", name)
			}
		}
	} else {
		return nil, errors.New("failed to extract required fields from node.Spec.ProviderID")
	}

	reqInfo.project = result["project"]
	reqInfo.zone = result["zone"]
	reqInfo.instance = result["instance"]

	return reqInfo, nil
}

// SendRebootSignal resets a GCE node by stopping and starting the instance.
// nolint:dupl // Similar code pattern as SendTerminateSignal is expected for CSP operations
func (c *Client) SendRebootSignal(ctx context.Context, node corev1.Node) (model.ResetSignalRequestRef, error) {
	logger := log.FromContext(ctx)

	instancesClient, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return "", err
	}

	defer func() {
		if cerr := instancesClient.Close(); cerr != nil {
			logger.Error(cerr, "failed to close instances client")
		}
	}()

	nodeFields, err := getNodeFields(node)
	if err != nil {
		return "", err
	}

	resetReq := &computepb.ResetInstanceRequest{
		Instance: nodeFields.instance,
		Project:  nodeFields.project,
		Zone:     nodeFields.zone,
	}

	logger.Info(fmt.Sprintf("Sending reset signal to %s", nodeFields.instance))

	op, err := instancesClient.Reset(ctx, resetReq)
	if err != nil {
		return "", err
	}

	return model.ResetSignalRequestRef(op.Proto().GetName()), nil
}

// IsNodeReady checks if the node is ready after a reboot operation.
func (c *Client) IsNodeReady(ctx context.Context, node corev1.Node, message string) (bool, error) {
	logger := log.FromContext(ctx)

	zoneOperationsClient, err := compute.NewZoneOperationsRESTClient(ctx)
	if err != nil {
		return false, err
	}

	defer func() {
		if cerr := zoneOperationsClient.Close(); cerr != nil {
			logger.Error(cerr, "failed to close zone operations client")
		}
	}()

	nodeFields, err := getNodeFields(node)
	if err != nil {
		return false, err
	}

	req := &computepb.GetZoneOperationRequest{
		Operation: message,
		Project:   nodeFields.project,
		Zone:      nodeFields.zone,
	}

	op, err := zoneOperationsClient.Get(ctx, req)
	if err != nil {
		return false, err
	}

	if *op.Status == computepb.Operation_DONE {
		return true, nil
	}

	return false, nil
}

// SendTerminateSignal deletes a GCE node.
// nolint:dupl // Similar code pattern as SendRebootSignal is expected for CSP operations
func (c *Client) SendTerminateSignal(ctx context.Context, node corev1.Node) (model.TerminateNodeRequestRef, error) {
	logger := log.FromContext(ctx)

	instancesClient, err := compute.NewInstancesRESTClient(ctx)
	if err != nil {
		return "", err
	}

	defer func() {
		if cerr := instancesClient.Close(); cerr != nil {
			logger.Error(cerr, "failed to close instances client")
		}
	}()

	nodeFields, err := getNodeFields(node)
	if err != nil {
		return "", err
	}

	deleteReq := &computepb.DeleteInstanceRequest{
		Instance: nodeFields.instance,
		Project:  nodeFields.project,
		Zone:     nodeFields.zone,
	}

	logger.Info(fmt.Sprintf("Sending delete signal to %s", nodeFields.instance))

	op, err := instancesClient.Delete(ctx, deleteReq)
	if err != nil {
		return "", err
	}

	return model.TerminateNodeRequestRef(op.Proto().GetName()), nil
}
