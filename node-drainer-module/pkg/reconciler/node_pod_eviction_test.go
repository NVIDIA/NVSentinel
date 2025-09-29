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
	storeconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/connectors/store"
	platform_connectors "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
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
	// Reduce the NotReady timeout so tests complete quickly (1 minute)
	k8sClient.notReadyTimeoutMinutes = ptr.To(1)
	k8sClient.pollInterval = 10 * time.Second

	namespaces := []string{"runai", "nvsentinel", "runai-prod", "runai-dev", "testing-ns"}

	for _, ns := range namespaces {
		_, err := Client.CoreV1().Namespaces().Create(context.TODO(), &v1.Namespace{
			ObjectMeta: metaV1.ObjectMeta{Name: ns},
		}, metaV1.CreateOptions{})
		if err != nil {
			log.Fatalf("Failed to create namespace %s: %v", ns, err)
		}
	}

	createNode(ctx, "node1", map[string]string{})
	createNode(ctx, "node2", map[string]string{})

	createTestPod(ctx, "runai", "pod1", "node1")
	createTestPod(ctx, "runai", "pod2", "node1")
	createTestPod(ctx, "runai", "pod3", "node2")
	createTestPod(ctx, "nvsentinel", "pod5", "node2")

	markPodHealthy(ctx, "runai", []string{"pod1", "pod2", "pod3"})
	markPodHealthy(ctx, "nvsentinel", []string{"pod5"})

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
	// Mark job-pod2 as healthy (Running & Ready)
	markPodHealthy(ctx, "nvsentinel", []string{"job-pod2"})

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
		{"nvsentinel", "node1", []string{"job-pod2"}}, // skip job-pod1 as it is in failed state
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
	nodeName := "node1"
	mockEvictionClient := k8sClient.eviction.(*MockEvictionClient)

	mockEvictionClient.EvictedPods = sync.Map{}

	go func() {
		err = Client.CoreV1().Pods("nvsentinel").Delete(ctx, "job-pod1", metaV1.DeleteOptions{})
		if err != nil {
			t.Log("error in deleting the pod job-pod1")
		}
		time.Sleep(12 * time.Second)
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

	err = k8sClient.MonitorPodCompletion(ctx, namespace, nodeName)
	if err != nil {
		t.Fatalf("Error is not expected while eviction of pods in namespace %s in Allow completion mode. Error: %v", namespace, err)
	}
	// daemonset pods should not be terminated
	assertPodNotDeleted(ctx, t, "nvsentinel", "daemonset1")

	reason := "AwaitingPodCompletion"
	events, err := Client.CoreV1().Events(metaV1.NamespaceDefault).List(ctx, metaV1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=%s,reason=%s", nodeName, "Node", reason),
	})
	assert.Greater(t, len(events.Items), 0, "expected at least one event for node %s", nodeName)

	latestEvt := events.Items[0]
	expectedMessage := fmt.Sprintf("Waiting for following pods to finish: [job-pod2] in namespace: %s", namespace)
	assert.Equal(t, latestEvt.Message, expectedMessage, "expected updated message to mention running pod")
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

func TestDeletePodsAfterTimeout(t *testing.T) {
	ctx := context.TODO()

	// Subtest 1: drain timeout already elapsed -> force delete
	t.Run("timeout elapsed - pods deleted", func(t *testing.T) {
		nodeName := "node1"
		node1, _ := Client.CoreV1().Nodes().Get(ctx, nodeName, metaV1.GetOptions{})
		node1.Labels = map[string]string{
			NodeDrainStatusLabelKey: string(InProgress),
		}
		Client.CoreV1().Nodes().Update(ctx, node1, metaV1.UpdateOptions{})

		// pod on node-timeout
		createTestPod(ctx, "nvsentinel", "timeout-pod", nodeName)

		err := k8sClient.DeletePodsAfterTimeout(ctx, nodeName, []string{"nvsentinel"}, 4, &storeconnector.HealthEventWithStatus{
			CreatedAt:   time.Now().Add(-230 * time.Second), // 3 minutes and 50 seconds
			HealthEvent: &platform_connectors.HealthEvent{},
			HealthEventStatus: storeconnector.HealthEventStatus{
				NodeQuarantined:        ptr.To(storeconnector.Quarantined),
				UserPodsEvictionStatus: storeconnector.OperationStatus{},
				FaultRemediated:        nil,
			},
		})
		assert.NoError(t, err)
		assertPodDeleted(ctx, t, "nvsentinel", "timeout-pod")
	})

	// Subtest 2: drain timeout not elapsed -> pods remain
	t.Run("timeout not elapsed - pods remain", func(t *testing.T) {
		nodeName := "node-no-timeout"
		podName := "early-pod"
		// create node with start time 30s ago
		createNode(ctx, nodeName, map[string]string{
			NodeDrainStatusLabelKey: string(InProgress),
		})

		createTestPod(ctx, "nvsentinel", podName, nodeName)
		timout := time.Now().UTC().Add(20 * time.Second).Format(time.RFC3339)
		expectedMessage := fmt.Sprintf("Waiting for following pods to finish: [%s] in namespace: [%s] or they will be force deleted on: %s", podName, "nvsentinel", timout)

		err := k8sClient.DeletePodsAfterTimeout(ctx, nodeName, []string{"nvsentinel"}, 1, &storeconnector.HealthEventWithStatus{
			CreatedAt:   time.Now().Add(-40 * time.Second), // 40 seconds
			HealthEvent: &platform_connectors.HealthEvent{},
			HealthEventStatus: storeconnector.HealthEventStatus{
				NodeQuarantined:        ptr.To(storeconnector.Quarantined),
				UserPodsEvictionStatus: storeconnector.OperationStatus{},
				FaultRemediated:        nil,
			},
		}) // 1 minute timeout
		assert.NoError(t, err)
		assertPodNotDeleted(ctx, t, "nvsentinel", podName)
		reason := "WaitingBeforeForceDelete"
		// Verify that an event has been recorded for this node indicating the drain is in progress
		events, err := Client.CoreV1().Events(metaV1.NamespaceDefault).List(ctx, metaV1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=%s,reason=%s", nodeName, "Node", reason),
		})
		assert.NoError(t, err)
		assert.Greater(t, len(events.Items), 0, "expected at least one event for node %s", nodeName)

		latestEvt := events.Items[0]
		assert.Equal(t, latestEvt.Message, expectedMessage)
	})

	// Subtest 3: node not draining (no label) -> function no-ops
	t.Run("node not draining", func(t *testing.T) {
		nodeName := "plain-node"
		createNode(ctx, nodeName, map[string]string{})

		err := k8sClient.DeletePodsAfterTimeout(ctx, nodeName, []string{"nvsentinel"}, 1, &storeconnector.HealthEventWithStatus{
			CreatedAt:   time.Now().Add(-3 * time.Minute),
			HealthEvent: &platform_connectors.HealthEvent{},
			HealthEventStatus: storeconnector.HealthEventStatus{
				NodeQuarantined:        ptr.To(storeconnector.Quarantined),
				UserPodsEvictionStatus: storeconnector.OperationStatus{},
				FaultRemediated:        nil,
			},
		})
		assert.NoError(t, err)
	})
}

func TestUpdateNodeEventUpdatesExistingEvent(t *testing.T) {
	ctx := context.TODO()
	nodeName := "event-node"

	createNode(ctx, nodeName, map[string]string{})

	eventsClient := Client.CoreV1().Events(metaV1.NamespaceDefault)
	deleteTimeUTC := time.Now().UTC().Format(time.RFC3339)
	message := fmt.Sprintf("Waiting for following pods to finish: %s in namespace: %s or they will be force deleted on: %s.", "[pod-A]", "[nvsentinel]", deleteTimeUTC)
	reason := "WaitingBeforeForceDelete"
	eventType := "NodeDraining"

	initialEvent := &v1.Event{
		ObjectMeta: metaV1.ObjectMeta{
			GenerateName: nodeName + "-",
			Namespace:    metaV1.NamespaceDefault,
		},
		InvolvedObject: v1.ObjectReference{
			Kind:       "Node",
			Name:       nodeName,
			APIVersion: "v1",
		},
		Reason:         reason,
		Message:        message,
		Type:           eventType,
		Source:         v1.EventSource{Component: "unit-test"},
		FirstTimestamp: metaV1.NewTime(time.Now()),
		LastTimestamp:  metaV1.NewTime(time.Now()),
		Count:          1,
	}

	createdEvt, err := eventsClient.Create(ctx, initialEvent, metaV1.CreateOptions{})

	assert.NoError(t, err)

	_ = k8sClient.updateNodeEvent(ctx, nodeName, reason, message)

	updated, err := eventsClient.Get(ctx, createdEvt.Name, metaV1.GetOptions{})
	assert.NoError(t, err)

	assert.Greater(t, updated.Count, int32(1), "expected event count to be incremented")
	assert.Equal(t, updated.Message, message, "expected updated message to mention running pod")
}

func TestPodStuckInTerminatingState(t *testing.T) {
	ctx := context.TODO()
	namespace := "testing-ns"

	// create a pod marked for deletion in the past (stuck terminating)
	grace := int64(1)
	pod := &v1.Pod{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      "terminating-pod",
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			NodeName:                      "node1",
			TerminationGracePeriodSeconds: &grace,
			Containers:                    []v1.Container{{Name: "pause", Image: "k8s.gcr.io/pause:3.1"}},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}

	pod, err := Client.CoreV1().Pods(namespace).Create(ctx, pod, metaV1.CreateOptions{})
	assert.NoError(t, err)

	// Issue a delete with the same grace period to set DeletionTimestamp
	err = Client.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metaV1.DeleteOptions{GracePeriodSeconds: &grace})
	assert.NoError(t, err)

	// Wait for grace period to elapse so that pod is considered stuck
	time.Sleep(2 * time.Second)

	done := make(chan struct{})
	go func() {
		_ = k8sClient.MonitorPodCompletion(ctx, namespace, "node1")
		close(done)
	}()

	select {
	case <-done:
		// success - returned without waiting forever
	case <-time.After(2 * time.Minute):
		t.Fatalf("MonitorPodCompletion did not return in expected time for terminating pod")
	}
}

func TestPodStuckInPendingState(t *testing.T) {
	ctx := context.TODO()
	namespace := "testing-ns"

	start := metaV1.NewTime(time.Now().Add(-3 * time.Minute))
	pod := &v1.Pod{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      "pending-pod",
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			NodeName:   "node1",
			Containers: []v1.Container{{Name: "pause", Image: "k8s.gcr.io/pause:3.1"}},
		},
		Status: v1.PodStatus{Phase: v1.PodPending, StartTime: &start},
	}

	_, err := Client.CoreV1().Pods(namespace).Create(ctx, pod, metaV1.CreateOptions{})
	assert.NoError(t, err)

	// Update status with NotReady condition so MonitorPodCompletion can skip it immediately
	pod.Status = v1.PodStatus{
		Phase:      v1.PodPending,
		StartTime:  &start,
		Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse, LastTransitionTime: start}},
	}
	_, err = Client.CoreV1().Pods(namespace).UpdateStatus(ctx, pod, metaV1.UpdateOptions{})
	assert.NoError(t, err)

	done := make(chan struct{})
	go func() {
		_ = k8sClient.MonitorPodCompletion(ctx, namespace, "node1")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatalf("MonitorPodCompletion did not return in expected time for pending pod")
	}
}

func TestPodStuckInCrashLoopBackOffState(t *testing.T) {
	ctx := context.TODO()
	namespace := "testing-ns"

	pod := &v1.Pod{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      "crash-pod",
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			NodeName:   "node1",
			Containers: []v1.Container{{Name: "pause", Image: "k8s.gcr.io/pause:3.1"}},
		},
	}

	pod, err := Client.CoreV1().Pods(namespace).Create(ctx, pod, metaV1.CreateOptions{})
	assert.NoError(t, err)
	// UpdateStatus to reflect CrashLoopBackOff state
	old := metaV1.NewTime(time.Now().Add(-2 * time.Minute))
	pod.Status = v1.PodStatus{
		Phase:      v1.PodRunning,
		Conditions: []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse, LastTransitionTime: old}},
		ContainerStatuses: []v1.ContainerStatus{{
			Name:         "pause",
			RestartCount: 5,
			State:        v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		}},
	}
	_, err = Client.CoreV1().Pods(namespace).UpdateStatus(ctx, pod, metaV1.UpdateOptions{})
	assert.NoError(t, err)

	done := make(chan struct{})
	go func() {
		_ = k8sClient.MonitorPodCompletion(ctx, namespace, "node1")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatalf("MonitorPodCompletion did not return in expected time for CrashLoopBackOff pod")
	}
}

func TestPodStuckInNotReadyState(t *testing.T) {
	ctx := context.TODO()
	namespace := "testing-ns"

	lastTransition := metaV1.NewTime(time.Now().Add(-3 * time.Minute))
	pod := &v1.Pod{
		ObjectMeta: metaV1.ObjectMeta{
			Name:      "notready-pod",
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			NodeName:   "node1",
			Containers: []v1.Container{{Name: "pause", Image: "k8s.gcr.io/pause:3.1"}},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			Conditions: []v1.PodCondition{{
				Type:               v1.PodReady,
				Status:             v1.ConditionFalse,
				LastTransitionTime: lastTransition,
			}},
		},
	}

	pod, err := Client.CoreV1().Pods(namespace).Create(ctx, pod, metaV1.CreateOptions{})
	assert.NoError(t, err)
	// Need to set status via UpdateStatus; the API ignores status on Create
	pod.Status = v1.PodStatus{
		Phase: v1.PodRunning,
		Conditions: []v1.PodCondition{{
			Type:               v1.PodReady,
			Status:             v1.ConditionFalse,
			LastTransitionTime: lastTransition,
		}},
	}
	_, err = Client.CoreV1().Pods(namespace).UpdateStatus(ctx, pod, metaV1.UpdateOptions{})
	assert.NoError(t, err)

	done := make(chan struct{})
	go func() {
		_ = k8sClient.MonitorPodCompletion(ctx, namespace, "node1")
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatalf("MonitorPodCompletion did not return in expected time for NotReady pod")
	}
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

func TestGetNamespacesMatchingPattern(t *testing.T) {
	ctx := context.Background()
	namespaces, err := k8sClient.GetNamespacesMatchingPattern(ctx, "runai*", ".*ai-prod$")
	assert.NoError(t, err, "Error in getting namespaces matching pattern")
	assert.Equal(t, []string{"runai", "runai-dev"}, namespaces, "Namespaces matching pattern mismatch")

	namespaces2, err := k8sClient.GetNamespacesMatchingPattern(ctx, "runai*", "")
	assert.NoError(t, err, "Error in getting namespaces matching pattern")
	assert.Equal(t, []string{"runai", "runai-dev", "runai-prod"}, namespaces2, "Namespaces matching pattern mismatch")

}

func markPodHealthy(ctx context.Context, namespace string, podNames []string) {
	for _, name := range podNames {
		pod, err := Client.CoreV1().Pods(namespace).Get(ctx, name, metaV1.GetOptions{})
		if err != nil {
			log.Fatalf("failed to get pod %s/%s: %v", namespace, name, err)
		}

		pod.Status.Phase = v1.PodRunning
		pod.Status.Conditions = []v1.PodCondition{
			{
				Type:               v1.PodReady,
				Status:             v1.ConditionTrue,
				LastProbeTime:      metaV1.Now(),
				LastTransitionTime: metaV1.Now(),
			},
		}
		pod.Status.ContainerStatuses = []v1.ContainerStatus{
			{
				Name:  "pause",
				Ready: true,
				State: v1.ContainerState{Running: &v1.ContainerStateRunning{}},
			},
		}

		_, err = Client.CoreV1().Pods(namespace).UpdateStatus(ctx, pod, metaV1.UpdateOptions{})
		if err != nil {
			log.Fatalf("failed to update pod status for %s/%s: %v", namespace, name, err)
		}
	}
}

func createNode(ctx context.Context, nodeName string, labels map[string]string) {
	node := &v1.Node{
		ObjectMeta: metaV1.ObjectMeta{
			Name:   nodeName,
			Labels: labels,
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{
				Type:   v1.NodeReady,
				Status: v1.ConditionTrue,
			}},
		},
	}
	_, err := Client.CoreV1().Nodes().Create(ctx, node, metaV1.CreateOptions{})
	if err != nil {
		log.Fatalf("Failed to create node %s: %v", nodeName, err)
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
