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

package csp

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// ResetSignalRequestRef represents a reference to a reboot/reset signal request
type ResetSignalRequestRef string

// TerminateNodeRequestRef represents a reference to a terminate node request
type TerminateNodeRequestRef string

// Client defines the interface for cloud service provider operations
type Client interface {
	// SendRebootSignal sends a reboot signal to the node via the CSP
	SendRebootSignal(ctx context.Context, node corev1.Node) (ResetSignalRequestRef, error)

	// IsNodeReady checks if the node is ready after a reboot operation
	IsNodeReady(ctx context.Context, node corev1.Node, message string) (bool, error)

	// SendTerminateSignal sends a termination signal to the node via the CSP
	SendTerminateSignal(ctx context.Context, node corev1.Node) (TerminateNodeRequestRef, error)
}

// NewClient creates a new CSP client based on the provider type
func NewClient(provider string) (Client, error) {
	switch provider {
	case "kind":
		return &kindClient{}, nil
	default:
		return nil, fmt.Errorf("unsupported CSP provider: %s", provider)
	}
}
