// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"tests/helpers"
)

type scaleTestContextKey int

const (
	keyNodes scaleTestContextKey = iota
	keyHealthCheckNodes
)

// 45% of nodes will be tested
const nodeSubsetPercentage = 45

func TestScaleHealthEvents(t *testing.T) {
	feature := features.New("TestScaleHealthEvents").
		WithLabel("suite", "scale")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		workloadNamespace := "workloads"

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		err = helpers.CreateNamespace(ctx, client, workloadNamespace)
		assert.NoError(t, err, "failed to create workloads namespace")

		nodes, err := helpers.GetAllNodesNames(ctx, client)
		assert.NoError(t, err, "failed to get cluster nodes")
		t.Logf("Found %d nodes in cluster", len(nodes))

		healthCheckNodes := selectNodeSubset(nodes, float64(nodeSubsetPercentage)/100)
		t.Logf("Selected %d nodes (%d%%) for health check operations", len(healthCheckNodes), nodeSubsetPercentage)

		ctx = context.WithValue(ctx, keyNamespace, workloadNamespace)
		ctx = context.WithValue(ctx, keyNodes, nodes)
		ctx = context.WithValue(ctx, keyHealthCheckNodes, healthCheckNodes)

		return ctx
	})

	feature.Assess("Create one pod per GPU on each node and wait for ready", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		namespaceName := ctx.Value(keyNamespace).(string)
		nodes := ctx.Value(keyNodes).([]string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		podTemplate := helpers.NewGPUPodSpec(namespaceName, 1)
		podTemplate.ObjectMeta.Annotations = map[string]string{
			"test.massive.annotation/config-1": generateRandomString(30000),
		}
		podTemplate.Spec.Containers[0].Env = []v1.EnvVar{
			{Name: "MASSIVE_CONFIG_1", Value: generateRandomString(30000)},
		}

		helpers.CreatePodsAndWaitTillRunning(ctx, t, client, nodes, podTemplate)

		return ctx
	})

	feature.Assess(fmt.Sprintf("Send unhealthy events to %d%% of nodes", nodeSubsetPercentage), func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		healthCheckNodes := ctx.Value(keyHealthCheckNodes).([]string)

		err := helpers.SendHealthEventsToNodes(healthCheckNodes, "data/fatal-health-event.json")
		assert.NoError(t, err, "failed to send unhealthy events")

		return ctx
	})

	feature.Assess(fmt.Sprintf("Validate %d%% of nodes are cordoned and have drain label", nodeSubsetPercentage), func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		healthCheckNodes := ctx.Value(keyHealthCheckNodes).([]string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		helpers.WaitForNodesCordonedAndDrained(ctx, t, client, healthCheckNodes)

		return ctx
	})

	feature.Assess("Check Prometheus metrics for API server health", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		// Check for slow LIST operations (>5 seconds at 90th percentile) // TODO this is causing failures depending on the load in CI, need a better way
		// to detect this.
		// err := helpers.CheckPrometheusMetricEmpty(ctx, t,
		// 	`histogram_quantile(0.90, sum(rate(apiserver_request_duration_seconds_bucket{verb="LIST"}[5m])) by (le, instance, resource)) > 5`,
		// 	"slow LIST operations (>5s at p90)")
		// assert.NoError(t, err, "API server LIST operations should not be slow")

		return ctx
	})

	feature.Assess(fmt.Sprintf("Send healthy events to %d%% of nodes", nodeSubsetPercentage), func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		healthCheckNodes := ctx.Value(keyHealthCheckNodes).([]string)

		err := helpers.SendHealthEventsToNodes(healthCheckNodes, "data/healthy-event.json")
		assert.NoError(t, err, "failed to send healthy events")

		return ctx
	})

	feature.Assess("Validate health check nodes are uncordoned", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		healthCheckNodes := ctx.Value(keyHealthCheckNodes).([]string)

		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		helpers.WaitForNodesCordonState(ctx, t, client, healthCheckNodes, false)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		assert.NoError(t, err, "failed to create kubernetes client")

		namespaceName := ctx.Value(keyNamespace).(string)
		err = helpers.DeleteNamespace(ctx, t, client, namespaceName)
		assert.NoError(t, err, "failed to delete workloads namespace")

		err = helpers.DeleteAllRebootNodeCRs(ctx, t, client)
		assert.NoError(t, err, "failed to delete RebootNode CRs")

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func selectNodeSubset(nodeNames []string, percentage float64) []string {
	if percentage >= 1.0 {
		return nodeNames
	}
	if percentage <= 0.0 {
		return []string{}
	}

	targetCount := int(float64(len(nodeNames)) * percentage)
	if targetCount == 0 && len(nodeNames) > 0 {
		targetCount = 1
	}

	if targetCount >= len(nodeNames) {
		return nodeNames
	}

	selected := make([]string, targetCount)
	copy(selected, nodeNames[:targetCount])

	return selected
}

func generateRandomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 ._-"
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}
