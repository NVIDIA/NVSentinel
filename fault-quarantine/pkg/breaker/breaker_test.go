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
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"crypto/rand"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
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
	"math/big"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
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
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       map[string]string{"status": string(initialStatus)},
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

func createTestNode(ctx context.Context, t *testing.T, name string) {
	t.Helper()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
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
	for i := 0; i < totalNodes; i++ {
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
	for i := 0; i < 3; i++ {
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
	for i := 0; i < 5; i++ {
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
	for i := 0; i < 6; i++ {
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
	for i := 0; i < 4; i++ {
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
