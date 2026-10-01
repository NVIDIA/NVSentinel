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

// Package labelprovider implements a janitor CSP client that requests reboot
// and terminate by labeling the Node. An external controller (for example NKE)
// watches those labels and performs the action. The node is considered ready
// once the reboot label has been removed and the node's boot ID has changed.
package labelprovider

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	"github.com/nvidia/nvsentinel/janitor-provider/pkg/model"
)

const (
	defaultRebootSpec    = "nke.nvidia.com/reboot=requested-by-nvsentinel"
	defaultTerminateSpec = "nke.nvidia.com/terminate=requested-by-nvsentinel"

	rebootKeyEnv    = "LABEL_REBOOT_KEY"
	terminateKeyEnv = "LABEL_TERMINATE_KEY"
)

var _ model.CSPClient = (*Client)(nil)

// Config holds the node labels used to request reboot and terminate.
// RebootKey and TerminateKey are Kubernetes label specs in key=value form so
// reboot and terminate can use different values.
type Config struct {
	// RebootKey is the node label spec used to request a reboot (key=value).
	RebootKey string
	// TerminateKey is the node label spec used to request termination (key=value).
	TerminateKey string
}

type labelSpec struct {
	key   string
	value string
}

// Client requests reboot and terminate by labeling the Node.
type Client struct {
	k8sClient kubernetes.Interface
	reboot    labelSpec
	terminate labelSpec
}

// NewClient creates a label provider client with an in-cluster Kubernetes client.
func NewClient(ctx context.Context) (*Client, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return NewClientWithK8s(ctx, clientset, loadConfigFromEnv())
}

// NewClientWithK8s creates a label provider client with a provided Kubernetes client.
func NewClientWithK8s(_ context.Context, k8sClient kubernetes.Interface, config Config) (*Client, error) {
	config = withDefaults(config)

	reboot, err := parseLabelSpec(config.RebootKey, "reboot")
	if err != nil {
		return nil, err
	}

	terminate, err := parseLabelSpec(config.TerminateKey, "terminate")
	if err != nil {
		return nil, err
	}

	return &Client{
		k8sClient: k8sClient,
		reboot:    reboot,
		terminate: terminate,
	}, nil
}

// SendRebootSignal patches the configured reboot label onto the node and
// returns the current boot ID so IsNodeReady can detect a reboot.
func (c *Client) SendRebootSignal(
	ctx context.Context, node corev1.Node, _ string,
) (model.ResetSignalRequestRef, error) {
	bootID := node.Status.NodeInfo.BootID
	if bootID == "" {
		return "", fmt.Errorf("node %s has no bootID", node.Name)
	}

	if err := c.applyLabel(ctx, node.Name, c.reboot); err != nil {
		return "", fmt.Errorf("failed to set reboot label on node %s: %w", node.Name, err)
	}

	slog.InfoContext(ctx, "Set reboot label on node",
		"node", node.Name, "label", c.reboot.key, "value", c.reboot.value)

	return model.ResetSignalRequestRef(bootID), nil
}

// IsNodeReady reports completion when the reboot label is gone and the node's
// boot ID differs from the pre-reboot boot ID returned by SendRebootSignal.
func (c *Client) IsNodeReady(ctx context.Context, node corev1.Node, requestID string) (bool, error) {
	if requestID == "" {
		return false, fmt.Errorf("node %s: missing pre-reboot boot ID", node.Name)
	}

	current, err := c.k8sClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get node %s: %w", node.Name, err)
	}

	if _, present := current.Labels[c.reboot.key]; present {
		slog.InfoContext(ctx, "Reboot label still present",
			"node", node.Name, "label", c.reboot.key)

		return false, nil
	}

	currentBootID := current.Status.NodeInfo.BootID
	if currentBootID == "" || currentBootID == requestID {
		slog.InfoContext(ctx, "Reboot label removed but boot ID has not changed",
			"node", node.Name, "bootID", currentBootID)

		return false, nil
	}

	slog.InfoContext(ctx, "Node ready after reboot",
		"node", node.Name, "label", c.reboot.key, "bootID", currentBootID)

	return true, nil
}

// SendTerminateSignal patches the configured terminate label onto the node.
func (c *Client) SendTerminateSignal(
	ctx context.Context, node corev1.Node,
) (model.TerminateNodeRequestRef, error) {
	if err := c.applyLabel(ctx, node.Name, c.terminate); err != nil {
		return "", fmt.Errorf("failed to set terminate label on node %s: %w", node.Name, err)
	}

	slog.InfoContext(ctx, "Set terminate label on node",
		"node", node.Name, "label", c.terminate.key, "value", c.terminate.value)

	return model.TerminateNodeRequestRef(c.terminate.key), nil
}

func (c *Client) applyLabel(ctx context.Context, nodeName string, spec labelSpec) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current, err := c.k8sClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if current.Labels == nil {
			current.Labels = make(map[string]string)
		}

		existing, present := current.Labels[spec.key]
		if present {
			if existing == spec.value {
				return nil
			}

			return fmt.Errorf("label %s already set to %q", spec.key, existing)
		}

		current.Labels[spec.key] = spec.value

		_, err = c.k8sClient.CoreV1().Nodes().Update(ctx, current, metav1.UpdateOptions{})

		return err
	})
}

func parseLabelSpec(spec, kind string) (labelSpec, error) {
	key, value, ok := strings.Cut(strings.TrimSpace(spec), "=")
	if !ok || key == "" || value == "" {
		return labelSpec{}, fmt.Errorf("invalid %s label %q, want key=value", kind, spec)
	}

	return labelSpec{key: key, value: value}, nil
}

func loadConfigFromEnv() Config {
	return withDefaults(Config{
		RebootKey:    os.Getenv(rebootKeyEnv),
		TerminateKey: os.Getenv(terminateKeyEnv),
	})
}

func withDefaults(config Config) Config {
	if config.RebootKey == "" {
		config.RebootKey = defaultRebootSpec
	}

	if config.TerminateKey == "" {
		config.TerminateKey = defaultTerminateSpec
	}

	return config
}
