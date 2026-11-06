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

// Package bootstrap is the code both roles of the platform connector share to
// come up and go down: it reads the shared config.json, builds the enabled
// connectors and the pipeline, serves the PlatformConnector gRPC service with
// the metrics and probe server beside it, and shuts everything down in order
// on a signal. A Role supplies what differs between the node-local DaemonSet
// and the deployment platform connector: whether the connectors run from
// queues or inside the request, where the server listens, how the connection
// is secured and which interceptors run.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/configfile"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/grpcsink"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/prom"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

// ConnectorOptions say how the connectors config.json enables are run.
type ConnectorOptions struct {
	// Queued gives every connector a ring buffer the handler enqueues into
	// and a loop that drains it, so a batch is acknowledged once queued and
	// each connector retries on its own with the retry counts config.json
	// carries (the node-local role). Otherwise the connectors run inside the
	// request: the store connector's write decides the reply, and every
	// other connector is best effort within BestEffortTimeout, so a slow node
	// update or sink never fails a batch that is safely stored (the
	// deployment role). Without a store connector a batch that passed
	// validation is acknowledged as it is on the node-local role.
	Queued            bool
	BestEffortTimeout time.Duration
}

// Connectors is the built connector set and what its shutdown needs.
type Connectors struct {
	// Set is what the request handler hands every batch to.
	Set connectors.Set
	// Store is the store connector, nil when config.json enables none.
	Store *store.DatabaseStoreConnector

	opts   Options
	mode   ConnectorOptions
	k8s    kubernetes.Settings
	sink   *grpcsink.GRPCSinkConnector
	queues []*ringbuffer.RingBuffer
	// stopCh, on the queued path, ends the Kubernetes connector's loop and
	// the retries of the batch it is on. Shutdown closes it, once, when the
	// loops are stopped: at the drain timeout, or after they have returned.
	stopCh   chan struct{}
	stopOnce sync.Once
	// loops are the queue loops on the queued path, which running tracks.
	// Each returns once its queue is shut down and empty, or once it is
	// stopped.
	loops   []*queueLoop
	running sync.WaitGroup
	// drainTimeout and stopTimeout, when set, replace queueDrainTimeout and
	// queueStopTimeout; tests shorten them.
	drainTimeout time.Duration
	stopTimeout  time.Duration
}

// queueLoop is a connector's queue loop as Shutdown sees it: its name, its
// queue, the cancel of its context, and whether it has returned.
type queueLoop struct {
	name  string
	queue *ringbuffer.RingBuffer
	stop  context.CancelFunc
	done  atomic.Bool
}

// queueDrainTimeout bounds the wait at shutdown for the queue loops to process
// the batches their queues still hold, which were acknowledged when they were
// queued. Counting from SIGTERM on the node-local DaemonSet, requests in flight
// take up to 10 s and this wait ends by 20 s, so the queued events are safe by
// then. Loops still running then are stopped and get up to queueStopTimeout to
// return, and the datastore disconnect, the metrics server and the trace
// exporter take up to 5 s each after that, 40 s in all, which the pod's
// termination grace period has to cover.
const queueDrainTimeout = 10 * time.Second

// queueStopTimeout bounds the wait, after the drain timeout, for the loops it
// stopped to return. A loop held by a datastore call that does not end with
// its context, a PostgreSQL insert to a host that stopped answering for
// example, would otherwise keep Shutdown from closing the connections until
// the pod is killed.
const queueStopTimeout = 5 * time.Second

// looped is a connector with the loop that drains its ring buffer on the
// queued path.
type looped interface {
	connectors.Connector
	FetchAndProcessHealthMetric(ctx context.Context)
}

// BuildConnectors builds the connectors config.json enables, in the mode
// selected. The Kubernetes connector takes its client rate limit and message
// limits from k8s, which the caller reads from the same file.
func BuildConnectors(
	ctx context.Context, raw map[string]any, k8s kubernetes.Settings, opts Options, mode ConnectorOptions,
) (*Connectors, error) {
	c := &Connectors{opts: opts, mode: mode, k8s: k8s}
	if mode.Queued {
		c.stopCh = make(chan struct{})
	}

	if err := c.addStore(ctx, raw); err != nil {
		return nil, err
	}

	if err := c.addKubernetes(ctx, raw); err != nil {
		return nil, err
	}

	if err := c.addSink(ctx, raw); err != nil {
		return nil, err
	}

	c.addProm(ctx, raw)

	slog.InfoContext(ctx, "Connectors built", "members", len(c.Set), "queued", mode.Queued,
		"store", c.Store != nil, "grpcSink", c.sink != nil)

	return c, nil
}

func (c *Connectors) addStore(ctx context.Context, raw map[string]any) error {
	if !configfile.Bool(raw, "enableMongoDBStorePlatformConnector") &&
		!configfile.Bool(raw, "enablePostgresDBStorePlatformConnector") {
		return nil
	}

	maxRetries, err := configfile.Int64(raw, "StoreConnectorMaxRetries")
	if err != nil {
		return fmt.Errorf("store connector settings: %w", err)
	}

	queue := c.queue(ctx, "databaseStore")

	c.Store, err = store.InitializeDatabaseStoreConnector(ctx, queue, c.opts.CertMountPath, int(maxRetries))
	if err != nil {
		return fmt.Errorf("failed to initialize database store connector: %w", err)
	}

	// Never best effort: inside the request its write is the reply.
	c.add(ctx, queue, "databaseStore", c.Store, false)

	return nil
}

func (c *Connectors) addKubernetes(ctx context.Context, raw map[string]any) error {
	if !configfile.Bool(raw, "enableK8sPlatformConnector") {
		return nil
	}

	queue := c.queue(ctx, "kubernetes")

	connector, _, err := kubernetes.InitializeK8sConnector(
		ctx, queue, c.k8s.QPS, c.k8s.Burst, c.stopCh, c.k8s.K8sConnectorConfig, c.opts.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to initialize Kubernetes connector: %w", err)
	}

	c.add(ctx, queue, "kubernetes", connector, true)

	return nil
}

func (c *Connectors) addSink(ctx context.Context, raw map[string]any) error {
	if !configfile.Bool(raw, "enableGRPCSinkConnector") {
		return nil
	}

	target, err := configfile.String(raw, "GRPCSinkTarget")
	if err != nil {
		return fmt.Errorf("gRPC sink connector settings: %w", err)
	}

	if target == "" {
		return errors.New("gRPC sink connector settings: GRPCSinkTarget not configured or empty")
	}

	// Empty sends no token.
	tokenPath, err := configfile.String(raw, "GRPCSinkTokenPath")
	if err != nil {
		return fmt.Errorf("gRPC sink connector settings: %w", err)
	}

	maxRetries, err := configfile.Int64(raw, "GRPCSinkConnectorMaxRetries")
	if err != nil {
		return fmt.Errorf("gRPC sink connector settings: %w", err)
	}

	queue := c.queue(ctx, "grpcSink")

	c.sink, err = grpcsink.InitializeGRPCSinkConnector(queue, target, int(maxRetries), tokenPath)
	if err != nil {
		return fmt.Errorf("failed to initialize gRPC sink connector: %w", err)
	}

	// The best-effort label is lowercase like the metric names; the queue
	// name above keeps the DaemonSet's existing "grpcSink" metric names.
	c.add(ctx, queue, "grpcsink", c.sink, true)

	return nil
}

func (c *Connectors) addProm(ctx context.Context, raw map[string]any) {
	if !configfile.Bool(raw, "enablePromPlatformConnector") {
		return
	}

	queue := c.queue(ctx, "prom")

	// Counting cannot fail, so there is nothing to bound: health_events_total
	// counts every batch the handler accepted, in both roles.
	c.add(ctx, queue, "prom", prom.InitializePromConnector(queue), false)
}

// queue returns a new ring buffer on the queued path, nil otherwise.
func (c *Connectors) queue(ctx context.Context, name string) *ringbuffer.RingBuffer {
	if !c.mode.Queued {
		return nil
	}

	queue := ringbuffer.NewRingBuffer(name, ctx)
	c.queues = append(c.queues, queue)

	return queue
}

// add puts a connector into the set: on the queued path its ring buffer is
// the member and its loop is started; otherwise the connector itself is,
// wrapped in BestEffort when its failure must not fail the batch.
func (c *Connectors) add(
	ctx context.Context, queue *ringbuffer.RingBuffer, name string, connector looped, bestEffort bool,
) {
	if queue != nil {
		// Shutdown waits for the loop, and cancels its context only when it
		// is still running at the drain timeout.
		loopCtx, stop := context.WithCancel(ctx)
		loop := &queueLoop{name: name, queue: queue, stop: stop}
		c.loops = append(c.loops, loop)

		c.running.Go(func() {
			defer loop.done.Store(true)

			connector.FetchAndProcessHealthMetric(loopCtx)
		})

		c.Set = append(c.Set, queue)

		return
	}

	if bestEffort {
		c.Set = append(c.Set, connectors.BestEffort(name, connector, c.mode.BestEffortTimeout))

		return
	}

	c.Set = append(c.Set, connector)
}

// Shutdown stops the connectors once the server accepts no more requests:
// every queue stops taking batches, every loop, the Kubernetes one included,
// processes what its queue holds, and the sink and datastore connections are
// closed once the loops have returned.
func (c *Connectors) Shutdown(ctx context.Context) {
	for _, queue := range c.queues {
		queue.ShutDownHealthMetricQueue()
	}

	c.waitForLoops(ctx)

	if c.sink != nil {
		if err := c.sink.Close(); err != nil {
			slog.WarnContext(ctx, "Error closing gRPC sink connector", "error", err)
		}
	}

	if c.Store != nil {
		disconnectCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := c.Store.Disconnect(disconnectCtx); err != nil {
			slog.WarnContext(ctx, "Error disconnecting store connector", "error", err)
		}
	}
}

// waitForLoops waits for the queue loops, so a batch acknowledged before the
// shutdown is processed before the connections it is written through close.
// A loop still running at the drain timeout, behind a datastore that stays
// down for example, is stopped (see stopLoops); the store connector logs at
// Error the batches it drops. A loop that has not returned queueStopTimeout
// later is left behind, so the connections still close and the pod exits
// within its grace period. Such a loop logs nothing about its batches, so they
// are logged here, at Error.
func (c *Connectors) waitForLoops(ctx context.Context) {
	returned := make(chan struct{})

	// This goroutine outlives Shutdown only while a loop left behind runs.
	go func() {
		c.running.Wait()
		close(returned)
	}()

	drainTimeout := orDefault(c.drainTimeout, queueDrainTimeout)
	if closedWithin(returned, drainTimeout) {
		c.stopLoops()

		return
	}

	slog.WarnContext(ctx, "Connector queues not drained before shutdown, stopping their loops",
		"timeout", drainTimeout, "loops", c.runningLoops())
	c.stopLoops()

	stopTimeout := orDefault(c.stopTimeout, queueStopTimeout)
	if closedWithin(returned, stopTimeout) {
		return
	}

	for _, loop := range c.loops {
		if !loop.done.Load() {
			// A loop still running holds the batch it is on, so batchCount is
			// that batch and what its queue still holds.
			slog.ErrorContext(ctx, "Connector loop still running after it was stopped, dropping its batches permanently",
				"loop", loop.name, "batchCount", loop.queue.CurrentLength()+1, "timeout", stopTimeout)
		}
	}
}

// runningLoops names the loops that have not returned.
func (c *Connectors) runningLoops() []string {
	var running []string

	for _, loop := range c.loops {
		if !loop.done.Load() {
			running = append(running, loop.name)
		}
	}

	return running
}

// closedWithin reports whether ch is closed within timeout.
func closedWithin(ch <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

// orDefault returns timeout, or def when timeout is not set.
func orDefault(timeout, def time.Duration) time.Duration {
	if timeout <= 0 {
		return def
	}

	return timeout
}

// stopLoops ends the loops: closing stopCh ends the retries and API calls of
// the batch the Kubernetes loop is on, and canceling each loop's context ends
// the others' work.
func (c *Connectors) stopLoops() {
	c.stopOnce.Do(func() {
		if c.stopCh != nil {
			close(c.stopCh)
		}
	})

	for _, loop := range c.loops {
		loop.stop()
	}
}
