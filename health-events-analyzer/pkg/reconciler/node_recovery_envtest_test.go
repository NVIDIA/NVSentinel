// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

type annotationTestDatabase struct {
	client.DatabaseClient
	mu          sync.Mutex
	events      []datamodels.HealthEventWithStatus
	unavailable bool
	readError   error
}

func (d *annotationTestDatabase) Find(context.Context, any, *client.FindOptions) (client.Cursor, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.readError != nil {
		return nil, d.readError
	}
	if d.unavailable {
		return nil, fmt.Errorf("store unavailable")
	}
	return newHealthEventCursor(append([]datamodels.HealthEventWithStatus(nil), d.events...)...), nil
}

func (d *annotationTestDatabase) append(event datamodels.HealthEventWithStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, event)
}

func (d *annotationTestDatabase) setUnavailable(value bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unavailable = value
}

type annotationTestSink struct {
	database *annotationTestDatabase
	captured chan *protos.HealthEvent
}

func (s *annotationTestSink) HealthEventOccurredV1(_ context.Context, events *protos.HealthEvents,
	_ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	for _, event := range events.Events {
		event = proto.Clone(event).(*protos.HealthEvent)
		s.captured <- event
		if s.database != nil {
			s.database.append(storedEvent(time.Now().UTC(), event))
		}
	}
	return &emptypb.Empty{}, nil
}

func newAnnotationTestReconciler(database *annotationTestDatabase, sink *annotationTestSink) *Reconciler {
	rule := annotationRule()
	return &Reconciler{
		config: HealthEventsAnalyzerReconcilerConfig{
			HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{rule}},
			Publisher:                 publisher.NewPublisher(sink, protos.ProcessingStrategy_EXECUTE_REMEDIATION),
		},
		databaseClient: database, recoveryPoll: time.Millisecond, recoveryRepublish: time.Second,
	}
}

func startAnnotationTestController(t *testing.T, kube kubernetes.Interface, r *Reconciler) func() {
	t.Helper()
	controller, err := newNodeRecoveryController(kube, r.config.HealthEventsAnalyzerRules, r.reconcileNodeRecovery)
	require.NoError(t, err)
	r.nodeRecovery = controller
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- controller.run(ctx, 2) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); require.NoError(t, <-done) }) }
	t.Cleanup(stop)
	select {
	case <-controller.ready:
	case err := <-done:
		t.Fatalf("controller stopped: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("node cache did not synchronize")
	}
	return stop
}

func setRecoveryAnnotation(t *testing.T, kube kubernetes.Interface, name, value string) *corev1.Node {
	t.Helper()
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{annotationRule().Recovery.AnnotationKey: value}}})
	require.NoError(t, err)
	node, err := kube.CoreV1().Nodes().Patch(t.Context(), name, types.MergePatchType, patch, metav1.PatchOptions{})
	require.NoError(t, err)
	return node
}

func requireAnnotationRemoved(t *testing.T, kube kubernetes.Interface, name string) {
	t.Helper()
	require.Eventually(t, func() bool {
		node, err := kube.CoreV1().Nodes().Get(t.Context(), name, metav1.GetOptions{})
		return err == nil && node.Annotations[annotationRule().Recovery.AnnotationKey] == ""
	}, 5*time.Second, 10*time.Millisecond)
}

func activeAnnotationFault(node, gpu string, at time.Time) datamodels.HealthEventWithStatus {
	event := derivedEvent(at, false, gpu)
	event.HealthEvent.NodeName = node
	event.HealthEvent.ComponentClass = "GPU"
	event.HealthEvent.Version = 1
	return event
}

func TestNodeRecovery_RealAPIServer(t *testing.T) {
	server := &envtest.Environment{}
	restConfig, err := server.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Stop()) })
	kube, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)

	t.Run("storage confirmation, identity, restart, and stale replay", func(t *testing.T) {
		node, err := kube.CoreV1().Nodes().Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "recovery-storage"}}, metav1.CreateOptions{})
		require.NoError(t, err)
		database := &annotationTestDatabase{}
		database.append(activeAnnotationFault(node.Name, "GPU-a", time.Now().UTC().Add(-time.Millisecond)))
		database.append(activeAnnotationFault(node.Name, "GPU-b", time.Now().UTC().Add(-time.Millisecond)))
		sink := &annotationTestSink{captured: make(chan *protos.HealthEvent, 16)}
		reconciler := newAnnotationTestReconciler(database, sink)
		stop := startAnnotationTestController(t, kube, reconciler)
		recoveredAt := time.Now().UTC()
		request := fmt.Sprintf(`{"recoveredAt":%q,"entities":[{"entityType":"GPU_UUID","entityValue":"GPU-a"}]}`, recoveredAt.Format(time.RFC3339Nano))
		setRecoveryAnnotation(t, kube, node.Name, request)
		var published *protos.HealthEvent
		select {
		case published = <-sink.captured:
		case <-time.After(5 * time.Second):
			t.Fatal("no derived recovery published")
		}
		require.True(t, published.IsHealthy)
		require.False(t, published.IsFatal)
		require.Equal(t, "health-events-analyzer", published.Agent)
		require.Equal(t, annotationRule().Name, published.CheckName)
		require.Equal(t, "GPU", published.ComponentClass)
		require.EqualValues(t, 1, published.Version)
		require.Equal(t, protos.RecommendedAction_NONE, published.RecommendedAction)
		require.Equal(t, protos.ProcessingStrategy_EXECUTE_REMEDIATION, published.ProcessingStrategy)
		require.Equal(t, []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: "GPU-a"}}, published.EntitiesImpacted)
		require.NotEmpty(t, published.Metadata[annotationRequestKey])
		current, err := kube.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, request, current.Annotations[annotationRule().Recovery.AnnotationKey], "RPC acceptance is not storage confirmation")
		database.append(storedEvent(time.Now().UTC(), published))
		requireAnnotationRemoved(t, kube, node.Name)
		stop()

		restartedSink := &annotationTestSink{database: database, captured: make(chan *protos.HealthEvent, 16)}
		restarted := newAnnotationTestReconciler(database, restartedSink)
		startAnnotationTestController(t, kube, restarted)
		boundary, err := restarted.recoveryBoundaryForEvent(t.Context(), annotationRule(), published)
		require.NoError(t, err)
		require.NotNil(t, boundary)
		require.True(t, recoveredAt.Equal(boundary.createdAt))
		require.True(t, recoveredAt.Equal(boundary.generated.AsTime()))
		other := proto.Clone(published).(*protos.HealthEvent)
		other.EntitiesImpacted[0].EntityValue = "GPU-b"
		otherIdentity, ok := recoveryIdentityForEvent(annotationRule(), other)
		require.True(t, ok)
		state, found, err := restarted.currentDerivedState(t.Context(), annotationRule(), otherIdentity)
		require.NoError(t, err)
		require.True(t, found)
		require.False(t, state.isHealthy, "another GPU must remain faulty")
		database.append(activeAnnotationFault(node.Name, "GPU-a", time.Now().UTC()))
		setRecoveryAnnotation(t, kube, node.Name, request)
		requireAnnotationRemoved(t, kube, node.Name)
		require.Empty(t, restartedSink.captured, "the old request must not clear the newer fault")
	})

	t.Run("startup request retries after store outage", func(t *testing.T) {
		node, err := kube.CoreV1().Nodes().Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "recovery-retry"}}, metav1.CreateOptions{})
		require.NoError(t, err)
		database := &annotationTestDatabase{}
		database.append(activeAnnotationFault(node.Name, "GPU-a", time.Now().UTC().Add(-time.Millisecond)))
		database.append(activeAnnotationFault(node.Name, "GPU-b", time.Now().UTC().Add(-time.Millisecond)))
		database.setUnavailable(true)
		sink := &annotationTestSink{database: database, captured: make(chan *protos.HealthEvent, 16)}
		request := time.Now().UTC().Format(time.RFC3339Nano)
		setRecoveryAnnotation(t, kube, node.Name, request)
		reconciler := newAnnotationTestReconciler(database, sink)
		startAnnotationTestController(t, kube, reconciler)
		require.ErrorContains(t, reconciler.reconcileNodeRecovery(t.Context(), node.Name), "store unavailable")
		current, err := kube.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, request, current.Annotations[annotationRule().Recovery.AnnotationKey])
		require.Empty(t, sink.captured)
		database.setUnavailable(false)
		requireAnnotationRemoved(t, kube, node.Name)
		require.Len(t, sink.captured, 2, "node-wide recovery must clear both active GPU identities")
		first, second := <-sink.captured, <-sink.captured
		require.NotEqual(t, first.EntitiesImpacted[0].EntityValue, second.EntitiesImpacted[0].EntityValue)
	})

	t.Run("invalid annotation remains visible", func(t *testing.T) {
		node, err := kube.CoreV1().Nodes().Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "recovery-invalid"}}, metav1.CreateOptions{})
		require.NoError(t, err)
		request := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		setRecoveryAnnotation(t, kube, node.Name, request)
		sink := &annotationTestSink{captured: make(chan *protos.HealthEvent, 1)}
		reconciler := newAnnotationTestReconciler(&annotationTestDatabase{}, sink)
		startAnnotationTestController(t, kube, reconciler)
		require.NoError(t, reconciler.reconcileNodeRecovery(t.Context(), node.Name))
		current, err := kube.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, request, current.Annotations[annotationRule().Recovery.AnnotationKey])
		require.Empty(t, sink.captured)
	})

	t.Run("permanent stored-state failure retains request without blocking source processing", func(t *testing.T) {
		node, err := kube.CoreV1().Nodes().Create(t.Context(), &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "recovery-permanent"},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		request := time.Now().UTC().Format(time.RFC3339Nano)
		setRecoveryAnnotation(t, kube, node.Name, request)
		database := &annotationTestDatabase{readError: client.PermanentError(fmt.Errorf("malformed stored event"))}
		sink := &annotationTestSink{captured: make(chan *protos.HealthEvent, 1)}
		reconciler := newAnnotationTestReconciler(database, sink)
		startAnnotationTestController(t, kube, reconciler)
		require.NoError(t, reconciler.reconcileNodeRecovery(t.Context(), node.Name))
		current, err := kube.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, request, current.Annotations[annotationRule().Recovery.AnnotationKey])
		require.Empty(t, sink.captured)
	})

	t.Run("cleanup preserves replacement requests and node identities", func(t *testing.T) {
		node, err := kube.CoreV1().Nodes().Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "recovery-replacement"}}, metav1.CreateOptions{})
		require.NoError(t, err)
		original := setRecoveryAnnotation(t, kube, node.Name, "original")
		setRecoveryAnnotation(t, kube, node.Name, "replacement")
		controller, err := newNodeRecoveryController(kube, &config.TomlConfig{}, nil)
		require.NoError(t, err)
		t.Cleanup(controller.queue.ShutDown)
		require.NoError(t, controller.removeRequest(t.Context(), original, annotationRule().Recovery.AnnotationKey, "original"))
		current, err := kube.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "replacement", current.Annotations[annotationRule().Recovery.AnnotationKey])
		require.NoError(t, kube.CoreV1().Nodes().Delete(t.Context(), node.Name, metav1.DeleteOptions{}))
		require.Eventually(t, func() bool {
			_, err := kube.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, time.Second, 10*time.Millisecond)
		_, err = kube.CoreV1().Nodes().Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Annotations: map[string]string{annotationRule().Recovery.AnnotationKey: "original"}}}, metav1.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, controller.removeRequest(t.Context(), original, annotationRule().Recovery.AnnotationKey, "original"))
		current, err = kube.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.NotEqual(t, original.UID, current.UID)
		require.Equal(t, "original", current.Annotations[annotationRule().Recovery.AnnotationKey])
	})
}
