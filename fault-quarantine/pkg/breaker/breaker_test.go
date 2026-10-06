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

package breaker

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"math"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
)

var (
	testClient *kubernetes.Clientset
	testEnv    *envtest.Environment
)

// generateTestID generates a random hexadecimal string for test IDs
func generateTestID() string {
	const chars = "0123456789abcdef"
	result := make([]byte, 24)
	for i := range result {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		result[i] = chars[n.Int64()]
	}
	return string(result)
}

func TestMain(m *testing.M) {
	var err error

	testEnv = &envtest.Environment{}

	testRestConfig, err := testEnv.Start()
	if err != nil {
		log.Fatalf("Failed to start test environment: %v", err)
	}

	testClient, err = kubernetes.NewForConfig(testRestConfig)
	if err != nil {
		log.Fatalf("Failed to create kubernetes client: %v", err)
	}

	exitCode := m.Run()

	if err := testEnv.Stop(); err != nil {
		log.Fatalf("Failed to stop test environment: %v", err)
	}
	os.Exit(exitCode)
}

type testK8sClient struct {
	clientset      kubernetes.Interface
	informer       cache.SharedIndexInformer
	informerSynced cache.InformerSynced

	// cordonedNodes is what GetCordonedNodes reports. Production reads this from the
	// quarantine informer index; tests set it directly to drive the standing bound.
	cordonedNodes    int
	cordonedNodesErr error
}

func (c *testK8sClient) GetCordonedNodes(ctx context.Context) (int, error) {
	if c.cordonedNodesErr != nil {
		return 0, c.cordonedNodesErr
	}

	return c.cordonedNodes, nil
}

func (c *testK8sClient) GetTotalNodes(ctx context.Context) (int, error) {
	if !c.informerSynced() {
		return 0, fmt.Errorf("node informer cache not synced yet")
	}

	allObjs := c.informer.GetIndexer().List()
	return len(allObjs), nil
}

func (c *testK8sClient) EnsureCircuitBreakerConfigMap(ctx context.Context, name, namespace string, initialStatus State) error {
	cmClient := c.clientset.CoreV1().ConfigMaps(namespace)

	_, err := cmClient.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}

	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to get config map %s in namespace %s: %w", name, namespace, err)
	}

	cm := &corev1.ConfigMap{
		Name: name, Namespace: namespace,
		Data: map[string]string{
			"status": string(initialStatus),
			"cursor": string(CursorModeResume),
		},
	}

	_, err = cmClient.Create(ctx, cm, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create config map %s in namespace %s: %w", name, namespace, err)
	}

	return nil
}

func (c *testK8sClient) ReadCircuitBreakerState(ctx context.Context, name, namespace string) (State, error) {
	cm, err := c.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get config map %s in namespace %s: %w", name, namespace, err)
	}

	if cm.Data == nil {
		return "", nil
	}

	return State(cm.Data["status"]), nil
}

func (c *testK8sClient) WriteCircuitBreakerState(ctx context.Context, name, namespace string, state State) error {
	cm, err := c.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if cm.Data == nil {
		cm.Data = map[string]string{}
	}

	cm.Data["status"] = string(state)

	_, err = c.clientset.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func (c *testK8sClient) ReadCursorMode(ctx context.Context, name, namespace string) (CursorMode, error) {
	cm, err := c.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return CursorModeResume, fmt.Errorf("failed to get config map %s in namespace %s: %w", name, namespace, err)
	}

	if cm.Data == nil {
		return CursorModeResume, nil
	}

	cursor := cm.Data["cursor"]
	if cursor == "" {
		return CursorModeResume, nil
	}

	return CursorMode(cursor), nil
}

func (c *testK8sClient) WriteCursorMode(ctx context.Context, name, namespace string, mode CursorMode) error {
	cm, err := c.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if cm.Data == nil {
		cm.Data = map[string]string{}
	}

	cm.Data["cursor"] = string(mode)

	_, err = c.clientset.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func createTestNode(ctx context.Context, t *testing.T, name string) {
	t.Helper()

	node := &corev1.Node{
		Name: name,
		Spec: corev1.NodeSpec{},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	_, err := testClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test node %s: %v", name, err)
	}
}

func setupTestClient(t *testing.T) *testK8sClient {
	t.Helper()

	informerFactory := informers.NewSharedInformerFactory(testClient, 0)
	nodeInformerObj := informerFactory.Core().V1().Nodes()

	client := &testK8sClient{
		clientset:      testClient,
		informer:       nodeInformerObj.Informer(),
		informerSynced: nodeInformerObj.Informer().HasSynced,
	}

	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })

	go client.informer.Run(stopCh)

	if ok := cache.WaitForCacheSync(stopCh, client.informerSynced); !ok {
		t.Fatalf("Failed to sync node informer cache")
	}

	return client
}

func newTestBreaker(t *testing.T, ctx context.Context, totalNodes int, tripPercentage float64, window time.Duration, initialState State) CircuitBreaker {
	t.Helper()

	k8sClient := setupTestClient(t)

	// Create the specified number of nodes
	nodeNames := make([]string, totalNodes)
	for i := range totalNodes {
		nodeName := fmt.Sprintf("test-node-%d-%s", i, generateTestID()[:6])
		nodeNames[i] = nodeName
		createTestNode(ctx, t, nodeName)
	}

	t.Cleanup(func() {
		for _, nodeName := range nodeNames {
			_ = testClient.CoreV1().Nodes().Delete(context.Background(), nodeName, metav1.DeleteOptions{})
		}
	})

	// Wait for nodes to be visible in informer cache
	require.Eventually(t, func() bool {
		actualNodes, err := k8sClient.GetTotalNodes(ctx)
		return err == nil && actualNodes == totalNodes
	}, 5*time.Second, 50*time.Millisecond, "NodeInformer should see all %d nodes", totalNodes)

	configMapName := "test-breaker-" + generateTestID()[:8]

	// If initialState is provided, create ConfigMap with that state
	if initialState != "" {
		err := k8sClient.EnsureCircuitBreakerConfigMap(ctx, configMapName, "default", initialState)
		if err != nil {
			t.Fatalf("failed to ensure circuit breaker ConfigMap: %v", err)
		}
	}

	cfg := Config{
		Window:             window,
		TripPercentage:     tripPercentage,
		K8sClient:          k8sClient,
		ConfigMapName:      configMapName,
		ConfigMapNamespace: "default",
	}

	b, err := NewSlidingWindowBreaker(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create breaker: %v", err)
	}

	t.Cleanup(func() {
		_ = testClient.CoreV1().ConfigMaps("default").Delete(context.Background(), configMapName, metav1.DeleteOptions{})
	})

	return b
}

func TestDoesNotTripBelowThreshold(t *testing.T) {
	ctx := context.Background()
	b := newTestBreaker(t, ctx, 10, 50, 1*time.Second, "")

	t.Log("Adding 3 cordon events (below threshold of 5)")
	for i := range 3 {
		b.AddCordonEvent(fmt.Sprintf("node%d", i))
	}
	tripped, err := b.IsTripped(ctx)
	if err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	}
	if tripped {
		t.Fatalf("breaker should not trip below threshold (3 < 5)")
	}

	// Verify utilization metric (30% for 3 out of 10 nodes)
	utilization := getGaugeValue(t, metrics.FaultQuarantineBreakerUtilization)
	assert.GreaterOrEqual(t, utilization, float64(0.25), "Utilization should be at least 25%")
	assert.LessOrEqual(t, utilization, float64(0.35), "Utilization should be at most 35%")
}

func TestTripsWhenAboveThreshold(t *testing.T) {
	ctx := context.Background()
	b := newTestBreaker(t, ctx, 10, 50, 1*time.Second, "")

	// Track metrics before
	beforeDuration := getHistogramVecCount(t, metrics.FaultQuarantineGetTotalNodesDuration, "success")

	t.Log("Adding 5 cordon events (at threshold, should trip)")
	for i := range 5 {
		b.AddCordonEvent(fmt.Sprintf("node%d", i))
	}
	tripped, err := b.IsTripped(ctx)
	if err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	}
	if !tripped {
		t.Fatalf("breaker should trip at threshold (5 >= 5)")
	}

	// Verify getTotalNodes duration metric
	afterDuration := getHistogramVecCount(t, metrics.FaultQuarantineGetTotalNodesDuration, "success")
	assert.GreaterOrEqual(t, afterDuration, beforeDuration+1, "GetTotalNodesDuration should record observations")
}

func TestForceStateOverridesComputation(t *testing.T) {
	ctx := context.Background()
	b := newTestBreaker(t, ctx, 10, 50, 1*time.Second, "")

	t.Log("Verify breaker starts in CLOSED state")
	assert.Equal(t, StateClosed, b.CurrentState(), "Breaker should start in CLOSED state")

	t.Log("Force state to TRIPPED")
	err := b.ForceState(ctx, StateTripped)
	if err != nil {
		t.Fatalf("force trip failed: %v", err)
	}
	tripped, err := b.IsTripped(ctx)
	if err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	}
	if !tripped {
		t.Fatalf("breaker should report tripped after ForceState(StateTripped)")
	}
	assert.Equal(t, StateTripped, b.CurrentState(), "Breaker state should be TRIPPED")

	t.Log("Force state to CLOSED")
	err = b.ForceState(ctx, StateClosed)
	if err != nil {
		t.Fatalf("force close failed: %v", err)
	}
	tripped, err = b.IsTripped(ctx)
	if err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	}
	if tripped {
		t.Fatalf("breaker should not be tripped after ForceState(StateClosed)")
	}
	assert.Equal(t, StateClosed, b.CurrentState(), "Breaker state should be CLOSED after force close")
}

func TestWindowExpiryResetsCounts(t *testing.T) {
	ctx := context.Background()
	b := newTestBreaker(t, ctx, 10, 50, 1*time.Second, "")

	t.Log("Adding 6 cordon events (exceeds threshold)")
	for i := range 6 {
		b.AddCordonEvent(fmt.Sprintf("node%d", i))
	}
	tripped, err := b.IsTripped(ctx)
	if err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	}
	if !tripped {
		t.Fatalf("breaker should trip when above threshold")
	}

	t.Log("Force close to test window advance")
	err = b.ForceState(ctx, StateClosed)
	if err != nil {
		t.Fatalf("force close failed: %v", err)
	}

	t.Log("Wait for window to roll over (1s granularity)")
	time.Sleep(1100 * time.Millisecond)

	tripped, err = b.IsTripped(ctx)
	if err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	}
	if tripped {
		t.Fatalf("breaker should not trip after window expiry with no new events")
	}
}

func TestInitializeFromReadState(t *testing.T) {
	ctx := context.Background()
	b := newTestBreaker(t, ctx, 10, 50, 1*time.Second, StateTripped)

	t.Log("Verify breaker initialized with TRIPPED state")
	tripped, err := b.IsTripped(ctx)
	if err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	}
	if !tripped {
		t.Fatalf("breaker should be tripped when initialized with TRIPPED state")
	}
	if got := b.CurrentState(); got != StateTripped {
		t.Fatalf("expected initial state TRIPPED, got %s", got)
	}
}

func TestFlappingNodeDoesNotMultiplyCount(t *testing.T) {
	ctx := context.Background()
	b := newTestBreaker(t, ctx, 10, 50, 5*time.Second, "")

	t.Log("Add the same node 10 times (simulating flapping)")
	for range 10 {
		b.AddCordonEvent("flapping-node")
	}

	t.Log("Verify breaker does not trip for single flapping node")
	tripped, err := b.IsTripped(ctx)
	if err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	}
	if tripped {
		t.Fatalf("breaker should not trip for single flapping node (1 < 5)")
	}

	t.Log("Add 4 more unique nodes (total 5 unique nodes)")
	for i := range 4 {
		b.AddCordonEvent(fmt.Sprintf("node%d", i))
	}

	// Now should trip because we have 5 unique nodes (at threshold, >= 5)
	tripped, err = b.IsTripped(ctx)
	if err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	}
	if !tripped {
		t.Fatalf("breaker should trip with 5 unique nodes (5 >= 5 threshold)")
	}
}

// Helper functions for reading Prometheus metrics

func getGaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	err := gauge.Write(metric)
	require.NoError(t, err)
	return metric.Gauge.GetValue()
}

func getHistogramVecCount(t *testing.T, histogramVec *prometheus.HistogramVec, labelValues ...string) uint64 {
	t.Helper()

	metricCh := make(chan prometheus.Metric, 1)
	histogramVec.Collect(metricCh)
	close(metricCh)

	for metric := range metricCh {
		dtoMetric := &dto.Metric{}
		if err := metric.Write(dtoMetric); err != nil {
			continue
		}

		if dtoMetric.Histogram != nil {
			labelMatch := true
			for i, label := range dtoMetric.Label {
				if i < len(labelValues) && label.GetValue() != labelValues[i] {
					labelMatch = false
					break
				}
			}
			if labelMatch {
				return dtoMetric.Histogram.GetSampleCount()
			}
		}
	}

	return 0
}

func TestTripThreshold_ConfiguredBounds_ReturnsEffectiveThreshold(t *testing.T) {
	tests := []struct {
		name          string
		percentage    float64
		maxNodes      int
		totalNodes    int
		wantThreshold int
		wantBound     Bound
	}{
		{
			name:          "percentage only, unchanged from previous behaviour",
			percentage:    50,
			totalNodes:    10,
			wantThreshold: 5,
			wantBound:     boundPercentage,
		},
		{
			name:          "percentage rounds up, so 1 percent of 288 is 3 not 2",
			percentage:    1,
			totalNodes:    288,
			wantThreshold: 3,
			wantBound:     boundPercentage,
		},
		{
			name:          "maxNodes only, independent of fleet size",
			maxNodes:      5,
			totalNodes:    288,
			wantThreshold: 5,
			wantBound:     boundMaxNodes,
		},
		{
			name:          "both set, maxNodes is lower so it binds",
			percentage:    50,
			maxNodes:      10,
			totalNodes:    288,
			wantThreshold: 10,
			wantBound:     boundMaxNodes,
		},
		{
			name:          "both set, percentage is lower so it binds",
			percentage:    1,
			maxNodes:      100,
			totalNodes:    288,
			wantThreshold: 3,
			wantBound:     boundPercentage,
		},
		{
			name:          "both set and equal, percentage binds",
			percentage:    50,
			maxNodes:      5,
			totalNodes:    10,
			wantThreshold: 5,
			wantBound:     boundPercentage,
		},
		{
			// The motivating case: a percentage silently tracks fleet growth, an absolute
			// bound does not.
			name:          "fleet growth cannot raise the effective limit past maxNodes",
			percentage:    5,
			maxNodes:      10,
			totalNodes:    1000,
			wantThreshold: 10,
			wantBound:     boundMaxNodes,
		},
		{
			// The initializer rejects this config; asserted here so the reason the guard
			// exists stays visible.
			name:          "neither bound set yields no threshold",
			totalNodes:    288,
			wantThreshold: 0,
			wantBound:     boundNone,
		},
		{
			// Without the clamp this would be unreachable and disable the breaker.
			name:          "maxNodes above the fleet size is clamped to it",
			maxNodes:      1000,
			totalNodes:    288,
			wantThreshold: 288,
			wantBound:     boundFleetSize,
		},
		{
			name:          "percentage above 100 is clamped to the fleet size",
			percentage:    150,
			totalNodes:    288,
			wantThreshold: 288,
			wantBound:     boundFleetSize,
		},
		{
			// A percentage this large makes the float-to-int conversion
			// implementation-defined, so the comparison happens in float space.
			name:          "percentage large enough to overflow int still clamps",
			percentage:    3.3e18,
			totalNodes:    288,
			wantThreshold: 288,
			wantBound:     boundFleetSize,
		},
		{
			name:          "maxNodes equal to the fleet size still binds as maxNodes",
			maxNodes:      288,
			totalNodes:    288,
			wantThreshold: 288,
			wantBound:     boundMaxNodes,
		},
		{
			name:          "percentage of 100 is exactly the fleet size and is not clamped",
			percentage:    100,
			totalNodes:    288,
			wantThreshold: 288,
			wantBound:     boundPercentage,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &slidingWindowBreaker{
				cfg: Config{TripPercentage: tc.percentage, TripMaxNodes: tc.maxNodes},
			}

			threshold, bound := b.tripThreshold(tc.totalNodes)

			assert.Equal(t, tc.wantThreshold, threshold, "threshold")
			assert.Equal(t, tc.wantBound, bound, "binding bound")
		})
	}
}

func TestValidateBounds_RejectsConfigsThatCannotLimit(t *testing.T) {
	tests := []struct {
		name       string
		percentage float64
		maxNodes   int
		wantErr    string
	}{
		{
			name:       "percentage only is valid",
			percentage: 50,
		},
		{
			name:     "maxNodes only is valid",
			maxNodes: 5,
		},
		{
			name:       "both set is valid",
			percentage: 10,
			maxNodes:   20,
		},
		{
			// Would leave threshold 0, and IsTripped compares with >=, so the breaker
			// would trip immediately with no cordons.
			name:    "neither bound set is rejected",
			wantErr: "requires percentage, maxNodes, maxCordonedNodes or maxCordonedPercentage",
		},
		{
			// NaN passes every ordinary comparison, then becomes int(NaN) == 0, which trips
			// the breaker at startup with no cordons.
			name:       "NaN percentage is rejected",
			percentage: math.NaN(),
			wantErr:    "must be a finite number",
		},
		{
			name:       "infinite percentage is rejected",
			percentage: math.Inf(1),
			wantErr:    "must be a finite number",
		},
		{
			name:       "negative percentage is rejected rather than ignored",
			percentage: -10,
			wantErr:    "percentage must not be negative",
		},
		{
			name:     "negative maxNodes is rejected rather than ignored",
			maxNodes: -10,
			wantErr:  "maxNodes must not be negative",
		},
		{
			// A negative alongside a usable bound is still a config error, not a typo to
			// silently drop.
			name:       "negative maxNodes is rejected even with a valid percentage",
			percentage: 50,
			maxNodes:   -1,
			wantErr:    "maxNodes must not be negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Config{TripPercentage: tc.percentage, TripMaxNodes: tc.maxNodes}.validateBounds()

			if tc.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestNewSlidingWindowBreaker_NoBoundsConfigured_ReturnsError(t *testing.T) {
	b, err := NewSlidingWindowBreaker(context.Background(), Config{
		Window:             time.Second,
		K8sClient:          setupTestClient(t),
		ConfigMapName:      "test-breaker-unbounded",
		ConfigMapNamespace: "default",
	})

	require.Error(t, err)
	assert.Nil(t, b)
	assert.Contains(t, err.Error(), "requires percentage, maxNodes, maxCordonedNodes or maxCordonedPercentage")
}

// newStandingBoundBreaker builds a breaker whose only configured limit is the standing
// bound, with the fake reporting cordonedNodes. The window bounds are left off so these
// tests exercise the standing bound alone.
func newStandingBoundBreaker(
	t *testing.T, ctx context.Context,
	maxCordonedNodes, maxCordonedPercentage, cordonedNodes int, cordonedErr error,
) (CircuitBreaker, *testK8sClient) {
	t.Helper()

	k8sClient := setupTestClient(t)
	k8sClient.cordonedNodes = cordonedNodes
	k8sClient.cordonedNodesErr = cordonedErr

	nodeName := fmt.Sprintf("standing-node-%s", generateTestID()[:6])
	createTestNode(ctx, t, nodeName)

	t.Cleanup(func() {
		_ = testClient.CoreV1().Nodes().Delete(context.Background(), nodeName, metav1.DeleteOptions{})
	})

	require.Eventually(t, func() bool {
		n, err := k8sClient.GetTotalNodes(ctx)
		return err == nil && n > 0
	}, 5*time.Second, 50*time.Millisecond, "NodeInformer should see the test node")

	configMapName := "test-standing-" + generateTestID()[:8]

	b, err := NewSlidingWindowBreaker(ctx, Config{
		Window:                    1 * time.Second,
		TripMaxCordonedNodes:      maxCordonedNodes,
		TripMaxCordonedPercentage: float64(maxCordonedPercentage),
		K8sClient:                 k8sClient,
		ConfigMapName:             configMapName,
		ConfigMapNamespace:        "default",
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = testClient.CoreV1().ConfigMaps("default").Delete(
			context.Background(), configMapName, metav1.DeleteOptions{})
	})

	return b, k8sClient
}

func TestIsTripped_StandingBoundReached_Trips(t *testing.T) {
	ctx := context.Background()
	// No cordon events are added, so only the standing bound can trip this.
	b, _ := newStandingBoundBreaker(t, ctx, 3, 0, 3, nil)

	tripped, err := b.IsTripped(ctx)
	require.NoError(t, err)
	require.True(t, tripped, "standing bound reached should trip the breaker")
	require.Equal(t, StateTripped, b.CurrentState())
}

func TestIsTripped_StandingBoundNotReached_DoesNotTrip(t *testing.T) {
	ctx := context.Background()
	b, _ := newStandingBoundBreaker(t, ctx, 5, 0, 4, nil)

	tripped, err := b.IsTripped(ctx)
	require.NoError(t, err)
	require.False(t, tripped, "one below the standing bound should not trip")
	require.Equal(t, StateClosed, b.CurrentState())
}

func TestIsTripped_StandingBoundDisabled_IgnoresCordonedCount(t *testing.T) {
	ctx := context.Background()
	// maxCordonedNodes is 0, so the feature is off even though many nodes are cordoned.
	// A window bound is required for the config to be valid at all.
	k8sClient := setupTestClient(t)
	k8sClient.cordonedNodes = 500

	nodeName := fmt.Sprintf("disabled-node-%s", generateTestID()[:6])
	createTestNode(ctx, t, nodeName)

	t.Cleanup(func() {
		_ = testClient.CoreV1().Nodes().Delete(context.Background(), nodeName, metav1.DeleteOptions{})
	})

	require.Eventually(t, func() bool {
		n, err := k8sClient.GetTotalNodes(ctx)
		return err == nil && n > 0
	}, 5*time.Second, 50*time.Millisecond, "NodeInformer should see the test node")

	configMapName := "test-disabled-" + generateTestID()[:8]

	b, err := NewSlidingWindowBreaker(ctx, Config{
		Window:             1 * time.Second,
		TripMaxNodes:       1000,
		K8sClient:          k8sClient,
		ConfigMapName:      configMapName,
		ConfigMapNamespace: "default",
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = testClient.CoreV1().ConfigMaps("default").Delete(
			context.Background(), configMapName, metav1.DeleteOptions{})
	})

	tripped, err := b.IsTripped(ctx)
	require.NoError(t, err)
	require.False(t, tripped, "a zero standing bound must not act on the cordoned count")
}

func TestIsTripped_CordonedNodeLookupFails_ReturnsError(t *testing.T) {
	ctx := context.Background()
	b, _ := newStandingBoundBreaker(t, ctx, 3, 0, 0, fmt.Errorf("informer cache not synced"))

	// A failed lookup must surface rather than being read as "under the bound", which
	// would silently disable the guard exactly when the cluster state is unknown.
	_, err := b.IsTripped(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cordoned node count")
}

func TestValidateBounds_StandingBoundOnly_IsAccepted(t *testing.T) {
	require.NoError(t, Config{TripMaxCordonedNodes: 5}.validateBounds(),
		"the standing bound alone should satisfy the at-least-one-bound requirement")

	err := Config{TripMaxCordonedNodes: -1}.validateBounds()
	require.Error(t, err)
	require.Contains(t, err.Error(), "maxCordonedNodes must not be negative")
}

func TestIsTripped_WindowBoundReachedAndCordonedLookupFails_StillTrips(t *testing.T) {
	ctx := context.Background()

	k8sClient := setupTestClient(t)
	k8sClient.cordonedNodesErr = fmt.Errorf("informer cache not synced")

	nodeName := fmt.Sprintf("both-bounds-node-%s", generateTestID()[:6])
	createTestNode(ctx, t, nodeName)

	t.Cleanup(func() {
		_ = testClient.CoreV1().Nodes().Delete(context.Background(), nodeName, metav1.DeleteOptions{})
	})

	require.Eventually(t, func() bool {
		n, err := k8sClient.GetTotalNodes(ctx)
		return err == nil && n > 0
	}, 5*time.Second, 50*time.Millisecond, "NodeInformer should see the test node")

	configMapName := "test-both-" + generateTestID()[:8]

	b, err := NewSlidingWindowBreaker(ctx, Config{
		Window:               1 * time.Minute,
		TripMaxNodes:         1,
		TripMaxCordonedNodes: 10,
		K8sClient:            k8sClient,
		ConfigMapName:        configMapName,
		ConfigMapNamespace:   "default",
	})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = testClient.CoreV1().ConfigMaps("default").Delete(
			context.Background(), configMapName, metav1.DeleteOptions{})
	})

	// The window bound is already satisfied. A failure reading the standing bound must not
	// stop the breaker latching for a reason it had already earned.
	b.AddCordonEvent("some-cordoned-node")

	tripped, err := b.IsTripped(ctx)
	require.NoError(t, err)
	require.True(t, tripped, "window bound reached should trip even if the cordoned lookup would fail")
	require.Equal(t, StateTripped, b.CurrentState())
}

func TestStandingThreshold_ConfiguredBounds_ReturnsEffectiveThreshold(t *testing.T) {
	tests := []struct {
		name             string
		maxCordonedNodes int
		maxCordonedPct   float64
		totalNodes       int
		wantThreshold    int
		wantBound        Bound
	}{
		{
			name:             "absolute only, unchanged from previous behaviour",
			maxCordonedNodes: 3,
			totalNodes:       12,
			wantThreshold:    3,
			wantBound:        boundMaxCordonedNodes,
		},
		{
			// The motivating case: 3 of 12 is a quarter today, and 19% after a resize to 16.
			name:           "percentage tracks the fleet so a resize keeps the same share",
			maxCordonedPct: 25,
			totalNodes:     16,
			wantThreshold:  4,
			wantBound:      boundMaxCordonedPercentage,
		},
		{
			name:           "percentage rounds up, so 25 percent of 12 is 3",
			maxCordonedPct: 25,
			totalNodes:     12,
			wantThreshold:  3,
			wantBound:      boundMaxCordonedPercentage,
		},
		{
			name:           "percentage rounds up rather than to zero on a small fleet",
			maxCordonedPct: 1,
			totalNodes:     12,
			wantThreshold:  1,
			wantBound:      boundMaxCordonedPercentage,
		},
		{
			name:             "both set, absolute is lower so it binds",
			maxCordonedNodes: 3,
			maxCordonedPct:   50,
			totalNodes:       287,
			wantThreshold:    3,
			wantBound:        boundMaxCordonedNodes,
		},
		{
			name:             "both set, percentage is lower so it binds",
			maxCordonedNodes: 100,
			maxCordonedPct:   1,
			totalNodes:       287,
			wantThreshold:    3,
			wantBound:        boundMaxCordonedPercentage,
		},
		{
			name:             "fleet growth cannot raise the effective limit past the absolute bound",
			maxCordonedNodes: 10,
			maxCordonedPct:   5,
			totalNodes:       1000,
			wantThreshold:    10,
			wantBound:        boundMaxCordonedNodes,
		},
		{
			name:          "neither bound set yields no threshold, which disables the lookup",
			totalNodes:    12,
			wantThreshold: 0,
			wantBound:     boundNone,
		},
		{
			// Unlike tripThreshold, which clamps. The quarantined count comes from the
			// annotation index over every watched node while totalNodes counts only
			// GPU-labelled ones, so it can exceed the fleet size and clamping here would
			// trip the breaker spuriously.
			name:             "absolute bound above the fleet size is NOT clamped",
			maxCordonedNodes: 1000,
			totalNodes:       288,
			wantThreshold:    1000,
			wantBound:        boundMaxCordonedNodes,
		},
		{
			// The int conversion is the unsafe part, so this one is still clamped.
			name:           "percentage large enough to overflow int still clamps",
			maxCordonedPct: 3.3e18,
			totalNodes:     288,
			wantThreshold:  288,
			wantBound:      boundFleetSize,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &slidingWindowBreaker{cfg: Config{
				TripMaxCordonedNodes:      tt.maxCordonedNodes,
				TripMaxCordonedPercentage: tt.maxCordonedPct,
			}}

			threshold, bound := b.standingThreshold(tt.totalNodes)
			require.Equal(t, tt.wantThreshold, threshold)
			require.Equal(t, tt.wantBound, bound)
		})
	}
}

func TestValidateBounds_StandingPercentage(t *testing.T) {
	require.NoError(t, Config{TripMaxCordonedPercentage: 25}.validateBounds(),
		"the standing percentage alone should satisfy the at-least-one-bound requirement")

	err := Config{TripMaxCordonedPercentage: -1}.validateBounds()
	require.Error(t, err)
	require.Contains(t, err.Error(), "maxCordonedPercentage must not be negative")

	err = Config{TripMaxCordonedPercentage: math.NaN()}.validateBounds()
	require.Error(t, err)
	require.Contains(t, err.Error(), "maxCordonedPercentage must be a finite number")

	err = Config{TripMaxCordonedPercentage: math.Inf(1)}.validateBounds()
	require.Error(t, err)
	require.Contains(t, err.Error(), "maxCordonedPercentage must be a finite number")
}

func TestIsTripped_StandingPercentageReached_Trips(t *testing.T) {
	ctx := context.Background()
	// One GPU node in the test fleet, so 100% is one node and the single cordoned node
	// reaches it. No cordon events are added, so only the standing bound can trip this.
	b, _ := newStandingBoundBreaker(t, ctx, 0, 100, 1, nil)

	tripped, err := b.IsTripped(ctx)
	require.NoError(t, err)
	require.True(t, tripped, "standing percentage reached should trip the breaker")
	require.Equal(t, StateTripped, b.CurrentState())
}

func TestIsTripped_StandingPercentageNotReached_DoesNotTrip(t *testing.T) {
	ctx := context.Background()
	b, _ := newStandingBoundBreaker(t, ctx, 0, 100, 0, nil)

	tripped, err := b.IsTripped(ctx)
	require.NoError(t, err)
	require.False(t, tripped, "no cordoned nodes should not reach the standing percentage")
	require.Equal(t, StateClosed, b.CurrentState())
}
