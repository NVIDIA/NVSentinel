// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package reconciler

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	policyv1client "k8s.io/client-go/kubernetes/typed/policy/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	Client    *kubernetes.Clientset
	Context   context.Context
	StopFunc  context.CancelFunc
	TestEnv   *envtest.Environment
	k8sClient *NodeDrainerClient
)

type MockEvictionClient struct {
	policyv1client.PolicyV1Interface
	EvictedPods sync.Map
}

func (m *MockEvictionClient) Evictions(namespace string) policyv1client.EvictionInterface {
	return &MockEvictionInterface{m, namespace}
}

type MockEvictionInterface struct {
	client    *MockEvictionClient
	namespace string
}

func (m *MockEvictionInterface) Evict(ctx context.Context, eviction *policyv1.Eviction) error {
	m.client.EvictedPods.Store(m.namespace+"/"+eviction.Name, true)
	return nil
}

func TestMain(m *testing.M) {
	var err error
	var cfg *rest.Config
	ctx, cancel := context.WithCancel(context.TODO())
	StopFunc = cancel
	Context = ctx

	TestEnv = &envtest.Environment{}
	cfg, err = TestEnv.Start()
	if err != nil {
		panic(err)
	}

	Client, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		panic(err)
	}

	mockEvictionClient := &MockEvictionClient{
		PolicyV1Interface: Client.PolicyV1(),
		EvictedPods:       sync.Map{},
	}

	k8sClient = &NodeDrainerClient{
		clientset: Client,
		eviction:  mockEvictionClient,
	}

	namespaces := []string{"runai", "nvsentinel"}

	for _, ns := range namespaces {
		_, err := Client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
			ObjectMeta: metaV1.ObjectMeta{Name: ns},
		}, metaV1.CreateOptions{})
		if err != nil {
			log.Fatalf("Failed to create namespace %s: %v", ns, err)
		}
	}

	createTestPod(ctx, "runai", "pod1", "node1")
	createTestPod(ctx, "runai", "pod2", "node1")
	createTestPod(ctx, "runai", "pod3", "node2")
	createTestPod(ctx, "nvsentinel", "pod5", "node2")

	jobPod1 := &v1.Pod{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      "job-pod1",
			Namespace: "nvsentinel",
			OwnerReferences: []metaV1.OwnerReference{
				{
					APIVersion: "batch/v1",
					Kind:       "Job",
					Name:       "test-job",
					UID:        "12345678-1234-1234-1234-123456789abc",
					Controller: ptr.To(true),
				},
			},
		},
		Spec: v1.PodSpec{
			NodeName: "node1",
			Containers: []v1.Container{
				{Name: "pause", Image: "k8s.gcr.io/pause:3.1"},
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodFailed,
		},
	}
	jobPod2 := &v1.Pod{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      "job-pod2",
			Namespace: "nvsentinel",
			OwnerReferences: []metaV1.OwnerReference{
				{
					APIVersion: "batch/v1",
					Kind:       "Job",
					Name:       "test-job",
					UID:        "12345678-1234-1234-1234-123456789abc",
					Controller: ptr.To(true),
				},
			},
		},
		Spec: v1.PodSpec{
			NodeName: "node1",
			Containers: []v1.Container{
				{Name: "pause", Image: "k8s.gcr.io/pause:3.1"},
			},
		},
	}

	jobPod1, err = Client.CoreV1().Pods("nvsentinel").Create(ctx, jobPod1, metaV1.CreateOptions{})
	if err != nil {
		log.Fatalf("Error in creating the pod %s", jobPod1.Name)
	}
	jobPod1.Status.Phase = v1.PodSucceeded

	_, err = Client.CoreV1().Pods("nvsentinel").UpdateStatus(ctx, jobPod1, metaV1.UpdateOptions{})
	if err != nil {
		log.Fatalf("Failed to update pod status: %v", err)
	}

	jobPod2, err = Client.CoreV1().Pods("nvsentinel").Create(ctx, jobPod2, metaV1.CreateOptions{})
	if err != nil {
		log.Fatalf("error occured while creating pod %s in namespace %s on node %s: %v", "job-pod2", "nvsentinel", "node1", err)
	}
	jobPod2.Status.Phase = v1.PodRunning
	_, err = Client.CoreV1().Pods("nvsentinel").UpdateStatus(ctx, jobPod2, metaV1.UpdateOptions{})
	if err != nil {
		log.Fatalf("Failed to update pod status: %v", err)
	}

	createDaemonSet(ctx, "nvsentinel", "daemonset1")
	createDaemonSet(ctx, "runai", "daemonset2")

	exitCode := m.Run()

	TearDownResources()
	os.Exit(exitCode)
}

func TestFindAllPodsInNamespaceAndNode(t *testing.T) {
	tests := []struct {
		namespace string
		node      string
		expected  []string // Expected pod names
	}{
		{"runai", "node1", []string{"pod1", "pod2"}},
		{"runai", "node2", []string{"pod3"}},
		{"nvsentinel", "node1", []string{"job-pod1", "job-pod2"}},
		{"nvsentinel", "node2", []string{"pod5"}},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("Namespace: %s, Node: %s", tc.namespace, tc.node), func(t *testing.T) {
			podList, err := k8sClient.findAllPodsInNamespaceAndNode(context.TODO(), tc.namespace, tc.node)
			assert.NoError(t, err, "Error retrieving pods")

			actualPodNames := []string{}
			for _, pod := range podList {
				actualPodNames = append(actualPodNames, pod.Name)
			}

			assert.ElementsMatch(t, tc.expected, actualPodNames, "Pod list mismatch")
		})
	}
}

func TestEvictAllPodsImmediately(t *testing.T) {
	ctx := context.TODO()
	mockEvictionClient := k8sClient.eviction.(*MockEvictionClient)

	mockEvictionClient.EvictedPods = sync.Map{}

	err := k8sClient.EvictAllPodsInImmediateMode(ctx, "runai", "node1", 60)
	assert.NoError(t, err, "Error in evicting the pods in namespace %s on node %s", "runai", "node1")

	if _, exist := mockEvictionClient.EvictedPods.Load("runai/pod1"); !exist {
		t.Errorf("Expected Pod1 in namespace runai to be  evicted from node1")
	}

	if _, exist := mockEvictionClient.EvictedPods.Load("runai/pod2"); !exist {
		t.Errorf("Expected Pod2 in namespace runai to be evicted from node1")
	}

	// daemonset pods should not be evicted
	assertPodNotDeleted(ctx, t, "runai", "daemonset2")
}

func TestMonitorPodCompletionWithContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	namespace := "nvsentinel"
	mockEvictionClient := k8sClient.eviction.(*MockEvictionClient)

	mockEvictionClient.EvictedPods = sync.Map{}

	go func() {
		time.Sleep(15 * time.Second)
		cancel()
	}()

	err := k8sClient.MonitorPodCompletion(ctx, namespace, "node1")
	if err != nil {
		t.Fatalf("Error while evicting pods in namespace %s on node %s: %v", namespace, "node1", err)
	}

	assertPodNotDeleted(ctx, t, namespace, "job-pod2")
	// daemonset pods should not be terminated
	assertPodNotDeleted(ctx, t, "nvsentinel", "daemonset1")
}

func TestMonitorPodCompletion(t *testing.T) {
	ctx := context.TODO()
	var err error
	namespace := "nvsentinel"
	mockEvictionClient := k8sClient.eviction.(*MockEvictionClient)

	mockEvictionClient.EvictedPods = sync.Map{}

	go func() {
		err = Client.CoreV1().Pods("nvsentinel").Delete(ctx, "job-pod1", metaV1.DeleteOptions{})
		if err != nil {
			t.Log("error in deleting the pod job-pod1")
		}
		time.Sleep(5 * time.Second)
		pod, err := Client.CoreV1().Pods(namespace).Get(ctx, "job-pod2", metaV1.GetOptions{})
		if err != nil {
			t.Logf("Failed to get pod: %v", err)
			return
		}
		pod.Status.Phase = v1.PodSucceeded
		_, err = Client.CoreV1().Pods(namespace).UpdateStatus(ctx, pod, metaV1.UpdateOptions{})
		if err != nil {
			t.Logf("Failed to update pod status: %v", err)
		}
		err = Client.CoreV1().Pods("nvsentinel").Delete(ctx, "job-pod2", metaV1.DeleteOptions{})
		if err != nil {
			t.Log("error in deleting the pod job-pod2")
		}
	}()

	err = k8sClient.MonitorPodCompletion(ctx, namespace, "node1")
	if err != nil {
		t.Fatalf("Error is not expected while eviction of pods in namespace %s in Allow completion mode", namespace)
	}
	// daemonset pods should not be terminated
	assertPodNotDeleted(ctx, t, "nvsentinel", "daemonset1")
}

func TestCheckIfAllPodsAreEvictedInImmediateMode(t *testing.T) {
	ctx := context.Background()

	evicted := k8sClient.CheckIfAllPodsAreEvictedInImmediateMode(ctx, []string{"runai"}, "node1", time.Duration(3*time.Second))
	if !evicted {
		t.Fatalf("Expected all pods in immediated mode to be evicted")
	}

	assertPodDeleted(ctx, t, "runai", "pod1")
	assertPodDeleted(ctx, t, "runai", "pod2")
}

func assertPodDeleted(ctx context.Context, t *testing.T, namespace, podName string) {
	time.Sleep(500 * time.Millisecond) // Allow API server time to update state
	_, err := Client.CoreV1().Pods(namespace).Get(ctx, podName, metaV1.GetOptions{})
	// Expect pod to be evicted
	assert.Error(t, err, "Pod %s is not evicted from namespace %s", podName, namespace)
}

func assertPodNotDeleted(ctx context.Context, t *testing.T, namespace, podName string) {
	pod, _ := Client.CoreV1().Pods(namespace).Get(ctx, podName, metaV1.GetOptions{})
	assert.NotNil(t, pod, "Pod %s is evicted from namespace %s", podName, namespace)
}

func createTestPod(ctx context.Context, namespace, name, node string) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			NodeName: node,
			Containers: []v1.Container{
				{Name: "pause", Image: "k8s.gcr.io/pause:3.1"},
			},
		},
	}
	createdPod, err := Client.CoreV1().Pods(namespace).Create(ctx, pod, metaV1.CreateOptions{})
	if err != nil {
		log.Fatalf("error occured while creating pod %s in namespace %s on node %s: %v", name, namespace, node, err)
	}
	return createdPod
}

func createDaemonSet(ctx context.Context, namespace, name string) {
	daemonSetPod := &v1.Pod{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metaV1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "DaemonSet",
					Name:       "test-daemonset",
					UID:        "87654321-4321-4321-4321-abcdefabcdef",
					Controller: ptr.To(true),
				},
			},
		},
		Spec: v1.PodSpec{
			NodeName: "node1",
			Containers: []v1.Container{
				{Name: "pause", Image: "k8s.gcr.io/pause:3.1"},
			},
		},
	}

	_, err := Client.CoreV1().Pods(namespace).Create(ctx, daemonSetPod, metaV1.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create DaemonSet pod %s: %v", name, err)
	}
}

func TearDownResources() {
	fmt.Println("Stopping manager...")
	StopFunc()
	err := TestEnv.Stop()
	if err != nil {
		log.Fatalf("error in stopping test environment: %v", err)
	}
	fmt.Println("Test environment is stopped")
}
