//go:build arm64_group
// +build arm64_group

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
	"testing"
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

const (
	cspPollingInterval     = 30 * time.Second
	originalArgsContextKey = "originalArgs"
)

// TestCSPHealthMonitorGCPMaintenanceEvent verifies the complete GCP maintenance event lifecycle:
// event detection, quarantine workflow, status transitions, and recovery behavior.
func TestCSPHealthMonitorGCPMaintenanceEvent(t *testing.T) {
	feature := features.New("GCP Maintenance Event Lifecycle").
		WithLabel("suite", "csp-health-monitor")

	var testCtx *helpers.CSPHealthMonitorTestContext
	var injectedEventID string
	var testInstanceID string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Setting up GCP health monitor test environment")

		var newCtx context.Context
		newCtx, testCtx = helpers.SetupCSPHealthMonitorTest(ctx, t, c, helpers.CSPGCP)

		t.Log("Clearing any existing GCP events from mock API")
		require.NoError(t, testCtx.CSPClient.ClearEvents(helpers.CSPGCP), "failed to clear GCP events")

		testInstanceID = fmt.Sprintf("%d", time.Now().UnixNano())
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Logf("Adding GCP instance annotation to node %s (instance_id=%s, zone=us-central1-a)", testCtx.NodeName, testInstanceID)
		require.NoError(t, helpers.AddGCPInstanceIDAnnotation(ctx, client, testCtx.NodeName, testInstanceID, "us-central1-a"))

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		require.Equal(t, testInstanceID, node.Annotations["container.googleapis.com/instance_id"], "GCP instance_id annotation not set")
		require.Equal(t, "us-central1-a", node.Labels["topology.kubernetes.io/zone"], "zone label not set")
		t.Log("Verified: node annotations and labels are set correctly")

		t.Log("Restarting csp-health-monitor to reset poll checkpoint and sync node informer")
		require.NoError(t, helpers.RestartDeployment(ctx, t, client, "csp-health-monitor", helpers.NVSentinelNamespace))

		// Wait for the monitor to complete at least one poll cycle
		helpers.WaitForCSPHealthMonitorPoll(t, testCtx.CSPClient, helpers.CSPGCP)

		return newCtx
	})

	feature.Assess("Step 1: Inject PENDING maintenance event scheduled 15 min ahead", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Injecting GCP maintenance event with PENDING status into mock Cloud Logging API")

		scheduledStart := time.Now().Add(15 * time.Minute)
		scheduledEnd := time.Now().Add(75 * time.Minute)
		event := helpers.CSPMaintenanceEvent{
			CSP:             helpers.CSPGCP,
			InstanceID:      testInstanceID,
			NodeName:        testCtx.NodeName,
			Zone:            "us-central1-a",
			ProjectID:       "test-project",
			Status:          "PENDING",
			EventTypeCode:   "compute.instances.upcomingMaintenance",
			MaintenanceType: "SCHEDULED",
			ScheduledStart:  &scheduledStart,
			ScheduledEnd:    &scheduledEnd,
			Description:     "Scheduled maintenance for GCP instance - e2e test",
		}

		var err error
		injectedEventID, _, err = testCtx.CSPClient.InjectEvent(event)
		require.NoError(t, err)
		t.Logf("Event injected: ID=%s, instanceID=%s, scheduledStart=%s", injectedEventID, testInstanceID, scheduledStart.Format(time.RFC3339))

		// Verify event was stored in mock
		eventCount, err := testCtx.CSPClient.GetEventCount(helpers.CSPGCP)
		require.NoError(t, err, "failed to get event count from mock")
		require.Equal(t, 1, eventCount, "expected 1 event in mock store after injection")
		t.Logf("Verified: mock store has %d GCP event(s)", eventCount)

		return ctx
	})

	feature.Assess("Step 2: Verify node is quarantined (cordoned + CSPMaintenance condition)", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Logf("Waiting for GCP monitor to poll (≤%v) and trigger engine to quarantine node", cspPollingInterval)

		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, true, true)
		t.Logf("Node %s is now quarantined with CSPMaintenance condition", testCtx.NodeName)

		return ctx
	})

	feature.Assess("Step 3: Transition to ONGOING status - node should remain quarantined", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Updating event status to ONGOING (maintenance in progress)")
		require.NoError(t, testCtx.CSPClient.UpdateEventStatus(helpers.CSPGCP, injectedEventID, "ONGOING"))

		t.Log("Waiting for monitor to poll and process status change")
		helpers.WaitForNextPoll(t, testCtx.CSPClient, helpers.CSPGCP)

		client, err := c.NewClient()
		require.NoError(t, err)
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		assert.True(t, node.Spec.Unschedulable, "Node should remain cordoned during ONGOING maintenance")
		t.Logf("Verified: Node %s remains cordoned during active maintenance", testCtx.NodeName)

		return ctx
	})

	feature.Assess("Step 4: Transition to COMPLETE status - node should recover (uncordon)", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Updating event status to COMPLETE (maintenance finished)")
		require.NoError(t, testCtx.CSPClient.UpdateEventStatus(helpers.CSPGCP, injectedEventID, "COMPLETE"))

		t.Log("Waiting for trigger engine to detect healthy state and uncordon node (1-min delay)")
		client, err := c.NewClient()
		require.NoError(t, err)
		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, false, false)
		t.Logf("Verified: Node %s has recovered (uncordoned, CSPMaintenance condition cleared)", testCtx.NodeName)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Tearing down: restoring original config and cleaning up test node")
		return helpers.TeardownCSPHealthMonitorTest(ctx, t, c, testCtx)
	})

	testEnv.Test(t, feature.Feature())
}

// TestCSPHealthMonitorAWSMaintenanceEvent verifies the complete AWS maintenance event lifecycle:
// AWS Health API integration, node mapping via providerID, and AWS-specific status transitions.
func TestCSPHealthMonitorAWSMaintenanceEvent(t *testing.T) {
	feature := features.New("AWS Maintenance Event Lifecycle").
		WithLabel("suite", "csp-health-monitor")

	var testCtx *helpers.CSPHealthMonitorTestContext
	var injectedEventID string
	var testInstanceID string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Setting up AWS health monitor test environment")

		var newCtx context.Context
		newCtx, testCtx = helpers.SetupCSPHealthMonitorTest(ctx, t, c, helpers.CSPAWS)

		t.Log("Clearing any existing AWS events from mock API")
		require.NoError(t, testCtx.CSPClient.ClearEvents(helpers.CSPAWS), "failed to clear AWS events")

		testInstanceID = fmt.Sprintf("i-%d", time.Now().UnixNano()%1000000000000)
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Logf("Adding AWS providerID to node %s (instance=%s, zone=us-east-1a)", testCtx.NodeName, testInstanceID)
		require.NoError(t, helpers.AddAWSProviderID(ctx, client, testCtx.NodeName, testInstanceID, "us-east-1a"))

		// Verify providerID was applied
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		expectedProviderID := fmt.Sprintf("aws:///us-east-1a/%s", testInstanceID)
		require.Equal(t, expectedProviderID, node.Spec.ProviderID, "AWS providerID not set correctly")
		t.Log("Verified: node providerID is set correctly")

		t.Log("Restarting csp-health-monitor to sync AWS node informer (uses AddFunc on initial cache sync)")
		require.NoError(t, helpers.RestartDeployment(ctx, t, client, "csp-health-monitor", helpers.NVSentinelNamespace))

		helpers.WaitForCSPHealthMonitorPoll(t, testCtx.CSPClient, helpers.CSPAWS)

		return newCtx
	})

	feature.Assess("Step 1: Inject 'upcoming' maintenance event scheduled 15 min ahead", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Injecting AWS maintenance event with 'upcoming' status into mock Health API")

		scheduledStart := time.Now().Add(15 * time.Minute)
		scheduledEnd := time.Now().Add(75 * time.Minute)
		event := helpers.CSPMaintenanceEvent{
			CSP:             helpers.CSPAWS,
			InstanceID:      testInstanceID,
			NodeName:        testCtx.NodeName,
			Region:          "us-east-1",
			AccountID:       "123456789012",
			Status:          "upcoming",
			EventTypeCode:   "AWS_EC2_MAINTENANCE_SCHEDULED",
			MaintenanceType: "SCHEDULED",
			ScheduledStart:  &scheduledStart,
			ScheduledEnd:    &scheduledEnd,
			Description:     "Instance is scheduled for maintenance.",
		}

		var err error
		var eventARN string
		injectedEventID, eventARN, err = testCtx.CSPClient.InjectEvent(event)
		require.NoError(t, err)
		t.Logf("Event injected: ID=%s, ARN=%s, instanceID=%s, scheduledStart=%s", injectedEventID, eventARN, testInstanceID, scheduledStart.Format(time.RFC3339))

		eventCount, err := testCtx.CSPClient.GetEventCount(helpers.CSPAWS)
		require.NoError(t, err, "failed to get event count from mock")
		require.Equal(t, 1, eventCount, "expected 1 event in mock store after injection")
		t.Logf("Verified: mock store has %d AWS event(s)", eventCount)

		return ctx
	})

	feature.Assess("Step 2: Verify node is quarantined (cordoned + CSPMaintenance condition)", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Logf("Waiting for AWS monitor to poll Health API (≤%v) and trigger engine to quarantine node", cspPollingInterval)

		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, true, true)
		t.Logf("Node %s is now quarantined with CSPMaintenance condition", testCtx.NodeName)

		return ctx
	})

	feature.Assess("Step 3: Transition to 'open' status - node should remain quarantined", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Updating event status to 'open' (maintenance in progress)")
		require.NoError(t, testCtx.CSPClient.UpdateEventStatus(helpers.CSPAWS, injectedEventID, "open"))

		t.Log("Waiting for monitor to poll and process status change")
		helpers.WaitForNextPoll(t, testCtx.CSPClient, helpers.CSPAWS)

		client, err := c.NewClient()
		require.NoError(t, err)
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		assert.True(t, node.Spec.Unschedulable, "Node should remain cordoned during 'open' maintenance")
		t.Logf("Verified: Node %s remains cordoned during active maintenance", testCtx.NodeName)

		return ctx
	})

	feature.Assess("Step 4: Transition to 'closed' status - node should recover (uncordon)", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Updating event status to 'closed' (maintenance finished)")
		require.NoError(t, testCtx.CSPClient.UpdateEventStatus(helpers.CSPAWS, injectedEventID, "closed"))

		t.Log("Waiting for monitor to poll and process status change")
		helpers.WaitForNextPoll(t, testCtx.CSPClient, helpers.CSPAWS)

		t.Log("Clearing mock API events to stop re-polling (re-polls would reset actualEndTime, blocking recovery)")
		require.NoError(t, testCtx.CSPClient.ClearEvents(helpers.CSPAWS))

		t.Log("Waiting for trigger engine to detect healthy state and uncordon node (1-min delay)")
		client, err := c.NewClient()
		require.NoError(t, err)
		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, false, false)
		t.Logf("Verified: Node %s has recovered (uncordoned, CSPMaintenance condition cleared)", testCtx.NodeName)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Tearing down: restoring original config and cleaning up test node")
		return helpers.TeardownCSPHealthMonitorTest(ctx, t, c, testCtx)
	})

	testEnv.Test(t, feature.Feature())
}

// TestCSPHealthMonitorQuarantineThreshold verifies the triggerQuarantineWorkflowTimeLimitMinutes threshold logic:
// trigger engine only quarantines nodes when scheduledStartTime <= now + threshold.
func TestCSPHealthMonitorQuarantineThreshold(t *testing.T) {
	feature := features.New("Quarantine Threshold Logic").
		WithLabel("suite", "csp-health-monitor")

	var testCtx *helpers.CSPHealthMonitorTestContext
	var injectedEventID string
	var testInstanceID string

	const thresholdMinutes = 1 // Use 1-minute threshold (minimum allowed) for faster testing

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Logf("Setting up GCP health monitor with %d-minute quarantine threshold", thresholdMinutes)

		var newCtx context.Context
		newCtx, testCtx = helpers.SetupCSPHealthMonitorTestWithThreshold(ctx, t, c, helpers.CSPGCP, thresholdMinutes)

		t.Log("Clearing any existing GCP events from mock API")
		require.NoError(t, testCtx.CSPClient.ClearEvents(helpers.CSPGCP), "failed to clear GCP events")

		testInstanceID = fmt.Sprintf("%d", time.Now().UnixNano())
		client, err := c.NewClient()
		require.NoError(t, err)

		t.Logf("Adding GCP instance annotation to node %s (instance_id=%s, zone=us-central1-a)", testCtx.NodeName, testInstanceID)
		require.NoError(t, helpers.AddGCPInstanceIDAnnotation(ctx, client, testCtx.NodeName, testInstanceID, "us-central1-a"))

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		require.Equal(t, testInstanceID, node.Annotations["container.googleapis.com/instance_id"], "GCP instance_id annotation not set")
		require.Equal(t, "us-central1-a", node.Labels["topology.kubernetes.io/zone"], "zone label not set")
		t.Log("Verified: node annotations and labels are set correctly")

		t.Log("Restarting csp-health-monitor to reset poll checkpoint and sync node informer")
		require.NoError(t, helpers.RestartDeployment(ctx, t, client, "csp-health-monitor", helpers.NVSentinelNamespace))

		helpers.WaitForCSPHealthMonitorPoll(t, testCtx.CSPClient, helpers.CSPGCP)

		return newCtx
	})

	feature.Assess("Step 1: Event OUTSIDE threshold (3 min ahead) should NOT trigger quarantine", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		scheduledStart := time.Now().Add(3 * time.Minute)
		scheduledEnd := time.Now().Add(63 * time.Minute)

		t.Logf("Injecting event scheduled 3 min ahead (outside %d-min threshold)", thresholdMinutes)
		event := helpers.CSPMaintenanceEvent{
			CSP:             helpers.CSPGCP,
			InstanceID:      testInstanceID,
			NodeName:        testCtx.NodeName,
			Zone:            "us-central1-a",
			ProjectID:       "test-project",
			Status:          "PENDING",
			EventTypeCode:   "compute.instances.upcomingMaintenance",
			MaintenanceType: "SCHEDULED",
			ScheduledStart:  &scheduledStart,
			ScheduledEnd:    &scheduledEnd,
			Description:     "Scheduled maintenance - threshold test",
		}

		var err error
		injectedEventID, _, err = testCtx.CSPClient.InjectEvent(event)
		require.NoError(t, err)
		t.Logf("Event injected: ID=%s, instanceID=%s, scheduledStart=%s", injectedEventID, testInstanceID, scheduledStart.Format(time.RFC3339))

		eventCount, err := testCtx.CSPClient.GetEventCount(helpers.CSPGCP)
		require.NoError(t, err, "failed to get event count from mock")
		require.Equal(t, 1, eventCount, "expected 1 event in mock store after injection")
		t.Logf("Verified: mock store has %d GCP event(s)", eventCount)

		t.Log("Waiting for monitor to poll and process event (should NOT trigger quarantine)")
		helpers.WaitForNextPoll(t, testCtx.CSPClient, helpers.CSPGCP)

		client, err := c.NewClient()
		require.NoError(t, err)
		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)

		assert.False(t, node.Spec.Unschedulable, "Node should NOT be cordoned - event scheduled beyond threshold")
		t.Logf("Verified: Node %s is NOT cordoned (scheduledStart > now + %d min threshold)", testCtx.NodeName, thresholdMinutes)

		return ctx
	})

	feature.Assess("Step 2: Update event to INSIDE threshold (50s ahead) should trigger quarantine", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		scheduledStart := time.Now().Add(50 * time.Second) // 50s accounts for polling delays

		t.Logf("Updating event scheduledStart to 50s ahead (inside %d-min threshold)", thresholdMinutes)
		require.NoError(t, testCtx.CSPClient.UpdateGCPEventScheduledTime(injectedEventID, scheduledStart))
		t.Logf("Event updated: scheduledStart=%s", scheduledStart.Format(time.RFC3339))

		t.Log("Waiting for trigger engine to detect event inside threshold and quarantine node")
		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, true, true)
		t.Logf("Verified: Node %s is now cordoned (scheduledStart <= now + %d min threshold)", testCtx.NodeName, thresholdMinutes)

		return ctx
	})

	feature.Assess("Step 3: Complete event and verify recovery", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Updating event status to COMPLETE")
		require.NoError(t, testCtx.CSPClient.UpdateEventStatus(helpers.CSPGCP, injectedEventID, "COMPLETE"))

		t.Log("Waiting for trigger engine to detect healthy state and uncordon node")
		client, err := c.NewClient()
		require.NoError(t, err)
		helpers.WaitForCSPMaintenanceCondition(ctx, t, client, testCtx.NodeName, false, false)
		t.Logf("Verified: Node %s has recovered (uncordoned, CSPMaintenance condition cleared)", testCtx.NodeName)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Tearing down: restoring original config and cleaning up test node")
		return helpers.TeardownCSPHealthMonitorTest(ctx, t, c, testCtx)
	})

	testEnv.Test(t, feature.Feature())
}

// TestCSPHealthMonitorStoreOnlyProcessingStrategy verifies the STORE_ONLY processing strategy:
// The event is stored in the database and exported as a CloudEvent, but does not trigger any cordoning or draining.
func TestCSPHealthMonitorStoreOnlyProcessingStrategy(t *testing.T) {
	feature := features.New("Processing Strategy").
		WithLabel("suite", "csp-health-monitor")

	var testCtx *helpers.CSPHealthMonitorTestContext
	var injectedEventID string
	var testInstanceID string

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		originalArgs, err := helpers.SetDeploymentArgs(ctx, t, client, "csp-health-monitor", helpers.NVSentinelNamespace, "maintenance-notifier", map[string]string{
			"--processing-strategy": "STORE_ONLY",
		})
		require.NoError(t, err)
		ctx = context.WithValue(ctx, originalArgsContextKey, originalArgs)

		helpers.WaitForDeploymentRollout(ctx, t, client, "csp-health-monitor", helpers.NVSentinelNamespace)

		var newCtx context.Context
		newCtx, testCtx = helpers.SetupCSPHealthMonitorTest(ctx, t, c, helpers.CSPGCP)

		t.Log("Clearing any existing GCP events from mock API")
		require.NoError(t, testCtx.CSPClient.ClearEvents(helpers.CSPGCP), "failed to clear GCP events")

		testInstanceID = fmt.Sprintf("%d", time.Now().UnixNano())

		t.Logf("Adding GCP instance annotation to node %s (instance_id=%s, zone=us-central1-a)", testCtx.NodeName, testInstanceID)
		require.NoError(t, helpers.AddGCPInstanceIDAnnotation(ctx, client, testCtx.NodeName, testInstanceID, "us-central1-a"))

		node, err := helpers.GetNodeByName(ctx, client, testCtx.NodeName)
		require.NoError(t, err)
		require.Equal(t, testInstanceID, node.Annotations["container.googleapis.com/instance_id"], "GCP instance_id annotation not set")
		require.Equal(t, "us-central1-a", node.Labels["topology.kubernetes.io/zone"], "zone label not set")
		t.Log("Verified: node annotations and labels are set correctly")

		// Wait for the monitor to complete at least one poll cycle
		helpers.WaitForCSPHealthMonitorPoll(t, testCtx.CSPClient, helpers.CSPGCP)

		return newCtx
	})

	feature.Assess("Injecting PENDING maintenance event and verifying node was not cordoned when processing STORE_ONLY strategy", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		t.Log("Injecting GCP maintenance event with PENDING status into mock Cloud Logging API")

		scheduledStart := time.Now().Add(15 * time.Minute)
		scheduledEnd := time.Now().Add(75 * time.Minute)
		event := helpers.CSPMaintenanceEvent{
			CSP:             helpers.CSPGCP,
			InstanceID:      testInstanceID,
			NodeName:        testCtx.NodeName,
			Zone:            "us-central1-a",
			ProjectID:       "test-project",
			Status:          "PENDING",
			EventTypeCode:   "compute.instances.upcomingMaintenance",
			MaintenanceType: "SCHEDULED",
			ScheduledStart:  &scheduledStart,
			ScheduledEnd:    &scheduledEnd,
			Description:     "Scheduled maintenance for GCP instance - e2e test",
		}

		var err error
		injectedEventID, _, err = testCtx.CSPClient.InjectEvent(event)
		require.NoError(t, err)
		t.Logf("Event injected: ID=%s, instanceID=%s, scheduledStart=%s", injectedEventID, testInstanceID, scheduledStart.Format(time.RFC3339))

		// Verify event was stored in mock
		eventCount, err := testCtx.CSPClient.GetEventCount(helpers.CSPGCP)
		require.NoError(t, err, "failed to get event count from mock")
		require.Equal(t, 1, eventCount, "expected 1 event in mock store after injection")
		t.Logf("Verified: mock store has %d GCP event(s)", eventCount)

		client, err := c.NewClient()
		require.NoError(t, err)

		helpers.EnsureNodeConditionNotPresent(ctx, t, client, testCtx.NodeName, "CSPMaintenance")
		t.Log("Verifying node was not cordoned when processing STORE_ONLY strategy")
		helpers.AssertQuarantineState(ctx, t, client, testCtx.NodeName, helpers.QuarantineAssertion{
			ExpectCordoned: false,
			AnnotationChecks: []helpers.AnnotationCheck{
				{Key: helpers.QuarantineHealthEventAnnotationKey, ShouldExist: false},
			},
		})

		t.Logf("Verified: node %s was not cordoned when processing STORE_ONLY strategy", testCtx.NodeName)

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		originalArgs := ctx.Value(originalArgsContextKey).([]string)

		err = helpers.RestoreDeploymentArgs(t, ctx, client, "csp-health-monitor", helpers.NVSentinelNamespace, "maintenance-notifier", originalArgs)
		require.NoError(t, err)

		helpers.WaitForDeploymentRollout(ctx, t, client, "csp-health-monitor", helpers.NVSentinelNamespace)

		helpers.TeardownCSPHealthMonitorTest(ctx, t, c, testCtx)

		return ctx
	})
	testEnv.Test(t, feature.Feature())
}
