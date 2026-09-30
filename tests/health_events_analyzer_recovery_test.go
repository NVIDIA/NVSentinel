//go:build mongodb && (amd64_group || arm64_group)

// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"tests/helpers"
)

const annotationRecoveryCheck = "AnnotationRecoveryTest"
const annotationRecoveryKey = "test.nvsentinel.nvidia.com/recover"

// Runs in the existing MongoDB E2E matrix, including the direct-publisher variants.
func TestHealthEventsAnalyzerAnnotationRecovery(t *testing.T) {
	feature := features.New("operator annotation recovers a derived condition").WithLabel("suite", "health-event-analyzer")
	var testCtx *helpers.HealthEventsAnalyzerTestContext
	var request string
	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		ctx = helpers.ApplyQuarantineConfig(ctx, t, c, "data/health-events-analyzer-recovery-quarantine.yaml")
		ctx, testCtx = helpers.SetupHealthEventsAnalyzerTest(ctx, t, c,
			"data/health-events-analyzer-annotation.yaml", "health-events-analyzer-recovery", "")
		return ctx
	})
	feature.Assess("a request without a fault does not cordon a healthy node", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		setRecoveryAnnotation(ctx, t, c, testCtx.NodeName, time.Now().UTC().Format(time.RFC3339Nano))
		waitRecoveryResult(ctx, t, c, testCtx.NodeName, "RecoverySkipped")
		requireRecoveryNodeState(ctx, t, c, testCtx.NodeName, false)
		return ctx
	})
	feature.Assess("fault, annotation, condition clear and uncordon", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		sendRecoverySource(ctx, t, testCtx.NodeName, "first")
		sendRecoverySource(ctx, t, testCtx.NodeName, "second")
		requireRecoveryNodeState(ctx, t, c, testCtx.NodeName, true)
		request = fmt.Sprintf(`{"recoveredAt":%q,"entities":[{"entityType":"GPU_UUID","entityValue":"GPU-recovery-test"}]}`,
			time.Now().UTC().Format(time.RFC3339Nano))
		setRecoveryAnnotation(ctx, t, c, testCtx.NodeName, request)
		waitRecoveryResult(ctx, t, c, testCtx.NodeName, "RecoveryCompleted")
		requireRecoveryNodeState(ctx, t, c, testCtx.NodeName, false)
		client, err := c.NewClient()
		require.NoError(t, err)
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		require.Equal(t, request, node.Annotations[annotationRecoveryKey])
		helpers.WaitForNodeConditionWithCheckName(ctx, t, client, testCtx.NodeName, annotationRecoveryCheck, "", "", corev1.ConditionFalse)
		return ctx
	})
	feature.Assess("restart restores history boundary and an old request cannot clear a new fault", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)
		require.NoError(t, helpers.RestartDeployment(ctx, t, client, helpers.HEALTH_EVENTS_ANALYZER_DEPLOYMENT_NAME, helpers.NVSentinelNamespace))
		sendRecoverySource(ctx, t, testCtx.NodeName, "third")
		// With the old history included, this single event would meet the threshold.
		require.Never(t, func() bool { return recoveryConditionActive(ctx, t, c, testCtx.NodeName) },
			15*time.Second, time.Second, "pre-recovery history must not re-trigger the rule")
		sendRecoverySource(ctx, t, testCtx.NodeName, "fourth")
		requireRecoveryNodeState(ctx, t, c, testCtx.NodeName, true)
		require.Never(t, func() bool { return !recoveryConditionActive(ctx, t, c, testCtx.NodeName) },
			10*time.Second, time.Second, "retained verification must not clear a newer fault")
		return ctx
	})
	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		defer helpers.RestoreQuarantineConfig(ctx, t, c)
		if testCtx == nil {
			return ctx
		}
		helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(testCtx.NodeName).
			WithAgent(helpers.HEALTH_EVENTS_ANALYZER_AGENT).WithCheckName(annotationRecoveryCheck).
			WithEntitiesImpacted([]helpers.EntityImpacted{{EntityType: "GPU_UUID", EntityValue: "GPU-recovery-test"}}).
			WithHealthy(true).WithFatal(false).WithRecommendedAction(int(pb.RecommendedAction_NONE)).
			WithProcessingStrategy(int(pb.ProcessingStrategy_EXECUTE_REMEDIATION)))
		setRecoveryAnnotation(ctx, t, c, testCtx.NodeName, nil)
		requireRecoveryNodeState(ctx, t, c, testCtx.NodeName, false)
		return helpers.TeardownHealthEventsAnalyzer(ctx, t, c, testCtx.NodeName, testCtx.ConfigMapBackup)
	})
	testEnv.Test(t, feature.Feature())
}

func sendRecoverySource(ctx context.Context, t *testing.T, node, code string) {
	t.Helper()
	helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(node).
		WithAgent("annotation-recovery-test").WithCheckName("RecoveryTestInput").
		WithEntitiesImpacted([]helpers.EntityImpacted{{EntityType: "GPU_UUID", EntityValue: "GPU-recovery-test"}}).
		WithErrorCode(code).WithFatal(false).WithHealthy(false).
		WithRecommendedAction(int(pb.RecommendedAction_NONE)).
		WithProcessingStrategy(int(pb.ProcessingStrategy_STORE_AND_ANALYSE)))
}

func setRecoveryAnnotation(ctx context.Context, t *testing.T, c *envconf.Config, name string, value any) {
	t.Helper()
	client, err := c.NewClient()
	require.NoError(t, err)
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{annotationRecoveryKey: value}}})
	require.NoError(t, err)
	node := &corev1.Node{}
	node.Name = name
	require.NoError(t, client.Resources().Patch(ctx, node, k8s.Patch{PatchType: types.MergePatchType, Data: patch}))
}

func recoveryConditionActive(ctx context.Context, t *testing.T, c *envconf.Config, name string) bool {
	t.Helper()
	client, err := c.NewClient()
	require.NoError(t, err)
	node, err := helpers.GetNodeByName(ctx, client, name)
	require.NoError(t, err)
	for _, condition := range node.Status.Conditions {
		if string(condition.Type) == annotationRecoveryCheck {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func requireRecoveryNodeState(ctx context.Context, t *testing.T, c *envconf.Config, name string, active bool) {
	t.Helper()
	client, err := c.NewClient()
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		node, err := helpers.GetNodeByName(ctx, client, name)
		return err == nil && node.Spec.Unschedulable == active && recoveryConditionActive(ctx, t, c, name) == active
	}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)
}

func waitRecoveryResult(ctx context.Context, t *testing.T, c *envconf.Config, name, reason string) {
	t.Helper()
	client, err := c.NewClient()
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		events := &corev1.EventList{}
		if err := client.Resources("default").List(ctx, events); err != nil {
			return false
		}
		for _, event := range events.Items {
			if event.InvolvedObject.Name == name && event.Source.Component == helpers.HEALTH_EVENTS_ANALYZER_AGENT && event.Reason == reason {
				return true
			}
		}
		return false
	}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)
}
