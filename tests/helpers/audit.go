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

package helpers

import (
	"context"
	_ "embed"
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
)

const auditLogDir = "/var/log/nvsentinel"

// auditCheckScript is embedded from audit_check.sh
// It validates audit logs exist and have correct JSON format using jq.
//
//go:embed audit_check.sh
var auditCheckScript string

// VerifyAuditLogsExist verifies audit logs exist for the specified components using Jobs.
func VerifyAuditLogsExist(ctx context.Context, t *testing.T, c klient.Client, components []string) {
	for _, comp := range components {
		t.Run("verify-audit-logs-"+comp, func(t *testing.T) {
			nodes, err := getNodesForComponent(ctx, t, c, comp)
			if err != nil {
				t.Fatalf("failed to get nodes for component %s: %v", comp, err)
			}

			if len(nodes) == 0 {
				t.Fatalf("no running pods found for %s - cannot verify audit logs", comp)
			}

			t.Logf("Checking audit logs for %s on nodes: %v", comp, nodes)

			for _, node := range nodes {
				found, err := runAuditCheck(ctx, t, c, node, comp)
				if err != nil {
					t.Logf("Audit check failed on node %s: %v", node, err)
					continue
				}

				if found {
					return
				}
			}

			t.Fatalf("no valid audit logs found for %s on any node", comp)
		})
	}
}

// getNodesForComponent returns the list of nodes where pods for the given component are running.
// Returns an error if the Kubernetes API call fails.
func getNodesForComponent(ctx context.Context, t *testing.T, c klient.Client, comp string) ([]string, error) {
	t.Helper()

	var podList v1.PodList

	err := c.Resources(NVSentinelNamespace).List(ctx, &podList,
		resources.WithLabelSelector(fmt.Sprintf("app.kubernetes.io/name=%s", comp)),
		resources.WithFieldSelector("status.phase=Running"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	if len(podList.Items) == 0 {
		return nil, nil
	}

	// Collect unique node names
	nodeSet := make(map[string]bool)

	for _, pod := range podList.Items {
		if pod.Spec.NodeName != "" {
			nodeSet[pod.Spec.NodeName] = true
		}
	}

	nodes := make([]string, 0, len(nodeSet))
	for node := range nodeSet {
		nodes = append(nodes, node)
	}

	return nodes, nil
}

// runAuditCheck creates a Job to verify audit logs on the specified node.
// Returns (true, nil) if valid logs found, (false, nil) if no logs, or (false, error) on infra failure.
func runAuditCheck(ctx context.Context, t *testing.T, c klient.Client, node, comp string) (bool, error) {
	t.Helper()

	zero, ttl := int32(0), int32(60)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "audit-check-", Namespace: NVSentinelNamespace},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &zero,
			TTLSecondsAfterFinished: &ttl,
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					NodeSelector:  map[string]string{"kubernetes.io/hostname": node},
					RestartPolicy: v1.RestartPolicyNever,
					Tolerations: []v1.Toleration{
						{
							Key:      "node-role.kubernetes.io/control-plane",
							Operator: v1.TolerationOpExists,
							Effect:   v1.TaintEffectNoSchedule,
						},
						{
							Key:      "node-role.kubernetes.io/master",
							Operator: v1.TolerationOpExists,
							Effect:   v1.TaintEffectNoSchedule,
						},
					},
					Containers: []v1.Container{{
						Name:    "c",
						Image:   "alpine:3.21",
						Command: []string{"sh", "-c", auditCheckScript},
						Args:    []string{"sh", comp, auditLogDir},
						VolumeMounts: []v1.VolumeMount{
							{Name: "v", MountPath: auditLogDir, ReadOnly: true},
						},
					}},
					Volumes: []v1.Volume{{
						Name:         "v",
						VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: auditLogDir}},
					}},
				},
			},
		},
	}

	if err := c.Resources(NVSentinelNamespace).Create(ctx, job); err != nil {
		return false, fmt.Errorf("failed to create audit check job: %w", err)
	}

	// job.Name is now populated with the generated name
	defer func() {
		_ = c.Resources(NVSentinelNamespace).Delete(ctx, job)
	}()

	return waitForJobCompletion(ctx, c, job.Name)
}

// waitForJobCompletion waits for a job to complete and returns whether it succeeded.
func waitForJobCompletion(ctx context.Context, c klient.Client, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	for {
		var job batchv1.Job

		if err := c.Resources(NVSentinelNamespace).Get(ctx, name, NVSentinelNamespace, &job); err != nil {
			return false, fmt.Errorf("failed to get job status: %w", err)
		}

		if job.Status.Succeeded > 0 {
			return true, nil
		}

		if job.Status.Failed > 0 {
			return false, nil
		}

		select {
		case <-ctx.Done():
			return false, fmt.Errorf("timeout waiting for job to complete")
		case <-time.After(time.Second):
		}
	}
}
