// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/flags"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	srv "github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/central"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/configfile"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/grpcsink"
	k8sconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/prom"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/dedup"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/metadata"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/overrides"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// One image, two roles: PC_MODE=deployment runs the deployment platform
	// connector, unset runs the node-local DaemonSet connector. The process
	// setup below is shared; the roles differ in their names and entrypoint.
	// The DaemonSet keeps the tracing service name it has always had.
	appName, tracingName, run := "platform-connectors", "platform-connector", runNodeLocal

	switch mode := os.Getenv("PC_MODE"); mode {
	case "deployment":
		appName, tracingName, run = central.AppName, central.AppName, central.Run
	case "":
		// The node-local role.
	default:
		fmt.Fprintf(os.Stderr, "unknown PC_MODE %q: use \"deployment\" or leave it unset for the node-local role\n", mode)
		os.Exit(1)
	}

	logger.SetDefaultStructuredLoggerWithTraceCorrelation(appName, version)
	setControllerRuntimeLogger()

	initCtx := context.Background()
	slog.InfoContext(initCtx, "Starting "+appName, "version", version, "commit", commit, "date", date)

	if err := auditlogger.InitAuditLogger(appName); err != nil {
		slog.WarnContext(initCtx, "Failed to initialize audit logger", "error", err)
	}

	if err := tracing.InitTracing(tracingName); err != nil {
		slog.WarnContext(initCtx, "Failed to initialize tracing", "error", err)
	}

	err := run()
	if err != nil {
		slog.ErrorContext(initCtx, appName+" exited with error", "error", err)
	}

	if closeErr := auditlogger.CloseAuditLogger(); closeErr != nil {
		slog.WarnContext(initCtx, "Failed to close audit logger", "error", closeErr)
	}

	if err != nil {
		os.Exit(1)
	}
}

// setControllerRuntimeLogger routes controller-runtime's logr output (the
// certificate watchers) through the process's slog handler; without a sink
// controller-runtime drops those lines and prints a "SetLogger(...) was never
// called" warning with a stack trace.
func setControllerRuntimeLogger() {
	ctrllog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
}

// initializeK8sConnector starts the K8s connector and returns the ring buffer
// its loop drains.
func initializeK8sConnector(
	ctx context.Context,
	config map[string]any,
	stopCh chan struct{},
	kubeconfigPath string,
) (*ringbuffer.RingBuffer, error) {
	k8sRingBuffer := ringbuffer.NewRingBuffer("kubernetes", ctx)

	settings, err := k8sconnector.SettingsFromConfig(config)
	if err != nil {
		return nil, fmt.Errorf("k8s connector settings: %w", err)
	}

	k8sConnector, _, err := k8sconnector.InitializeK8sConnector(
		ctx, k8sRingBuffer, settings.QPS, settings.Burst, stopCh, settings.K8sConnectorConfig, kubeconfigPath,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize K8sConnector: %w", err)
	}

	go k8sConnector.FetchAndProcessHealthMetric(ctx)

	return k8sRingBuffer, nil
}

// initializeDatabaseStoreConnector starts the store connector and returns it
// with the ring buffer its loop drains.
func initializeDatabaseStoreConnector(
	ctx context.Context,
	config map[string]any,
	databaseClientCertMountPath string,
) (*store.DatabaseStoreConnector, *ringbuffer.RingBuffer, error) {
	ringBuffer := ringbuffer.NewRingBuffer("databaseStore", ctx)

	maxRetries, err := configfile.Int64(config, "StoreConnectorMaxRetries")
	if err != nil {
		return nil, nil, fmt.Errorf("store connector settings: %w", err)
	}

	storeConnector, err := store.InitializeDatabaseStoreConnector(
		ctx, ringBuffer, databaseClientCertMountPath, int(maxRetries))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize database store connector: %w", err)
	}

	go storeConnector.FetchAndProcessHealthMetric(ctx)

	return storeConnector, ringBuffer, nil
}

// startGRPCServer serves the PlatformConnector service on the Unix socket.
// Every accepted batch goes to queues, one ring buffer per connector, and is
// acknowledged at once; the connectors process it from their queues.
func startGRPCServer(
	ctx context.Context,
	socket string,
	pipeline *pipeline.Pipeline,
	interceptor grpc.UnaryServerInterceptor,
	queues connectors.Connector,
) (net.Listener, error) {
	slog.InfoContext(ctx, "Starting gRPC server on Unix socket", "socket", socket)

	err := os.Remove(socket)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove existing socket: %w", err)
	}

	lc := &net.ListenConfig{}

	lis, err := lc.Listen(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on unix socket %s: %w", socket, err)
	}

	// The socket stays group/world accessible: the publishers that write to it
	// run non-root at assorted UIDs, so tightening the mode here would require
	// every publisher to change. Which node a caller may report on is decided
	// by the node-binding interceptor below, not by file permissions.
	if err := os.Chmod(socket, 0o666); err != nil {
		return nil, fmt.Errorf("failed to set socket permissions: %w", err)
	}

	slog.InfoContext(ctx, "gRPC server socket created successfully", "socket", socket, "permissions", "0666")

	var opts []grpc.ServerOption

	if interceptor != nil {
		opts = append(opts, grpc.UnaryInterceptor(interceptor))
	}

	grpcServer := grpc.NewServer(opts...)
	pb.RegisterPlatformConnectorServer(grpcServer, &server.PlatformConnectorServer{
		Pipeline:  pipeline,
		Connector: queues,
	})

	go func() {
		slog.InfoContext(ctx, "Starting gRPC server listener", "socket", socket)

		err = grpcServer.Serve(lis)
		if err != nil {
			slog.ErrorContext(ctx, "Not able to accept incoming connections", "error", err)
			os.Exit(1)
		}
	}()

	return lis, nil
}

// TokenReview client rate limit for one node's callers, passed to the shared
// auth.NewTokenReviewValidator. client-go's defaults (5 QPS, 10 burst) are
// meant for controllers that write occasionally, not for a call on the path of
// every cross-node health event: at 5 QPS a burst of events from the
// cluster-scoped publishers would queue in the client's rate limiter.
const (
	tokenReviewQPS   = 50
	tokenReviewBurst = 100
)

// initializeAuthInterceptor builds the node-binding interceptor that keeps a
// publisher on one node from submitting health events naming another node,
// from the settings the shared config.json carries. It returns nil when node
// binding is explicitly disabled, in which case any caller may name any node;
// that is not a supported production configuration.
func initializeAuthInterceptor(
	ctx context.Context,
	config map[string]any,
	kubeconfigPath string,
) (grpc.UnaryServerInterceptor, error) {
	settings, err := auth.SettingsFromConfig(config)
	if err != nil {
		return nil, fmt.Errorf("node-binding auth settings: %w", err)
	}

	if !settings.Enabled {
		slog.WarnContext(ctx, "Node-binding authentication is DISABLED. Any caller able to reach the "+
			"platform-connector socket may submit health events naming any node in the cluster.")

		return nil, nil
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return nil, fmt.Errorf("NODE_NAME environment variable is required when node-binding auth is enabled")
	}

	validator, err := auth.NewTokenReviewValidator(kubeconfigPath, settings.Audience, tokenReviewQPS, tokenReviewBurst)
	if err != nil {
		return nil, err
	}

	interceptor, err := auth.NewNodeBindingInterceptor(auth.Config{
		NodeName:                 nodeName,
		Validator:                validator,
		CrossNodeServiceAccounts: settings.CrossNodeServiceAccounts,
		Mode:                     settings.Mode,
		FailOpenOnUnavailable:    settings.FailOpenOnUnavailable,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build node-binding interceptor: %w", err)
	}

	return interceptor, nil
}

// initializeGRPCSinkConnector starts the gRPC sink connector and returns it
// with the ring buffer its loop drains.
func initializeGRPCSinkConnector(
	ctx context.Context,
	config map[string]any,
) (*grpcsink.GRPCSinkConnector, *ringbuffer.RingBuffer, error) {
	ringBuffer := ringbuffer.NewRingBuffer("grpcSink", ctx)

	settings, err := grpcsink.SettingsFromConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("gRPC sink connector settings: %w", err)
	}

	// Only the queue loop retries, so the retry count is this role's alone.
	maxRetries, err := configfile.Int64(config, "GRPCSinkConnectorMaxRetries")
	if err != nil {
		return nil, nil, fmt.Errorf("gRPC sink connector settings: %w", err)
	}

	connector, err := grpcsink.InitializeGRPCSinkConnector(
		ringBuffer, settings.Target, int(maxRetries), settings.TokenPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize gRPC sink connector: %w", err)
	}

	go connector.FetchAndProcessHealthMetric(ctx)

	return connector, ringBuffer, nil
}

// connectorSet holds the connectors started by initializeConnectors. Grouping them keeps
// adding a connector from growing the signature of every function that starts or stops them.
type connectorSet struct {
	// queues is what the gRPC server hands each accepted batch to: one ring
	// buffer per enabled connector, each drained by that connector's loop.
	queues        connectors.Set
	k8sRingBuffer *ringbuffer.RingBuffer
	store         *store.DatabaseStoreConnector
	grpcSink      *grpcsink.GRPCSinkConnector
	prom          *prom.PromConnector
}

func initializeConnectors(
	ctx context.Context,
	config map[string]any,
	stopCh chan struct{},
	databaseClientCertMountPath string,
	kubeconfigPath string,
) (*connectorSet, error) {
	var (
		set = &connectorSet{}
		err error
	)

	if configfile.Bool(config, "enableK8sPlatformConnector") {
		set.k8sRingBuffer, err = initializeK8sConnector(ctx, config, stopCh, kubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize K8s connector: %w", err)
		}

		set.queues = append(set.queues, set.k8sRingBuffer)
	}

	// Keep the legacy config key name for backward compatibility with existing ConfigMaps
	if configfile.Bool(config, "enableMongoDBStorePlatformConnector") ||
		configfile.Bool(config, "enablePostgresDBStorePlatformConnector") {
		var storeQueue *ringbuffer.RingBuffer

		set.store, storeQueue, err = initializeDatabaseStoreConnector(ctx, config, databaseClientCertMountPath)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize database store connector: %w", err)
		}

		set.queues = append(set.queues, storeQueue)
	}

	if configfile.Bool(config, "enableGRPCSinkConnector") {
		var sinkQueue *ringbuffer.RingBuffer

		set.grpcSink, sinkQueue, err = initializeGRPCSinkConnector(ctx, config)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize gRPC sink connector: %w", err)
		}

		set.queues = append(set.queues, sinkQueue)
	}

	if configfile.Bool(config, "enablePromPlatformConnector") {
		var promQueue *ringbuffer.RingBuffer

		set.prom, promQueue = initializePromConnector(ctx)
		set.queues = append(set.queues, promQueue)
	}

	return set, nil
}

// initializePromConnector starts the connector that records health events as
// Prometheus counters and returns it with the ring buffer its loop drains. It
// cannot fail: there is no external dependency to reach.
func initializePromConnector(ctx context.Context) (*prom.PromConnector, *ringbuffer.RingBuffer) {
	ringBuffer := ringbuffer.NewRingBuffer("prom", ctx)

	promConnector := prom.InitializePromConnector(ringBuffer)
	go promConnector.FetchAndProcessHealthMetric(ctx)

	return promConnector, ringBuffer
}

func cleanupResources(
	ctx context.Context,
	socket string,
	lis net.Listener,
	set *connectorSet,
) error {
	closeListener(ctx, socket, lis, set.k8sRingBuffer)

	return shutdownConnectors(ctx, set)
}

// closeListener drains the K8s ring buffer, closes the listener and removes the socket.
func closeListener(ctx context.Context, socket string, lis net.Listener, k8sRingBuffer *ringbuffer.RingBuffer) {
	if lis == nil {
		return
	}

	if k8sRingBuffer != nil {
		k8sRingBuffer.ShutDownHealthMetricQueue()
	}

	if err := lis.Close(); err != nil {
		slog.ErrorContext(ctx, "Failed to close listener", "error", err)
	}

	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		slog.ErrorContext(ctx, "Failed to remove socket file", "error", err)
	}
}

// shutdownConnectors drains each configured connector's ring buffer and closes its
// external connections. Only the store connector's failure is fatal.
func shutdownConnectors(ctx context.Context, set *connectorSet) error {
	if set.store != nil {
		set.store.ShutdownRingBuffer(ctx)

		disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer disconnectCancel()

		if err := set.store.Disconnect(disconnectCtx); err != nil {
			return fmt.Errorf("error disconnecting database store connector: %w", err)
		}
	}

	if set.grpcSink != nil {
		set.grpcSink.ShutdownRingBuffer()

		if err := set.grpcSink.Close(); err != nil {
			slog.Error("Failed to close gRPC sink connector", "error", err)
		}
	}

	if set.prom != nil {
		set.prom.ShutdownRingBuffer(ctx)
	}

	return nil
}

type platformConnectorConfig struct {
	socket                      string
	configFilePath              string
	metricsPort                 int
	databaseClientCertMountPath string
	kubeconfigPath              string
}

func parseFlags() (*platformConnectorConfig, error) {
	socket := flag.String("socket", "", "unix socket path")
	configFilePath := flag.String("config", "/etc/config/config.json", "path to the config file")
	metricsPort := flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")
	kubeconfigPath := flag.String("kubeconfig", "", "path to a kubeconfig file for out-of-cluster Kubernetes auth")

	// Register database certificate flags using common package
	certConfig := flags.RegisterDatabaseCertFlags()

	flag.Parse()

	if *socket == "" {
		return nil, fmt.Errorf("socket is not present")
	}

	portInt, err := strconv.Atoi(*metricsPort)
	if err != nil {
		return nil, fmt.Errorf("invalid metrics port: %w", err)
	}

	return &platformConnectorConfig{
		socket:                      *socket,
		configFilePath:              *configFilePath,
		metricsPort:                 portInt,
		databaseClientCertMountPath: certConfig.ResolveCertPath(),
		kubeconfigPath:              *kubeconfigPath,
	}, nil
}

func handleShutdown(
	gCtx context.Context,
	sigs chan os.Signal,
	stopCh chan struct{},
	cfg *platformConnectorConfig,
	lis net.Listener,
	set *connectorSet,
	cancel context.CancelFunc,
) error {
	slog.InfoContext(gCtx, "Waiting for SIGINT/SIGTERM or context cancellation")
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	defer func() {
		signal.Stop(sigs)
		close(sigs)
	}()

	select {
	case sig := <-sigs:
		slog.InfoContext(gCtx, "Received signal", "signal", sig)
	case <-gCtx.Done():
		slog.InfoContext(gCtx, "Context cancelled, initiating shutdown")
	}

	close(stopCh)

	if err := cleanupResources(gCtx, cfg.socket, lis, set); err != nil {
		return err
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := tracing.ShutdownTracing(shutdownCtx); err != nil {
		slog.WarnContext(shutdownCtx, "Failed to shutdown tracing", "error", err)
	}

	cancel()

	return nil
}

func runNodeLocal() error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}

	sigs := make(chan os.Signal, 1)
	stopCh := make(chan struct{})

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	defer cancel()

	if cfg.kubeconfigPath == "" {
		slog.InfoContext(ctx, "Using in-cluster Kubernetes authentication")
	} else {
		slog.InfoContext(ctx, "Using explicit kubeconfig for Kubernetes authentication", "path", cfg.kubeconfigPath)
	}

	config, err := configfile.Load(cfg.configFilePath)
	if err != nil {
		return err
	}

	set, err := initializeConnectors(ctx,
		config, stopCh, cfg.databaseClientCertMountPath, cfg.kubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to initialize connectors: %w", err)
	}

	pipeline, err := pipeline.NewFromRawConfig(ctx, config, pipeline.Options{
		KubeconfigPath: cfg.kubeconfigPath,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize pipeline: %w", err)
	}
	defer pipeline.Close()

	authInterceptor, err := initializeAuthInterceptor(ctx, config, cfg.kubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to initialize auth interceptor: %w", err)
	}

	lis, err := startGRPCServer(ctx, cfg.socket, pipeline, authInterceptor, set.queues)
	if err != nil {
		return err
	}

	srv := srv.NewServer(
		srv.WithPort(cfg.metricsPort),
		srv.WithPrometheusMetrics(),
		srv.WithSimpleHealth(),
	)

	g, gCtx := errgroup.WithContext(ctx)

	// Metrics server failures are logged but do NOT terminate the service
	g.Go(func() error {
		slog.InfoContext(gCtx, "Starting metrics server", "port", cfg.metricsPort)

		if err := srv.Serve(gCtx); err != nil {
			slog.ErrorContext(gCtx, "Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	g.Go(func() error {
		return handleShutdown(gCtx, sigs, stopCh, cfg, lis, set, cancel)
	})

	return g.Wait()
}
