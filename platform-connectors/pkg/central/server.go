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

// Package central is the deployment platform connector: the platform
// connector binary (PC_MODE=deployment) serving the same PlatformConnector
// gRPC service as the node-local DaemonSet, but over TCP with TLS to the whole
// fleet, with a small fixed pool of datastore connections.
//
// It reuses the gRPC handler, the event pipeline, the node-binding
// interceptor and the connectors as they are, built from the shared
// config.json with the readers the DaemonSet uses. What differs from the
// DaemonSet role:
//   - callers authenticate with projected ServiceAccount tokens (TokenReview)
//     and every batch is pinned to the caller token's node claim;
//   - there is no queue: the handler hands the batch to the connectors
//     themselves instead of to their ring buffers. With the store connector
//     enabled its write decides the reply and node conditions and the gRPC
//     sink run alongside it, best effort inside a bounded wait; without it
//     every enabled connector's result counts;
//   - every batch carries an idempotency key, so a resent batch is never
//     stored twice;
//   - node conditions are updated, and Kubernetes Events written, only when
//     the batch would change them;
//   - connections are closed after a set age or idle time, and shutdown waits
//     a bounded time for open connections;
//   - a replica is ready only while the idempotency index is verified, and
//     refuses writes otherwise.
package central

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"

	srv "github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/configfile"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/grpcsink"
	k8sconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/prom"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/dedup"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/metadata"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/overrides"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// AppName is the deployment platform connector's application name: main
// names the logger, the audit log and the tracing service with it, and the
// chart derives the Kubernetes objects, the TLS certificate and the audience
// from the same name.
const AppName = "platform-connector-deployment"

// indexVerifyInterval is how often an unready replica re-checks the
// idempotency index while waiting for the datastore setup to create it;
// indexRecheckInterval is how often a ready replica confirms the index is
// still there.
const (
	indexVerifyInterval  = 5 * time.Second
	indexRecheckInterval = 5 * time.Minute
	// indexVerifyTimeout bounds one index check, so a datastore call that
	// never returns cannot silence the loop for the rest of the replica's life.
	indexVerifyTimeout = 30 * time.Second
	// shutdownTimeout bounds the wait for open connections at shutdown, so one
	// unresponsive client cannot block it. The chart's termination grace period
	// is sized from it (this wait, then the datastore disconnect and the trace
	// exporter, 5 s each), so it is fixed here rather than configured.
	shutdownTimeout = 20 * time.Second
)

// readiness gates /readyz and the write path: ready only when the idempotency
// index has been verified (so no batch is acknowledged before the datastore
// can suppress its duplicates) and the replica is not shutting down. A failed
// write leaves readiness alone: refusing writes on every transient error would
// turn a small error rate into a total refusal on the replica, and replicas
// stay in the Service through datastore outages, as the design requires.
type readiness struct {
	indexVerified atomic.Bool
	shuttingDown  atomic.Bool
}

// newReadiness is the state a replica starts in: with a store connector the
// index is unverified until verifyIndexLoop confirms it, so no batch is
// acknowledged before the datastore can suppress its duplicates; without one
// there is no index to check and the replica is ready at start.
func newReadiness(storeEnabled bool) *readiness {
	r := &readiness{}
	if !storeEnabled {
		r.indexVerified.Store(true)
	}

	return r
}

// writesAllowed reports whether a batch may be written: the index is verified.
func (r *readiness) writesAllowed() bool {
	return r.indexVerified.Load()
}

func (r *readiness) Ready(_ context.Context) error {
	if r.shuttingDown.Load() {
		return errors.New("shutting down")
	}

	if !r.indexVerified.Load() {
		return errors.New("idempotency index not verified yet")
	}

	return nil
}

// indexVerifier is the one store connector call the readiness loop needs.
type indexVerifier interface {
	VerifyIdempotencyIndex(ctx context.Context) error
}

// verifyIndexLoop keeps readiness tied to the idempotency index for as long
// as the replica runs: every verifyEvery until the index matches its expected
// definition (this is what orders the datastore setup before any client
// traffic), then every recheckEvery. A definitive answer, the index missing or
// defined differently, turns the replica unready and makes the readiness
// interceptor refuse writes, so a database restored without the index stops
// taking writes as soon as the next check notices (within recheckEvery); once
// the datastore setup has recreated it the loop turns the replica ready again.
// A datastore failure changes nothing: replicas stay ready through outages,
// as the design requires. Every check has its own deadline (attemptTimeout);
// a check that runs out of time counts as a datastore failure. The loop ends
// with ctx, and a check cut short by that end is not applied.
func verifyIndexLoop(
	ctx context.Context, connector indexVerifier, ready *readiness,
	verifyEvery, recheckEvery, attemptTimeout time.Duration,
) {
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		err := connector.VerifyIdempotencyIndex(attemptCtx)

		cancel()

		if ctx.Err() != nil {
			return
		}

		applyIndexCheck(ctx, ready, err, verifyEvery)

		interval := verifyEvery
		if ready.writesAllowed() {
			interval = recheckEvery
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// applyIndexCheck folds one check's result into readiness: a verified index
// makes the replica ready; a confirmed missing or mismatched index makes it
// unready; any other failure changes nothing.
func applyIndexCheck(ctx context.Context, ready *readiness, err error, retryIn time.Duration) {
	switch {
	case err == nil:
		if !ready.indexVerified.Swap(true) {
			slog.InfoContext(ctx, "Idempotency index verified, replica is ready")
		}
	case errors.Is(err, datastore.ErrIndexMissing), errors.Is(err, datastore.ErrIndexMismatch):
		if ready.indexVerified.Swap(false) {
			slog.ErrorContext(ctx, "Idempotency index lost; the replica is unready and refuses writes until "+
				"the datastore setup recreates it: the MongoDB setup Job on its next run, or on PostgreSQL the "+
				"start of a component that sets the tables up (fault-quarantine, fault-remediation, "+
				"health-events-analyzer or node-drainer)", "error", err)
		} else {
			slog.WarnContext(ctx, "Idempotency index not verified yet, retrying",
				"error", err, "retryIn", retryIn)
		}
	default:
		slog.WarnContext(ctx, "Idempotency index check failed, readiness unchanged", "error", err)
	}
}

// buildServerOptions assembles the gRPC server options: transport security,
// connection lifetimes, per-connection buffer sizes and the interceptors, run
// in the order given. grpc-go itself spreads MaxConnectionAge by plus or
// minus 10 percent per connection, so fleet connections do not expire in
// synchronized waves. The returned certificate watcher, when non-nil, must be
// started for rotation to take effect.
func buildServerOptions(cfg *config, interceptors ...grpc.UnaryServerInterceptor) (
	[]grpc.ServerOption, *certwatcher.CertWatcher, error,
) {
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(interceptors...),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:  cfg.maxConnAge,
			MaxConnectionIdle: cfg.maxConnIdle,
		}),
	}

	if cfg.grpcReadBufferBytes > 0 {
		opts = append(opts, grpc.ReadBufferSize(cfg.grpcReadBufferBytes))
	}

	if cfg.grpcWriteBufferBytes > 0 {
		opts = append(opts, grpc.WriteBufferSize(cfg.grpcWriteBufferBytes))
	}

	if cfg.tlsCertDir == "" {
		// loadConfigFromEnv only allows this with the explicitly named
		// insecure development mode.
		slog.Warn("Serving PLAINTEXT: TLS_INSECURE_DEVELOPMENT_MODE is set; never use this outside development")

		return opts, nil, nil
	}

	cw, err := newCertWatcher(cfg.tlsCertDir)
	if err != nil {
		return nil, nil, err
	}

	opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfigFor(cw))))

	return opts, cw, nil
}

// components is everything Run wires together and shutdown tears down.
type components struct {
	cfg        *config
	ready      *readiness
	store      *store.DatabaseStoreConnector
	handler    *server.PlatformConnectorServer
	grpcServer *grpc.Server
	sink       *grpcsink.GRPCSinkConnector
	pipeline   *pipeline.Pipeline
	// stopIndexLoop ends the index verification loop, before the store it
	// checks is disconnected.
	stopIndexLoop context.CancelFunc
}

// initComponents builds the connectors config.json enables, the pipeline and
// the request handler from the environment and the shared config.json, read
// with the same code as the DaemonSet role, into one set without queues. With
// the store connector enabled its write is the reply and every other member
// is best effort inside the condition update timeout, so a slow node update
// or sink never fails a batch that is safely stored. Without it there is
// nothing to protect: every member's result counts, a failure is returned and
// the caller retries, and the replica is ready at start since there is no
// index to verify.
func initComponents(ctx context.Context, cfg *config, raw map[string]any) (*components, error) {
	storeEnabled := configfile.Bool(raw, "enableMongoDBStorePlatformConnector") ||
		configfile.Bool(raw, "enablePostgresDBStorePlatformConnector")

	c := &components{cfg: cfg, ready: newReadiness(storeEnabled)}

	var set connectors.Set

	if storeEnabled {
		storeConnector, err := store.InitializeDatabaseStoreConnector(ctx, nil, cfg.certMountPath, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize store connector: %w", err)
		}

		c.store = storeConnector
		set = append(set, storeConnector)
	} else {
		slog.WarnContext(ctx, "No store connector enabled: batches are not stored, every enabled connector's "+
			"result decides the reply, resent batches are not deduplicated and the replica is ready at start")
	}

	k8sSettings, err := fleetK8sSettings(cfg, raw)
	if err != nil {
		return nil, fmt.Errorf("k8s connector settings: %w", err)
	}

	set, err = c.appendOptionalConnectors(ctx, set, raw, k8sSettings)
	if err != nil {
		return nil, err
	}

	c.pipeline, err = pipeline.NewFromRawConfig(ctx, raw, pipeline.Options{
		KubeClientQPS:         k8sSettings.QPS,
		KubeClientBurst:       k8sSettings.Burst,
		NodeMetadataCacheSize: cfg.nodeMetadataCacheSize,
		NodeMetadataCacheTTL:  cfg.nodeMetadataCacheTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize pipeline: %w", err)
	}

	c.handler = &server.PlatformConnectorServer{Pipeline: c.pipeline, Connector: set}

	return c, nil
}

// fleetK8sSettings reads the k8s connector's settings from the shared
// config.json and applies the fleet-sized overrides from the environment to
// the client rate limit. It is read whether or not the k8s connector is on:
// the metadata transformer's client is sized by the same rate limit.
func fleetK8sSettings(cfg *config, raw map[string]any) (k8sconnector.Settings, error) {
	settings, err := k8sconnector.SettingsFromConfig(raw)
	if err != nil {
		return k8sconnector.Settings{}, err
	}

	if cfg.k8sClientQPS > 0 {
		settings.QPS = cfg.k8sClientQPS
	}

	if cfg.k8sClientBurst > 0 {
		settings.Burst = cfg.k8sClientBurst
	}

	return settings, nil
}

// appendOptionalConnectors adds the connectors config.json enables to set,
// built without queues: the k8s connector updates node conditions and writes
// Events inside the request, on change, and the gRPC sink is called once per
// batch, both run as member describes; the Prometheus connector counts every
// batch.
func (c *components) appendOptionalConnectors(
	ctx context.Context, set connectors.Set, raw map[string]any, k8sSettings k8sconnector.Settings,
) (connectors.Set, error) {
	if configfile.Bool(raw, "enableK8sPlatformConnector") {
		connector, _, err := k8sconnector.InitializeK8sConnector(
			ctx, nil, k8sSettings.QPS, k8sSettings.Burst, nil, k8sSettings.K8sConnectorConfig, "")
		if err != nil {
			return nil, fmt.Errorf("failed to initialize k8s connector: %w", err)
		}

		set = append(set, c.member("kubernetes", connector))

		slog.InfoContext(ctx, "k8s connector enabled: node conditions and Events are written inside requests, on change")
	}

	if configfile.Bool(raw, "enableGRPCSinkConnector") {
		sink, err := grpcsink.SettingsFromConfig(raw)
		if err != nil {
			return nil, fmt.Errorf("gRPC sink connector settings: %w", err)
		}

		// No queue, so no retry loop: next to a store the failure is logged
		// and counted, without one it is returned and the caller retries.
		c.sink, err = grpcsink.InitializeGRPCSinkConnector(nil, sink.Target, 0, sink.TokenPath)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize gRPC sink connector: %w", err)
		}

		set = append(set, c.member("grpcsink", c.sink))

		slog.InfoContext(ctx, "gRPC sink connector enabled", "target", sink.Target)
	}

	if configfile.Bool(raw, "enablePromPlatformConnector") {
		// No ring buffer: the set hands it one batch at a time, so
		// health_events_total counts every batch that passed validation and
		// the pipeline, as the node-local role counts what reaches its queue.
		// The write's outcome is not waited for, so a batch resent after a
		// failed write is counted again.
		set = append(set, prom.InitializePromConnector(nil))

		slog.InfoContext(ctx, "Prometheus connector enabled: every accepted batch is counted in health_events_total")
	}

	return set, nil
}

// member returns how the set runs an optional connector: best effort next to
// a store connector, whose write is the reply, so a slow node update or sink
// never fails a stored batch; as it is without one, so its failure is
// returned and the caller retries, since nothing else would.
func (c *components) member(name string, connector connectors.Connector) connectors.Connector {
	if c.store == nil {
		return connector
	}

	return connectors.BestEffort(name, connector, c.cfg.conditionUpdateTimeout)
}

// Run is the deployment platform connector: the platform connector binary
// enters it with PC_MODE=deployment once main has set up logging, the audit
// logger and tracing under AppName.
func Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := loadConfigFromEnv()
	if err != nil {
		return err
	}

	raw, err := configfile.Load(cfg.configPath)
	if err != nil {
		return err
	}

	authSettings, err := deploymentAuthSettings(raw)
	if err != nil {
		return err
	}

	slog.InfoContext(ctx, "Deployment platform connector configured", "listenAddr", cfg.listenAddr,
		"audience", authSettings.Audience, "conditionUpdateTimeout", cfg.conditionUpdateTimeout)

	c, err := initComponents(ctx, cfg, raw)
	if err != nil {
		return err
	}

	defer c.pipeline.Close()

	if c.store != nil {
		indexCtx, stopIndexLoop := context.WithCancel(ctx)
		c.stopIndexLoop = stopIndexLoop

		go verifyIndexLoop(indexCtx, c.store, c.ready, indexVerifyInterval, indexRecheckInterval, indexVerifyTimeout)
	}

	callerValidator, err := newValidator(cfg, authSettings.Audience)
	if err != nil {
		return fmt.Errorf("failed to build caller token validator: %w", err)
	}

	authInterceptor, err := newAuthInterceptor(ctx, authSettings, callerValidator)
	if err != nil {
		return err
	}

	// Authenticate and scope the caller, then key the batch, then refuse it
	// while the index is unverified; the handler validates, runs the
	// pipeline and hands the batch to the connectors.
	serverOpts, certWatcher, err := buildServerOptions(cfg,
		authInterceptor, idempotencyInterceptor(), readinessInterceptor(c.ready))
	if err != nil {
		return err
	}

	c.grpcServer = grpc.NewServer(serverOpts...)
	pb.RegisterPlatformConnectorServer(c.grpcServer, c.handler)

	return serve(ctx, cancel, c, certWatcher)
}

// serve runs the gRPC server, the metrics server, the certificate watcher and
// the signal handler until one of them stops the group.
func serve(ctx context.Context, cancel context.CancelFunc, c *components, certWatcher *certwatcher.CertWatcher) error {
	cfg := c.cfg

	var lc net.ListenConfig

	lis, err := lc.Listen(ctx, "tcp", cfg.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", cfg.listenAddr, err)
	}

	g, gCtx := errgroup.WithContext(ctx)

	// The watcher's event and polling loops are what pick up certificate
	// rotations; without them the listener would serve the startup pair
	// forever.
	if certWatcher != nil {
		g.Go(func() error {
			if err := certWatcher.Start(gCtx); err != nil {
				return fmt.Errorf("certificate watcher failed: %w", err)
			}

			return nil
		})
	}

	g.Go(func() error {
		slog.InfoContext(gCtx, "Deployment platform connector gRPC listening",
			"addr", cfg.listenAddr, "tls", cfg.tlsCertDir != "")

		err := c.grpcServer.Serve(lis)
		if err != nil {
			slog.ErrorContext(gCtx, "gRPC Serve returned", "error", err)

			return err
		}

		// Serve returns nil once Stop or GracefulStop has run: the normal end
		// of a shutdown, not an error.
		slog.InfoContext(gCtx, "gRPC server stopped")

		return nil
	})

	metricsSrv := srv.NewServer(
		srv.WithPort(cfg.metricsPort),
		srv.WithPrometheusMetrics(),
		srv.WithSimpleHealth(),
		srv.WithReadinessCheck(c.ready),
	)

	// This server also answers the readiness and liveness probes, so its
	// failure stops the replica instead of leaving it serving unobserved.
	g.Go(func() error {
		if err := metricsSrv.Serve(gCtx); err != nil {
			return fmt.Errorf("metrics and probe server failed: %w", err)
		}

		return nil
	})

	g.Go(func() error {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

		select {
		case sig := <-sigs:
			slog.InfoContext(gCtx, "Received signal, shutting down", "signal", sig)
		case <-gCtx.Done():
			slog.InfoContext(gCtx, "errgroup context done, shutting down", "cause", context.Cause(gCtx))
		}

		shutdown(gCtx, c)
		cancel()

		return nil
	})

	return g.Wait()
}

// shutdown turns the replica unready, lets requests in flight finish for a
// bounded time, then disconnects. The server holds nothing between requests,
// so there is nothing to drain: a client whose request did not complete
// resends it, with the same idempotency key, to another replica.
func shutdown(ctx context.Context, c *components) {
	if c.stopIndexLoop != nil {
		c.stopIndexLoop()
	}

	c.ready.shuttingDown.Store(true)

	// GracefulStop waits for every open connection; one wedged client would
	// otherwise block shutdown forever, so it is bounded.
	done := make(chan struct{})

	go func() {
		c.grpcServer.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		slog.WarnContext(ctx, "GracefulStop timed out, forcing Stop", "timeout", shutdownTimeout)
		c.grpcServer.Stop()
	}

	if c.sink != nil {
		if err := c.sink.Close(); err != nil {
			slog.WarnContext(ctx, "Error closing gRPC sink connector", "error", err)
		}
	}

	if c.store != nil {
		disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer disconnectCancel()

		if err := c.store.Disconnect(disconnectCtx); err != nil {
			slog.WarnContext(ctx, "Error disconnecting store connector", "error", err)
		}
	}

	tracingCtx, tracingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer tracingCancel()

	if err := tracing.ShutdownTracing(tracingCtx); err != nil {
		slog.WarnContext(ctx, "Error shutting down tracing", "error", err)
	}
}
