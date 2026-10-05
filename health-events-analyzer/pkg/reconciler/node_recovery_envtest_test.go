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
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	datamodels "github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/config"
	"github.com/nvidia/nvsentinel/health-events-analyzer/pkg/publisher"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

type recoveryTestDB struct {
	client.DatabaseClient
	mu            sync.Mutex
	events        []datamodels.HealthEventWithStatus
	failure       error
	decodeFailure error
	finds         atomic.Int32
}

func (d *recoveryTestDB) append(event *protos.HealthEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, datamodels.HealthEventWithStatus{CreatedAt: time.Now(),
		HealthEvent: proto.Clone(event).(*protos.HealthEvent), HealthEventStatus: &protos.HealthEventStatus{}})
}

func (d *recoveryTestDB) Find(_ context.Context, filter any, _ *client.FindOptions) (client.Cursor, error) {
	d.finds.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failure != nil {
		return nil, d.failure
	}
	cursor := &recoveryTestCursor{decodeFailure: d.decodeFailure}
	for _, event := range d.events {
		if matchesRecoveryFilter(event.HealthEvent, filter.(map[string]any)) {
			cursor.events = append(cursor.events, event)
		}
	}
	return cursor, nil
}

func matchesRecoveryFilter(event *protos.HealthEvent, filter map[string]any) bool {
	fields := map[string]any{"healthevent.agent": event.Agent, "healthevent.checkname": event.CheckName,
		fieldNodeName: event.NodeName, "healthevent.ishealthy": event.IsHealthy}
	for key, value := range filter {
		var got any
		if strings.HasPrefix(key, "healthevent.metadata.") {
			got = event.Metadata[strings.TrimPrefix(key, "healthevent.metadata.")]
		} else {
			got = fields[key]
		}
		if got != value {
			return false
		}
	}
	return true
}

type recoveryTestCursor struct {
	events        []datamodels.HealthEventWithStatus
	position      int
	decodeFailure error
}

func (c *recoveryTestCursor) Next(context.Context) bool {
	c.position++
	return c.position <= len(c.events)
}
func (c *recoveryTestCursor) Decode(out any) error {
	if c.decodeFailure != nil {
		return c.decodeFailure
	}
	*out.(*datamodels.HealthEventWithStatus) = c.events[c.position-1]
	return nil
}
func (c *recoveryTestCursor) Close(context.Context) error { return nil }
func (c *recoveryTestCursor) Err() error                  { return nil }
func (c *recoveryTestCursor) All(context.Context, any) error {
	return fmt.Errorf("unused cursor operation")
}

type recoveryTestSink struct {
	database *recoveryTestDB
	captured chan *protos.HealthEvent
	reject   bool
	calls    atomic.Int32
}

func (s *recoveryTestSink) HealthEventOccurredV1(_ context.Context, events *protos.HealthEvents,
	_ ...grpc.CallOption) (*emptypb.Empty, error) {
	s.calls.Add(1)
	if s.reject {
		return nil, status.Error(codes.InvalidArgument, "invalid test event")
	}
	for _, event := range events.Events {
		copied := proto.Clone(event).(*protos.HealthEvent)
		if s.database != nil {
			s.database.append(copied)
		}
		s.captured <- copied
	}
	return &emptypb.Empty{}, nil
}

func testRecoveryFault(node, gpu string) *protos.HealthEvent {
	return &protos.HealthEvent{Version: 1, Agent: agentName, CheckName: annotationRule().Name,
		NodeName: node, ComponentClass: "GPU", IsFatal: true, ErrorCode: []string{"94"},
		EntitiesImpacted:   []*protos.Entity{{EntityType: "GPU_UUID", EntityValue: gpu}},
		GeneratedTimestamp: timestamppb.New(time.Now().Add(-time.Millisecond)),
		ProcessingStrategy: protos.ProcessingStrategy_EXECUTE_REMEDIATION}
}

func newRecoveryReconciler(db *recoveryTestDB, sink *recoveryTestSink, opts ...healthpub.Option) *Reconciler {
	return &Reconciler{config: HealthEventsAnalyzerReconcilerConfig{
		HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{annotationRule()}},
		Publisher:                 publisher.NewPublisher(sink, protos.ProcessingStrategy_EXECUTE_REMEDIATION, opts...),
	}, databaseClient: db, recoveryPoll: time.Millisecond, recoveryRepublish: 100 * time.Millisecond}
}

func startRecoveryController(t *testing.T, kube kubernetes.Interface, r *Reconciler) func() {
	t.Helper()
	return startRecoveryControllerWithResync(t, kube, r, recoveryRequestResyncPeriod)
}

func startRecoveryControllerWithResync(t *testing.T, kube kubernetes.Interface, r *Reconciler,
	resyncPeriod time.Duration) func() {
	t.Helper()
	controller, err := newNodeRecoveryController(kube, r.config.HealthEventsAnalyzerRules, r.reconcileNodeRecovery, resyncPeriod)
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
	case <-time.After(10 * time.Second):
		t.Fatal("node cache did not sync")
	}
	return stop
}

func createRecoveryNode(t *testing.T, kube kubernetes.Interface, name string) *corev1.Node {
	t.Helper()
	node, err := kube.CoreV1().Nodes().Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	require.NoError(t, err)
	return node
}

func annotateRecoveryNode(t *testing.T, kube kubernetes.Interface, node *corev1.Node, request string) {
	t.Helper()
	current, err := kube.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	current.Annotations = map[string]string{annotationRule().Recovery.AnnotationKey: request}
	_, err = kube.CoreV1().Nodes().Update(t.Context(), current, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func requireRecoveryEvent(t *testing.T, kube kubernetes.Interface, node *corev1.Node, reason string) {
	t.Helper()
	require.Eventually(t, func() bool {
		events, err := kube.CoreV1().Events("default").List(t.Context(), metav1.ListOptions{})
		if err != nil {
			return false
		}
		for _, event := range events.Items {
			if event.InvolvedObject.UID == node.UID && event.Reason == reason {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
}

func TestAnnotationRecovery_RealAPIServer_RetainsRequestsAndReportsResults(t *testing.T) {
	server := &envtest.Environment{}
	restConfig, err := server.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Stop()) })
	kube, err := kubernetes.NewForConfig(restConfig)
	require.NoError(t, err)

	t.Run("storage confirmation, restart and stale request", func(t *testing.T) {
		node := createRecoveryNode(t, kube, "recovery-storage")
		db := &recoveryTestDB{}
		fault := testRecoveryFault(node.Name, "GPU-a")
		fault.QuarantineOverrides = &protos.BehaviourOverrides{Force: true}
		fault.CustomRecommendedAction = "test-action"
		db.append(fault)
		db.append(testRecoveryFault(node.Name, "GPU-b"))
		sink := &recoveryTestSink{captured: make(chan *protos.HealthEvent, 20)}
		r := newRecoveryReconciler(db, sink)
		stop := startRecoveryController(t, kube, r)
		verified := time.Now().UTC()
		request := fmt.Sprintf(`{"recoveredAt":%q,"entities":[{"entityType":"GPU_UUID","entityValue":"GPU-a"}]}`, verified.Format(time.RFC3339Nano))
		annotateRecoveryNode(t, kube, node, request)
		var clear *protos.HealthEvent
		select {
		case clear = <-sink.captured:
		case <-time.After(5 * time.Second):
			t.Fatal("clear not published")
		}
		require.True(t, clear.IsHealthy)
		require.False(t, clear.IsFatal)
		require.EqualValues(t, 1, clear.Version)
		require.Equal(t, "GPU", clear.ComponentClass)
		require.Empty(t, clear.ErrorCode)
		require.Nil(t, clear.QuarantineOverrides)
		require.Empty(t, clear.CustomRecommendedAction)
		require.Equal(t, protos.RecommendedAction_NONE, clear.RecommendedAction)
		require.Equal(t, protos.ProcessingStrategy_EXECUTE_REMEDIATION, clear.ProcessingStrategy)
		require.Equal(t, "GPU-a", clear.EntitiesImpacted[0].EntityValue)
		require.Never(t, func() bool {
			events, err := kube.CoreV1().Events("default").List(t.Context(), metav1.ListOptions{})
			require.NoError(t, err)
			for _, event := range events.Items {
				if event.InvolvedObject.UID == node.UID && event.Reason == "RecoveryCompleted" {
					return true
				}
			}
			return false
		}, 30*time.Millisecond, 5*time.Millisecond, "queue acceptance must not report stored recovery")
		db.append(clear)
		requireRecoveryEvent(t, kube, node, "RecoveryCompleted")
		current, err := kube.CoreV1().Nodes().Get(t.Context(), node.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, request, current.Annotations[annotationRule().Recovery.AnnotationKey])
		stop()
		nextSink := &recoveryTestSink{database: db, captured: make(chan *protos.HealthEvent, 20)}
		restarted := newRecoveryReconciler(db, nextSink)
		startRecoveryController(t, kube, restarted)
		identity, ok := recoveryIdentityForEvent(annotationRule(), clear)
		require.True(t, ok)
		boundary, err := restarted.latestRecoveryTime(t.Context(), annotationRule(), identity)
		require.NoError(t, err)
		require.True(t, boundary.Equal(verified))
		require.NoError(t, restarted.reconcileNodeRecovery(t.Context(), node.Name))
		require.Zero(t, nextSink.calls.Load(), "startup replay must not publish another clear")
		db.append(testRecoveryFault(node.Name, "GPU-a"))
		require.NoError(t, restarted.reconcileNodeRecovery(t.Context(), node.Name))
		require.Zero(t, nextSink.calls.Load(), "old verification cannot clear a new fault")
		states, err := restarted.derivedStatesForNode(t.Context(), annotationRule(), node.Name)
		require.NoError(t, err)
		for _, state := range states {
			require.False(t, state.HealthEvent.IsHealthy)
		}
		// Recreating the Kubernetes node must invalidate the old UID's boundary.
		require.NoError(t, kube.CoreV1().Nodes().Delete(t.Context(), node.Name, metav1.DeleteOptions{}))
		recreated := createRecoveryNode(t, kube, node.Name)
		require.NotEqual(t, node.UID, recreated.UID)
		require.Eventually(t, func() bool {
			n, err := restarted.nodeRecovery.nodes.Get(node.Name)
			return err == nil && n.UID == recreated.UID
		}, time.Second, 10*time.Millisecond)
		boundary, err = restarted.latestRecoveryTime(t.Context(), annotationRule(), identity)
		require.NoError(t, err)
		require.True(t, boundary.IsZero())
	})

	t.Run("node-wide request retries store failure", func(t *testing.T) {
		node := createRecoveryNode(t, kube, "recovery-retry")
		db := &recoveryTestDB{failure: fmt.Errorf("temporary store failure")}
		db.append(testRecoveryFault(node.Name, "GPU-a"))
		db.append(testRecoveryFault(node.Name, "GPU-b"))
		request := time.Now().UTC().Format(time.RFC3339Nano)
		annotateRecoveryNode(t, kube, node, request)
		sink := &recoveryTestSink{database: db, captured: make(chan *protos.HealthEvent, 20)}
		startRecoveryController(t, kube, newRecoveryReconciler(db, sink))
		requireRecoveryEvent(t, kube, node, "RecoveryFailed")
		require.Zero(t, sink.calls.Load())
		db.mu.Lock()
		db.failure = nil
		db.mu.Unlock()
		requireRecoveryEvent(t, kube, node, "RecoveryCompleted")
		require.EqualValues(t, 2, sink.calls.Load())
	})

	t.Run("recovery preserves downstream class and version identities", func(t *testing.T) {
		node := createRecoveryNode(t, kube, "recovery-variants")
		db := &recoveryTestDB{}
		fault := testRecoveryFault(node.Name, "GPU-a")
		db.append(fault)
		fault.Version = 2
		db.append(fault)
		fault.ComponentClass = "accelerator"
		db.append(fault)
		annotateRecoveryNode(t, kube, node, time.Now().UTC().Format(time.RFC3339Nano))
		sink := &recoveryTestSink{database: db, captured: make(chan *protos.HealthEvent, 10)}
		r := newRecoveryReconciler(db, sink)
		startRecoveryController(t, kube, r)
		requireRecoveryEvent(t, kube, node, "RecoveryCompleted")
		require.EqualValues(t, 3, sink.calls.Load())
		states, err := r.derivedStatesForNode(t.Context(), annotationRule(), node.Name)
		require.NoError(t, err)
		require.Len(t, states, 3)
		for _, state := range states {
			require.True(t, state.HealthEvent.IsHealthy)
		}
	})

	t.Run("malformed stored data fails closed", func(t *testing.T) {
		node := createRecoveryNode(t, kube, "recovery-malformed")
		db := &recoveryTestDB{decodeFailure: fmt.Errorf("malformed stored record")}
		db.append(testRecoveryFault(node.Name, "GPU-a"))
		annotateRecoveryNode(t, kube, node, time.Now().UTC().Format(time.RFC3339Nano))
		sink := &recoveryTestSink{captured: make(chan *protos.HealthEvent, 1)}
		startRecoveryController(t, kube, newRecoveryReconciler(db, sink))
		requireRecoveryEvent(t, kube, node, "RecoveryFailed")
		require.Zero(t, sink.calls.Load())
	})

	t.Run("invalid timestamp does not recover", func(t *testing.T) {
		node := createRecoveryNode(t, kube, "recovery-invalid")
		annotateRecoveryNode(t, kube, node, time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))
		sink := &recoveryTestSink{captured: make(chan *protos.HealthEvent, 1)}
		startRecoveryController(t, kube, newRecoveryReconciler(&recoveryTestDB{}, sink))
		requireRecoveryEvent(t, kube, node, "RecoveryInvalid")
		require.Zero(t, sink.calls.Load())
	})

	t.Run("permanent rejection is attempted only once", func(t *testing.T) {
		node := createRecoveryNode(t, kube, "recovery-rejected")
		db := &recoveryTestDB{}
		db.append(testRecoveryFault(node.Name, "GPU-a"))
		annotateRecoveryNode(t, kube, node, time.Now().UTC().Format(time.RFC3339Nano))
		sink := &recoveryTestSink{reject: true}

		restricted, grantEvents := recoveryReadOnlyClient(t, kube, restConfig)
		r := newRecoveryReconciler(db, sink)
		startRecoveryController(t, restricted, r)
		require.Eventually(t, func() bool { return sink.calls.Load() == 1 }, time.Second, time.Millisecond)
		// Event creation is forbidden. Retrying that report must never resubmit
		// the permanently rejected health event.
		require.ErrorContains(t, r.reconcileNodeRecovery(t.Context(), node.Name), "forbidden")
		require.EqualValues(t, 1, sink.calls.Load())
		grantEvents()
		require.Eventually(t, func() bool {
			return r.reconcileNodeRecovery(t.Context(), node.Name) == nil
		}, 5*time.Second, 10*time.Millisecond)
		requireRecoveryEvent(t, kube, node, "RecoveryFailed")
		require.EqualValues(t, 1, sink.calls.Load())
		// Event permissions must not grant Node writes.
		_, err := restricted.CoreV1().Nodes().Update(t.Context(), node, metav1.UpdateOptions{})
		require.ErrorContains(t, err, "forbidden")
	})
}

func TestPublishRecovery_DirectMode_DoesNotPollStorage(t *testing.T) {
	t.Setenv("HEALTH_PUBLISH_TARGET", "127.0.0.1:1")
	t.Setenv("HEALTH_PUBLISH_TOKEN_PATH", "/unused-test-token")
	t.Setenv("HEALTH_PUBLISH_INSECURE", "true")
	_, _, option, err := healthpub.DialFromEnvOr(nil)
	require.NoError(t, err)
	// The real direct-mode publisher uses this in-memory transport. Its successful
	// response models the deployment connector's durable acknowledgment.
	db := &recoveryTestDB{}
	sink := &recoveryTestSink{captured: make(chan *protos.HealthEvent, 1)}
	r := newRecoveryReconciler(db, sink, option)
	t.Cleanup(r.config.Publisher.Close)
	fault := testRecoveryFault("node-direct", "GPU-a")
	identity, ok := recoveryIdentityForEvent(annotationRule(), fault)
	require.True(t, ok)
	counter := recoveryEventsPublishedTotal.WithLabelValues(annotationRule().Name)
	before := counterValue(t, counter)
	require.NoError(t, r.publishRecoveryUntilStored(t.Context(), fault, annotationRule(), identity))
	require.Zero(t, db.finds.Load())
	require.EqualValues(t, 1, sink.calls.Load())
	require.Equal(t, before+1, counterValue(t, counter))
}

func TestEventProcessor_AnnotationRecovery_KeepsCheckpointAndContinue(t *testing.T) {
	cfg := newEventProcessorConfig(HealthEventsAnalyzerReconcilerConfig{
		HealthEventsAnalyzerRules: &config.TomlConfig{Rules: []config.HealthEventsAnalyzerRule{annotationRule()}}, Workers: 2,
	})
	require.True(t, cfg.MarkProcessedOnError)
}

func recoveryReadOnlyClient(t *testing.T, admin kubernetes.Interface, cfg *rest.Config) (kubernetes.Interface, func()) {
	t.Helper()
	name := "recovery-node-reader"
	_, err := admin.RbacV1().ClusterRoles().Create(t.Context(), &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch"}}},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	subjects := []rbacv1.Subject{{Kind: "User", Name: name, APIGroup: rbacv1.GroupName}}
	_, err = admin.RbacV1().ClusterRoleBindings().Create(t.Context(), &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name}, Subjects: subjects,
		RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: name, APIGroup: rbacv1.GroupName},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	impersonated := rest.CopyConfig(cfg)
	impersonated.Impersonate.UserName = name
	restricted, err := kubernetes.NewForConfig(impersonated)
	require.NoError(t, err)
	return restricted, func() {
		_, err := admin.RbacV1().Roles("default").Create(t.Context(), &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create"}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		_, err = admin.RbacV1().RoleBindings("default").Create(t.Context(), &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name}, Subjects: subjects,
			RoleRef: rbacv1.RoleRef{Kind: "Role", Name: name, APIGroup: rbacv1.GroupName},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
}
