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
	"testing"
	"tests/helpers"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestEventExporterChangeStream(t *testing.T) {
	feature := features.New("EventExporterChangeStream").
		WithLabel("suite", helpers.EventExporterName)

	var nodeName string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Waiting for event-exporter to be available")
		err = wait.For(
			conditions.New(client.Resources()).DeploymentAvailable(helpers.EventExporterName, helpers.NVSentinelNamespace),
			wait.WithTimeout(helpers.EventuallyWaitTimeout),
		)
		require.NoError(t, err, "event-exporter not running")

		t.Log("Selecting test node from unused pool")
		nodeName = helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)
		return ctx
	})

	feature.Assess("exports event via changestream", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Getting initial mock event count")
		initialCount := helpers.GetMockEventCount(t, c)
		t.Logf("Initial mock event count: %d", initialCount)

		t.Log("Sending health event to node")
		testMessage := "XID 79 injected from event exporter changestream test"
		event := helpers.NewHealthEvent(nodeName).
			WithErrorCode("79").
			WithMessage(testMessage)
		helpers.SendHealthEvent(ctx, t, event)

		t.Log("Waiting for event to be exported via changestream")
		var receivedEvent map[string]any
		require.Eventually(t, func() bool {
			events := helpers.GetMockEvents(t, c)
			event, found := helpers.FindEventByNodeAndMessage(events, nodeName, testMessage)
			if found {
				receivedEvent = event
				return true
			}
			return false
		}, helpers.NeverWaitTimeout, helpers.WaitInterval, "event should be exported via changestream")

		t.Log("Validating received CloudEvent")
		require.NotNil(t, receivedEvent)
		helpers.ValidateCloudEvent(t, receivedEvent, nodeName, testMessage, "GpuXidError", "79")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Sending healthy event to node")
		helpers.SendHealthyEvent(ctx, t, nodeName)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func TestEventExporterResumeToken(t *testing.T) {
	feature := features.New("EventExporterResumeToken").
		WithLabel("suite", helpers.EventExporterName)

	var nodeName string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Waiting for event-exporter to be available")
		err = wait.For(
			conditions.New(client.Resources()).DeploymentAvailable(helpers.EventExporterName, helpers.NVSentinelNamespace),
			wait.WithTimeout(helpers.EventuallyWaitTimeout),
		)
		require.NoError(t, err)

		t.Log("Selecting test node from unused pool")
		nodeName = helpers.SelectTestNodeFromUnusedPool(ctx, t, client)
		t.Logf("Selected test node: %s", nodeName)

		t.Log("Waiting for exporter to create resume token")
		time.Sleep(10 * time.Second)

		return ctx
	})

	feature.Assess("resumes from checkpoint after restart", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Log("Getting event-exporter deployment")
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      helpers.EventExporterName,
				Namespace: helpers.NVSentinelNamespace,
			},
		}
		err = client.Resources().Get(ctx, helpers.EventExporterName, helpers.NVSentinelNamespace, deployment)
		require.NoError(t, err)

		t.Log("Scaling down event-exporter to 0 replicas")
		originalReplicas := *deployment.Spec.Replicas
		zero := int32(0)
		deployment.Spec.Replicas = &zero
		err = client.Resources().Update(ctx, deployment)
		require.NoError(t, err)

		t.Log("Waiting for event-exporter to be scaled down")
		require.Eventually(t, func() bool {
			err := client.Resources().Get(ctx, helpers.EventExporterName, helpers.NVSentinelNamespace, deployment)
			if err != nil {
				return false
			}
			return deployment.Status.ReadyReplicas == 0
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "deployment should have 0 ready replicas")

		t.Log("Injecting health event while exporter is down")
		testMessage := "XID 79 injected from event exporter resume token test"
		event := helpers.NewHealthEvent(nodeName).
			WithErrorCode("79").
			WithMessage(testMessage)
		helpers.SendHealthEvent(ctx, t, event)

		t.Log("Getting initial mock event count")
		initialCount := helpers.GetMockEventCount(t, c)
		t.Logf("Mock sink events before restart: %d", initialCount)

		t.Log("Scaling back event-exporter to original replicas")
		deployment.Spec.Replicas = &originalReplicas
		err = client.Resources().Update(ctx, deployment)
		require.NoError(t, err)

		t.Log("Waiting for event-exporter to be scaled back and ready")
		err = wait.For(
			conditions.New(client.Resources()).DeploymentAvailable(helpers.EventExporterName, helpers.NVSentinelNamespace),
			wait.WithTimeout(helpers.EventuallyWaitTimeout),
		)
		require.NoError(t, err)

		t.Log("Waiting for missed event to be exported after resume")
		require.Eventually(t, func() bool {
			events := helpers.GetMockEvents(t, c)
			_, found := helpers.FindEventByNodeAndMessage(events, nodeName, testMessage)
			if found {
				t.Logf("Found matching event after resume: nodeName=%s message=%s", nodeName, testMessage)
				return true
			}
			t.Logf("No matching event found yet, total events: %d", len(events))
			return false
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "missed event should be exported after resume")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Sending healthy event to node")
		helpers.SendHealthyEvent(ctx, t, nodeName)
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
