// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nodeinfo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/fault-quarantine-module/pkg/common"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

func TestGetQuarantinedNodesMap(t *testing.T) {
	nodeInfo := NewNodeInfo(make(chan struct{}, 1))
	nodeInfo.quarantinedNodesMap["node1"] = true

	// Test GetQuarantinedNodesCount
	count := nodeInfo.GetQuarantinedNodesCount()
	if count != 1 {
		t.Errorf("Expected count to be 1, got %d", count)
	}

	// Test GetQuarantinedNodesCopy
	mapCopy := nodeInfo.GetQuarantinedNodesCopy()
	if len(mapCopy) != 1 {
		t.Errorf("Expected map to have 1 entry, got %d", len(mapCopy))
	}

	if !mapCopy["node1"] {
		t.Error("Expected node1 to be in the map with value true")
	}
}

func TestBuildQuarantinedNodesMap(t *testing.T) {
	tests := []struct {
		name            string
		nodes           []v1.Node
		expectedMapKeys []string
		expectedError   error
	}{
		{
			name:            "Empty node list",
			nodes:           []v1.Node{},
			expectedMapKeys: []string{},
			expectedError:   nil,
		},
		{
			name: "No quarantined nodes",
			nodes: []v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "node1",
						Annotations: map[string]string{},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node2",
						Annotations: map[string]string{
							"other-annotation": "value",
						},
					},
				},
			},
			expectedMapKeys: []string{},
			expectedError:   nil,
		},
		{
			name: "Some quarantined nodes",
			nodes: []v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
						Annotations: map[string]string{
							common.QuarantineHealthEventIsCordonedAnnotationKey: common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "node2",
						Annotations: map[string]string{},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node3",
						Annotations: map[string]string{
							common.QuarantineHealthEventIsCordonedAnnotationKey: common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
						},
					},
				},
			},
			expectedMapKeys: []string{"node1", "node3"},
			expectedError:   nil,
		},
		{
			name: "Incorrect annotation value",
			nodes: []v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node1",
						Annotations: map[string]string{
							common.QuarantineHealthEventIsCordonedAnnotationKey: "False",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "node2",
						Annotations: map[string]string{
							common.QuarantineHealthEventIsCordonedAnnotationKey: common.QuarantineHealthEventIsCordonedAnnotationValueTrue,
						},
					},
				},
			},
			expectedMapKeys: []string{"node2"},
			expectedError:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a fake k8s client with the test nodes
			fakeClient := fake.NewSimpleClientset()
			for _, node := range tt.nodes {
				_, err := fakeClient.CoreV1().Nodes().Create(context.Background(), &node, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create fake node: %v", err)
				}
			}

			nodeInfo := NewNodeInfo(make(chan struct{}, 1))
			err := nodeInfo.BuildQuarantinedNodesMap(fakeClient)

			// Check error
			if (err != nil && tt.expectedError == nil) || (err == nil && tt.expectedError != nil) {
				t.Errorf("Expected error %v, got %v", tt.expectedError, err)
			}

			// Check map contents
			nodeMap := nodeInfo.quarantinedNodesMap
			if len(nodeMap) != len(tt.expectedMapKeys) {
				t.Errorf("Expected %d nodes in map, got %d", len(tt.expectedMapKeys), len(nodeMap))
			}

			for _, key := range tt.expectedMapKeys {
				if !nodeMap[key] {
					t.Errorf("Expected node %s to be in map", key)
				}
			}
		})
	}
}

func TestMarkNodeQuarantineStatusCache(t *testing.T) {
	tests := []struct {
		name                 string
		initialMap           map[string]bool
		nodeToMark           string
		isQuarantined        bool
		expectedMap          map[string]bool
		expectedSignalCalled bool
	}{
		{
			name: "Mark node as quarantined",
			initialMap: map[string]bool{
				"node1": true,
			},
			nodeToMark:    "node2",
			isQuarantined: true,
			expectedMap: map[string]bool{
				"node1": true,
				"node2": true,
			},
			expectedSignalCalled: true,
		},
		{
			name: "Mark node as unquarantined",
			initialMap: map[string]bool{
				"node1": true,
				"node2": true,
			},
			nodeToMark:    "node2",
			isQuarantined: false,
			expectedMap: map[string]bool{
				"node1": true,
			},
			expectedSignalCalled: true,
		},
		{
			name: "Mark non-existent node as unquarantined",
			initialMap: map[string]bool{
				"node1": true,
			},
			nodeToMark:    "node2",
			isQuarantined: false,
			expectedMap: map[string]bool{
				"node1": true,
			},
			expectedSignalCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workSignal := make(chan struct{}, 1)
			nodeInfo := NewNodeInfo(workSignal)

			// Initialize the map
			for node, status := range tt.initialMap {
				nodeInfo.quarantinedNodesMap[node] = status
			}

			// Call the method
			nodeInfo.MarkNodeQuarantineStatusCache(tt.nodeToMark, tt.isQuarantined, false)

			// Check the map was updated correctly
			if len(nodeInfo.quarantinedNodesMap) != len(tt.expectedMap) {
				t.Errorf("Expected map size %d, got %d", len(tt.expectedMap), len(nodeInfo.quarantinedNodesMap))
			}

			for node, expectedStatus := range tt.expectedMap {
				actualStatus, exists := nodeInfo.quarantinedNodesMap[node]
				if !exists {
					t.Errorf("Expected node %s to be in map", node)
					continue
				}
				if actualStatus != expectedStatus {
					t.Errorf("Expected node %s status to be %v, got %v", node, expectedStatus, actualStatus)
				}
			}

			// Check if signal was called
			if tt.expectedSignalCalled {
				select {
				case <-workSignal:
					// Signal was received, as expected
				case <-time.After(100 * time.Millisecond):
					t.Error("Expected work signal to be sent, but it wasn't")
				}
			}
		})
	}
}

func TestGetNodeQuarantineStatusCache(t *testing.T) {
	tests := []struct {
		name         string
		initialMap   map[string]bool
		nodeToCheck  string
		expectedBool bool
	}{
		{
			name: "Node is quarantined",
			initialMap: map[string]bool{
				"node1": true,
				"node2": true,
			},
			nodeToCheck:  "node1",
			expectedBool: true,
		},
		{
			name: "Node is not quarantined",
			initialMap: map[string]bool{
				"node1": true,
			},
			nodeToCheck:  "node2",
			expectedBool: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeInfo := NewNodeInfo(make(chan struct{}, 1))

			// Initialize the map
			for node, status := range tt.initialMap {
				nodeInfo.quarantinedNodesMap[node] = status
			}

			// Call the method
			result := nodeInfo.GetNodeQuarantineStatusCache(tt.nodeToCheck)

			// Check the result
			if result != tt.expectedBool {
				t.Errorf("Expected %v, got %v", tt.expectedBool, result)
			}
		})
	}
}

func TestSignalWork(t *testing.T) {
	tests := []struct {
		name             string
		workSignal       chan struct{}
		preloadChannel   bool
		expectSignalSent bool
	}{
		{
			name:             "Signal sent on empty channel",
			workSignal:       make(chan struct{}, 1),
			preloadChannel:   false,
			expectSignalSent: true,
		},
		{
			name:             "Signal not sent on full channel",
			workSignal:       make(chan struct{}, 1),
			preloadChannel:   true,
			expectSignalSent: false,
		},
		{
			name:             "Nil channel",
			workSignal:       nil,
			preloadChannel:   false,
			expectSignalSent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeInfo := NewNodeInfo(tt.workSignal)

			// Preload the channel if needed
			if tt.preloadChannel && tt.workSignal != nil {
				tt.workSignal <- struct{}{}
			}

			// Call the method
			nodeInfo.signalWork()

			// Check if signal was sent
			if tt.expectSignalSent {
				select {
				case <-tt.workSignal:
					// Signal was received, as expected
				case <-time.After(100 * time.Millisecond):
					t.Error("Expected work signal to be sent, but it wasn't")
				}
			} else if tt.workSignal != nil {
				// Only check for unexpected signals if the channel is not nil
				select {
				case <-tt.workSignal:
					if !tt.preloadChannel {
						t.Error("Unexpected signal received")
					}
				case <-time.After(100 * time.Millisecond):
					// No signal, as expected for a full channel
				}
			}
		})
	}
}

// MockK8sClient is a mock implementation of kubernetes.Interface for error case testing
type MockK8sClient struct {
	kubernetes.Interface
	shouldReturnError bool
}

func (m *MockK8sClient) CoreV1() corev1.CoreV1Interface {
	return &MockCoreV1Client{shouldReturnError: m.shouldReturnError}
}

type MockCoreV1Client struct {
	corev1.CoreV1Interface
	shouldReturnError bool
}

func (m *MockCoreV1Client) Nodes() corev1.NodeInterface {
	return &MockNodeClient{shouldReturnError: m.shouldReturnError}
}

type MockNodeClient struct {
	corev1.NodeInterface
	shouldReturnError bool
}

func (m *MockNodeClient) List(ctx context.Context, opts metav1.ListOptions) (*v1.NodeList, error) {
	if m.shouldReturnError {
		return nil, errors.New("mock error")
	}
	return &v1.NodeList{}, nil
}

// TestMarkNodeQuarantineStatusCache_ManualUncordon tests the specific scenario
// where a node is manually uncordoned but still has quarantine annotations
func TestMarkNodeQuarantineStatusCache_ManualUncordon(t *testing.T) {
	workSignal := make(chan struct{}, 10)
	nodeInfo := NewNodeInfo(workSignal)

	// Step 1: Node is quarantined by FQM
	nodeInfo.MarkNodeQuarantineStatusCache("test-node", true, true)

	// Verify node is in quarantine map
	if !nodeInfo.GetNodeQuarantineStatusCache("test-node") {
		t.Fatal("Expected node to be quarantined after initial quarantine")
	}

	// Drain any existing signals
	for {
		select {
		case <-workSignal:
			// keep draining
		default:
			goto done
		}
	}
done:

	// Step 2: Node is manually uncordoned (isQuarantined=false) but annotation still exists
	nodeInfo.MarkNodeQuarantineStatusCache("test-node", false, true)

	// Verify node is STILL in quarantine map because annotation exists
	if !nodeInfo.GetNodeQuarantineStatusCache("test-node") {
		t.Error("Expected node to remain in quarantine map when manually uncordoned but annotation exists")
	}

	// Verify the internal map still has the node
	nodeInfo.mutex.RLock()
	_, exists := nodeInfo.quarantinedNodesMap["test-node"]
	nodeInfo.mutex.RUnlock()

	if !exists {
		t.Error("Expected node to remain in internal quarantinedNodesMap when annotation exists")
	}

	// Verify work signal was sent
	select {
	case <-workSignal:
		// Good, signal was sent
	case <-time.After(100 * time.Millisecond):
		t.Error("Expected work signal when marking manual uncordon")
	}

	// Step 3: Annotation is removed (proper cleanup)
	nodeInfo.MarkNodeQuarantineStatusCache("test-node", false, false)

	// Now node should be removed from quarantine map
	if nodeInfo.GetNodeQuarantineStatusCache("test-node") {
		t.Error("Expected node to be removed from quarantine map when annotation is removed")
	}

	// Verify work signal was sent for cleanup
	select {
	case <-workSignal:
		// Good, signal was sent
	case <-time.After(100 * time.Millisecond):
		t.Error("Expected work signal when removing from quarantine")
	}
}

// TestMarkNodeQuarantineStatusCache_EdgeCases tests various edge cases
func TestMarkNodeQuarantineStatusCache_EdgeCases(t *testing.T) {
	tests := []struct {
		name            string
		nodeName        string
		isQuarantined   bool
		annotationExist bool
		expectedInMap   bool
		description     string
	}{
		{
			name:            "Quarantine with annotation",
			nodeName:        "node1",
			isQuarantined:   true,
			annotationExist: true,
			expectedInMap:   true,
			description:     "Normal quarantine operation",
		},
		{
			name:            "Quarantine without annotation (edge case)",
			nodeName:        "node2",
			isQuarantined:   true,
			annotationExist: false,
			expectedInMap:   true,
			description:     "Quarantine even without annotation",
		},
		{
			name:            "Unquarantine without annotation",
			nodeName:        "node3",
			isQuarantined:   false,
			annotationExist: false,
			expectedInMap:   false,
			description:     "Normal unquarantine operation",
		},
		{
			name:            "Manual uncordon (annotation exists)",
			nodeName:        "node4",
			isQuarantined:   false,
			annotationExist: true,
			expectedInMap:   true,
			description:     "Node manually uncordoned but annotation remains",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workSignal := make(chan struct{}, 1)
			nodeInfo := NewNodeInfo(workSignal)

			// If testing unquarantine or manual uncordon, first quarantine the node
			if !tt.isQuarantined || tt.name == "Manual uncordon (annotation exists)" {
				nodeInfo.MarkNodeQuarantineStatusCache(tt.nodeName, true, true)
				// Drain signal
				select {
				case <-workSignal:
					// drained
				default:
					// nothing to drain - continue safely
				}
			}

			// Perform the test operation
			nodeInfo.MarkNodeQuarantineStatusCache(tt.nodeName, tt.isQuarantined, tt.annotationExist)

			// Check if node is in map
			isInMap := nodeInfo.GetNodeQuarantineStatusCache(tt.nodeName)

			if isInMap != tt.expectedInMap {
				t.Errorf("%s: expected node in map = %v, got %v. %s",
					tt.name, tt.expectedInMap, isInMap, tt.description)
			}

			// Verify work signal was sent
			select {
			case <-workSignal:
				// Good
			case <-time.After(100 * time.Millisecond):
				t.Errorf("%s: expected work signal to be sent", tt.name)
			}
		})
	}
}
