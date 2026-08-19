// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package informer

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestStripNodeStatus(t *testing.T) {
	input := testFullNode()
	wantMetadata := input.ObjectMeta.DeepCopy()
	wantSpec := input.Spec.DeepCopy()

	transformed, err := stripNodeStatus(input)
	if err != nil {
		t.Fatalf("stripNodeStatus() error = %v", err)
	}

	node, ok := transformed.(*v1.Node)
	if !ok {
		t.Fatalf("stripNodeStatus() returned %T", transformed)
	}

	if node != input {
		t.Fatal("stripNodeStatus() returned a copy instead of mutating in place")
	}
	if !reflect.DeepEqual(node.ObjectMeta, *wantMetadata) {
		t.Fatalf("cached node metadata changed:\n got: %#v\nwant: %#v", node.ObjectMeta, *wantMetadata)
	}
	if !reflect.DeepEqual(node.Spec, *wantSpec) {
		t.Fatalf("cached node spec changed:\n got: %#v\nwant: %#v", node.Spec, *wantSpec)
	}
	if !reflect.DeepEqual(node.Status, v1.NodeStatus{}) {
		t.Fatalf("cached node retained status: %#v", node.Status)
	}
}

func TestNewNodeInformerStripsStatus(t *testing.T) {
	client := fake.NewClientset(testFullNode())
	nodeInformer, err := NewNodeInformer(client, 0, GPUNodeLabel, GPUNodeLabelValue)
	if err != nil {
		t.Fatalf("NewNodeInformer() error = %v", err)
	}

	stopCh := make(chan struct{})
	t.Cleanup(func() { close(stopCh) })

	if err := nodeInformer.Run(stopCh); err != nil {
		t.Fatalf("NodeInformer.Run() error = %v", err)
	}

	node, err := nodeInformer.GetNode("test-node")
	if err != nil {
		t.Fatalf("GetNode() error = %v", err)
	}

	if !reflect.DeepEqual(node.Status, v1.NodeStatus{}) {
		t.Fatalf("cached node retained status: %#v", node.Status)
	}
	if node.Labels["label"] != "value" || node.Spec.PodCIDR != "10.0.0.0/24" {
		t.Fatal("cached node is missing metadata or spec fields")
	}
}

func BenchmarkStripNodeStatus(b *testing.B) {
	fullNode := testFullNode()
	fullJSON, err := json.Marshal(fullNode)
	if err != nil {
		b.Fatalf("json.Marshal(full node) error = %v", err)
	}

	transformed, err := stripNodeStatus(fullNode)
	if err != nil {
		b.Fatalf("stripNodeStatus() error = %v", err)
	}
	slimJSON, err := json.Marshal(transformed)
	if err != nil {
		b.Fatalf("json.Marshal(slim node) error = %v", err)
	}

	b.ResetTimer()

	for b.Loop() {
		if _, err := stripNodeStatus(fullNode); err != nil {
			b.Fatal(err)
		}
	}

	b.ReportMetric(float64(len(fullJSON)), "full-json-bytes")
	b.ReportMetric(float64(len(slimJSON)), "cached-json-bytes")
}

func testFullNode() *v1.Node {
	return &v1.Node{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-node",
			UID:             types.UID("test-uid"),
			ResourceVersion: "42",
			Labels:          map[string]string{"label": "value", GPUNodeLabel: GPUNodeLabelValue},
			Annotations:     map[string]string{"annotation": "value"},
			OwnerReferences: []metav1.OwnerReference{{Name: "owner"}},
			ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "manager"}},
		},
		Spec: v1.NodeSpec{
			Unschedulable: true,
			Taints: []v1.Taint{{
				Key:       "key",
				Value:     "value",
				Effect:    v1.TaintEffectNoSchedule,
				TimeAdded: &metav1.Time{Time: time.Unix(1, 0)},
			}},
			PodCIDR: "10.0.0.0/24",
		},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{v1.ResourceCPU: resource.MustParse("8")},
			Conditions: []v1.NodeCondition{{
				Type:   v1.NodeReady,
				Status: v1.ConditionTrue,
			}},
		},
	}
}
