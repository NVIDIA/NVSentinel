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
// once the reboot label has been removed.
package labelprovider

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	"github.com/nvidia/nvsentinel/janitor-provider/pkg/model"
)

const (
	defaultRebootKey    = "nke.nvidia.com/reboot"
	defaultTerminateKey = "nke.nvidia.com/terminate"
	defaultLabelValue   = "requested-by-nvsentinel"

	rebootKeyEnv    = "LABEL_REBOOT_KEY"
	terminateKeyEnv = "LABEL_TERMINATE_KEY"
	valueEnv        = "LABEL_VALUE"
)

var _ model.CSPClient = (*Client)(nil)

// Config holds the node labels used to request reboot and terminate.
type Config struct {
	// RebootKey is the node label key used to request a reboot.
	RebootKey string
	// TerminateKey is the node label key used to request termination.
	TerminateKey string
	// Value is written on the request label.
	Value string
}

// Client requests reboot and terminate by labeling the Node.
type Client struct {
	k8sClient kubernetes.Interface
	config    Config
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

	return NewClientWithK8s(ctx, clientset, loadConfigFromEnv()), nil
}

// NewClientWithK8s creates a label provider client with a provided Kubernetes client.
func NewClientWithK8s(_ context.Context, k8sClient kubernetes.Interface, config Config) *Client {
	return &Client{
		k8sClient: k8sClient,
		config:    withDefaults(config),
	}
}

// SendRebootSignal patches the configured reboot label onto the node.
func (c *Client) SendRebootSignal(
	ctx context.Context, node corev1.Node, _ string,
) (model.ResetSignalRequestRef, error) {
	if err := c.applyLabel(ctx, node.Name, c.config.RebootKey, c.config.Value); err != nil {
		return "", fmt.Errorf("failed to set reboot label on node %s: %w", node.Name, err)
	}

	slog.InfoContext(ctx, "Set reboot label on node",
		"node", node.Name, "label", c.config.RebootKey, "value", c.config.Value)

	return model.ResetSignalRequestRef(c.config.RebootKey), nil
}

// IsNodeReady reports completion when the reboot label is no longer on the node.
// The live Node is read so this follows the external controller removing the label.
func (c *Client) IsNodeReady(ctx context.Context, node corev1.Node, _ string) (bool, error) {
	current, err := c.k8sClient.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to get node %s: %w", node.Name, err)
	}

	if _, present := current.Labels[c.config.RebootKey]; present {
		slog.InfoContext(ctx, "Reboot label still present",
			"node", node.Name, "label", c.config.RebootKey)

		return false, nil
	}

	slog.InfoContext(ctx, "Reboot label removed",
		"node", node.Name, "label", c.config.RebootKey)

	return true, nil
}

// SendTerminateSignal patches the configured terminate label onto the node.
func (c *Client) SendTerminateSignal(
	ctx context.Context, node corev1.Node,
) (model.TerminateNodeRequestRef, error) {
	if err := c.applyLabel(ctx, node.Name, c.config.TerminateKey, c.config.Value); err != nil {
		return "", fmt.Errorf("failed to set terminate label on node %s: %w", node.Name, err)
	}

	slog.InfoContext(ctx, "Set terminate label on node",
		"node", node.Name, "label", c.config.TerminateKey, "value", c.config.Value)

	return model.TerminateNodeRequestRef(c.config.TerminateKey), nil
}

func (c *Client) applyLabel(ctx context.Context, nodeName, key, value string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current, err := c.k8sClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if current.Labels == nil {
			current.Labels = make(map[string]string)
		}

		current.Labels[key] = value

		_, err = c.k8sClient.CoreV1().Nodes().Update(ctx, current, metav1.UpdateOptions{})

		return err
	})
}

func loadConfigFromEnv() Config {
	return withDefaults(Config{
		RebootKey:    os.Getenv(rebootKeyEnv),
		TerminateKey: os.Getenv(terminateKeyEnv),
		Value:        os.Getenv(valueEnv),
	})
}

func withDefaults(config Config) Config {
	if config.RebootKey == "" {
		config.RebootKey = defaultRebootKey
	}

	if config.TerminateKey == "" {
		config.TerminateKey = defaultTerminateKey
	}

	if config.Value == "" {
		config.Value = defaultLabelValue
	}

	return config
}
