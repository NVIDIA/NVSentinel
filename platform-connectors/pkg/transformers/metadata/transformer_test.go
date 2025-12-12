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

package metadata

import (
	"context"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
func TestMain(m *testing.M) {
	testEnv = &envtest.Environment{}

	cfg, err := testEnv.Start()
	if err != nil {
		log.Fatalf("Failed to start test environment: %v", err)
	}

	testClient, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("Failed to create test client: %v", err)
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		log.Printf("Failed to stop test environment: %v", err)
	}

	os.Exit(code)
}

func createTestNode(t *testing.T, node *corev1.Node) {
	t.Helper()
	_, err := testClient.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create test node")
}

func deleteTestNode(t *testing.T, nodeName string) {
	t.Helper()
	err := testClient.CoreV1().Nodes().Delete(context.Background(), nodeName, metav1.DeleteOptions{})
	assert.NoError(t, err, "failed to delete test node")
}

func createTestAugmentor(config *Config) *Augmentor {
	if config == nil {
		config = &Config{
			CacheSize:     100,
			CacheTTL:      1 * time.Hour,
			AllowedLabels: []string{},
		}
	}
	cache := expirable.NewLRU[string, *NodeMetadata](
		config.CacheSize,
		nil,
		config.CacheTTL,
	)
	return &Augmentor{
		config:    config,
		clientset: testClient,
		cache:     cache,
	}
}

// TestAugmentorTransform tests various augmentation scenarios
func TestAugmentorTransform(t *testing.T) {
	tests := []struct {
		name           string
		node           *corev1.Node
		nodes          []*corev1.Node
		config         *Config
		eventNodeName  string
		existingMeta   map[string]string
		expectError    bool
		validateResult func(t *testing.T, event *pb.HealthEvent)
	}{
		{
			name: "successful augmentation with labels",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node-1",
					Labels: map[string]string{
						"topology.kubernetes.io/zone":      "us-west-2a",
						"topology.kubernetes.io/region":    "us-west-2",
						"node.kubernetes.io/instance-type": "p4d.24xlarge",
					},
				},
				Spec: corev1.NodeSpec{
					ProviderID: "aws:///us-west-2a/i-1234567890abcdef0",
				},
			},
			config: &Config{
				CacheSize: 100,
				CacheTTL:  1 * time.Hour,
				AllowedLabels: []string{
					"topology.kubernetes.io/zone",
					"topology.kubernetes.io/region",
				},
			},
			eventNodeName: "test-node-1",
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.Equal(t, "aws:///us-west-2a/i-1234567890abcdef0", event.Metadata["providerID"])
				assert.Equal(t, "us-west-2a", event.Metadata["topology.kubernetes.io/zone"])
				assert.Equal(t, "us-west-2", event.Metadata["topology.kubernetes.io/region"])
				assert.NotContains(t, event.Metadata, "node.kubernetes.io/instance-type", "should not include non-allowed labels")
			},
		},
		{
			name:          "empty node name",
			node:          nil,
			eventNodeName: "",
			expectError:   true,
		},
		{
			name:          "node not found",
			node:          nil,
			eventNodeName: "non-existent-node",
			expectError:   true,
		},
		{
			name: "nil metadata initialization",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node-2"},
				Spec:       corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-abc123"},
			},
			eventNodeName: "test-node-2",
			existingMeta:  nil,
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.NotNil(t, event.Metadata)
				assert.Equal(t, "aws:///us-west-2a/i-abc123", event.Metadata["providerID"])
			},
		},
		{
			name: "existing metadata preservation",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node-3"},
				Spec:       corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-def456"},
			},
			eventNodeName: "test-node-3",
			existingMeta:  map[string]string{"existing-key": "existing-value"},
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.Equal(t, "existing-value", event.Metadata["existing-key"])
				assert.Equal(t, "aws:///us-west-2a/i-def456", event.Metadata["providerID"])
			},
		},
		{
			name: "no provider ID",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "test-node-4",
					Labels: map[string]string{"test-label": "test-value"},
				},
				Spec: corev1.NodeSpec{},
			},
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				AllowedLabels: []string{"test-label"},
			},
			eventNodeName: "test-node-4",
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.NotContains(t, event.Metadata, "providerID")
				assert.Equal(t, "test-value", event.Metadata["test-label"])
			},
		},
		{
			name: "no allowed labels configured",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "test-node-5"},
				Spec:       corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-ghi789"},
			},
			eventNodeName: "test-node-5",
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.Equal(t, "aws:///us-west-2a/i-ghi789", event.Metadata["providerID"])
				assert.Len(t, event.Metadata, 1)
			},
		},
		{
			name: "cloud-specific topology labels",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node-6",
					Labels: map[string]string{
						"topology.k8s.aws/capacity-block-id":    "cbr-01234567",
						"topology.k8s.aws/network-node-layer-1": "nn-abcd",
						"oci.oraclecloud.com/host.id":           "971b2",
						"cloud.google.com/gce-topology-block":   "9b6c",
						"cloud.google.com/gce-topology-host":    "7007",
						"node.kubernetes.io/instance-type":      "p4d.24xlarge",
					},
				},
				Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-cloud123"},
			},
			config: &Config{
				CacheSize: 100,
				CacheTTL:  1 * time.Hour,
				AllowedLabels: []string{
					"topology.k8s.aws/capacity-block-id",
					"topology.k8s.aws/network-node-layer-1",
					"oci.oraclecloud.com/host.id",
					"cloud.google.com/gce-topology-block",
					"cloud.google.com/gce-topology-host",
				},
			},
			eventNodeName: "test-node-6",
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.Equal(t, "aws:///us-west-2a/i-cloud123", event.Metadata["providerID"])
				assert.Equal(t, "cbr-01234567", event.Metadata["topology.k8s.aws/capacity-block-id"])
				assert.Equal(t, "nn-abcd", event.Metadata["topology.k8s.aws/network-node-layer-1"])
				assert.Equal(t, "971b2", event.Metadata["oci.oraclecloud.com/host.id"])
				assert.Equal(t, "9b6c", event.Metadata["cloud.google.com/gce-topology-block"])
				assert.Equal(t, "7007", event.Metadata["cloud.google.com/gce-topology-host"])
				assert.NotContains(t, event.Metadata, "node.kubernetes.io/instance-type", "should not include non-allowed labels")
			},
		},
		{
			name: "multiple nodes enrichment",
			nodes: []*corev1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "multi-test-node-1",
						Labels: map[string]string{
							"topology.kubernetes.io/zone": "us-west-2a",
						},
					},
					Spec: corev1.NodeSpec{
						ProviderID: "aws:///us-west-2a/i-test1",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "multi-test-node-2",
						Labels: map[string]string{
							"topology.kubernetes.io/zone": "us-west-2b",
						},
					},
					Spec: corev1.NodeSpec{
						ProviderID: "aws:///us-west-2b/i-test2",
					},
				},
			},
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				AllowedLabels: []string{"topology.kubernetes.io/zone"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Handle multiple nodes test case
			if tt.nodes != nil {
				for _, node := range tt.nodes {
					createTestNode(t, node)
					defer deleteTestNode(t, node.Name)
				}

				p := createTestAugmentor(tt.config)
				ctx := context.Background()

				for _, node := range tt.nodes {
					event := &pb.HealthEvent{
						NodeName: node.Name,
						Metadata: make(map[string]string),
					}
					err := p.Transform(ctx, event)
					require.NoError(t, err)
					assert.Equal(t, node.Spec.ProviderID, event.Metadata["providerID"])
					assert.Equal(t, node.Labels["topology.kubernetes.io/zone"], event.Metadata["topology.kubernetes.io/zone"])
				}
				return
			}

			if tt.node != nil {
				createTestNode(t, tt.node)
				defer deleteTestNode(t, tt.node.Name)
			}

			p := createTestAugmentor(tt.config)

			ctx := context.Background()
			event := &pb.HealthEvent{
				NodeName: tt.eventNodeName,
				Metadata: tt.existingMeta,
			}
			if event.Metadata == nil && !tt.expectError && tt.name != "nil metadata initialization" {
				event.Metadata = make(map[string]string)
			}

			err := p.Transform(ctx, event)

			if tt.expectError {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)

			if tt.validateResult != nil {
				tt.validateResult(t, event)
			}
		})
	}
}

func TestProcessorCachingBehavior(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cache-test-node"},
		Spec:       corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-cache123"},
	}

	createTestNode(t, node)

	config := &Config{
		CacheSize: 100,
		CacheTTL:  1 * time.Hour,
	}

	p := createTestAugmentor(config)
	ctx := context.Background()

	event1 := &pb.HealthEvent{
		NodeName: "cache-test-node",
		Metadata: make(map[string]string),
	}
	require.NoError(t, p.Transform(ctx, event1))
	assert.NotEmpty(t, event1.Metadata["providerID"])

	// Delete node to prove second call uses cache (not API)
	deleteTestNode(t, node.Name)

	event2 := &pb.HealthEvent{
		NodeName: "cache-test-node",
		Metadata: make(map[string]string),
	}
	require.NoError(t, p.Transform(ctx, event2))

	// If cache wasn't used, this would fail because node is deleted
	assert.Equal(t, event1.Metadata["providerID"], event2.Metadata["providerID"])
}

func TestProcessorContextCancellation(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "context-test-node"},
		Spec:       corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-ctx123"},
	}

	createTestNode(t, node)
	defer deleteTestNode(t, node.Name)

	config := &Config{
		CacheSize: 100,
		CacheTTL:  1 * time.Hour,
	}

	p := createTestAugmentor(config)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	event := &pb.HealthEvent{
		NodeName: "context-test-node",
		Metadata: make(map[string]string),
	}

	err := p.Transform(ctx, event)
	if err != nil {
		assert.Contains(t, err.Error(), "context")
	}
}

func TestProcessorConcurrentAugmentations(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "concurrent-test-node",
			Labels: map[string]string{
				"test-label": "test-value",
			},
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-west-2a/i-concurrent123",
		},
	}

	createTestNode(t, node)
	defer deleteTestNode(t, node.Name)

	config := &Config{
		CacheSize:     100,
		CacheTTL:      1 * time.Hour,
		AllowedLabels: []string{"test-label"},
	}

	p := createTestAugmentor(config)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			event := &pb.HealthEvent{
				NodeName: "concurrent-test-node",
				Metadata: make(map[string]string),
			}
			err := p.Transform(ctx, event)
			assert.NoError(t, err)
			assert.Equal(t, "aws:///us-west-2a/i-concurrent123", event.Metadata["providerID"])
			assert.Equal(t, "test-value", event.Metadata["test-label"])
		}()
	}

	wg.Wait()
}

func TestNewProcessorValidation(t *testing.T) {
	tests := []struct {
		name        string
		config      *Config
		clientset   kubernetes.Interface
		expectError bool
		errorMsg    string
	}{
		{
			name: "invalid config",
			config: &Config{
				CacheSize: 0, // invalid
				CacheTTL:  1 * time.Hour,
			},
			clientset:   testClient,
			expectError: true,
			errorMsg:    "invalid config",
		},
		{
			name: "valid processor",
			config: &Config{
				CacheSize: 100,
				CacheTTL:  1 * time.Hour,
			},
			clientset:   testClient,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor, err := New(context.Background(), tt.config, tt.clientset)

			if tt.expectError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
				assert.Nil(t, processor)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, processor)
			}
		})
	}
}
