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

// FQM Scale Test - Lightweight End-to-End Latency Measurement
//
// Measures: SIGUSR1 → Event Generator → Platform Connector → MongoDB → FQM → Node Cordoned
//
// Uses worker pool (default 20 workers) to send signals efficiently
// Polls cordoned node count to measure throughput
//
// Usage:
//   go build -o fqm-scale-test .
//   ./fqm-scale-test -nodes=750 -workers=20 -stagger=30 -timeout=600

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

const (
	EventGeneratorLabel = "app=event-generator"
)

// QueueSnapshot represents the FQM queue state at a specific point in time,
// including cordoned node count, queue depth, and processing rate.
type QueueSnapshot struct {
	Timestamp      time.Time
	ElapsedSeconds float64
	CordonedCount  int
	QueueDepth     int
	ProcessingRate float64
}

func main() {
	// Suppress client-go's verbose logging
	klog.SetOutput(io.Discard)

	// Parse flags
	numNodes := flag.Int("nodes", 150, "Number of nodes to test")
	namespace := flag.String("namespace", "nvsentinel", "NVSentinel namespace")
	kubeconfig := flag.String("kubeconfig", "", "Path to kubeconfig (default: ~/.kube/config)")
	k8sContext := flag.String("context", "rs3", "Kubernetes context")
	timeout := flag.Int("timeout", 600, "Timeout in seconds")
	outputDir := flag.String("output", "./results", "Output directory")
	maxStagger := flag.Int("stagger", 0, "Max seconds to stagger events (0 = BLAST mode)")
	pollInterval := flag.Int("poll", 5, "Poll interval in seconds")
	workers := flag.Int("workers", 50, "Number of concurrent workers")
	flag.Parse()

	// Print configuration
	log.Printf("🔬 NVSentinel FQM Scale Test (Lightweight)")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("Nodes:      %d", *numNodes)
	log.Printf("Workers:    %d", *workers)
	log.Printf("Stagger:    0-%ds", *maxStagger)
	log.Printf("Timeout:    %ds", *timeout)
	log.Printf("Namespace:  %s", *namespace)
	log.Printf("Context:    %s", *k8sContext)
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("")

	// Create Kubernetes client
	clientset, config, err := createK8sClient(*kubeconfig, *k8sContext)
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

	// Run test
	startTime := time.Now()
	snapshots := runTest(ctx, clientset, config, nodes, *namespace, *timeout, *maxStagger, *pollInterval, *workers)

	// Calculate and display results
	displayResults(snapshots, len(nodes), startTime)

	// Save results
	timestamp := time.Now().Format("20060102-150405")
	saveResults(snapshots, len(nodes), *outputDir, timestamp)

	log.Printf("")
	log.Printf("✅ Test complete!")
}

func createK8sClient(kubeconfig, k8sContext string) (*kubernetes.Clientset, *rest.Config, error) {
	if kubeconfig == "" {
		kubeconfig = clientcmd.RecommendedHomeFile
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: k8sContext},
	).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	// Increase rate limits for high-concurrency testing (default is 5 QPS, 10 burst)
	config.QPS = 100
	config.Burst = 200

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	return clientset, config, nil
}

func getSchedulableNodes(ctx context.Context, clientset *kubernetes.Clientset, limit int) ([]string, error) {
	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	var schedulableNodes []string
	var cordonedCount int

	for _, node := range nodeList.Items {
		// Skip system-workload nodes
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

	if len(schedulableNodes) > limit {
		schedulableNodes = schedulableNodes[:limit]
	}

	return schedulableNodes, nil
}

func sendFatalEvent(ctx context.Context, clientset *kubernetes.Clientset, config *rest.Config, nodeName, namespace string) error {
	// Get event generator pod on this node using the Kubernetes client
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: EventGeneratorLabel,
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", nodeName),
	})
	if err != nil {
		return fmt.Errorf("failed to list pods on node %s: %w", nodeName, err)
	}

	if len(pods.Items) == 0 {
		return fmt.Errorf("no event-generator pod on node %s", nodeName)
	}

	podName := pods.Items[0].Name

	// Send SIGUSR1 using remotecommand exec
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: []string{"sh", "-c", "kill -USR1 1"},
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor for pod %s: %w", podName, err)
	}

	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("failed to send signal to pod %s: %w (stderr: %s)", podName, err, stderr.String())
	}

	return nil
}

func runTest(ctx context.Context, clientset *kubernetes.Clientset, config *rest.Config, nodes []string, namespace string, timeout int, maxStagger int, pollInterval int, numWorkers int) []QueueSnapshot {
	log.Printf("🚀 Starting test with %d workers...", numWorkers)

	startTime := time.Now()

	// Baseline the number of nodes already cordoned by NVSentinel so progress
	// and completion are measured only for this test run.
	baselineCordoned := countCordonedNodes(ctx, clientset)
	log.Printf("📊 Baseline NVSentinel-cordoned nodes: %d", baselineCordoned)

	// PHASE 1: Send ALL signals first (before any cordoning can evict pods)
	log.Printf("🔀 Phase 1: Sending signals to %d nodes with %d workers (stagger 0-%ds)...", len(nodes), numWorkers, maxStagger)

	workCh := make(chan string, len(nodes))
	var sentCount, errorCount int64
	var mu sync.Mutex
	var wg sync.WaitGroup

	// Progress ticker
	progressDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-ticker.C:
				mu.Lock()
				s, e := sentCount, errorCount
				mu.Unlock()
				log.Printf("   Progress: %d/%d sent, %d errors", s, len(nodes), e)
			}
		}
	}()

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for nodeName := range workCh {
				// Random stagger within window
				if maxStagger > 0 {
					stagger := time.Duration(rand.Intn(maxStagger+1)) * time.Second
					time.Sleep(stagger)
				}

				if err := sendFatalEvent(ctx, clientset, config, nodeName, namespace); err != nil {
					mu.Lock()
					errorCount++
					mu.Unlock()
				} else {
					mu.Lock()
					sentCount++
					mu.Unlock()
				}
			}
		}()
	}

	// Queue all nodes
	for _, node := range nodes {
		workCh <- node
	}
	close(workCh)

	// Wait for ALL signals to be sent
	wg.Wait()
	close(progressDone)
	signalsDone := time.Now()
	log.Printf("✅ Phase 1 complete: %d sent, %d errors in %.1fs", sentCount, errorCount, signalsDone.Sub(startTime).Seconds())

	// PHASE 2: Poll until all cordoned
	// Guard against invalid poll intervals (e.g. -poll=0)
	if pollInterval <= 0 {
		pollInterval = 1
	}
	log.Printf("📊 Phase 2: Polling cordoned count every %ds...", pollInterval)

	var snapshots []QueueSnapshot
	pollDone := make(chan struct{})

	go func() {
		ticker := time.NewTicker(time.Duration(pollInterval) * time.Second)
		defer ticker.Stop()
		deadline := time.After(time.Duration(timeout) * time.Second)
		lastCordoned := 0

		for {
			select {
			case <-deadline:
				log.Printf("⏰ Timeout reached")
				close(pollDone)
				return
			case <-ticker.C:
				rawCordoned := countCordonedNodes(ctx, clientset)
				// Measure only new cordons from this run
				cordoned := rawCordoned - baselineCordoned
				if cordoned < 0 {
					cordoned = 0
				}

				elapsed := time.Since(startTime).Seconds()
				rate := float64(cordoned-lastCordoned) / float64(pollInterval)
				lastCordoned = cordoned

				queueDepth := int(sentCount) - cordoned
				if queueDepth < 0 {
					queueDepth = 0
				}

				snapshot := QueueSnapshot{
					Timestamp:      time.Now(),
					ElapsedSeconds: elapsed,
					CordonedCount:  cordoned,
					QueueDepth:     queueDepth,
					ProcessingRate: rate,
				}
				snapshots = append(snapshots, snapshot)

				log.Printf("[T+%.0fs] Cordoned (this run): %d/%d | Queue: %d | Rate: %.1f/sec",
					elapsed, cordoned, sentCount, queueDepth, rate)

				if cordoned >= int(sentCount) {
					log.Printf("✅ All %d nodes cordoned!", sentCount)
					close(pollDone)
					return
				}
			}
		}
	}()

	// Wait for polling to finish
	<-pollDone

	return snapshots
}

func countCordonedNodes(ctx context.Context, clientset *kubernetes.Clientset) int {
	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: "k8saas.nvidia.com/cordon-by=NVSentinel",
	})
	if err != nil {
		return 0
	}
	return len(nodeList.Items)
}

func displayResults(snapshots []QueueSnapshot, totalNodes int, startTime time.Time) {
	if len(snapshots) == 0 {
		log.Printf("No data collected")
		return
	}

	// Find when all nodes were cordoned
	var completionTime float64
	for _, s := range snapshots {
		if s.CordonedCount >= totalNodes {
			completionTime = s.ElapsedSeconds
			break
		}
	}
	if completionTime == 0 {
		completionTime = snapshots[len(snapshots)-1].ElapsedSeconds
	}

	// Calculate stats
	var peakQueue int
	var totalRate float64
	var rateCount int

	for _, s := range snapshots {
		if s.QueueDepth > peakQueue {
			peakQueue = s.QueueDepth
		}
		if s.ProcessingRate > 0 {
			totalRate += s.ProcessingRate
			rateCount++
		}
	}

	var avgRate float64
	if rateCount > 0 {
		avgRate = totalRate / float64(rateCount)
	}
	finalCordoned := snapshots[len(snapshots)-1].CordonedCount

	log.Printf("")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("📊 RESULTS")
	log.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	log.Printf("   Total nodes:     %d", totalNodes)
	log.Printf("   Cordoned:        %d (%.1f%%)", finalCordoned, float64(finalCordoned)/float64(totalNodes)*100)
	log.Printf("   Time to complete: %.1fs", completionTime)
	log.Printf("   Peak queue:      %d nodes", peakQueue)
	log.Printf("   Avg rate:        %.2f nodes/sec", avgRate)
}

func saveResults(snapshots []QueueSnapshot, totalNodes int, outputDir, timestamp string) {
	csvFile := fmt.Sprintf("%s/lightweight-%s.csv", outputDir, timestamp)
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
