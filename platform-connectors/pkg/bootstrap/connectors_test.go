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
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

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

// held is a queued connector whose loop keeps its batch until stopCh closes,
// the way the Kubernetes connector's loop ends its retries on stopCh.
type held struct {
	queue   *ringbuffer.RingBuffer
	stopCh  <-chan struct{}
	holding chan struct{}
}

func (*held) ProcessBatch(context.Context, *pb.HealthEvents) error { return nil }

func (h *held) FetchAndProcessHealthMetric(ctx context.Context) {
	item, quit := h.queue.Dequeue()
	if quit {
		return
	}

	close(h.holding)

	select {
	case <-h.stopCh:
	case <-ctx.Done():
	}

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
}

// gatedSink is the far end of the gRPC sink, a PlatformConnector server that
// holds every batch until the test opens the gate and counts what it gets.
type gatedSink struct {
	pb.UnimplementedPlatformConnectorServer
	gate     chan struct{}
	received atomic.Int32
}

func (s *gatedSink) HealthEventOccurredV1(ctx context.Context, _ *pb.HealthEvents) (*emptypb.Empty, error) {
	select {
	case <-s.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	s.received.Add(1)

	return &emptypb.Empty{}, nil
}

// TestShutdown_QueuedSinkBatches_DeliveredBeforeTheSinkCloses: Shutdown closes
// the sink and the datastore only after the loops have finished, so every batch
// the gRPC sink's queue held at shutdown reaches the sink, including the one in
// flight when Shutdown started.
func TestShutdown_QueuedSinkBatches_DeliveredBeforeTheSinkCloses(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	sink := &gatedSink{gate: make(chan struct{})}
	server := grpc.NewServer()
	pb.RegisterPlatformConnectorServer(server, sink)

	go func() { _ = server.Serve(lis) }()

	t.Cleanup(server.Stop)

	ctx := context.Background()
	c := &Connectors{mode: ConnectorOptions{Queued: true}}
	require.NoError(t, c.addSink(ctx, map[string]any{
		"enableGRPCSinkConnector":     "true",
		"GRPCSinkTarget":              lis.Addr().String(),
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

// TestShutdown_LoopEndingOnStopCh_DoesNotWaitForTheDrainTimeout: Shutdown
// closes stopCh before it waits for the loops, so a loop that ends its retries
// on stopCh, as the Kubernetes connector's does, returns at once instead of
// holding every shutdown to the drain timeout.
func TestShutdown_LoopEndingOnStopCh_DoesNotWaitForTheDrainTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c := &Connectors{mode: ConnectorOptions{Queued: true}, stopCh: make(chan struct{}), drainTimeout: 5 * time.Second}
	queue := c.queue(ctx, "held")
	loop := &held{queue: queue, stopCh: c.stopCh, holding: make(chan struct{})}
	c.add(ctx, queue, "held", loop, true)

	require.NoError(t, queue.ProcessBatch(ctx, &pb.HealthEvents{Events: []*pb.HealthEvent{{NodeName: "node-1"}}}))
	require.Eventually(t, func() bool { return isClosed(loop.holding) }, 5*time.Second, 5*time.Millisecond)

	start := time.Now()
	shutdown := shutdownInBackground(ctx, c)

	require.Eventually(t, func() bool { return isClosed(shutdown) }, 10*time.Second, 5*time.Millisecond)
	require.Less(t, time.Since(start), time.Second, "Shutdown waited for a loop that stopCh should have ended")
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
