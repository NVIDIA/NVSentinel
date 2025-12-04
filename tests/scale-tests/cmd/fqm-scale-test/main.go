// FQM Scale Test - End-to-End Latency and Queue Depth Measurement
//
// Measures the full pipeline: SIGUSR1 → Event Generator → Platform Connector → MongoDB → FQM → Node Cordoned
//
// Modes:
//   - latency:     Measure P90 latency from fatal event → node cordon (Scenario 3)
//   - queue-depth: Measure queue depth and processing rate (Scenario 4)
//   - combined:    Run both measurements together
//
// Usage:
//   go build -o fqm-scale-test .
//   ./fqm-scale-test -mode=latency -nodes=50 -concurrent=false    # Scenario 3a (baseline)
//   ./fqm-scale-test -mode=latency -nodes=150 -concurrent=true    # Scenario 3b (under load)
//   ./fqm-scale-test -mode=combined -nodes=150                    # Scenario 4 (queue depth)

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// Configuration - matches existing event-generator-daemonset.yaml
const (
	EventGeneratorLabel = "app=event-generator"
)

// ============================================================================
// Data Structures
// ============================================================================

// LatencyMeasurement tracks individual node cordon latency
type LatencyMeasurement struct {
	NodeName   string
	EventTime  time.Time // T0: when we sent SIGUSR1
	CordonTime time.Time // T1: when node became unschedulable
	LatencyMs  int64
	Status     string // "success", "timeout", "error"
}

// QueueSnapshot tracks queue state at a point in time
type QueueSnapshot struct {
	Timestamp      time.Time
	ElapsedSeconds float64
	CordonedCount  int
	QueueDepth     int     // nodes waiting to be cordoned
	ProcessingRate float64 // nodes/second
}

// ============================================================================
// Main
// ============================================================================

func main() {
	// Parse flags
	mode := flag.String("mode", "combined", "Test mode: latency, queue-depth, or combined")
	numNodes := flag.Int("nodes", 150, "Number of nodes to test")
	namespace := flag.String("namespace", "nvsentinel", "NVSentinel namespace")
	kubeconfig := flag.String("kubeconfig", "", "Path to kubeconfig (default: ~/.kube/config)")
	k8sContext := flag.String("context", "rs3", "Kubernetes context")
	timeout := flag.Int("timeout", 180, "Timeout in seconds")
	outputDir := flag.String("output", "./results", "Output directory")
	concurrent := flag.Bool("concurrent", true, "Run events concurrently (false = sequential baseline)")
	maxStagger := flag.Int("stagger", 30, "Max seconds to stagger concurrent events")
	pollInterval := flag.Int("poll", 1, "Poll interval in seconds for queue depth")
	flag.Parse()

	// Validate mode
	validModes := map[string]bool{"latency": true, "queue-depth": true, "combined": true}
	if !validModes[*mode] {
		log.Fatalf("Invalid mode: %s. Must be: latency, queue-depth, or combined", *mode)
	}

	// Print configuration
	log.Printf("🔬 NVSentinel FQM Scale Test")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("Mode:       %s", *mode)
	log.Printf("Nodes:      %d", *numNodes)
	log.Printf("Namespace:  %s", *namespace)
	log.Printf("Context:    %s", *k8sContext)
	log.Printf("Timeout:    %ds", *timeout)
	if *concurrent {
		log.Printf("Execution:  Concurrent (stagger: 0-%ds)", *maxStagger)
	} else {
		log.Printf("Execution:  Sequential (baseline)")
	}
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("")

	// Create Kubernetes client
	clientset, err := createK8sClient(*kubeconfig, *k8sContext)
	if err != nil {
		log.Fatalf("Failed to create K8s client: %v", err)
	}

	ctx := context.Background()

	// Get schedulable nodes
	nodes, err := getSchedulableNodes(ctx, clientset, *numNodes)
	if err != nil {
		log.Fatalf("Failed to get nodes: %v", err)
	}
	log.Printf("✅ Selected %d nodes for testing", len(nodes))
	log.Printf("")

	// Create output directory
	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		log.Fatalf("Failed to create output directory: %v", err)
	}

	// Run test based on mode
	var latencyResults []LatencyMeasurement
	var queueResults []QueueSnapshot

	switch *mode {
	case "latency":
		latencyResults = runLatencyTest(ctx, clientset, nodes, *namespace, *timeout, *concurrent, *maxStagger)
	case "queue-depth":
		queueResults = runQueueDepthTest(ctx, clientset, nodes, *namespace, *timeout, *maxStagger, *pollInterval)
	case "combined":
		latencyResults, queueResults = runCombinedTest(ctx, clientset, nodes, *namespace, *timeout, *maxStagger, *pollInterval)
	}

	// Save and display results
	timestamp := time.Now().Format("20060102-150405")

	if len(latencyResults) > 0 {
		displayLatencyStats(latencyResults)
		saveLatencyResults(latencyResults, *outputDir, timestamp, *mode, *concurrent)
	}

	if len(queueResults) > 0 {
		displayQueueStats(queueResults, len(nodes))
		saveQueueResults(queueResults, len(nodes), *outputDir, timestamp)
	}

	log.Printf("")
	log.Printf("✅ Test complete!")
}

// ============================================================================
// Kubernetes Client
// ============================================================================

func createK8sClient(kubeconfig, k8sContext string) (*kubernetes.Clientset, error) {
	if kubeconfig == "" {
		kubeconfig = clientcmd.RecommendedHomeFile
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: k8sContext},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	return kubernetes.NewForConfig(config)
}

func getSchedulableNodes(ctx context.Context, clientset *kubernetes.Clientset, limit int) ([]string, error) {
	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var schedulableNodes []string
	var cordonedCount int

	for _, node := range nodeList.Items {
		// Skip system-workload nodes (no event generators on them)
		if node.Labels["dedicated"] == "system-workload" {
			continue
		}

		if node.Spec.Unschedulable {
			cordonedCount++
		} else {
			schedulableNodes = append(schedulableNodes, node.Name)
		}
	}

	log.Printf("📊 Cluster State:")
	log.Printf("   Total nodes:      %d", len(nodeList.Items))
	log.Printf("   Already cordoned: %d", cordonedCount)
	log.Printf("   Available:        %d", len(schedulableNodes))

	// Circuit breaker warning (FQM stops at ~50%)
	circuitBreakerLimit := len(nodeList.Items) / 2
	if limit > circuitBreakerLimit-cordonedCount {
		log.Printf("   ⚠️  WARNING: May hit circuit breaker (~50%% of cluster)")
	}

	if len(schedulableNodes) > limit {
		schedulableNodes = schedulableNodes[:limit]
	}

	return schedulableNodes, nil
}

// ============================================================================
// Node Watcher - Watches for cordon events via K8s API
// ============================================================================

type NodeWatcher struct {
	clientset   *kubernetes.Clientset
	cordonTimes map[string]time.Time
	mu          sync.RWMutex
}

func NewNodeWatcher(clientset *kubernetes.Clientset) *NodeWatcher {
	return &NodeWatcher{
		clientset:   clientset,
		cordonTimes: make(map[string]time.Time),
	}
}

func (w *NodeWatcher) Start(ctx context.Context, stopCh <-chan struct{}) {
	watcher, err := w.clientset.CoreV1().Nodes().Watch(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("⚠️  Failed to start node watcher: %v", err)
		return
	}
	defer watcher.Stop()

	for {
		select {
		case <-stopCh:
			return
		case event := <-watcher.ResultChan():
			if event.Type == watch.Modified {
				node, ok := event.Object.(*corev1.Node)
				if !ok {
					continue
				}
				// Record first time we see node become unschedulable
				if node.Spec.Unschedulable {
					w.mu.Lock()
					if _, exists := w.cordonTimes[node.Name]; !exists {
						w.cordonTimes[node.Name] = time.Now()
					}
					w.mu.Unlock()
				}
			}
		}
	}
}

func (w *NodeWatcher) GetCordonTime(nodeName string) (time.Time, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	t, ok := w.cordonTimes[nodeName]
	return t, ok
}

// ============================================================================
// Event Triggering - Send SIGUSR1 to event generators
// ============================================================================

func sendFatalEvent(nodeName, namespace string) error {
	// Get event generator pod on this node
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace,
		"-l", EventGeneratorLabel,
		"--field-selector", fmt.Sprintf("spec.nodeName=%s", nodeName),
		"-o", "jsonpath={.items[0].metadata.name}")

	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to find event generator pod: %w", err)
	}

	podName := strings.TrimSpace(string(output))
	if podName == "" {
		return fmt.Errorf("no event generator pod on node %s", nodeName)
	}

	// Send SIGUSR1 to trigger fatal event
	cmd = exec.Command("kubectl", "exec", "-n", namespace, podName, "--",
		"sh", "-c", "kill -USR1 1")

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to send SIGUSR1: %w", err)
	}

	return nil
}

// ============================================================================
// Test Runners
// ============================================================================

func runLatencyTest(ctx context.Context, clientset *kubernetes.Clientset, nodes []string, namespace string, timeout int, concurrent bool, maxStagger int) []LatencyMeasurement {
	log.Printf("🚀 Starting latency test...")
	log.Printf("")

	// Start node watcher
	watcher := NewNodeWatcher(clientset)
	stopCh := make(chan struct{})
	go watcher.Start(ctx, stopCh)
	time.Sleep(2 * time.Second) // Let watcher initialize

	var results []LatencyMeasurement

	if concurrent {
		results = triggerConcurrentEvents(ctx, nodes, namespace, timeout, maxStagger, watcher)
	} else {
		results = triggerSequentialEvents(ctx, nodes, namespace, timeout, watcher)
	}

	close(stopCh)
	return results
}

func runQueueDepthTest(ctx context.Context, clientset *kubernetes.Clientset, nodes []string, namespace string, timeout int, maxStagger int, pollInterval int) []QueueSnapshot {
	log.Printf("🚀 Starting queue depth test...")
	log.Printf("")

	startTime := time.Now()

	// Trigger all events with stagger (fire and forget)
	log.Printf("🔀 Triggering %d fatal events with 0-%ds stagger...", len(nodes), maxStagger)
	triggerEventsFireAndForget(ctx, nodes, namespace, maxStagger)
	log.Printf("✅ All signals sent")
	log.Printf("")

	// Poll queue depth
	log.Printf("📊 Polling queue depth every %ds...", pollInterval)
	return pollQueueDepth(ctx, clientset, nodes, startTime, timeout, pollInterval)
}

func runCombinedTest(ctx context.Context, clientset *kubernetes.Clientset, nodes []string, namespace string, timeout int, maxStagger int, pollInterval int) ([]LatencyMeasurement, []QueueSnapshot) {
	log.Printf("🚀 Starting combined test (latency + queue depth)...")
	log.Printf("")

	// Start node watcher for latency
	watcher := NewNodeWatcher(clientset)
	stopCh := make(chan struct{})
	go watcher.Start(ctx, stopCh)
	time.Sleep(2 * time.Second)

	startTime := time.Now()

	// Start queue polling in background
	queueChan := make(chan []QueueSnapshot, 1)
	go func() {
		queueChan <- pollQueueDepth(ctx, clientset, nodes, startTime, timeout, pollInterval)
	}()

	// Trigger events and measure latency
	latencyResults := triggerConcurrentEvents(ctx, nodes, namespace, timeout, maxStagger, watcher)

	close(stopCh)

	// Collect queue results
	queueResults := <-queueChan

	return latencyResults, queueResults
}

// ============================================================================
// Event Trigger Implementations
// ============================================================================

func triggerSequentialEvents(ctx context.Context, nodes []string, namespace string, timeout int, watcher *NodeWatcher) []LatencyMeasurement {
	results := make([]LatencyMeasurement, 0, len(nodes))

	for i, nodeName := range nodes {
		log.Printf("[%d/%d] Testing %s...", i+1, len(nodes), nodeName)

		// Record T0 and send event
		eventTime := time.Now()
		if err := sendFatalEvent(nodeName, namespace); err != nil {
			log.Printf("   ❌ Error: %v", err)
			results = append(results, LatencyMeasurement{
				NodeName:  nodeName,
				EventTime: eventTime,
				Status:    fmt.Sprintf("error: %v", err),
			})
			continue
		}

		// Wait for cordon
		measurement := waitForCordon(nodeName, eventTime, timeout, watcher)
		results = append(results, measurement)

		if measurement.Status == "success" {
			log.Printf("   ✅ Cordoned in %dms (%.2fs)", measurement.LatencyMs, float64(measurement.LatencyMs)/1000)
		} else {
			log.Printf("   ❌ %s", measurement.Status)
		}

		// Brief pause between nodes
		time.Sleep(2 * time.Second)
	}

	return results
}

func triggerConcurrentEvents(ctx context.Context, nodes []string, namespace string, timeout int, maxStagger int, watcher *NodeWatcher) []LatencyMeasurement {
	rand.Seed(time.Now().UnixNano())

	resultChan := make(chan LatencyMeasurement, len(nodes))
	var wg sync.WaitGroup

	log.Printf("🔀 Triggering %d concurrent events with 0-%ds stagger...", len(nodes), maxStagger)

	for i, nodeName := range nodes {
		wg.Add(1)
		stagger := time.Duration(rand.Intn(maxStagger+1)) * time.Second

		go func(idx int, name string, delay time.Duration) {
			defer wg.Done()

			time.Sleep(delay)

			eventTime := time.Now()
			if err := sendFatalEvent(name, namespace); err != nil {
				log.Printf("[%d] ❌ %s: %v", idx+1, name, err)
				resultChan <- LatencyMeasurement{
					NodeName:  name,
					EventTime: eventTime,
					Status:    fmt.Sprintf("error: %v", err),
				}
				return
			}

			measurement := waitForCordon(name, eventTime, timeout, watcher)
			resultChan <- measurement

			if measurement.Status == "success" {
				log.Printf("[%d] ✅ %s: %dms", idx+1, name, measurement.LatencyMs)
			} else {
				log.Printf("[%d] ❌ %s: %s", idx+1, name, measurement.Status)
			}
		}(i, nodeName, stagger)
	}

	wg.Wait()
	close(resultChan)

	results := make([]LatencyMeasurement, 0, len(nodes))
	for m := range resultChan {
		results = append(results, m)
	}

	return results
}

func triggerEventsFireAndForget(ctx context.Context, nodes []string, namespace string, maxStagger int) {
	rand.Seed(time.Now().UnixNano())
	var wg sync.WaitGroup

	for _, nodeName := range nodes {
		wg.Add(1)
		stagger := time.Duration(rand.Intn(maxStagger+1)) * time.Second

		go func(name string, delay time.Duration) {
			defer wg.Done()
			time.Sleep(delay)
			if err := sendFatalEvent(name, namespace); err != nil {
				log.Printf("⚠️  Failed to trigger %s: %v", name, err)
			}
		}(nodeName, stagger)
	}

	wg.Wait()
}

func waitForCordon(nodeName string, eventTime time.Time, timeoutSec int, watcher *NodeWatcher) LatencyMeasurement {
	deadline := time.After(time.Duration(timeoutSec) * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return LatencyMeasurement{
				NodeName:  nodeName,
				EventTime: eventTime,
				Status:    "timeout",
			}
		case <-ticker.C:
			if cordonTime, ok := watcher.GetCordonTime(nodeName); ok {
				latency := cordonTime.Sub(eventTime)
				return LatencyMeasurement{
					NodeName:   nodeName,
					EventTime:  eventTime,
					CordonTime: cordonTime,
					LatencyMs:  latency.Milliseconds(),
					Status:     "success",
				}
			}
		}
	}
}

// ============================================================================
// Queue Depth Polling
// ============================================================================

func pollQueueDepth(ctx context.Context, clientset *kubernetes.Clientset, nodes []string, startTime time.Time, timeoutSec int, intervalSec int) []QueueSnapshot {
	var snapshots []QueueSnapshot
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()

	deadline := time.After(time.Duration(timeoutSec) * time.Second)
	lastCordoned := 0

	for {
		select {
		case <-deadline:
			log.Printf("⏰ Timeout reached")
			return snapshots

		case <-ticker.C:
			cordoned := countCordonedNodes(ctx, clientset, nodes)
			queueDepth := len(nodes) - cordoned
			elapsed := time.Since(startTime).Seconds()

			// Calculate processing rate
			rate := float64(cordoned-lastCordoned) / float64(intervalSec)
			lastCordoned = cordoned

			snapshot := QueueSnapshot{
				Timestamp:      time.Now(),
				ElapsedSeconds: elapsed,
				CordonedCount:  cordoned,
				QueueDepth:     queueDepth,
				ProcessingRate: rate,
			}
			snapshots = append(snapshots, snapshot)

			log.Printf("[T+%.0fs] Cordoned: %d/%d | Queue: %d | Rate: %.1f/sec",
				elapsed, cordoned, len(nodes), queueDepth, rate)

			if cordoned >= len(nodes) {
				log.Printf("✅ All nodes cordoned")
				return snapshots
			}
		}
	}
}

func countCordonedNodes(ctx context.Context, clientset *kubernetes.Clientset, nodes []string) int {
	count := 0
	for _, name := range nodes {
		node, err := clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		if node.Spec.Unschedulable {
			count++
		}
	}
	return count
}

// ============================================================================
// Statistics & Display
// ============================================================================

func displayLatencyStats(measurements []LatencyMeasurement) {
	var successLatencies []int64
	var failures int

	for _, m := range measurements {
		if m.Status == "success" {
			successLatencies = append(successLatencies, m.LatencyMs)
		} else {
			failures++
		}
	}

	log.Printf("")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("📊 LATENCY RESULTS")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("   Total:    %d nodes", len(measurements))
	log.Printf("   Success:  %d", len(successLatencies))
	log.Printf("   Failed:   %d", failures)

	if len(successLatencies) == 0 {
		return
	}

	sort.Slice(successLatencies, func(i, j int) bool { return successLatencies[i] < successLatencies[j] })

	var sum int64
	for _, l := range successLatencies {
		sum += l
	}

	p := func(pct float64) int64 {
		idx := int(float64(len(successLatencies)) * pct)
		if idx >= len(successLatencies) {
			idx = len(successLatencies) - 1
		}
		return successLatencies[idx]
	}

	log.Printf("")
	log.Printf("⏱️  Latency (SIGUSR1 → Node Cordoned):")
	log.Printf("   Min:  %6dms  (%.2fs)", successLatencies[0], float64(successLatencies[0])/1000)
	log.Printf("   P50:  %6dms  (%.2fs)", p(0.50), float64(p(0.50))/1000)
	log.Printf("   P90:  %6dms  (%.2fs)", p(0.90), float64(p(0.90))/1000)
	log.Printf("   P95:  %6dms  (%.2fs)", p(0.95), float64(p(0.95))/1000)
	log.Printf("   P99:  %6dms  (%.2fs)", p(0.99), float64(p(0.99))/1000)
	log.Printf("   Max:  %6dms  (%.2fs)", successLatencies[len(successLatencies)-1], float64(successLatencies[len(successLatencies)-1])/1000)
	log.Printf("   Mean: %6.0fms  (%.2fs)", float64(sum)/float64(len(successLatencies)), float64(sum)/float64(len(successLatencies))/1000)
}

func displayQueueStats(snapshots []QueueSnapshot, totalNodes int) {
	if len(snapshots) == 0 {
		return
	}

	var peakQueue, totalQueue int
	var peakRate, totalRate float64
	var rateCount int

	for _, s := range snapshots {
		if s.QueueDepth > peakQueue {
			peakQueue = s.QueueDepth
		}
		totalQueue += s.QueueDepth

		if s.ProcessingRate > peakRate {
			peakRate = s.ProcessingRate
		}
		if s.ProcessingRate > 0 {
			totalRate += s.ProcessingRate
			rateCount++
		}
	}

	avgQueue := float64(totalQueue) / float64(len(snapshots))
	var avgRate float64
	if rateCount > 0 {
		avgRate = totalRate / float64(rateCount)
	}

	log.Printf("")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("📊 QUEUE DEPTH RESULTS")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("   Total nodes:  %d", totalNodes)
	log.Printf("   Duration:     %.1fs", snapshots[len(snapshots)-1].ElapsedSeconds)
	log.Printf("")
	log.Printf("📈 Queue Depth:")
	log.Printf("   Peak:    %d nodes", peakQueue)
	log.Printf("   Average: %.1f nodes", avgQueue)
	log.Printf("")
	log.Printf("⚡ Processing Rate:")
	log.Printf("   Peak:    %.2f nodes/sec", peakRate)
	log.Printf("   Average: %.2f nodes/sec", avgRate)
}

// ============================================================================
// Save Results
// ============================================================================

func saveLatencyResults(measurements []LatencyMeasurement, outputDir, timestamp, mode string, concurrent bool) {
	// CSV file
	csvFile := fmt.Sprintf("%s/latency-%s.csv", outputDir, timestamp)
	f, err := os.Create(csvFile)
	if err != nil {
		log.Printf("⚠️  Failed to create CSV: %v", err)
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "node,event_time,cordon_time,latency_ms,status\n")
	for _, m := range measurements {
		fmt.Fprintf(f, "%s,%s,%s,%d,%s\n",
			m.NodeName,
			m.EventTime.Format(time.RFC3339Nano),
			m.CordonTime.Format(time.RFC3339Nano),
			m.LatencyMs,
			m.Status)
	}

	log.Printf("📁 Saved: %s", csvFile)
}

func saveQueueResults(snapshots []QueueSnapshot, totalNodes int, outputDir, timestamp string) {
	csvFile := fmt.Sprintf("%s/queue-depth-%s.csv", outputDir, timestamp)
	f, err := os.Create(csvFile)
	if err != nil {
		log.Printf("⚠️  Failed to create CSV: %v", err)
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "timestamp,elapsed_sec,cordoned,queue_depth,rate\n")
	for _, s := range snapshots {
		fmt.Fprintf(f, "%s,%.1f,%d,%d,%.2f\n",
			s.Timestamp.Format(time.RFC3339),
			s.ElapsedSeconds,
			s.CordonedCount,
			s.QueueDepth,
			s.ProcessingRate)
	}

	log.Printf("📁 Saved: %s", csvFile)
}
