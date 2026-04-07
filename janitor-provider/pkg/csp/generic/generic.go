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

package generic

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"github.com/nvidia/nvsentinel/janitor-provider/pkg/model"
)

const (
	defaultRebootImage         = "public.ecr.aws/docker/library/busybox:1.37.0"
	defaultRebootJobTTLSeconds = 3600
	jobLabelKey                = "nvsentinel.nvidia.com/reboot-job"
	jobNodeLabelKey            = "nvsentinel.nvidia.com/reboot-node"
	hostMountPath              = "/host"
)

var _ model.CSPClient = (*Client)(nil)

// Config holds the configuration for the generic provider.
type Config struct {
	RebootImage          string
	RebootJobNamespace   string
	RebootJobTTL         int32
	RebootJobPullSecrets []string
}

// Client is the generic bare-metal implementation of the CSP Client interface.
// It reboots nodes by creating a privileged Job that runs chroot /host reboot.
type Client struct {
	k8sClient kubernetes.Interface
	config    Config
}

// NewClient creates a new generic provider client with an in-cluster Kubernetes client.
func NewClient(ctx context.Context) (*Client, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	config := loadConfigFromEnv()
	if config.RebootJobNamespace == "" {
		return nil, fmt.Errorf("GENERIC_REBOOT_JOB_NAMESPACE must be set for the generic provider")
	}

	return &Client{
		k8sClient: clientset,
		config:    config,
	}, nil
}

// NewClientWithK8s creates a generic provider client with a provided Kubernetes client (for testing).
func NewClientWithK8s(k8sClient kubernetes.Interface, config Config) *Client {
	return &Client{
		k8sClient: k8sClient,
		config:    config,
	}
}

// SendRebootSignal creates a privileged Job on the target node that executes
// chroot /host reboot. Returns the node's pre-reboot bootID as the requestID.
func (c *Client) SendRebootSignal(ctx context.Context, node corev1.Node) (model.ResetSignalRequestRef, error) {
	preRebootBootID := node.Status.NodeInfo.BootID
	if preRebootBootID == "" {
		slog.Error("Node has no bootID", "node", node.Name)
		return "", fmt.Errorf("node %s has no bootID", node.Name)
	}

	job := c.buildRebootJob(node.Name)

	slog.Info("Creating reboot Job", "node", node.Name, "namespace", c.config.RebootJobNamespace)

	created, err := c.k8sClient.BatchV1().Jobs(c.config.RebootJobNamespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create reboot job for node %s: %w", node.Name, err)
	}

	slog.Info("Reboot Job created", "node", node.Name, "job", created.Name,
		"jobNamespace", c.config.RebootJobNamespace, "bootID", preRebootBootID)

	return model.ResetSignalRequestRef(preRebootBootID), nil
}

// IsNodeReady checks whether the node has rebooted by comparing the current bootID
// with the pre-reboot bootID (passed as requestID). It first checks the reboot Job's
// pod status for early failure detection (e.g., ImagePullBackOff).
func (c *Client) IsNodeReady(ctx context.Context, node corev1.Node, requestID string) (bool, error) {
	preRebootBootID := requestID

	if err := c.checkRebootJobPodStatus(ctx, node.Name); err != nil {
		return false, fmt.Errorf("failed to check reboot job pod status for node %s: %w", node.Name, err)
	}

	currentBootID := node.Status.NodeInfo.BootID
	if currentBootID == preRebootBootID {
		slog.Info("Node has not yet rebooted", "node", node.Name, "bootID", currentBootID)
		return false, nil
	}

	if !isNodeReady(node) {
		slog.Info("Node rebooted but not yet Ready", "node", node.Name,
			"oldBootID", preRebootBootID, "newBootID", currentBootID)

		return false, nil
	}

	slog.Info("Node rebooted and Ready", "node", node.Name,
		"oldBootID", preRebootBootID, "newBootID", currentBootID)

	c.cleanupRebootJob(ctx, node.Name)

	return true, nil
}

// SendTerminateSignal is not supported for bare-metal nodes.
func (c *Client) SendTerminateSignal(ctx context.Context, node corev1.Node) (model.TerminateNodeRequestRef, error) {
	return "", fmt.Errorf("terminate operation is not supported for generic provider (node: %s)", node.Name)
}

func (c *Client) buildRebootJob(nodeName string) *batchv1.Job {
	image := c.config.RebootImage
	ttl := c.config.RebootJobTTL

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("reboot-%s-", nodeName),
			Namespace:    c.config.RebootJobNamespace,
			Labels: map[string]string{
				jobLabelKey:     "true",
				jobNodeLabelKey: nodeName,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(0)),
			TTLSecondsAfterFinished: ptr.To(ttl),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						jobLabelKey:     "true",
						jobNodeLabelKey: nodeName,
					},
				},
				Spec: corev1.PodSpec{
					NodeName:         nodeName,
					RestartPolicy:    corev1.RestartPolicyNever,
					ImagePullSecrets: buildImagePullSecrets(c.config.RebootJobPullSecrets),
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists},
					},
					Containers: []corev1.Container{
						{
							Name:    "reboot",
							Image:   image,
							Command: []string{"chroot", hostMountPath, "reboot"},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "host-root",
									MountPath: hostMountPath,
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "host-root",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/",
								},
							},
						},
					},
				},
			},
		},
	}
}

// checkRebootJobPodStatus checks the reboot Job's pod for terminal failures
// (e.g., ImagePullBackOff) that indicate the reboot was never attempted.
func (c *Client) checkRebootJobPodStatus(ctx context.Context, nodeName string) error {
	namespace := c.config.RebootJobNamespace
	labelSelector := fmt.Sprintf("%s=true,%s=%s", jobLabelKey, jobNodeLabelKey, nodeName)

	pods, err := c.k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		slog.Warn("Failed to list reboot job pods, skipping pod status check", "node", nodeName, "error", err)
		return nil
	}

	if len(pods.Items) == 0 {
		return nil
	}

	for _, pod := range pods.Items {
		if !isPodScheduled(pod) {
			continue
		}

		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Waiting != nil {
				reason := cs.State.Waiting.Reason
				if isTransientWaitingReason(reason) {
					continue
				}

				if reason != "" {
					return fmt.Errorf("reboot job pod failed to start on node %s: %s", nodeName, reason)
				}
			}
		}
	}

	return nil
}

func (c *Client) cleanupRebootJob(ctx context.Context, nodeName string) {
	namespace := c.config.RebootJobNamespace
	labelSelector := fmt.Sprintf("%s=true,%s=%s", jobLabelKey, jobNodeLabelKey, nodeName)
	propagation := metav1.DeletePropagationBackground

	jobs, err := c.k8sClient.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		slog.Warn("Failed to list reboot jobs for cleanup", "node", nodeName, "error", err)
		return
	}

	for i := range jobs.Items {
		err := c.k8sClient.BatchV1().Jobs(namespace).Delete(ctx, jobs.Items[i].Name, metav1.DeleteOptions{
			PropagationPolicy: &propagation,
		})
		if err != nil {
			slog.Warn("Failed to cleanup reboot job", "job", jobs.Items[i].Name, "node", nodeName, "error", err)
		} else {
			slog.Info("Cleaned up reboot job", "job", jobs.Items[i].Name, "node", nodeName)
		}
	}
}

func buildImagePullSecrets(names []string) []corev1.LocalObjectReference {
	secrets := make([]corev1.LocalObjectReference, 0, len(names))
	for _, name := range names {
		secrets = append(secrets, corev1.LocalObjectReference{Name: name})
	}

	return secrets
}

func isPodScheduled(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func isTransientWaitingReason(reason string) bool {
	switch reason {
	case "ContainerCreating", "PodInitializing":
		return true
	default:
		return false
	}
}

func isNodeReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func loadConfigFromEnv() Config {
	image := os.Getenv("GENERIC_REBOOT_IMAGE")
	if image == "" {
		image = defaultRebootImage
	}

	namespace := os.Getenv("GENERIC_REBOOT_JOB_NAMESPACE")
	ttl := int32(defaultRebootJobTTLSeconds)

	if ttlStr := os.Getenv("GENERIC_REBOOT_JOB_TTL"); ttlStr != "" {
		parsed, err := strconv.ParseInt(ttlStr, 10, 32)
		if err != nil || parsed < 0 {
			slog.Warn("Invalid GENERIC_REBOOT_JOB_TTL, using default", "value", ttlStr, "default", defaultRebootJobTTLSeconds)
		} else {
			ttl = int32(parsed)
		}
	}

	var pullSecrets []string

	if ps := os.Getenv("GENERIC_REBOOT_IMAGE_PULL_SECRETS"); ps != "" {
		for s := range strings.SplitSeq(ps, ",") {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				pullSecrets = append(pullSecrets, trimmed)
			}
		}
	}

	return Config{
		RebootImage:          image,
		RebootJobNamespace:   namespace,
		RebootJobTTL:         ttl,
		RebootJobPullSecrets: pullSecrets,
	}
}
