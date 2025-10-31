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

package nodemetadata

import (
	"context"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	testClient *kubernetes.Clientset
	testEnv    *envtest.Environment
)

// TestMain sets up envtest environment for all tests
// To run tests, use: make test
// Or manually: go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
//              source <(setup-envtest use -p env 1.30.0)
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

// createTestNode creates a node in the test API server and returns it
func createTestNode(t *testing.T, node *corev1.Node) *corev1.Node {
	t.Helper()
	ctx := context.Background()
	created, err := testClient.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test node: %v", err)
	}
	return created
}

// cleanupTestNode deletes a node from the test API server
func cleanupTestNode(t *testing.T, nodeName string) {
	t.Helper()
	ctx := context.Background()
	err := testClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	if err != nil {
		t.Logf("Failed to cleanup test node %s: %v", nodeName, err)
	}
}

func TestProcessorAugmentHealthEvent(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			Labels: map[string]string{
				"topology.kubernetes.io/zone":   "us-west-2a",
				"topology.kubernetes.io/region": "us-west-2",
				"node.kubernetes.io/instance-type": "p4d.24xlarge",
			},
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-west-2a/i-1234567890abcdef0",
		},
	}

	createTestNode(t, node)
	defer cleanupTestNode(t, node.Name)

	config := &Config{
		Enabled:          true,
		CacheSize:        100,
		CacheTTL:         1 * time.Hour,
		AllowedLabels: []string{
			"topology.kubernetes.io/zone",
			"topology.kubernetes.io/region",
		},
	}

	p := &processor{
		config:    config,
		clientset: testClient,
		stopCh:    make(chan struct{}),
	}
	p.cache = expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	ctx := context.Background()
	event := &pb.HealthEvent{
		NodeName: "test-node",
		Metadata: make(map[string]string),
	}

	err := p.AugmentHealthEvent(ctx, event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if event.Metadata["node.providerID"] != "aws:///us-west-2a/i-1234567890abcdef0" {
		t.Errorf("expected raw provider ID to be set")
	}

	if event.Metadata["topology.kubernetes.io/zone"] != "us-west-2a" {
		t.Errorf("expected zone label to be set")
	}

	if event.Metadata["topology.kubernetes.io/region"] != "us-west-2" {
		t.Errorf("expected region label to be set")
	}

	if _, exists := event.Metadata["node.kubernetes.io/instance-type"]; exists {
		t.Error("expected instance-type label to not be set (not in allowed list)")
	}
}

func TestProcessorAugmentHealthEventEmptyNodeName(t *testing.T) {
	config := &Config{
		Enabled:          true,
		CacheSize:        100,
		CacheTTL:         1 * time.Hour,
		AllowedLabels:    []string{},
	}

	p := &processor{
		config:    config,
		clientset: testClient,
		stopCh:    make(chan struct{}),
	}
	p.cache = expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	ctx := context.Background()
	event := &pb.HealthEvent{
		NodeName: "",
		Metadata: make(map[string]string),
	}

	err := p.AugmentHealthEvent(ctx, event)
	if err == nil {
		t.Error("expected error for empty node name")
	}
}

func TestProcessorAugmentHealthEventNodeNotFound(t *testing.T) {
	config := &Config{
		Enabled:          true,
		CacheSize:        100,
		CacheTTL:         1 * time.Hour,
		AllowedLabels:    []string{},
	}

	p := &processor{
		config:    config,
		clientset: testClient,
		stopCh:    make(chan struct{}),
	}
	p.cache = expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	ctx := context.Background()
	event := &pb.HealthEvent{
		NodeName: "non-existent-node",
		Metadata: make(map[string]string),
	}

	err := p.AugmentHealthEvent(ctx, event)
	if err == nil {
		t.Error("expected error for non-existent node")
	}
}

func TestProcessorStartStop(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-west-2a/i-1234567890abcdef0",
		},
	}

	createTestNode(t, node)
	defer cleanupTestNode(t, node.Name)

	config := &Config{
		Enabled:          true,
		CacheSize:        100,
		CacheTTL:         100 * time.Millisecond,
		AllowedLabels:    []string{},
	}

	p := &processor{
		config:    config,
		clientset: testClient,
		stopCh:    make(chan struct{}),
	}
	p.cache = expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.Start(ctx)

	time.Sleep(50 * time.Millisecond)

	p.Stop()

	select {
	case <-p.stopCh:
	default:
		t.Error("expected stopCh to be closed")
	}
}

func TestProcessorCachingBehavior(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-west-2a/i-1234567890abcdef0",
		},
	}

	createTestNode(t, node)
	defer cleanupTestNode(t, node.Name)

	config := &Config{
		Enabled:          true,
		CacheSize:        100,
		CacheTTL:         1 * time.Hour,
		AllowedLabels:    []string{},
	}

	p := &processor{
		config:    config,
		clientset: testClient,
		stopCh:    make(chan struct{}),
	}
	p.cache = expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	ctx := context.Background()

	event1 := &pb.HealthEvent{
		NodeName: "test-node",
		Metadata: make(map[string]string),
	}

	err := p.AugmentHealthEvent(ctx, event1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	event2 := &pb.HealthEvent{
		NodeName: "test-node",
		Metadata: make(map[string]string),
	}

	err = p.AugmentHealthEvent(ctx, event2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if event1.Metadata["node.providerID"] != event2.Metadata["node.providerID"] {
		t.Error("expected both events to have same metadata (from cache)")
	}
}

func TestProcessorAugmentHealthEventNilMetadata(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-west-2a/i-1234567890abcdef0",
		},
	}

	createTestNode(t, node)
	defer cleanupTestNode(t, node.Name)

	config := &Config{
		Enabled:          true,
		CacheSize:        100,
		CacheTTL:         1 * time.Hour,
		AllowedLabels:    []string{},
	}

	p := &processor{
		config:    config,
		clientset: testClient,
		stopCh:    make(chan struct{}),
	}
	p.cache = expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	ctx := context.Background()
	event := &pb.HealthEvent{
		NodeName: "test-node",
		Metadata: nil,
	}

	err := p.AugmentHealthEvent(ctx, event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if event.Metadata == nil {
		t.Error("expected metadata to be initialized")
	}

	if event.Metadata["node.providerID"] != "aws:///us-west-2a/i-1234567890abcdef0" {
		t.Errorf("expected provider ID to be set")
	}
}

func TestProcessorAugmentHealthEventExistingMetadata(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-west-2a/i-1234567890abcdef0",
		},
	}

	createTestNode(t, node)
	defer cleanupTestNode(t, node.Name)

	config := &Config{
		Enabled:          true,
		CacheSize:        100,
		CacheTTL:         1 * time.Hour,
		AllowedLabels:    []string{},
	}

	p := &processor{
		config:    config,
		clientset: testClient,
		stopCh:    make(chan struct{}),
	}
	p.cache = expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	ctx := context.Background()
	event := &pb.HealthEvent{
		NodeName: "test-node",
		Metadata: map[string]string{
			"existing.key": "existing.value",
		},
	}

	err := p.AugmentHealthEvent(ctx, event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if event.Metadata["existing.key"] != "existing.value" {
		t.Error("expected existing metadata to be preserved")
	}

	if event.Metadata["node.providerID"] != "aws:///us-west-2a/i-1234567890abcdef0" {
		t.Errorf("expected provider ID to be set")
	}
}

func TestProcessorAugmentHealthEventNoProviderID(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			Labels: map[string]string{
				"test-label": "test-value",
			},
		},
		Spec: corev1.NodeSpec{
			ProviderID: "",
		},
	}

	createTestNode(t, node)
	defer cleanupTestNode(t, node.Name)

	config := &Config{
		Enabled:          true,
		CacheSize:        100,
		CacheTTL:         1 * time.Hour,
		AllowedLabels:    []string{"test-label"},
	}

	p := &processor{
		config:    config,
		clientset: testClient,
		stopCh:    make(chan struct{}),
	}
	p.cache = expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	ctx := context.Background()
	event := &pb.HealthEvent{
		NodeName: "test-node",
		Metadata: make(map[string]string),
	}

	err := p.AugmentHealthEvent(ctx, event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, exists := event.Metadata["node.providerID"]; exists {
		t.Error("expected no provider ID metadata when provider ID is empty")
	}

	if event.Metadata["test-label"] != "test-value" {
		t.Error("expected label to be set even without provider ID")
	}
}

func TestProcessorAugmentHealthEventEmptyLabels(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-node",
			Labels: map[string]string{},
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-west-2a/i-1234567890abcdef0",
		},
	}

	createTestNode(t, node)
	defer cleanupTestNode(t, node.Name)

	config := &Config{
		Enabled:          true,
		CacheSize:        100,
		CacheTTL:         1 * time.Hour,
		AllowedLabels:    []string{"non-existent-label"},
	}

	p := &processor{
		config:    config,
		clientset: testClient,
		stopCh:    make(chan struct{}),
	}
	p.cache = expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	ctx := context.Background()
	event := &pb.HealthEvent{
		NodeName: "test-node",
		Metadata: make(map[string]string),
	}

	err := p.AugmentHealthEvent(ctx, event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, exists := event.Metadata["non-existent-label"]; exists {
		t.Error("expected no label metadata when node has no matching labels")
	}
}

func TestProcessorContextCancellation(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-west-2a/i-1234567890abcdef0",
		},
	}

	createTestNode(t, node)
	defer cleanupTestNode(t, node.Name)

	config := &Config{
		Enabled:          true,
		CacheSize:        100,
		CacheTTL:         100 * time.Millisecond,
		AllowedLabels:    []string{},
	}

	p := &processor{
		config:    config,
		clientset: testClient,
		stopCh:    make(chan struct{}),
	}
	p.cache = expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	ctx, cancel := context.WithCancel(context.Background())

	go p.Start(ctx)

	time.Sleep(50 * time.Millisecond)

	cancel()

	time.Sleep(100 * time.Millisecond)
}

func TestProcessorConcurrentAugmentations(t *testing.T) {
	nodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			Spec:       corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-111"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node2"},
			Spec:       corev1.NodeSpec{ProviderID: "aws:///us-west-2b/i-222"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node3"},
			Spec:       corev1.NodeSpec{ProviderID: "aws:///us-west-2c/i-333"},
		},
	}

	// Create all test nodes
	for _, node := range nodes {
		createTestNode(t, node)
		defer cleanupTestNode(t, node.Name)
	}

	config := &Config{
		Enabled:          true,
		CacheSize:        100,
		CacheTTL:         1 * time.Hour,
		AllowedLabels:    []string{},
	}

	p := &processor{
		config:    config,
		clientset: testClient,
		stopCh:    make(chan struct{}),
	}
	p.cache = expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)

	ctx := context.Background()
	var wg sync.WaitGroup

	for i, node := range nodes {
		wg.Add(1)
		go func(nodeName string, idx int) {
			defer wg.Done()
			event := &pb.HealthEvent{
				NodeName: nodeName,
				Metadata: make(map[string]string),
			}
			err := p.AugmentHealthEvent(ctx, event)
			if err != nil {
				t.Errorf("unexpected error for %s: %v", nodeName, err)
			}
		}(node.Name, i)
	}

	wg.Wait()
}

func TestNewProcessorValidation(t *testing.T) {
	tests := []struct {
		name      string
		config    *Config
		expectErr bool
	}{
		{
			name: "invalid cache size",
			config: &Config{
				Enabled:          true,
				CacheSize:        0,
				CacheTTL:         1 * time.Hour,
				AllowedLabels:    []string{},
			},
			expectErr: true,
		},
		{
			name: "invalid TTL",
			config: &Config{
				Enabled:          true,
				CacheSize:        100,
				CacheTTL:         0,
				AllowedLabels:    []string{},
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			_, err := NewProcessor(ctx, tt.config, testClient)

			if tt.expectErr && err == nil {
				t.Error("expected error but got none")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

