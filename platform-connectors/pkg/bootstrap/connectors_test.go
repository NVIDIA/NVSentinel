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

package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

// stubKubeconfig points at nothing; building a clientset from it never
// contacts an API server, which is all a connector's construction does.
func stubKubeconfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster:
      server: https://127.0.0.1:1
contexts:
  - name: test
    context:
      cluster: test
      user: test
current-context: test
users:
  - name: test
    user: {}
`), 0o600))

	return path
}

var k8sSettings = kubernetes.Settings{
	K8sConnectorConfig: kubernetes.K8sConnectorConfig{MaxNodeConditionMessageLength: 1024, CompactedHealthEventMsgLen: 256},
	QPS:                5,
	Burst:              10,
}

// TestBuildConnectors_QueuedAndDirect: the same enable flags build the same
// connectors in both modes. Queued, every member is a ring buffer and its
// connector drains it; direct, the connectors themselves are the members.
// Shutdown drains the queues and returns.
func TestBuildConnectors_QueuedAndDirect(t *testing.T) {
	ctx := context.Background()
	raw := map[string]any{
		"enableK8sPlatformConnector":  "true",
		"enablePromPlatformConnector": "true",
		"enableGRPCSinkConnector":     "false",
	}

	queued, err := BuildConnectors(ctx, raw, k8sSettings, Options{KubeconfigPath: stubKubeconfig(t)}, ConnectorOptions{Queued: true})
	require.NoError(t, err)
	require.Len(t, queued.Set, 2)
	require.Nil(t, queued.Store, "no store enabled")

	for _, member := range queued.Set {
		_, isQueue := member.(*ringbuffer.RingBuffer)
		require.True(t, isQueue, "queued members are ring buffers")
	}

	done := make(chan struct{})

	go func() {
		queued.Shutdown(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown of the queued connectors did not return")
	}

	direct, err := BuildConnectors(ctx, raw, k8sSettings,
		Options{KubeconfigPath: stubKubeconfig(t)}, ConnectorOptions{BestEffortTimeout: time.Second})
	require.NoError(t, err)
	require.Len(t, direct.Set, 2)

	for _, member := range direct.Set {
		_, isQueue := member.(*ringbuffer.RingBuffer)
		require.False(t, isQueue, "direct members are the connectors themselves")
	}

	direct.Shutdown(ctx)
}

// TestBuildConnectors_SinkSettings: the sink needs a target; the retry count
// config.json carries is read in both modes, as the chart always writes it.
func TestBuildConnectors_SinkSettings(t *testing.T) {
	ctx := context.Background()

	_, err := BuildConnectors(ctx, map[string]any{"enableGRPCSinkConnector": "true", "GRPCSinkTarget": ""},
		k8sSettings, Options{}, ConnectorOptions{})
	require.ErrorContains(t, err, "GRPCSinkTarget")

	_, err = BuildConnectors(ctx, map[string]any{"enableGRPCSinkConnector": "true", "GRPCSinkTarget": "sink.example:9000"},
		k8sSettings, Options{}, ConnectorOptions{})
	require.ErrorContains(t, err, "GRPCSinkConnectorMaxRetries")

	direct, err := BuildConnectors(ctx, map[string]any{
		"enableGRPCSinkConnector": "true", "GRPCSinkTarget": "sink.example:9000", "GRPCSinkConnectorMaxRetries": json.Number("3"),
	}, k8sSettings, Options{}, ConnectorOptions{BestEffortTimeout: time.Second})
	require.NoError(t, err)
	require.Len(t, direct.Set, 1)

	direct.Shutdown(ctx)
}

// failing is a connector whose every batch fails.
type failing struct{}

func (failing) ProcessBatch(context.Context, *pb.HealthEvents) error { return errors.New("boom") }
func (failing) FetchAndProcessHealthMetric(context.Context)          {}

// TestAdd_QueuedDirectAndBestEffort pins how a member enters the set: as its
// ring buffer when queued, as itself when it must decide the reply, and
// wrapped in BestEffort when its failure must not fail the batch.
func TestAdd_QueuedDirectAndBestEffort(t *testing.T) {
	ctx := context.Background()
	batch := &pb.HealthEvents{}

	queued := &Connectors{mode: ConnectorOptions{Queued: true}}
	queued.add(ctx, queued.queue(ctx, "test"), "test", failing{}, true)
	_, isQueue := queued.Set[0].(*ringbuffer.RingBuffer)
	require.True(t, isQueue)
	queued.Shutdown(ctx)

	direct := &Connectors{mode: ConnectorOptions{BestEffortTimeout: time.Second}}
	direct.add(ctx, nil, "bare", failing{}, false)
	direct.add(ctx, nil, "best-effort", failing{}, true)
	require.EqualError(t, direct.Set[0].ProcessBatch(ctx, batch), "boom", "a member that decides the reply fails the batch")
	require.NoError(t, direct.Set[1].ProcessBatch(ctx, batch), "a best-effort member never fails the batch")
}

// gated is a queued connector that takes a batch only when the test lets it,
// and holds none while it waits, the moment between two batches.
type gated struct {
	queue     *ringbuffer.RingBuffer
	next      chan struct{}
	processed atomic.Int32
}

func (*gated) ProcessBatch(context.Context, *pb.HealthEvents) error { return nil }

func (g *gated) FetchAndProcessHealthMetric(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.next:
		}

		item, quit := g.queue.Dequeue()
		if quit {
			return
		}

		g.processed.Add(1)
		g.queue.HealthMetricEleProcessingCompleted(item)
	}
}

// stuck is a queued connector whose write does not return until its context
// is canceled, like a write to a datastore that stays down.
type stuck struct {
	queue    *ringbuffer.RingBuffer
	holding  chan struct{}
	canceled atomic.Bool
}

func (*stuck) ProcessBatch(context.Context, *pb.HealthEvents) error { return nil }

func (s *stuck) FetchAndProcessHealthMetric(ctx context.Context) {
	item, quit := s.queue.Dequeue()
	if quit {
		return
	}

	close(s.holding)
	<-ctx.Done()
	s.canceled.Store(true)
	s.queue.HealthMetricEleProcessingCompleted(item)
}

// hung is a queued connector whose loop keeps its batch until the test ends
// and watches neither its context nor stopCh, like a loop held by a datastore
// call that does not end with its context.
type hung struct {
	queue   *ringbuffer.RingBuffer
	holding chan struct{}
	release chan struct{}
}

func (*hung) ProcessBatch(context.Context, *pb.HealthEvents) error { return nil }

func (h *hung) FetchAndProcessHealthMetric(context.Context) {
	item, quit := h.queue.Dequeue()
	if quit {
		return
	}

	close(h.holding)
	<-h.release
	h.queue.HealthMetricEleProcessingCompleted(item)
}

// held is a queued connector whose loop keeps its batch until stopCh closes,
// the way stopCh ends the retries of the batch the Kubernetes connector's loop
// is on. It does not watch its context, so only stopCh can end it.
type held struct {
	queue   *ringbuffer.RingBuffer
	stopCh  <-chan struct{}
	holding chan struct{}
}

func (*held) ProcessBatch(context.Context, *pb.HealthEvents) error { return nil }

func (h *held) FetchAndProcessHealthMetric(context.Context) {
	item, quit := h.queue.Dequeue()
	if quit {
		return
	}

	close(h.holding)
	<-h.stopCh
	h.queue.HealthMetricEleProcessingCompleted(item)
}

// syncBuffer is a log sink a test can read while a loop may still write to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// captureLogs sends the default logger to a buffer until the test ends.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()

	logs := &syncBuffer{}
	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return logs
}

// shutdownInBackground runs Shutdown; the channel is closed when it returns.
func shutdownInBackground(ctx context.Context, c *Connectors) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		c.Shutdown(ctx)
		close(done)
	}()

	return done
}

// TestShutdown_QueuedBatches_ProcessedBeforeShutdownReturns: a batch is
// acknowledged once it is queued, so Shutdown does not return until every loop
// has processed what its queue held, also when the loop is between two batches
// as Shutdown starts.
func TestShutdown_QueuedBatches_ProcessedBeforeShutdownReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := &Connectors{mode: ConnectorOptions{Queued: true}}
	queue := c.queue(ctx, "gated")
	loop := &gated{queue: queue, next: make(chan struct{})}
	c.add(ctx, queue, "gated", loop, false)

	const batches = 3
	for range batches {
		require.NoError(t, queue.ProcessBatch(ctx, &pb.HealthEvents{Events: []*pb.HealthEvent{{NodeName: "node-1"}}}))
	}

	shutdown := shutdownInBackground(ctx, c)

	for range batches {
		require.Never(t, func() bool { return isClosed(shutdown) }, 50*time.Millisecond, 5*time.Millisecond,
			"Shutdown returned with acknowledged batches still queued")

		loop.next <- struct{}{}
	}

	// One more turn finds the queue empty and shut down, and the loop returns.
	loop.next <- struct{}{}

	require.Eventually(t, func() bool { return isClosed(shutdown) }, 5*time.Second, 5*time.Millisecond)
	require.EqualValues(t, batches, loop.processed.Load())
}

// TestShutdown_LoopStillBusyAtTheDrainTimeout_IsStopped: Shutdown waits for the
// loops no longer than the drain timeout. A loop still busy then, behind a
// datastore that stays down for example, is stopped through its context, so
// shutdown is not held past the pod's grace period, and the loop is named in a
// Warn line.
func TestShutdown_LoopStillBusyAtTheDrainTimeout_IsStopped(t *testing.T) {
	logs := &syncBuffer{}
	previous := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := &Connectors{mode: ConnectorOptions{Queued: true}, drainTimeout: 100 * time.Millisecond}
	queue := c.queue(ctx, "stuck")
	loop := &stuck{queue: queue, holding: make(chan struct{})}
	c.add(ctx, queue, "stuck", loop, false)

	require.NoError(t, queue.ProcessBatch(ctx, &pb.HealthEvents{Events: []*pb.HealthEvent{{NodeName: "node-1"}}}))
	require.Eventually(t, func() bool { return isClosed(loop.holding) }, 5*time.Second, 5*time.Millisecond)

	shutdown := shutdownInBackground(ctx, c)

	require.Eventually(t, func() bool { return isClosed(shutdown) }, 5*time.Second, 5*time.Millisecond,
		"Shutdown kept waiting for a loop past the drain timeout")
	require.True(t, loop.canceled.Load(), "the loop still busy at the timeout was stopped through its context")
	require.Contains(t, logs.String(),
		`level=WARN msg="Connector queues not drained before shutdown, stopping their loops" timeout=100ms loops=[stuck]`)
	require.NotContains(t, logs.String(), "still running after it was stopped",
		"a loop that returned once it was stopped is not reported as left behind")
}

// TestShutdown_LoopIgnoringItsStop_DoesNotHoldShutdown: a loop that does not
// return once it is stopped, behind a PostgreSQL insert whose host stopped
// answering for example, holds Shutdown no longer than the stop timeout after
// the drain timeout, so the connections still close before the pod is killed.
// The batches it is left with are logged at Error, since it cannot log them; a
// loop that did return once it was stopped is not reported.
func TestShutdown_LoopIgnoringItsStop_DoesNotHoldShutdown(t *testing.T) {
	logs := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := &Connectors{
		mode: ConnectorOptions{Queued: true}, stopCh: make(chan struct{}),
		drainTimeout: 100 * time.Millisecond, stopTimeout: 100 * time.Millisecond,
	}
	stoppedQueue := c.queue(ctx, "stuck")
	stopped := &stuck{queue: stoppedQueue, holding: make(chan struct{})}
	c.add(ctx, stoppedQueue, "stuck", stopped, false)

	queue := c.queue(ctx, "hung")
	loop := &hung{queue: queue, holding: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(loop.release) })
	c.add(ctx, queue, "hung", loop, false)

	require.NoError(t, stoppedQueue.ProcessBatch(ctx, &pb.HealthEvents{Events: []*pb.HealthEvent{{NodeName: "node-1"}}}))

	for range 3 {
		require.NoError(t, queue.ProcessBatch(ctx, &pb.HealthEvents{Events: []*pb.HealthEvent{{NodeName: "node-1"}}}))
	}

	require.Eventually(t, func() bool { return isClosed(stopped.holding) && isClosed(loop.holding) },
		5*time.Second, 5*time.Millisecond)

	shutdown := shutdownInBackground(ctx, c)

	require.Eventually(t, func() bool { return isClosed(shutdown) }, 5*time.Second, 5*time.Millisecond,
		"Shutdown kept waiting for a loop that did not return once it was stopped")
	require.True(t, c.loops[0].done.Load(), "the loop that returned once it was stopped was waited for")
	require.False(t, c.loops[1].done.Load(), "the loop that did not return was left behind")
	require.Contains(t, logs.String(),
		`level=WARN msg="Connector queues not drained before shutdown, stopping their loops" timeout=100ms loops="[stuck hung]"`)
	require.Contains(t, logs.String(), `level=ERROR msg="Connector loop still running after it was stopped, `+
		`dropping its batches permanently" loop=hung batchCount=3 timeout=100ms`)
	require.NotContains(t, logs.String(), "loop=stuck", "a loop that returned once it was stopped is not reported")
}

// gatedSink is the far end of the gRPC sink, a PlatformConnector server that
// holds every batch until the test opens the gate and counts what it gets.
// With failFirst it answers the first batch with an error once the gate
// opens, as a sink that is briefly unavailable does.
type gatedSink struct {
	pb.UnimplementedPlatformConnectorServer
	gate      chan struct{}
	failFirst bool
	calls     atomic.Int32
	received  atomic.Int32
}

func (s *gatedSink) HealthEventOccurredV1(ctx context.Context, _ *pb.HealthEvents) (*emptypb.Empty, error) {
	select {
	case <-s.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if s.calls.Add(1) == 1 && s.failFirst {
		return nil, status.Error(codes.Unavailable, "sink briefly unavailable")
	}

	s.received.Add(1)

	return &emptypb.Empty{}, nil
}

// serveSink serves sink on a local port until the test ends and returns its
// address.
func serveSink(t *testing.T, sink *gatedSink) string {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := grpc.NewServer()
	pb.RegisterPlatformConnectorServer(server, sink)

	go func() { _ = server.Serve(lis) }()

	t.Cleanup(server.Stop)

	return lis.Addr().String()
}

// TestShutdown_QueuedSinkBatches_DeliveredBeforeTheSinkCloses: Shutdown closes
// the sink and the datastore only after the loops have finished, so every batch
// the gRPC sink's queue held at shutdown reaches the sink, including the one in
// flight when Shutdown started.
func TestShutdown_QueuedSinkBatches_DeliveredBeforeTheSinkCloses(t *testing.T) {
	sink := &gatedSink{gate: make(chan struct{})}
	target := serveSink(t, sink)

	ctx := context.Background()
	c := &Connectors{mode: ConnectorOptions{Queued: true}}
	require.NoError(t, c.addSink(ctx, map[string]any{
		"enableGRPCSinkConnector":     "true",
		"GRPCSinkTarget":              target,
		"GRPCSinkConnectorMaxRetries": json.Number("3"),
	}))

	const batches = 20
	for range batches {
		require.NoError(t, c.Set.ProcessBatch(ctx, &pb.HealthEvents{Events: []*pb.HealthEvent{{NodeName: "node-1"}}}))
	}

	shutdown := shutdownInBackground(ctx, c)

	require.Never(t, func() bool { return isClosed(shutdown) }, 100*time.Millisecond, 5*time.Millisecond,
		"Shutdown returned while the sink still held a batch")
	close(sink.gate)

	require.Eventually(t, func() bool { return isClosed(shutdown) }, 5*time.Second, 5*time.Millisecond)
	require.EqualValues(t, batches, sink.received.Load(), "every queued batch reached the sink before it was closed")
}

// TestShutdown_SinkSendFailingDuringTheDrain_IsRetried: a send to the gRPC sink
// that fails while Shutdown waits for the loops is retried, and every batch
// the sink's queue held at shutdown still reaches the sink before it is
// closed. A queue that is shut down takes no batch back, so a failed batch
// handed back to it would be lost.
func TestShutdown_SinkSendFailingDuringTheDrain_IsRetried(t *testing.T) {
	sink := &gatedSink{gate: make(chan struct{}), failFirst: true}
	target := serveSink(t, sink)

	ctx := context.Background()
	c := &Connectors{mode: ConnectorOptions{Queued: true}}
	require.NoError(t, c.addSink(ctx, map[string]any{
		"enableGRPCSinkConnector":     "true",
		"GRPCSinkTarget":              target,
		"GRPCSinkConnectorMaxRetries": json.Number("3"),
	}))

	const batches = 5
	for range batches {
		require.NoError(t, c.Set.ProcessBatch(ctx, &pb.HealthEvents{Events: []*pb.HealthEvent{{NodeName: "node-1"}}}))
	}

	shutdown := shutdownInBackground(ctx, c)

	require.Never(t, func() bool { return isClosed(shutdown) }, 100*time.Millisecond, 5*time.Millisecond,
		"Shutdown returned while the sink still held a batch")
	close(sink.gate)

	require.Eventually(t, func() bool { return isClosed(shutdown) }, 5*time.Second, 5*time.Millisecond)
	require.EqualValues(t, batches, sink.received.Load(),
		"the batch whose send failed during the drain was sent again before the sink was closed")
}

// TestShutdown_LoopHeldUntilStopCh_IsStoppedAtTheDrainTimeout: Shutdown keeps
// stopCh open while the loops drain their queues, so the Kubernetes loop writes
// what its queue holds like the others, and closes it at the drain timeout,
// which ends the retries of the batch a loop still holds then.
func TestShutdown_LoopHeldUntilStopCh_IsStoppedAtTheDrainTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := &Connectors{
		mode: ConnectorOptions{Queued: true}, stopCh: make(chan struct{}), drainTimeout: 500 * time.Millisecond,
	}
	queue := c.queue(ctx, "held")
	loop := &held{queue: queue, stopCh: c.stopCh, holding: make(chan struct{})}
	c.add(ctx, queue, "held", loop, true)

	require.NoError(t, queue.ProcessBatch(ctx, &pb.HealthEvents{Events: []*pb.HealthEvent{{NodeName: "node-1"}}}))
	require.Eventually(t, func() bool { return isClosed(loop.holding) }, 5*time.Second, 5*time.Millisecond)

	shutdown := shutdownInBackground(ctx, c)

	require.Never(t, func() bool { return isClosed(c.stopCh) }, 200*time.Millisecond, 5*time.Millisecond,
		"stopCh was closed while the loops still had time to drain their queues")
	require.Eventually(t, func() bool { return isClosed(shutdown) }, 5*time.Second, 5*time.Millisecond)
	require.True(t, c.loops[0].done.Load(), "the loop held until stopCh closes returned before Shutdown did")
}

// kubernetesLoop adds the Kubernetes connector to c on the queued path, as
// addKubernetes does, over a fake clientset the test controls.
func kubernetesLoop(ctx context.Context, c *Connectors, client *fake.Clientset) *ringbuffer.RingBuffer {
	queue := c.queue(ctx, "kubernetes")
	connector := kubernetes.NewK8sConnector(client, queue, c.stopCh, ctx, k8sSettings.K8sConnectorConfig)
	c.add(ctx, queue, "kubernetes", connector, true)

	return queue
}

// nodeEventBatch is a batch of one fault that is not fatal, which the
// Kubernetes connector writes as a Kubernetes Event named after its message.
func nodeEventBatch(message string) *pb.HealthEvents {
	return &pb.HealthEvents{Events: []*pb.HealthEvent{{
		NodeName: "node-1", CheckName: "GpuXidError", Agent: "gpu-health-monitor",
		Message: message, GeneratedTimestamp: timestamppb.Now(),
	}}}
}

// TestShutdown_QueuedKubernetesBatches_WrittenBeforeShutdownReturns: a batch is
// acknowledged once it is queued, so the Kubernetes loop writes every batch its
// queue holds at shutdown, as the other loops do, before Shutdown returns.
func TestShutdown_QueuedKubernetesBatches_WrittenBeforeShutdownReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Every Event create waits until the test releases it, so the loop is in
	// the middle of its first batch when Shutdown starts.
	inFlight := make(chan struct{})
	release := make(chan struct{})

	var (
		created  atomic.Int32
		announce sync.Once
	)

	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "events", func(ktesting.Action) (bool, runtime.Object, error) {
		announce.Do(func() { close(inFlight) })
		<-release
		created.Add(1)

		return false, nil, nil
	})

	c := &Connectors{mode: ConnectorOptions{Queued: true}, stopCh: make(chan struct{})}
	queue := kubernetesLoop(ctx, c, client)

	const batches = 5
	for i := range batches {
		require.NoError(t, queue.ProcessBatch(ctx, nodeEventBatch(fmt.Sprintf("fault %d", i))))
	}

	require.Eventually(t, func() bool { return isClosed(inFlight) }, 5*time.Second, 5*time.Millisecond)

	shutdown := shutdownInBackground(ctx, c)

	require.Never(t, func() bool { return isClosed(shutdown) }, 100*time.Millisecond, 5*time.Millisecond,
		"Shutdown returned while an Event write was in flight")
	close(release)

	require.Eventually(t, func() bool { return isClosed(shutdown) }, 5*time.Second, 5*time.Millisecond)
	require.EqualValues(t, batches, created.Load(), "every queued batch was written before Shutdown returned")
	require.True(t, isClosed(c.stopCh), "stopCh is closed once the loops have returned")
}

// TestShutdown_KubernetesWriteStillFailingAtTheDrainTimeout_IsCut: a Kubernetes
// write that keeps failing is retried while the queues drain, and stopCh ends
// its retries at the drain timeout, so a slow API server holds shutdown no
// longer than the drain timeout. The write is dropped as a shutdown drop, at
// Info, as before.
func TestShutdown_KubernetesWriteStillFailingAtTheDrainTimeout_IsCut(t *testing.T) {
	logs := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "events", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("API server unavailable")
	})

	c := &Connectors{mode: ConnectorOptions{Queued: true}, stopCh: make(chan struct{}), drainTimeout: time.Second}
	queue := kubernetesLoop(ctx, c, client)

	require.NoError(t, queue.ProcessBatch(ctx, nodeEventBatch("fault")))
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), `retryCount=1 retryDelay=500ms`)
	}, 5*time.Second, 5*time.Millisecond, "the first write never failed")

	shutdown := shutdownInBackground(ctx, c)

	require.Eventually(t, func() bool { return isClosed(shutdown) }, 5*time.Second, 5*time.Millisecond,
		"the retries of the Kubernetes write were not ended at the drain timeout")

	// The retry 500 ms after the first failure runs during the drain; the next
	// one, a second later, would come after the drain timeout.
	require.Contains(t, logs.String(), `retryCount=2 retryDelay=1s`, "the write was not retried during the drain")
	require.Contains(t, logs.String(),
		`level=INFO msg="Discarding unsuccessful Kubernetes write" operation=node_event node=node-1 reason=shutdown`+
			` isHealthy=false retryCount=2`)
}

// TestReadiness_ShuttingDownWinsOverReady: /readyz follows the role's
// condition until shutdown begins, then answers unready whatever the role
// says, so the probes take the replica out of the Service first.
func TestReadiness_ShuttingDownWinsOverReady(t *testing.T) {
	roleReady := errors.New("not yet")
	probe := &readiness{ready: func() error { return roleReady }}

	require.ErrorIs(t, probe.Ready(context.Background()), roleReady)

	roleReady = nil
	require.NoError(t, probe.Ready(context.Background()))

	probe.shuttingDown.Store(true)
	require.ErrorContains(t, probe.Ready(context.Background()), "shutting down")
}
