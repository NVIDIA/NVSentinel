//go:build amd64_group
// +build amd64_group

// Preflight E2E test. Run (cluster must be up, e.g. after make dev-env or Tilt):
//   cd tests && go test -tags=amd64_group -run TestPreflightEndToEnd -v ./...

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

package tests

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// TestPreflightEndToEnd creates a 2-member KAI Scheduler PodGroup and verifies:
//  1. Webhook injects init containers on both gang pods
//  2. Init containers pass (exitCode=0)
//  3. Gang ConfigMap is fully populated (expected_count=2, 2 peers)
func TestPreflightEndToEnd(t *testing.T) {
	suffix := fmt.Sprintf("%d", time.Now().UnixMilli()%100000)
	testNS := "preflight-e2e-" + suffix
	pgName := "gang-pg-" + suffix

	const minMember = 2

	feature := features.New("Preflight webhook mutation and KAI gang coordination").
		WithLabel("suite", "preflight")

	var testCtx *helpers.PreflightTestContext

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		var newCtx context.Context
		newCtx, testCtx = helpers.SetupPreflightTest(
			ctx, t, c, testNS, pgName, minMember,
		)

		return newCtx
	})

	feature.Assess("webhook injected per-container inheritance config on both pods",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			for _, podName := range testCtx.PodNames {
				var pod v1.Pod

				err = client.Resources().Get(
					ctx, podName, testCtx.TestNamespace, &pod,
				)
				require.NoError(t, err)

				var initNames []string
				for _, ic := range pod.Spec.InitContainers {
					initNames = append(initNames, ic.Name)
				}

				require.NotEmpty(t, initNames,
					"pod %s should have init containers", podName)
				require.Contains(t, initNames,
					helpers.PreflightInheritEnabledName,
					"pod %s missing %s in %v",
					podName, helpers.PreflightInheritEnabledName, initNames)
				require.Contains(t, initNames,
					helpers.PreflightInheritDisabledName,
					"pod %s missing %s in %v",
					podName, helpers.PreflightInheritDisabledName, initNames)

				enabled := helpers.RequireInitContainer(t, pod, helpers.PreflightInheritEnabledName)
				disabled := helpers.RequireInitContainer(t, pod, helpers.PreflightInheritDisabledName)

				require.Equal(t, helpers.PreflightInheritedEnvValue,
					helpers.FindEnvValue(enabled.Env, helpers.PreflightInheritedEnvName),
					"pod %s: opted-in init container should inherit workload env", podName)
				require.True(t, helpers.HasVolumeMount(enabled.VolumeMounts, helpers.PreflightInheritedVolumeName),
					"pod %s: opted-in init container should inherit workload volume mount", podName)

				require.Empty(t,
					helpers.FindEnvValue(disabled.Env, helpers.PreflightInheritedEnvName),
					"pod %s: opted-out init container should not inherit workload env", podName)
				require.False(t, helpers.HasVolumeMount(disabled.VolumeMounts, helpers.PreflightInheritedVolumeName),
					"pod %s: opted-out init container should not inherit workload volume mount", podName)

				t.Logf("Pod %s init containers: %v", podName, initNames)
			}

			return ctx
		})

	feature.Assess("preflight init containers pass (exitCode=0)",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			for _, podName := range testCtx.PodNames {
				pod := helpers.WaitForPodInitContainerStatuses(
					ctx, t, client, testCtx.TestNamespace, podName,
				)

				for _, st := range pod.Status.InitContainerStatuses {
					if !strings.HasPrefix(st.Name, "preflight-") {
						continue
					}

					require.NotNil(t, st.State.Terminated,
						"pod %s init %s should have terminated",
						podName, st.Name)
					require.Equal(t, int32(0),
						st.State.Terminated.ExitCode,
						"pod %s init %s should pass (exitCode=0)",
						podName, st.Name)

					t.Logf("Pod %s init %s: passed (exitCode=0)",
						podName, st.Name)
				}
			}

			return ctx
		})

	feature.Assess("gang ConfigMap has expected_count=2 and 2 peers",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			gangID := helpers.ExpectedKAIGangID(testNS, pgName)
			helpers.AssertGangConfigMap(
				ctx, t, client, testCtx, gangID, minMember,
			)

			return ctx
		})

	feature.Assess("gang ConfigMap is garbage-collected with PodGroup owner",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			gangID := helpers.ExpectedKAIGangID(testNS, pgName)
			gangCM := helpers.AssertGangConfigMap(
				ctx, t, client, testCtx, gangID, minMember,
			)
			podGroup := helpers.GetKAIPodGroup(ctx, t, client, testNS, pgName)

			require.Contains(t, gangCM.OwnerReferences, metav1.OwnerReference{
				APIVersion: "scheduling.run.ai/v2alpha2",
				Kind:       "PodGroup",
				Name:       pgName,
				UID:        podGroup.GetUID(),
			}, "gang ConfigMap should be owned by the KAI PodGroup")

			helpers.DeleteKAIPodGroup(ctx, client, testNS, pgName)
			testCtx.PodGroupName = ""

			helpers.WaitForGangConfigMapDeleted(ctx, t, client, gangCM.Namespace, gangCM.Name)

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		return helpers.TeardownPreflightTest(ctx, t, c, testCtx)
	})

	testEnv.Test(t, feature.Feature())
}
