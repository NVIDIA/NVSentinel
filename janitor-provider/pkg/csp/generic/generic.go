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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/nvsentinel/janitor-provider/pkg/model"
)

const (
	defaultRebootImage         = "public.ecr.aws/docker/library/busybox:1.37.0"
	defaultRebootJobTTLSeconds = 3600
	jobLabelKey                = "nvsentinel.nvidia.com/reboot-job"
	jobNodeLabelKey            = "nvsentinel.nvidia.com/reboot-node"
	hostMountPath              = "/host"
	hostProcMountPath          = "/host-proc"
	// preRebootBootIDEnvVar is the reboot container env var holding the node's boot ID
	// recorded when the Job was created.
	preRebootBootIDEnvVar = "PRE_REBOOT_BOOT_ID"
	// bootIDPath is the kernel boot ID file. It is not namespaced, so the container reads
	// the host's value.
	bootIDPath = "/proc/sys/kernel/random/boot_id"
	// rejectedPodGracePeriod is how long the reboot Job must have failed, with a pod that
	// never started and the node's boot ID unchanged, before the attempt is reported failed.
	// After a reboot the kubelet can reject the pod again and publish that before the node
	// status that carries the new boot ID; this leaves time for that node status to arrive.
	rejectedPodGracePeriod = 2 * time.Minute
)

var _ model.CSPClient = (*Client)(nil)

// Config holds the configuration for the generic provider.
type Config struct {
	// RebootImage is the container image used for the reboot Job.
	RebootImage string
	// UseSysrqReboot triggers an immediate kernel reboot via sysrq-trigger when true.
	UseSysrqReboot bool
	// RebootJobNamespace is the namespace where reboot Jobs are spawned.
	RebootJobNamespace string
	// RebootJobTTL is the TTL in seconds after finish for the reboot Job.
	RebootJobTTL int32
	// RebootJobPullSecrets lists image pull secrets for the reboot Job image.
	RebootJobPullSecrets []string
	// WriteSyslog writes an attribution entry to host syslog before rebooting when true.
	WriteSyslog bool
}

// Client is the generic bare-metal implementation of the CSP Client interface.
// It reboots nodes by creating a privileged Job that runs the configured reboot method.
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
// the configured reboot command. Returns the node's pre-reboot bootID as the requestID.
//
// A Job left by an earlier attempt is removed first, and the attempt is not started when
// that fails. The janitor does not tell the provider when it gives up on a RebootNode
// (timeout, failed readiness check), so such a Job stays until the node is deleted: its
// pod would run the reboot command whenever the node next schedules it, and its state
// would be read as this attempt's by IsNodeReady. Background deletion may briefly
// leave pods from both attempts running. Each pod's boot ID guard skips the reboot
// if the node has already rebooted since that pod's Job was created.
func (c *Client) SendRebootSignal(ctx context.Context, node corev1.Node, crName string) (model.ResetSignalRequestRef, error) {
	preRebootBootID := node.Status.NodeInfo.BootID
	if preRebootBootID == "" {
		slog.ErrorContext(ctx, "Node has no bootID", "node", node.Name)
		return "", fmt.Errorf("node %s has no bootID", node.Name)
	}

	if err := c.cleanupRebootJob(ctx, node.Name); err != nil {
		return "", fmt.Errorf("failed to remove the previous reboot job for node %s: %w", node.Name, err)
	}

	job := c.buildRebootJob(node.Name, crName, preRebootBootID)

	slog.InfoContext(ctx, "Creating reboot Job", "node", node.Name, "namespace", c.config.RebootJobNamespace,
		"useSysrqReboot", c.config.UseSysrqReboot, "writeSyslog", c.config.WriteSyslog)

	created, err := c.k8sClient.BatchV1().Jobs(c.config.RebootJobNamespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create reboot job for node %s: %w", node.Name, err)
	}

	slog.InfoContext(ctx, "Reboot Job created", "node", node.Name, "job", created.Name,
		"jobNamespace", c.config.RebootJobNamespace, "bootID", preRebootBootID,
		"useSysrqReboot", c.config.UseSysrqReboot, "writeSyslog", c.config.WriteSyslog)

	return model.ResetSignalRequestRef(preRebootBootID), nil
}

// IsNodeReady checks whether the node has rebooted by comparing the current bootID
// with the pre-reboot bootID (passed as requestID). It first checks the reboot Job's
// pod status for early failure detection (e.g., ImagePullBackOff). The janitor fails
// the RebootNode on that error, so the Job is removed before it is returned: left in
// place, its pod would still run the reboot command once the image pull succeeds.
func (c *Client) IsNodeReady(ctx context.Context, node corev1.Node, requestID string) (bool, error) {
	preRebootBootID := requestID
	currentBootID := node.Status.NodeInfo.BootID

	if err := c.checkRebootJobPodStatus(ctx, node.Name, currentBootID != preRebootBootID); err != nil {
		if cleanupErr := c.cleanupRebootJob(ctx, node.Name); cleanupErr != nil {
			slog.WarnContext(ctx, "Failed to remove the reboot job of the failed attempt", "node", node.Name, "error", cleanupErr)
		}

		return false, fmt.Errorf("failed to check reboot job pod status for node %s: %w", node.Name, err)
	}

	if currentBootID == preRebootBootID {
		slog.InfoContext(ctx, "Node has not yet rebooted", "node", node.Name, "bootID", currentBootID)
		return false, nil
	}

	if !isNodeReady(node) {
		slog.InfoContext(ctx, "Node rebooted but not yet Ready", "node", node.Name,
			"oldBootID", preRebootBootID, "newBootID", currentBootID)

		return false, nil
	}

	slog.InfoContext(ctx, "Node rebooted and Ready", "node", node.Name,
		"oldBootID", preRebootBootID, "newBootID", currentBootID)

	if err := c.cleanupRebootJob(ctx, node.Name); err != nil {
		slog.WarnContext(ctx, "Failed to remove the reboot job after the reboot", "node", node.Name, "error", err)
	}

	return true, nil
}

// SendTerminateSignal is not supported for bare-metal nodes.
func (c *Client) SendTerminateSignal(ctx context.Context, node corev1.Node) (model.TerminateNodeRequestRef, error) {
	return "", fmt.Errorf("terminate operation is not supported for generic provider (node: %s)", node.Name)
}

// buildRebootJob constructs the privileged batchv1.Job specification for rebooting the node.
// preRebootBootID is passed to the container as an env var so the reboot command can
// skip the reboot once the node has booted again.
func (c *Client) buildRebootJob(nodeName, crName, preRebootBootID string) *batchv1.Job {
	image := c.config.RebootImage
	ttl := c.config.RebootJobTTL
	command := c.config.rebootCommand(nodeName, crName)

	var (
		volumeMounts []corev1.VolumeMount
		volumes      []corev1.Volume
	)

	if c.config.UseSysrqReboot {
		volumeMounts = []corev1.VolumeMount{
			{
				Name:      "host-proc",
				MountPath: hostProcMountPath,
			},
		}
		volumes = []corev1.Volume{
			{
				Name: "host-proc",
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/proc",
				},
			},
		}

		if c.config.WriteSyslog {
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      "host-root",
				MountPath: hostMountPath,
			})
			volumes = append(volumes, corev1.Volume{
				Name: "host-root",
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/",
				},
			})
		}
	} else {
		volumeMounts = []corev1.VolumeMount{
			{
				Name:      "host-root",
				MountPath: hostMountPath,
			},
		}
		volumes = []corev1.Volume{
			{
				Name: "host-root",
				HostPath: &corev1.HostPathVolumeSource{
					Path: "/",
				},
			},
		}
	}

	return &batchv1.Job{
		GenerateName: fmt.Sprintf("reboot-%s-", nodeName),
		Namespace:    c.config.RebootJobNamespace,
		Labels: map[string]string{
			jobLabelKey:     "true",
			jobNodeLabelKey: nodeName,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(int32(0)),
			TTLSecondsAfterFinished: new(ttl),
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
							Command: command,
							Env: []corev1.EnvVar{
								{Name: preRebootBootIDEnvVar, Value: preRebootBootID},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: new(true),
							},
							VolumeMounts: volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}
}

// rebootCommand generates the container command slice based on sysrq and syslog settings.
// Every variant runs the boot ID guard before any side effect. The kubelet can start the
// same pod again after the node boots, because the reboot killed the container before it
// reported a terminal status; the guard makes that second run exit without rebooting.
func (c Config) rebootCommand(nodeName, crName string) []string {
	return []string{"sh", "-c", bootIDGuard() + c.rebootAction(nodeName, crName)}
}

// bootIDGuard returns a shell prefix that exits 0 when the host boot ID differs from
// PRE_REBOOT_BOOT_ID, and exits non-zero when either value is missing. The boot ID is read
// from the env var only and is never interpolated into the script.
func bootIDGuard() string {
	return fmt.Sprintf(
		`cur=$(cat %[1]s) || { echo "nvsentinel-reboot: cannot read boot ID, not rebooting" >&2; exit 1; }; `+
			`if [ -z "$%[2]s" ] || [ -z "$cur" ]; then `+
			`echo "nvsentinel-reboot: boot ID missing, not rebooting" >&2; exit 1; fi; `+
			`if [ "$cur" != "$%[2]s" ]; then `+
			`echo "nvsentinel-reboot: boot ID changed from $%[2]s to $cur, skipping reboot"; exit 0; fi; `,
		bootIDPath, preRebootBootIDEnvVar)
}

// rebootAction returns the shell commands that write the optional syslog entry and reboot
// the node. The reboot runs last so a failed reboot makes the container exit non-zero.
func (c Config) rebootAction(nodeName, crName string) string {
	attribution := fmt.Sprintf("NVSentinel reboot: node=%s", nodeName)
	if crName != "" {
		attribution = fmt.Sprintf("NVSentinel reboot: node=%s cr=%s", nodeName, crName)
	}

	if c.UseSysrqReboot {
		if c.WriteSyslog {
			return fmt.Sprintf(
				"chroot %s logger -t nvsentinel-reboot -p daemon.notice %q || true; sync || true; echo b > %s/sysrq-trigger",
				hostMountPath, attribution, hostProcMountPath)
		}

		return fmt.Sprintf("echo b > %s/sysrq-trigger", hostProcMountPath)
	}

	if c.WriteSyslog {
		return fmt.Sprintf("chroot %s logger -t nvsentinel-reboot -p daemon.notice %q || true; chroot %s reboot",
			hostMountPath, attribution, hostMountPath)
	}

	return fmt.Sprintf("chroot %s reboot", hostMountPath)
}

// checkRebootJobPodStatus checks the reboot Job's pod for terminal failures
// (e.g., ImagePullBackOff, or rejection by the kubelet) that indicate the reboot was never attempted.
// bootIDChanged reports whether the node has booted since the Job was created.
func (c *Client) checkRebootJobPodStatus(ctx context.Context, nodeName string, bootIDChanged bool) error {
	pods, err := c.currentRebootPods(ctx, nodeName)
	if err != nil {
		slog.WarnContext(ctx, "Failed to list reboot job pods, skipping pod status check", "node", nodeName, "error", err)
		return nil
	}

	for _, pod := range pods {
		// A pod the kubelet rejects at admission (e.g., Evicted under DiskPressure) fails before
		// its container starts, so it cannot change the boot ID. The kubelet may reject the
		// pod again after the node boots, which does not undo the reboot, and the boot ID read
		// here can still be the old one for a while; see rejectedPodGracePeriod.
		if !bootIDChanged && failedBeforeStarting(pod) && c.jobFailedLongerThan(ctx, pod, rejectedPodGracePeriod) {
			return fmt.Errorf("reboot job pod failed before starting on node %s: %s: %s",
				nodeName, pod.Status.Reason, pod.Status.Message)
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

// currentRebootPods returns the node's scheduled or failed reboot pods that belong to a
// reboot Job which still exists and are not being deleted. A pod the kubelet rejects at
// admission has no PodScheduled condition. After cleanupRebootJob deletes a
// Job, its pods stay listed until the garbage collector removes them, and their state
// belongs to the earlier attempt.
func (c *Client) currentRebootPods(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	namespace := c.config.RebootJobNamespace
	labelSelector := fmt.Sprintf("%s=true,%s=%s", jobLabelKey, jobNodeLabelKey, nodeName)

	pods, err := c.k8sClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}

	if len(pods.Items) == 0 {
		return nil, nil
	}

	jobs, err := c.k8sClient.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}

	existingJobs := make(map[types.UID]struct{}, len(jobs.Items))
	for i := range jobs.Items {
		existingJobs[jobs.Items[i].UID] = struct{}{}
	}

	current := make([]corev1.Pod, 0, len(pods.Items))

	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil && (isPodScheduled(pod) || pod.Status.Phase == corev1.PodFailed) &&
			ownedByExistingJob(pod, existingJobs) {
			current = append(current, pod)
		}
	}

	return current, nil
}

// ownedByExistingJob reports whether pod belongs to one of the listed reboot Jobs. A pod
// whose owning Job is gone was left by an earlier attempt and is waiting for garbage
// collection. Pods without a Job owner are accepted so that nothing is skipped by mistake.
func ownedByExistingJob(pod corev1.Pod, existingJobs map[types.UID]struct{}) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind != "Job" {
			continue
		}

		_, exists := existingJobs[owner.UID]

		return exists
	}

	return true
}

// cleanupRebootJob deletes the node's reboot Jobs. It returns an error when the Jobs could
// not be listed or one of them could not be deleted; a Job that is already gone is not an
// error.
func (c *Client) cleanupRebootJob(ctx context.Context, nodeName string) error {
	namespace := c.config.RebootJobNamespace
	labelSelector := fmt.Sprintf("%s=true,%s=%s", jobLabelKey, jobNodeLabelKey, nodeName)
	propagation := metav1.DeletePropagationBackground

	jobs, err := c.k8sClient.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return fmt.Errorf("failed to list reboot jobs for node %s: %w", nodeName, err)
	}

	var errs []error

	for i := range jobs.Items {
		err := c.k8sClient.BatchV1().Jobs(namespace).Delete(ctx, jobs.Items[i].Name, metav1.DeleteOptions{
			PropagationPolicy: &propagation,
		})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("failed to delete reboot job %s: %w", jobs.Items[i].Name, err))
			continue
		}

		slog.InfoContext(ctx, "Cleaned up reboot job", "job", jobs.Items[i].Name, "node", nodeName)
	}

	return errors.Join(errs...)
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

// jobFailedLongerThan reports whether the Job owning pod has had its Failed condition for at
// least d. It returns false when the Job cannot be read or has not failed yet.
func (c *Client) jobFailedLongerThan(ctx context.Context, pod corev1.Pod, d time.Duration) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind != "Job" {
			continue
		}

		job, err := c.k8sClient.BatchV1().Jobs(c.config.RebootJobNamespace).Get(ctx, owner.Name, metav1.GetOptions{})
		if err != nil {
			slog.WarnContext(ctx, "Failed to get the reboot job of a failed pod",
				"pod", pod.Name, "job", owner.Name, "error", err)

			return false
		}

		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				return time.Since(condition.LastTransitionTime.Time) >= d
			}
		}
	}

	return false
}

// failedBeforeStarting reports whether pod failed before any of its containers ran. A container
// killed by the reboot it ran may leave its pod Failed before the node reports the new boot ID.
func failedBeforeStarting(pod corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodFailed {
		return false
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil {
			return false
		}
	}

	return true
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

// loadConfigFromEnv loads generic provider configuration from environment variables.
func loadConfigFromEnv() Config {
	image := os.Getenv("GENERIC_REBOOT_IMAGE")
	if image == "" {
		image = defaultRebootImage
	}

	useSysrqReboot := false

	if useSysrqStr := os.Getenv("GENERIC_REBOOT_USE_SYSRQ"); useSysrqStr != "" {
		parsed, err := strconv.ParseBool(useSysrqStr)
		if err != nil {
			slog.Warn("Invalid GENERIC_REBOOT_USE_SYSRQ, using default", "value", useSysrqStr, "default", false)
		} else {
			useSysrqReboot = parsed
		}
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

	writeSyslog := false

	if writeSyslogStr := os.Getenv("GENERIC_REBOOT_WRITE_SYSLOG"); writeSyslogStr != "" {
		parsed, err := strconv.ParseBool(writeSyslogStr)
		if err != nil {
			slog.Warn("Invalid GENERIC_REBOOT_WRITE_SYSLOG, using default", "value", writeSyslogStr, "default", false)
		} else {
			writeSyslog = parsed
		}
	}

	return Config{
		RebootImage:          image,
		UseSysrqReboot:       useSysrqReboot,
		RebootJobNamespace:   namespace,
		RebootJobTTL:         ttl,
		RebootJobPullSecrets: pullSecrets,
		WriteSyslog:          writeSyslog,
	}
}
