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
	"math/rand/v2"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type kindClient struct{}

// SendRebootSignal simulates sending a reboot signal for a kind node
func (c *kindClient) SendRebootSignal(ctx context.Context, node corev1.Node) (ResetSignalRequestRef, error) {
	// nolint:gosec // G404: Using weak random for simulation is acceptable
	// wait some random time to simulate a real csp (very short for fast tests)
	time.Sleep(time.Duration(rand.IntN(3)) * time.Second)

	return "", nil
}

// IsNodeReady checks if the node is ready (simulated with randomness for kind)
func (c *kindClient) IsNodeReady(ctx context.Context, node corev1.Node, message string) (bool, error) {
	// nolint:gosec // G404: Using weak random for simulation is acceptable
	// simulate some randomness if the node is ready or not (very high success rate for fast tests)
	return rand.IntN(100) > 5, nil
}

// SendTerminateSignal simulates terminating a kind node by removing the docker container
// nolint:funlen,gocyclo,cyclop // Complex docker interaction logic
func (c *kindClient) SendTerminateSignal(
	ctx context.Context,
	node corev1.Node,
) (TerminateNodeRequestRef, error) {
	logger := log.FromContext(ctx)

	// Check if provider ID has the correct prefix
	if !strings.HasPrefix(node.Spec.ProviderID, "kind://") {
		return "", fmt.Errorf("invalid provider ID format: %s", node.Spec.ProviderID)
	}

	// Extract container name from provider ID
	parts := strings.Split(node.Spec.ProviderID, "/")
	if len(parts) < 5 {
		return "", fmt.Errorf("invalid provider ID format: %s", node.Spec.ProviderID)
	}

	containerName := parts[len(parts)-1]
	clusterName := parts[3]

	logger.Info("Attempting to terminate node", "node", node.Name, "container", containerName)

	// Create a timeout context for docker operations
	dockerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Check if container exists
	// nolint:gosec // G204: Command args are derived from kubernetes API, not user input
	cmd := exec.CommandContext(
		dockerCtx,
		"docker",
		"ps",
		"-a",
		"--filter",
		fmt.Sprintf("label=io.x-k8s.kind.cluster=%s", clusterName),
		"--format",
		"{{.Names}}",
	)

	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("timeout while listing containers: %w", err)
		}

		return "", fmt.Errorf("failed to list containers: %w", err)
	}

	// nolint:nestif // Complex docker interaction logic migrated from old code
	// If container exists, delete it
	if strings.Contains(string(output), containerName) {
		logger.Info("Found container, attempting deletion", "container", containerName)

		// nolint:gosec // G204: Command args are derived from kubernetes API, not user input
		cmd = exec.CommandContext(dockerCtx, "docker", "rm", "-f", containerName)

		if err := cmd.Run(); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return "", fmt.Errorf("timeout while deleting container: %w", err)
			}

			return "", fmt.Errorf("failed to delete container: %w", err)
		}

		// Verify container is actually gone
		// nolint:gosec // G204: Command args are derived from kubernetes API, not user input
		cmd = exec.CommandContext(
			dockerCtx,
			"docker",
			"ps",
			"-a",
			"--filter",
			fmt.Sprintf("name=^%s$", containerName),
			"--format",
			"{{.Names}}",
		)

		output, err = cmd.Output()
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return "", fmt.Errorf("timeout while verifying container deletion: %w", err)
			}

			return "", fmt.Errorf("failed to verify container deletion: %w", err)
		}

		if strings.Contains(string(output), containerName) {
			return "", fmt.Errorf("container %s still exists after deletion attempt", containerName)
		}

		logger.Info("Successfully deleted container", "container", containerName)
	} else {
		logger.Info("Container not found, assuming already deleted", "container", containerName)
	}

	return "", nil
}
