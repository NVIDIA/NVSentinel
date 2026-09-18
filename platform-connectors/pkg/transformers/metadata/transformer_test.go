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
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
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

func createTestAugmentor(t *testing.T, config *Config) *Augmentor {
	t.Helper()

	if config == nil {
		config = &Config{
			CacheSize:     100,
			CacheTTL:      1 * time.Hour,
			AllowedLabels: []string{},
		}
	}

	augmentor, err := New(context.Background(), config, testClient)
	require.NoError(t, err, "test config must be valid")

	return augmentor
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
				Name: "test-node-1",
				Labels: map[string]string{
					"topology.kubernetes.io/zone":      "us-west-2a",
					"topology.kubernetes.io/region":    "us-west-2",
					"node.kubernetes.io/instance-type": "p4d.24xlarge",
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
			name:          "node not found fails open",
			node:          nil,
			eventNodeName: "non-existent-node",
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.NotEqual(t, pb.ProcessingStrategy_STORE_ONLY, event.ProcessingStrategy,
					"lookup failure should fail open and not gate the event")
			},
		},
		{
			name: "nil metadata initialization",
			node: &corev1.Node{
				Name: "test-node-2",
				Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-abc123"},
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
				Name: "test-node-3",
				Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-def456"},
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
				Name:   "test-node-4",
				Labels: map[string]string{"test-label": "test-value"},
				Spec:   corev1.NodeSpec{},
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
				Name: "test-node-5",
				Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-ghi789"},
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
				Name: "test-node-6",
				Labels: map[string]string{
					"topology.k8s.aws/capacity-block-id":    "cbr-01234567",
					"topology.k8s.aws/network-node-layer-1": "nn-abcd",
					"oci.oraclecloud.com/host.id":           "971b2",
					"cloud.google.com/gce-topology-block":   "9b6c",
					"cloud.google.com/gce-topology-host":    "7007",
					"node.kubernetes.io/instance-type":      "p4d.24xlarge",
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
			name: "topograph topology labels",
			node: &corev1.Node{
				Name: "test-node-7",
				Labels: map[string]string{
					"accelerator.topograph.run/domain": "clique-7",
					"fabric.topograph.run/tier-0":      "leaf-42",
					"fabric.topograph.run/tier-1":      "spine-3",
					"fabric.topograph.run/tier-2":      "core-1",
					"node.kubernetes.io/instance-type": "p5.48xlarge",
				},
				Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-topograph1"},
			},
			config: &Config{
				CacheSize: 100,
				CacheTTL:  1 * time.Hour,
				AllowedLabels: []string{
					"accelerator.topograph.run/domain",
					"fabric.topograph.run/tier-0",
					"fabric.topograph.run/tier-1",
					"fabric.topograph.run/tier-2",
				},
			},
			eventNodeName: "test-node-7",
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.Equal(t, "aws:///us-west-2a/i-topograph1", event.Metadata["providerID"])
				assert.Equal(t, "clique-7", event.Metadata["accelerator.topograph.run/domain"])
				assert.Equal(t, "leaf-42", event.Metadata["fabric.topograph.run/tier-0"])
				assert.Equal(t, "spine-3", event.Metadata["fabric.topograph.run/tier-1"])
				assert.Equal(t, "core-1", event.Metadata["fabric.topograph.run/tier-2"])
				assert.NotContains(t, event.Metadata, "node.kubernetes.io/instance-type", "should not include non-allowed labels")
			},
		},
		{
			name: "skip label match gates event to STORE_ONLY",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "skip-label-node-1",
					Labels: map[string]string{
						"nvsentinel.dgxc.nvidia.com/managed": "false",
						"topology.kubernetes.io/zone":        "us-west-2a",
					},
				},
				Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-skip1"},
			},
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				AllowedLabels: []string{"topology.kubernetes.io/zone"},
				SkipNodeLabel: "nvsentinel.dgxc.nvidia.com/managed=false",
			},
			eventNodeName: "skip-label-node-1",
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.Equal(t, pb.ProcessingStrategy_STORE_ONLY, event.ProcessingStrategy)
				assert.Equal(t, "aws:///us-west-2a/i-skip1", event.Metadata["providerID"],
					"gated events should still get metadata enrichment")
				assert.Equal(t, "us-west-2a", event.Metadata["topology.kubernetes.io/zone"],
					"gated events should still get label enrichment")
			},
		},
		{
			name: "no skip label match passes through",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "skip-label-node-2",
					Labels: map[string]string{
						"topology.kubernetes.io/zone": "us-east-1a",
					},
				},
				Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-noskip1"},
			},
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				AllowedLabels: []string{"topology.kubernetes.io/zone"},
				SkipNodeLabel: "nvsentinel.dgxc.nvidia.com/managed=false",
			},
			eventNodeName: "skip-label-node-2",
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.NotEqual(t, pb.ProcessingStrategy_STORE_ONLY, event.ProcessingStrategy,
					"event without skip label should not be gated")
				assert.Equal(t, "aws:///us-east-1a/i-noskip1", event.Metadata["providerID"])
			},
		},
		{
			name: "skip label with wrong value passes through",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "skip-label-node-3",
					Labels: map[string]string{
						"nvsentinel.dgxc.nvidia.com/managed": "true",
					},
				},
				Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-wrongval1"},
			},
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				SkipNodeLabel: "nvsentinel.dgxc.nvidia.com/managed=false",
			},
			eventNodeName: "skip-label-node-3",
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.NotEqual(t, pb.ProcessingStrategy_STORE_ONLY, event.ProcessingStrategy,
					"label with wrong value should not gate")
			},
		},
		{
			name: "empty skip label key and value disables gate",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "skip-label-node-4",
					Labels: map[string]string{
						"nvsentinel.dgxc.nvidia.com/managed": "false",
					},
				},
				Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-nogate1"},
			},
			config: &Config{
				CacheSize: 100,
				CacheTTL:  1 * time.Hour,
			},
			eventNodeName: "skip-label-node-4",
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.NotEqual(t, pb.ProcessingStrategy_STORE_ONLY, event.ProcessingStrategy,
					"empty skip key should disable gate")
			},
		},
		{
			name: "skip label gates healthy events too",
			node: &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "skip-label-node-5",
					Labels: map[string]string{
						"nvsentinel.dgxc.nvidia.com/managed": "false",
					},
				},
				Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-healthy1"},
			},
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				SkipNodeLabel: "nvsentinel.dgxc.nvidia.com/managed=false",
			},
			eventNodeName: "skip-label-node-5",
			expectError:   false,
			validateResult: func(t *testing.T, event *pb.HealthEvent) {
				assert.Equal(t, pb.ProcessingStrategy_STORE_ONLY, event.ProcessingStrategy,
					"healthy events should also be gated")
			},
		},
		{
			name: "multiple nodes enrichment",
			nodes: []*corev1.Node{
				{
					Name: "multi-test-node-1",
					Labels: map[string]string{
						"topology.kubernetes.io/zone": "us-west-2a",
					},
					Spec: corev1.NodeSpec{
						ProviderID: "aws:///us-west-2a/i-test1",
					},
				},
				{
					Name: "multi-test-node-2",
					Labels: map[string]string{
						"topology.kubernetes.io/zone": "us-west-2b",
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

				p := createTestAugmentor(t, tt.config)
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

			p := createTestAugmentor(t, tt.config)

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
		Name: "cache-test-node",
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-cache123"},
	}

	createTestNode(t, node)

	config := &Config{
		CacheSize: 100,
		CacheTTL:  1 * time.Hour,
	}

	p := createTestAugmentor(t, config)
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
		Name: "context-test-node",
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-ctx123"},
	}

	createTestNode(t, node)
	defer deleteTestNode(t, node.Name)

	config := &Config{
		CacheSize: 100,
		CacheTTL:  1 * time.Hour,
	}

	p := createTestAugmentor(t, config)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	event := &pb.HealthEvent{
		NodeName: "context-test-node",
		Metadata: make(map[string]string),
	}

	err := p.Transform(ctx, event)
	assert.NoError(t, err, "lookup failure should fail open and return nil")
}

func TestProcessorConcurrentAugmentations(t *testing.T) {
	node := &corev1.Node{
		Name: "concurrent-test-node",
		Labels: map[string]string{
			"test-label": "test-value",
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

	p := createTestAugmentor(t, config)
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			event := &pb.HealthEvent{
				NodeName: "concurrent-test-node",
				Metadata: make(map[string]string),
			}
			err := p.Transform(ctx, event)
			assert.NoError(t, err)
			assert.Equal(t, "aws:///us-west-2a/i-concurrent123", event.Metadata["providerID"])
			assert.Equal(t, "test-value", event.Metadata["test-label"])
		})
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
		{
			name: "skipNodeLabel missing equals sign is invalid",
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				SkipNodeLabel: "nvsentinel.dgxc.nvidia.com/managed",
			},
			clientset:   testClient,
			expectError: true,
			errorMsg:    "skipNodeLabel must be in key=value format",
		},
		{
			name: "skipNodeLabel with empty value is invalid",
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				SkipNodeLabel: "nvsentinel.dgxc.nvidia.com/managed=",
			},
			clientset:   testClient,
			expectError: true,
			errorMsg:    "skipNodeLabel must be in key=value format",
		},
		{
			name: "skipNodeLabel with empty key is invalid",
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				SkipNodeLabel: "=false",
			},
			clientset:   testClient,
			expectError: true,
			errorMsg:    "skipNodeLabel must be in key=value format",
		},
		{
			name: "skipNodeLabel with invalid Kubernetes label key is rejected",
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				SkipNodeLabel: "!!!invalid/key=false",
			},
			clientset:   testClient,
			expectError: true,
			errorMsg:    "not a valid Kubernetes label name",
		},
		{
			name: "skipNodeLabel with invalid Kubernetes label value is rejected",
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				SkipNodeLabel: "nvsentinel.dgxc.nvidia.com/managed=.starts-with-dot",
			},
			clientset:   testClient,
			expectError: true,
			errorMsg:    "not a valid Kubernetes label value",
		},
		{
			name: "skipNodeLabel with equals in value is rejected",
			config: &Config{
				CacheSize:     100,
				CacheTTL:      1 * time.Hour,
				SkipNodeLabel: "valid/key=val=ue",
			},
			clientset:   testClient,
			expectError: true,
			errorMsg:    "not a valid Kubernetes label value",
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestGetOrFetchMetadata_SlowNodeDoesNotBlockOthers: a cache miss whose
// Kubernetes read hangs must not hold up a miss for a different node. The
// deployment platform connector runs this for the whole fleet, where one
// slow lookup used to stall every other node's events behind one lock. The
// hang is injected in the HTTP transport in front of the envtest API server,
// because the fake clientset serializes every call behind one lock itself.
func TestGetOrFetchMetadata_SlowNodeDoesNotBlockOthers(t *testing.T) {
	ctx := context.Background()
	slowNode, fastNode := "singleflight-slow", "singleflight-fast"

	for _, name := range []string{slowNode, fastNode} {
		_, err := testClient.CoreV1().Nodes().Create(ctx,
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		require.NoError(t, err)

		t.Cleanup(func() {
			_ = testClient.CoreV1().Nodes().Delete(context.Background(), name, metav1.DeleteOptions{})
		})
	}

	started := make(chan struct{})
	release := make(chan struct{})

	var startedOnce sync.Once

	restCfg := rest.CopyConfig(testEnv.Config)
	restCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/nodes/"+slowNode) {
				startedOnce.Do(func() { close(started) })
				<-release
			}

			return rt.RoundTrip(req)
		})
	})

	clientset, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)

	augmentor, err := New(ctx, &Config{CacheSize: 10, CacheTTL: time.Hour}, clientset)
	require.NoError(t, err)

	slowDone := make(chan error, 1)

	go func() {
		_, err := augmentor.getOrFetchMetadata(ctx, slowNode)
		slowDone <- err
	}()

	<-started

	fastDone := make(chan error, 1)

	go func() {
		_, err := augmentor.getOrFetchMetadata(ctx, fastNode)
		fastDone <- err
	}()

	select {
	case err := <-fastDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a lookup for another node waited behind the slow one")
	}

	close(release)
	require.NoError(t, <-slowDone)
}

// TestGetOrFetchMetadata_SameNodeSharesOneRead: concurrent misses for one
// node cost a single Kubernetes read, and every caller gets its result.
func TestGetOrFetchMetadata_SameNodeSharesOneRead(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{})
	release := make(chan struct{})

	var gets atomic.Int32

	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "shared"},
		Spec:       corev1.NodeSpec{ProviderID: "aws:///us-west-2a/i-shared"},
	})
	clientset.PrependReactor("get", "nodes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		if gets.Add(1) == 1 {
			close(started)
		}

		<-release

		return false, nil, nil
	})

	augmentor, err := New(ctx, &Config{CacheSize: 10, CacheTTL: time.Hour}, clientset)
	require.NoError(t, err)

	const callers = 5

	var wg sync.WaitGroup

	results := make(chan *NodeMetadata, callers)

	for range callers {
		wg.Go(func() {
			metadata, err := augmentor.getOrFetchMetadata(ctx, "shared")
			if !assert.NoError(t, err) {
				return
			}
			results <- metadata
		})
	}

	<-started
	close(release)
	wg.Wait()
	close(results)

	for metadata := range results {
		require.Equal(t, "aws:///us-west-2a/i-shared", metadata.ProviderID)
	}

	require.EqualValues(t, 1, gets.Load(), "concurrent misses for one node share a single read")
}

// TestTransform_StalledLookupFailsOpenWithinTimeout: a node read that hangs
// ends with the lookup timeout, and the event proceeds without metadata
// (fail-open), so a stalled API server delays storage by at most that long.
func TestTransform_StalledLookupFailsOpenWithinTimeout(t *testing.T) {
	ctx := context.Background()
	node := "stalled-lookup"

	_, err := testClient.CoreV1().Nodes().Create(ctx,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}}, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = testClient.CoreV1().Nodes().Delete(context.Background(), node, metav1.DeleteOptions{})
	})

	restCfg := rest.CopyConfig(testEnv.Config)
	restCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/nodes/"+node) {
				// Hang until the request gives up.
				<-req.Context().Done()

				return nil, req.Context().Err()
			}

			return rt.RoundTrip(req)
		})
	})

	clientset, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)

	augmentor, err := New(ctx, &Config{CacheSize: 10, CacheTTL: time.Hour, LookupTimeout: 100 * time.Millisecond}, clientset)
	require.NoError(t, err)

	event := &pb.HealthEvent{NodeName: node, ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION}

	start := time.Now()
	require.NoError(t, augmentor.Transform(ctx, event), "a failed lookup never fails the event")
	require.Less(t, time.Since(start), 2*time.Second, "the lookup ends with its timeout")
	require.Empty(t, event.Metadata, "no metadata was added")
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, event.ProcessingStrategy, "the strategy is untouched")
}

// blockingNodeTransport wraps the envtest transport so reads of one node wait
// for release, honouring the request context.
func blockingNodeTransport(t *testing.T, node string, started chan<- struct{}, release <-chan struct{}) kubernetes.Interface {
	t.Helper()

	var startedOnce sync.Once

	restCfg := rest.CopyConfig(testEnv.Config)
	restCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/nodes/"+node) {
				startedOnce.Do(func() { close(started) })

				select {
				case <-release:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			}

			return rt.RoundTrip(req)
		})
	})

	clientset, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)

	return clientset
}

func ensureNode(t *testing.T, name string) {
	t.Helper()

	_, err := testClient.CoreV1().Nodes().Create(context.Background(),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = testClient.CoreV1().Nodes().Delete(context.Background(), name, metav1.DeleteOptions{})
	})
}

// TestGetOrFetchMetadata_LeaderCancellationDoesNotFailFollowers: the caller
// that started the shared read giving up must not fail the others waiting on
// the same node; the read continues, bounded by the lookup timeout, and they
// get their metadata.
func TestGetOrFetchMetadata_LeaderCancellationDoesNotFailFollowers(t *testing.T) {
	node := "shared-read-leader-cancel"
	ensureNode(t, node)

	started := make(chan struct{})
	release := make(chan struct{})
	clientset := blockingNodeTransport(t, node, started, release)

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour, LookupTimeout: 10 * time.Second}, clientset)
	require.NoError(t, err)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)

	go func() {
		_, err := augmentor.getOrFetchMetadata(leaderCtx, node)
		leaderDone <- err
	}()

	<-started

	followerDone := make(chan error, 1)

	go func() {
		_, err := augmentor.getOrFetchMetadata(context.Background(), node)
		followerDone <- err
	}()

	cancelLeader()

	select {
	case err := <-leaderDone:
		require.ErrorIs(t, err, context.Canceled, "the leader leaves on its own context")
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled leader did not return")
	}

	select {
	case err := <-followerDone:
		t.Fatalf("the follower must keep waiting for the shared read, got %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-followerDone:
		require.NoError(t, err, "the follower gets the metadata from the shared read")
	case <-time.After(5 * time.Second):
		t.Fatal("the follower did not get its metadata")
	}

	_, found := augmentor.cache.Get(node)
	require.True(t, found, "the shared read still fills the cache")
}

// TestGetOrFetchMetadata_CancelledFollowerReturnsPromptly: a waiter whose own
// context ends leaves at once without stopping the shared read.
func TestGetOrFetchMetadata_CancelledFollowerReturnsPromptly(t *testing.T) {
	node := "shared-read-follower-cancel"
	ensureNode(t, node)

	started := make(chan struct{})
	release := make(chan struct{})
	clientset := blockingNodeTransport(t, node, started, release)

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour, LookupTimeout: 10 * time.Second}, clientset)
	require.NoError(t, err)

	leaderDone := make(chan error, 1)

	go func() {
		_, err := augmentor.getOrFetchMetadata(context.Background(), node)
		leaderDone <- err
	}()

	<-started

	followerCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = augmentor.getOrFetchMetadata(followerCtx, node)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the follower leaves on its own deadline")

	close(release)
	require.NoError(t, <-leaderDone, "the shared read was not stopped by the follower leaving")
}

// TestTransform_CancelledCallerFailsOpenAtOnce: once the caller's context has
// ended, a cache miss fails open immediately instead of waiting a whole
// lookup timeout per event.
func TestTransform_CancelledCallerFailsOpenAtOnce(t *testing.T) {
	node := "cancelled-caller"
	ensureNode(t, node)

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour}, testClient)
	require.NoError(t, err)

	gone, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	event := &pb.HealthEvent{NodeName: node, ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION}

	start := time.Now()
	require.NoError(t, augmentor.Transform(gone, event))
	require.Less(t, time.Since(start), time.Second)
	require.Empty(t, event.Metadata)
}

// TestGetOrFetchMetadata_CancelledCallerStartsNoReads: once the caller's
// context has ended, the remaining uncached nodes fail open without starting
// any read, detached or not.
func TestGetOrFetchMetadata_CancelledCallerStartsNoReads(t *testing.T) {
	var reads atomic.Int32

	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("get", "nodes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		reads.Add(1)

		return false, nil, nil
	})

	augmentor, err := New(context.Background(), &Config{CacheSize: 10, CacheTTL: time.Hour}, clientset)
	require.NoError(t, err)

	gone, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	for i := range 20 {
		event := &pb.HealthEvent{NodeName: fmt.Sprintf("uncached-%d", i)}
		require.NoError(t, augmentor.Transform(gone, event), "every event fails open")
	}

	time.Sleep(50 * time.Millisecond)
	require.Zero(t, reads.Load(), "no read is started for a caller that will not wait for it")
}

// TestNew_IgnoresTheLabelNamedLikeTheIdempotencyKey: the platform connector
// owns the idempotency key metadata field, so a node label of the same name
// is never copied over it, whatever the allowed labels say.
func TestNew_IgnoresTheLabelNamedLikeTheIdempotencyKey(t *testing.T) {
	cfg := &Config{
		CacheSize:     10,
		CacheTTL:      time.Hour,
		AllowedLabels: []string{"topology.kubernetes.io/zone", datastore.HealthEventIdempotencyKeyMetadataField, "nvidia.com/gpu.product"},
	}

	augmentor, err := New(context.Background(), cfg, fake.NewSimpleClientset())
	require.NoError(t, err)
	require.Equal(t, []string{"topology.kubernetes.io/zone", "nvidia.com/gpu.product"}, augmentor.config.AllowedLabels)
	require.Len(t, cfg.AllowedLabels, 3, "the caller's config is left alone")
}
