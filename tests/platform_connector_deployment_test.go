//go:build amd64_group || arm64_group
// +build amd64_group arm64_group

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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"tests/helpers"
)

// These tests exercise the deployment platform connector (ADR 052) the way a
// monitor does: over TLS with a pod-bound token and an idempotency key. They
// need the deployment platform connector, which tilt renders with
// USE_DEPLOYMENT_PLATFORM_CONNECTOR=1; without it they skip.

type platformConnectorDeploymentContextKey string

const (
	keyPCDFacts        platformConnectorDeploymentContextKey = "pcdFacts"
	keyPCDConn         platformConnectorDeploymentContextKey = "pcdConn"
	keyPCDPlainConn    platformConnectorDeploymentContextKey = "pcdPlainConn"
	keyPCDTargetNode   platformConnectorDeploymentContextKey = "pcdTargetNode"
	keyPCDClientToken  platformConnectorDeploymentContextKey = "pcdClientToken"
	keyPCDUnboundToken platformConnectorDeploymentContextKey = "pcdUnboundToken"
	keyPCDMonitorToken platformConnectorDeploymentContextKey = "pcdMonitorToken"
	keyPCDMonitorNode  platformConnectorDeploymentContextKey = "pcdMonitorNode"
	keyPCDSyslogNode   platformConnectorDeploymentContextKey = "pcdSyslogNode"
	keyPCDSyslogPod    platformConnectorDeploymentContextKey = "pcdSyslogPod"
	keyPCDStopChan     platformConnectorDeploymentContextKey = "pcdStopChan"
	keyPCDOriginalArgs platformConnectorDeploymentContextKey = "pcdOriginalArgs"
	keyPCDRules        platformConnectorDeploymentContextKey = "pcdRules"
	keyPCDCheckName    platformConnectorDeploymentContextKey = "pcdCheckName"

	// Local ports for the tests' own port-forwards; 8080, 9090 and 9091 are
	// taken by the Tilt forwards and the syslog stub journal.
	pcdTLSLocalPort       = 15051
	pcdPlaintextLocalPort = 15052

	// simpleHealthClientPodLabel selects the e2e fault-injection client,
	// whose ServiceAccount is on the connector's cross-node allowlist.
	simpleHealthClientPodLabel = "app=simple-health-client"

	// pcdMetricsSettleTimeout bounds how long a metric increment may lag the
	// request that caused it (the connector counts in-request, so this is
	// generous).
	pcdMetricsSettleTimeout = 30 * time.Second
	// pcdOutageDeliveryTimeout is how long a batch held during an outage
	// may take to land once the connector is back: the direct-mode client
	// retries at most 30 s apart and reconnects at most 10 s apart.
	// With the 45 s hold and a rollout of up to 2 min it stays inside the
	// client's 5 min retry window, so a slow rollout cannot drop the batch.
	pcdOutageDeliveryTimeout = 2 * time.Minute
	// pcdOutageHoldTime is how long the connector stays scaled to zero with
	// the batch pending, long enough for several client retries to fail.
	pcdOutageHoldTime = 45 * time.Second
	// pcdRBACSettleTime lets the API server's RBAC informers observe a
	// ClusterRole change before the next request depends on it.
	pcdRBACSettleTime = 5 * time.Second

	// pcdXIDCondition is the node condition the syslog monitor writes for XIDs.
	pcdXIDCondition = "SysLogsXIDError"
	// pcdMonitorAgent is the monitor whose pod-bound token the suite borrows.
	pcdMonitorAgent = "syslog-health-monitor"
)

// TestPlatformConnectorDeploymentCallerContract checks the connector's
// contract with its callers over the real TLS listener: no token, an unbound
// token, a batch without an idempotency key and a node-scoped token naming
// another node are all refused with the documented codes and counted, while
// a proper batch is stored once and its resend reported as a duplicate.
func TestPlatformConnectorDeploymentCallerContract(t *testing.T) {
	helpers.SkipWithoutPlatformConnectorDeployment(t, testEnv.EnvConf().Client())

	feature := features.New("Platform Connector Deployment - Caller Contract").
		WithLabel("suite", "platform-connector").
		WithLabel("component", "deployment-auth")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		facts := helpers.GetPlatformConnectorDeploymentFacts(ctx, t, client)
		ctx = context.WithValue(ctx, keyPCDFacts, facts)

		replica := helpers.GetReadyPlatformConnectorPod(ctx, t, client)
		t.Logf("Using connector replica %s (audience %s, TLS name %s)", replica.Name, facts.Audience, facts.ServerName)

		conn := helpers.DialPlatformConnectorPod(ctx, t, c.Client().RESTConfig(), replica, facts, pcdTLSLocalPort, false)
		ctx = context.WithValue(ctx, keyPCDConn, conn)

		plain := helpers.DialPlatformConnectorPod(ctx, t, c.Client().RESTConfig(), replica, facts, pcdPlaintextLocalPort, true)
		ctx = context.WithValue(ctx, keyPCDPlainConn, plain)

		// The cross-node caller: the e2e fault-injection client, whose
		// ServiceAccount is allowlisted for events naming any node.
		clientPods := &v1.PodList{}
		require.NoError(t, client.Resources(helpers.NVSentinelNamespace).List(ctx, clientPods,
			func(opts *metav1.ListOptions) { opts.LabelSelector = simpleHealthClientPodLabel }))
		require.NotEmpty(t, clientPods.Items, "no simple-health-client pod found")

		clientPod := &clientPods.Items[0]
		ctx = context.WithValue(ctx, keyPCDClientToken,
			helpers.MintPodBoundToken(ctx, t, c.Client().RESTConfig(), clientPod.Spec.ServiceAccountName, facts.Audience, clientPod))
		ctx = context.WithValue(ctx, keyPCDUnboundToken,
			helpers.MintPodBoundToken(ctx, t, c.Client().RESTConfig(), clientPod.Spec.ServiceAccountName, facts.Audience, nil))

		// A node-scoped caller: the syslog monitor on a real worker, whose
		// token is bound to that pod and so to that node.
		monitorPod, err := helpers.GetDaemonSetPodOnWorkerNode(ctx, t, client, helpers.SyslogDaemonSetName, "syslog-health-monitor-regular")
		require.NoError(t, err, "failed to find a syslog health monitor pod on a worker node")
		ctx = context.WithValue(ctx, keyPCDMonitorToken,
			helpers.MintPodBoundToken(ctx, t, c.Client().RESTConfig(), monitorPod.Spec.ServiceAccountName, facts.Audience, monitorPod))
		ctx = context.WithValue(ctx, keyPCDMonitorNode, monitorPod.Spec.NodeName)

		ctx = context.WithValue(ctx, keyPCDTargetNode, helpers.SelectTestNodeFromUnusedPool(ctx, t, client))

		return ctx
	})

	feature.Assess("refuses a plaintext connection", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		plain := ctx.Value(keyPCDPlainConn).(*helpers.PlatformConnectorConn)
		token := ctx.Value(keyPCDClientToken).(string)
		node := ctx.Value(keyPCDTargetNode).(string)

		plainCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		err := helpers.SendBatchToPlatformConnector(plainCtx, plain.Client, token, "plaintext-"+uuid.NewString(),
			helpers.NewPlatformConnectorBatch(helpers.NewProbeHealthEvent("simple-health-client", node, "PCDContractProbe", "plaintext")))
		require.Error(t, err, "a plaintext connection must not be served")
		assert.Contains(t, []codes.Code{codes.Unavailable, codes.DeadlineExceeded}, status.Code(err),
			"plaintext should fail at the transport, got %v", err)

		return ctx
	})

	feature.Assess("refuses a request without a token", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		conn := ctx.Value(keyPCDConn).(*helpers.PlatformConnectorConn)
		node := ctx.Value(keyPCDTargetNode).(string)

		err := helpers.SendBatchToPlatformConnector(ctx, conn.Client, "", "no-token-"+uuid.NewString(),
			helpers.NewPlatformConnectorBatch(helpers.NewProbeHealthEvent("simple-health-client", node, "PCDContractProbe", "no token")))
		require.Equal(t, codes.Unauthenticated, status.Code(err), "expected Unauthenticated, got %v", err)

		return ctx
	})

	feature.Assess("refuses a token that is not bound to a pod", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		conn := ctx.Value(keyPCDConn).(*helpers.PlatformConnectorConn)
		node := ctx.Value(keyPCDTargetNode).(string)
		unbound := ctx.Value(keyPCDUnboundToken).(string)

		err := helpers.SendBatchToPlatformConnector(ctx, conn.Client, unbound, "unbound-"+uuid.NewString(),
			helpers.NewPlatformConnectorBatch(helpers.NewProbeHealthEvent("simple-health-client", node, "PCDContractProbe", "unbound token")))
		require.Equal(t, codes.PermissionDenied, status.Code(err), "expected PermissionDenied, got %v", err)
		assert.Contains(t, status.Convert(err).Message(), "pod-bound")

		return ctx
	})

	feature.Assess("refuses a batch without an idempotency key", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		conn := ctx.Value(keyPCDConn).(*helpers.PlatformConnectorConn)
		node := ctx.Value(keyPCDTargetNode).(string)
		token := ctx.Value(keyPCDClientToken).(string)

		err := helpers.SendBatchToPlatformConnector(ctx, conn.Client, token, "",
			helpers.NewPlatformConnectorBatch(helpers.NewProbeHealthEvent("simple-health-client", node, "PCDContractProbe", "no key")))
		require.Equal(t, codes.InvalidArgument, status.Code(err), "expected InvalidArgument, got %v", err)
		assert.Contains(t, status.Convert(err).Message(), helpers.IdempotencyKeyHeader)

		return ctx
	})

	feature.Assess("refuses a node-scoped token naming another node and counts it",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			conn := ctx.Value(keyPCDConn).(*helpers.PlatformConnectorConn)
			facts := ctx.Value(keyPCDFacts).(*helpers.PlatformConnectorDeploymentFacts)
			node := ctx.Value(keyPCDTargetNode).(string)
			monitorToken := ctx.Value(keyPCDMonitorToken).(string)
			monitorNode := ctx.Value(keyPCDMonitorNode).(string)
			agent := pcdMonitorAgent
			require.NotEqual(t, monitorNode, node, "the test node must differ from the monitor's node")

			violations := map[string]string{"reason": "node_mismatch"}
			before := helpers.PlatformConnectorPodMetric(ctx, t, c.Client().RESTConfig(), conn.Pod, facts.MetricsPort,
				helpers.PlatformConnectorAuthViolationsMetric, violations)

			err := helpers.SendBatchToPlatformConnector(ctx, conn.Client, monitorToken, "scope-"+uuid.NewString(),
				helpers.NewPlatformConnectorBatch(helpers.NewProbeHealthEvent(agent, node, "PCDContractProbe", "wrong node")))
			require.Equal(t, codes.PermissionDenied, status.Code(err), "expected PermissionDenied, got %v", err)
			assert.Contains(t, status.Convert(err).Message(), "may only report health events for node")

			require.Eventually(t, func() bool {
				after := helpers.PlatformConnectorPodMetric(ctx, t, c.Client().RESTConfig(), conn.Pod, facts.MetricsPort,
					helpers.PlatformConnectorAuthViolationsMetric, violations)
				t.Logf("scope violations on %s: before=%v after=%v", conn.Pod.Name, before, after)

				return after >= before+1
			}, pcdMetricsSettleTimeout, time.Second, "the scope violation should be counted on the serving replica")

			return ctx
		})

	feature.Assess("accepts a node-scoped token for its own node and stamps a blank node name",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			conn := ctx.Value(keyPCDConn).(*helpers.PlatformConnectorConn)
			facts := ctx.Value(keyPCDFacts).(*helpers.PlatformConnectorDeploymentFacts)
			monitorToken := ctx.Value(keyPCDMonitorToken).(string)
			monitorNode := ctx.Value(keyPCDMonitorNode).(string)
			agent := pcdMonitorAgent

			stored := map[string]string{"outcome": "stored"}
			before := helpers.PlatformConnectorPodMetric(ctx, t, c.Client().RESTConfig(), conn.Pod, facts.MetricsPort,
				helpers.PlatformConnectorStoreBatchesMetric, stored)

			own := helpers.NewProbeHealthEvent(agent, monitorNode, "PCDContractProbe", "own node")
			require.NoError(t, helpers.SendBatchToPlatformConnector(ctx, conn.Client, monitorToken, "own-"+uuid.NewString(),
				helpers.NewPlatformConnectorBatch(own)), "a node-scoped token naming its own node must be accepted")

			blank := helpers.NewProbeHealthEvent(agent, "", "PCDContractProbe", "blank node name")
			require.NoError(t, helpers.SendBatchToPlatformConnector(ctx, conn.Client, monitorToken, "blank-"+uuid.NewString(),
				helpers.NewPlatformConnectorBatch(blank)), "a blank node name must be stamped from the token, not refused")

			require.Eventually(t, func() bool {
				after := helpers.PlatformConnectorPodMetric(ctx, t, c.Client().RESTConfig(), conn.Pod, facts.MetricsPort,
					helpers.PlatformConnectorStoreBatchesMetric, stored)
				t.Logf("stored batches on %s: before=%v after=%v", conn.Pod.Name, before, after)

				return after >= before+2
			}, pcdMetricsSettleTimeout, time.Second, "both batches should be counted as stored")

			return ctx
		})

	feature.Assess("stores a keyed batch once and reports its resend as a duplicate",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			conn := ctx.Value(keyPCDConn).(*helpers.PlatformConnectorConn)
			facts := ctx.Value(keyPCDFacts).(*helpers.PlatformConnectorDeploymentFacts)
			node := ctx.Value(keyPCDTargetNode).(string)
			token := ctx.Value(keyPCDClientToken).(string)

			stored := map[string]string{"outcome": "stored"}
			duplicate := map[string]string{"outcome": "duplicate"}
			storedBefore := helpers.PlatformConnectorPodMetric(ctx, t, c.Client().RESTConfig(), conn.Pod, facts.MetricsPort,
				helpers.PlatformConnectorStoreBatchesMetric, stored)
			duplicateBefore := helpers.PlatformConnectorPodMetric(ctx, t, c.Client().RESTConfig(), conn.Pod, facts.MetricsPort,
				helpers.PlatformConnectorStoreBatchesMetric, duplicate)

			key := "resend-" + uuid.NewString()
			batch := helpers.NewPlatformConnectorBatch(
				helpers.NewProbeHealthEvent("simple-health-client", node, "PCDContractProbe", "keyed batch"))

			require.NoError(t, helpers.SendBatchToPlatformConnector(ctx, conn.Client, token, key, batch), "first send")
			require.NoError(t, helpers.SendBatchToPlatformConnector(ctx, conn.Client, token, key, batch),
				"a resend with the same key must be acknowledged, not refused")

			require.Eventually(t, func() bool {
				storedAfter := helpers.PlatformConnectorPodMetric(ctx, t, c.Client().RESTConfig(), conn.Pod, facts.MetricsPort,
					helpers.PlatformConnectorStoreBatchesMetric, stored)
				duplicateAfter := helpers.PlatformConnectorPodMetric(ctx, t, c.Client().RESTConfig(), conn.Pod, facts.MetricsPort,
					helpers.PlatformConnectorStoreBatchesMetric, duplicate)
				t.Logf("replica %s: stored %v -> %v, duplicate %v -> %v",
					conn.Pod.Name, storedBefore, storedAfter, duplicateBefore, duplicateAfter)

				// The replica's counters are shared with every monitor publishing to
				// it, so they can only show that both outcomes happened.
				return storedAfter >= storedBefore+1 && duplicateAfter >= duplicateBefore+1
			}, pcdMetricsSettleTimeout, time.Second, "a stored and a duplicate outcome expected on the replica")

			// Exactly-once is checked on the store itself: one document under this
			// test's key, however many times the batch was sent. PostgreSQL-backed
			// runs have no MongoDB pod and keep the metric evidence alone.
			if mongoPod, ok := helpers.TryGetMongoDBPrimaryPodName(ctx, t, c.Client()); ok {
				js := fmt.Sprintf(`print("DOCS=" + db.getSiblingDB("%s").HealthEvents.countDocuments(`+
					`{"healthevent.metadata.idempotencyKey": {$regex: "#%s#0$"}}) + ";")`, helpers.MongoDBDatabase, key)
				stdout, _ := helpers.ExecMongosh(ctx, t, c.Client().RESTConfig(), c.Client(), mongoPod, js)
				require.Contains(t, stdout, "DOCS=1;", "exactly one document for the resent key")
			}

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		if conn, ok := ctx.Value(keyPCDConn).(*helpers.PlatformConnectorConn); ok {
			conn.Close()
		}

		if plain, ok := ctx.Value(keyPCDPlainConn).(*helpers.PlatformConnectorConn); ok {
			plain.Close()
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestPlatformConnectorDeploymentDeliversAfterOutage scales the connector to
// zero, has a real node-local monitor detect a fault meanwhile, and checks
// that the monitor keeps the batch (no restart, no drop) and delivers it
// once the connector is back.
func TestPlatformConnectorDeploymentDeliversAfterOutage(t *testing.T) {
	helpers.SkipWithoutPlatformConnectorDeployment(t, testEnv.EnvConf().Client())

	feature := features.New("Platform Connector Deployment - Delivery After Outage").
		WithLabel("suite", "platform-connector").
		WithLabel("component", "deployment-outage")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		facts := helpers.GetPlatformConnectorDeploymentFacts(ctx, t, client)
		require.Positive(t, facts.Replicas, "the connector Deployment should have replicas to scale back to")
		ctx = context.WithValue(ctx, keyPCDFacts, facts)

		// A real worker's syslog monitor with a stub journal we can feed; the
		// node is left unmanaged so the fatal XID does not cordon it.
		nodeName, syslogPod, stopChan, originalArgs := helpers.SetUpSyslogHealthMonitor(ctx, t, client, nil, false)
		ctx = context.WithValue(ctx, keyPCDSyslogNode, nodeName)
		ctx = context.WithValue(ctx, keyPCDSyslogPod, syslogPod.Name)
		ctx = context.WithValue(ctx, keyPCDStopChan, stopChan)
		ctx = context.WithValue(ctx, keyPCDOriginalArgs, originalArgs)

		// Start from a healthy XID condition whatever an earlier test left on
		// the node, so the only fault the node can show afterwards is ours.
		helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(nodeName).
			WithAgent(pcdMonitorAgent).
			WithCheckName(pcdXIDCondition).
			WithComponentClass("GPU").
			WithHealthy(true).
			WithFatal(false).
			WithMessage("No Health Failures").
			WithEntitiesImpacted([]helpers.EntityImpacted{}))

		require.Eventually(t, func() bool {
			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName, pcdXIDCondition, pcdXIDCondition+"IsNotHealthy")
			if err != nil {
				t.Logf("failed to read node conditions: %v", err)

				return false
			}

			return condition == nil
		}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "the node should start without an unhealthy %s condition", pcdXIDCondition)

		return ctx
	})

	feature.Assess("a fault detected during the outage is delivered once the connector returns",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err, "failed to create kubernetes client")

			facts := ctx.Value(keyPCDFacts).(*helpers.PlatformConnectorDeploymentFacts)
			nodeName := ctx.Value(keyPCDSyslogNode).(string)
			podName := ctx.Value(keyPCDSyslogPod).(string)

			restartsBefore, err := helpers.PodRestartCount(ctx, client, helpers.NVSentinelNamespace, podName)
			require.NoError(t, err)

			t.Logf("Scaling %s to zero", helpers.PlatformConnectorDeploymentName)
			require.NoError(t, helpers.ScaleDeployment(ctx, t, client, helpers.PlatformConnectorDeploymentName,
				helpers.NVSentinelNamespace, 0))
			helpers.WaitForNoPlatformConnectorPods(ctx, t, client, 3*time.Minute)

			// The pid makes this run's XID recognisable in the condition message.
			pid := fmt.Sprintf("pid=%d", time.Now().Unix())
			xidMessages := []string{
				"kernel: [16450076.435595] NVRM: Xid (PCI:0002:00:00): 119, " + pid + ", name=nvc:[driver], " +
					"Timeout after 6s of waiting for RPC response from GPU1 GSP! Expected function 76 (GSP_RM_CONTROL) (0x20802a02 0x8).",
			}
			helpers.InjectSyslogMessages(t, helpers.StubJournalHTTPPort, xidMessages)

			t.Logf("Holding the outage for %s with the batch pending in the monitor", pcdOutageHoldTime)
			time.Sleep(pcdOutageHoldTime)

			condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName, pcdXIDCondition, pcdXIDCondition+"IsNotHealthy")
			require.NoError(t, err)
			require.Nil(t, condition, "nothing can have delivered the fault while the connector had no replicas")

			restartsDuring, err := helpers.PodRestartCount(ctx, client, helpers.NVSentinelNamespace, podName)
			require.NoError(t, err)
			require.Equal(t, restartsBefore, restartsDuring,
				"the monitor must stay alive while it waits on the connector (liveness hook)")

			t.Logf("Scaling %s back to %d", helpers.PlatformConnectorDeploymentName, facts.Replicas)
			require.NoError(t, helpers.ScaleDeployment(ctx, t, client, helpers.PlatformConnectorDeploymentName,
				helpers.NVSentinelNamespace, facts.Replicas))
			// Well inside the pending batch's 5 minute retry window, which started
			// at the monitor's Publish call during the outage.
			helpers.WaitForDeploymentRolloutWithTimeout(ctx, t, client, helpers.PlatformConnectorDeploymentName,
				helpers.NVSentinelNamespace, 2*time.Minute)

			connectorBack := time.Now()

			require.Eventually(t, func() bool {
				condition, err := helpers.CheckNodeConditionExists(ctx, client, nodeName, pcdXIDCondition, pcdXIDCondition+"IsNotHealthy")
				if err != nil {
					t.Logf("failed to read node conditions: %v", err)

					return false
				}

				return condition != nil && condition.Status == v1.ConditionTrue && strings.Contains(condition.Message, pid)
			}, pcdOutageDeliveryTimeout, time.Second,
				"the batch pending during the outage should land once the connector is back")
			t.Logf("Fault delivered %s after the connector was back", time.Since(connectorBack).Round(time.Second))

			restartsAfter, err := helpers.PodRestartCount(ctx, client, helpers.NVSentinelNamespace, podName)
			require.NoError(t, err)
			assert.Equal(t, restartsBefore, restartsAfter, "the monitor must not have restarted")

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		// Whatever happened above, the connector must be back before the
		// syslog teardown, whose restarted monitor publishes a healthy event.
		if facts, ok := ctx.Value(keyPCDFacts).(*helpers.PlatformConnectorDeploymentFacts); ok {
			if err := helpers.ScaleDeployment(ctx, t, client, helpers.PlatformConnectorDeploymentName,
				helpers.NVSentinelNamespace, facts.Replicas); err != nil {
				t.Logf("Warning: failed to scale the connector back: %v", err)
			}

			// A failed wait must not skip the syslog cleanup below, so it is
			// deferred past it.
			defer helpers.WaitForDeploymentRolloutWithTimeout(ctx, t, client, helpers.PlatformConnectorDeploymentName,
				helpers.NVSentinelNamespace, 5*time.Minute)
		}

		nodeName, _ := ctx.Value(keyPCDSyslogNode).(string)
		podName, _ := ctx.Value(keyPCDSyslogPod).(string)
		stopChan, _ := ctx.Value(keyPCDStopChan).(chan struct{})
		originalArgs, _ := ctx.Value(keyPCDOriginalArgs).([]string)

		if nodeName != "" && stopChan != nil {
			helpers.TearDownSyslogHealthMonitor(ctx, t, client, nodeName, stopChan, originalArgs, podName)
		}

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

// TestPlatformConnectorDeploymentAcknowledgesWhenNodeUpdatesFail takes the
// connector's right to write node status away, so the store keeps working
// while every node condition update is forbidden: the batch must still be
// acknowledged, the failure counted, and conditions must land again once the
// right is restored.
func TestPlatformConnectorDeploymentAcknowledgesWhenNodeUpdatesFail(t *testing.T) {
	helpers.SkipWithoutPlatformConnectorDeployment(t, testEnv.EnvConf().Client())

	feature := features.New("Platform Connector Deployment - Node Update Failure").
		WithLabel("suite", "platform-connector").
		WithLabel("component", "deployment-conditions")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		ctx = context.WithValue(ctx, keyPCDFacts, helpers.GetPlatformConnectorDeploymentFacts(ctx, t, client))

		// A check name of this run's own: a healthy event for a check the node
		// has never shown creates the condition, so the first write is always
		// attempted (an unchanged healthy condition is skipped, not written).
		ctx = context.WithValue(ctx, keyPCDCheckName,
			"PCDNodeUpdateProbe"+strings.ToUpper(strings.ReplaceAll(uuid.NewString()[:8], "-", "")))

		rules, err := helpers.GetClusterRoleRules(ctx, client, helpers.PlatformConnectorDeploymentName)
		require.NoError(t, err)
		require.NotEmpty(t, rules, "the connector ClusterRole has no rules")
		ctx = context.WithValue(ctx, keyPCDRules, rules)

		ctx = context.WithValue(ctx, keyPCDTargetNode, helpers.SelectTestNodeFromUnusedPool(ctx, t, client))

		return ctx
	})

	feature.Assess("acknowledges a stored batch whose node update is forbidden and counts the failure",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err, "failed to create kubernetes client")

			facts := ctx.Value(keyPCDFacts).(*helpers.PlatformConnectorDeploymentFacts)
			rules := ctx.Value(keyPCDRules).([]rbacv1.PolicyRule)
			node := ctx.Value(keyPCDTargetNode).(string)
			checkName := ctx.Value(keyPCDCheckName).(string)

			failures := func() float64 {
				return helpers.SumPlatformConnectorMetric(ctx, t, c.Client().RESTConfig(), client, facts.MetricsPort,
					helpers.PlatformConnectorBestEffortFailuresMetric,
					map[string]string{"connector": "kubernetes", "reason": "failed"}) +
					helpers.SumPlatformConnectorMetric(ctx, t, c.Client().RESTConfig(), client, facts.MetricsPort,
						helpers.PlatformConnectorBestEffortFailuresMetric,
						map[string]string{"connector": "kubernetes", "reason": "timeout"})
			}
			before := failures()

			t.Logf("Removing the connector's node write permissions (probe condition %s on %s)", checkName, node)
			require.NoError(t, helpers.SetClusterRoleRules(ctx, client, helpers.PlatformConnectorDeploymentName,
				helpers.ReadOnlyNodeRules(rules)))
			time.Sleep(pcdRBACSettleTime)

			// A healthy event: the store accepts it, the k8s connector writes a
			// node condition for it (healthy and fatal events become conditions,
			// non-fatal unhealthy ones do not), and nothing quarantines the node
			// over it.
			helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(node).
				WithCheckName(checkName).
				WithComponentClass("GPU").
				WithHealthy(true).
				WithFatal(false).
				WithMessage("node update forbidden probe").
				WithEntitiesImpacted([]helpers.EntityImpacted{}))

			require.Eventually(t, func() bool {
				after := failures()
				t.Logf("condition update failures across replicas: before=%v after=%v", before, after)

				return after >= before+1
			}, pcdMetricsSettleTimeout, time.Second, "the failed node update should be counted")

			condition, err := helpers.CheckNodeConditionExists(ctx, client, node, checkName, checkName+"IsHealthy")
			require.NoError(t, err)
			assert.Nil(t, condition, "the forbidden write cannot have landed on the node")

			t.Log("Restoring the connector's ClusterRole")
			require.NoError(t, helpers.SetClusterRoleRules(ctx, client, helpers.PlatformConnectorDeploymentName, rules))
			time.Sleep(pcdRBACSettleTime)

			helpers.SendHealthEvent(ctx, t, helpers.NewHealthEvent(node).
				WithCheckName(checkName).
				WithComponentClass("GPU").
				WithHealthy(true).
				WithFatal(false).
				WithMessage("node update restored probe").
				WithEntitiesImpacted([]helpers.EntityImpacted{}))

			require.Eventually(t, func() bool {
				condition, err := helpers.CheckNodeConditionExists(ctx, client, node, checkName, checkName+"IsHealthy")
				if err != nil {
					t.Logf("failed to read node conditions: %v", err)

					return false
				}

				return condition != nil && condition.Status == v1.ConditionFalse
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval, "the condition should land once the permission is back")

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		if rules, ok := ctx.Value(keyPCDRules).([]rbacv1.PolicyRule); ok {
			if err := helpers.SetClusterRoleRules(ctx, client, helpers.PlatformConnectorDeploymentName, rules); err != nil {
				t.Logf("Warning: failed to restore the connector ClusterRole: %v", err)
			}
		}

		// The probe leaves a healthy condition of its own name on the node,
		// which harms nothing and needs no clearing.
		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
