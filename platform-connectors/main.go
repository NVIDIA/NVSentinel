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

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/kubernetes"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/flags"
	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	srv "github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/grpcsink"
	k8sconnector "github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/kubeconfig"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/dedup"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/metadata"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/overrides"
)

const (
	True = "true"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	logger.SetDefaultStructuredLoggerWithTraceCorrelation("platform-connectors", version)

	initCtx := context.Background()
	slog.InfoContext(initCtx, "Starting platform-connectors", "version", version, "commit", commit, "date", date)

	if err := auditlogger.InitAuditLogger("platform-connectors"); err != nil {
		slog.WarnContext(initCtx, "Failed to initialize audit logger", "error", err)
	}

	if err := tracing.InitTracing("platform-connector"); err != nil {
		slog.WarnContext(initCtx, "Failed to initialize tracing", "error", err)
	}

	if err := run(); err != nil {
		slog.ErrorContext(initCtx, "Platform connectors exited with error", "error", err)

		if closeErr := auditlogger.CloseAuditLogger(); closeErr != nil {
			slog.WarnContext(initCtx, "Failed to close audit logger", "error", closeErr)
		}

		os.Exit(1)
	}

	if err := auditlogger.CloseAuditLogger(); err != nil {
		slog.WarnContext(initCtx, "Failed to close audit logger", "error", err)
	}
}

func loadConfig(configFilePath string) (map[string]any, error) {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read platform-connector-configmap with err %w", err)
	}

	result := make(map[string]any)

	err = json.Unmarshal(data, &result)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal platform-connector-configmap with err %w", err)
	}

	return result, nil
}

// initializeK8sConnector creates the K8s connector and node metadata processor.
// Processor is returned here because it depends on the clientset from K8s initialization.
func initializeK8sConnector(
	ctx context.Context,
	config map[string]any,
	stopCh chan struct{},
	kubeconfigPath string,
) (*ringbuffer.RingBuffer, error) {
	k8sRingBuffer := ringbuffer.NewRingBuffer("kubernetes", ctx)
	server.InitializeAndAttachRingBufferForConnectors(k8sRingBuffer)

	qpsTemp, ok := config["K8sConnectorQps"].(float64)
	if !ok {
		return nil, fmt.Errorf("failed to convert K8sConnectorQps to float: %v", config["K8sConnectorQps"])
	}

	qps := float32(qpsTemp)

	maxNodeConditionMessageLength, ok := config["MaxNodeConditionMessageLength"].(int64)
	if !ok {
		return nil, fmt.Errorf("failed to convert MaxNodeConditionMessageLength to int64: %v",
			config["MaxNodeConditionMessageLength"])
	}

	compactedEventMsgLen, ok := config["CompactedHealthEventMsgLen"].(int64)
	if !ok {
		return nil, fmt.Errorf("failed to convert CompactedHealthEventMsgLen to int64: %v",
			config["CompactedHealthEventMsgLen"])
	}

	burst, ok := config["K8sConnectorBurst"].(int64)
	if !ok {
		return nil, fmt.Errorf("failed to convert K8sConnectorBurst to int: %v", config["K8sConnectorBurst"])
	}

	k8sConnectorCfg := k8sconnector.K8sConnectorConfig{
		MaxNodeConditionMessageLength: maxNodeConditionMessageLength,
		CompactedHealthEventMsgLen:    compactedEventMsgLen,
	}

	k8sConnector, _, err := k8sconnector.InitializeK8sConnector(
		ctx, k8sRingBuffer, qps, int(burst), stopCh, k8sConnectorCfg, kubeconfigPath,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize K8sConnector: %w", err)
	}

	go k8sConnector.FetchAndProcessHealthMetric(ctx)

	return k8sRingBuffer, nil
}

func initializeDatabaseStoreConnector(
	ctx context.Context,
	config map[string]any,
	databaseClientCertMountPath string,
) (*store.DatabaseStoreConnector, error) {
	ringBuffer := ringbuffer.NewRingBuffer("databaseStore", ctx)
	server.InitializeAndAttachRingBufferForConnectors(ringBuffer)

	maxRetriesInt64, ok := config["StoreConnectorMaxRetries"].(int64)
	if !ok {
		return nil, fmt.Errorf("failed to convert StoreConnectorMaxRetries to int: %v", config["StoreConnectorMaxRetries"])
	}

	maxRetries := int(maxRetriesInt64)

	storeConnector, err := store.InitializeDatabaseStoreConnector(ctx, ringBuffer, databaseClientCertMountPath, maxRetries)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database store connector: %w", err)
	}

	go storeConnector.FetchAndProcessHealthMetric(ctx)

	return storeConnector, nil
}

func initializePipeline(ctx context.Context, config map[string]any, opts pipeline.Options) (*pipeline.Pipeline, error) {
	pipelineCfg, ok := config["pipeline"].([]any)
	if !ok || len(pipelineCfg) == 0 {
		slog.ErrorContext(ctx, "No pipeline configuration found, events will not be transformed")
		return pipeline.New(), fmt.Errorf("no pipeline configuration found")
	}

	var transformerConfigs []pipeline.Config

	for _, item := range pipelineCfg {
		configMap, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("failed to convert pipeline configuration to map: %v", item)
		}

		name, ok := configMap["name"].(string)
		if !ok {
			return nil, fmt.Errorf("pipeline config missing or invalid 'name' field: %v", configMap["name"])
		}

		enabled, ok := configMap["enabled"].(bool)
		if !ok {
			return nil, fmt.Errorf("pipeline config missing or invalid 'enabled' field: %v", configMap["enabled"])
		}

		configPath, ok := configMap["config"].(string)
		if !ok {
			return nil, fmt.Errorf("pipeline config missing or invalid 'config' field: %v", configMap["config"])
		}

		transformerConfigs = append(transformerConfigs, pipeline.Config{
			Name:       name,
			Enabled:    enabled,
			ConfigPath: configPath,
		})
	}

	return pipeline.NewFromConfigs(ctx, transformerConfigs, opts)
}

func startGRPCServer(
	ctx context.Context,
	socket string,
	pipeline *pipeline.Pipeline,
	interceptor grpc.UnaryServerInterceptor,
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
		Pipeline: pipeline,
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

// TokenReview QPS and per-request timeout. client-go's defaults (5 QPS, 10
// burst, no timeout) are meant for controllers that write occasionally, not for
// a call on the path of every cross-node health event: at 5 QPS a burst of
// events from the cluster-scoped publishers would queue in the client's rate
// limiter, and with no timeout a wedged API server would hold those calls open
// indefinitely instead of letting the publisher retry.
const (
	tokenReviewQPS     = 50
	tokenReviewBurst   = 100
	tokenReviewTimeout = 10 * time.Second
)

// newK8sClientset builds a Kubernetes clientset for the auth interceptor. It is
// kept separate from the K8s connector's clientset so that node-binding
// enforcement does not depend on enableK8sPlatformConnector being on, and so
// TokenReview traffic does not share the connector's node-patching QPS budget.
//
// This client is deliberately NOT wrapped with auditlogger. A TokenReview is a
// question, not a change to the cluster, so there is nothing here for a change
// audit to record. It is only a POST because that is the shape of the API, and
// the audit round tripper treats every POST as a write: it would emit an entry
// per authenticated batch, and with AUDIT_LOG_REQUEST_BODY on it would copy the
// request body into the log. That body is the caller's ServiceAccount token.
func newK8sClientset(kubeconfigPath string) (kubernetes.Interface, error) {
	restConfig, err := kubeconfig.Load(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubernetes auth configuration: %w", err)
	}

	restConfig.QPS = tokenReviewQPS
	restConfig.Burst = tokenReviewBurst
	restConfig.Timeout = tokenReviewTimeout

	clientSet, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset for auth: %w", err)
	}

	return clientSet, nil
}

// stringSliceFromConfig reads a JSON array of strings out of the ConfigMap.
//
// A missing key and an explicit null are errors, not empty lists. Silently
// reading either as "no cross-node publishers" would start the connector in a
// configuration where every cluster-scoped monitor is pinned to one node and
// its events rejected — a failure that surfaces far from its cause. Only an
// explicit [] says that on purpose.
func stringSliceFromConfig(config map[string]any, key string) ([]string, error) {
	raw, present := config[key]
	if !present {
		return nil, fmt.Errorf("%s is not set: it must be a list of canonical "+
			"ServiceAccount usernames, or an explicit empty list to declare that no "+
			"publisher may name other nodes", key)
	}

	if raw == nil {
		return nil, fmt.Errorf("%s is null: use an explicit empty list to declare "+
			"that no publisher may name other nodes", key)
	}

	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a list of strings, got %T", key, raw)
	}

	result := make([]string, 0, len(items))

	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be a list of strings, found element of type %T", key, item)
		}

		result = append(result, s)
	}

	return result, nil
}

// nodeBindingEnabled reports whether node binding is on.
//
// The flag must be present and must be exactly true or false. It is not
// defaulted in either direction: guessing "on" would silently enforce against
// a config that never asked for it, and guessing "off" would silently drop the
// check that keeps a publisher on one node from reporting faults about
// another. A ConfigMap that predates the flag is missing the audience and
// allowlist too, so it cannot work either way — saying so plainly is more
// useful than inferring an answer.
//
//	true / "true"     -> enabled
//	false / "false"   -> disabled
//	absent or other   -> refuse to start
//
// Values arrive as JSON, where the chart quotes them; an unquoted bool from a
// hand-edited ConfigMap is accepted too.
func nodeBindingEnabled(config map[string]any) (bool, error) {
	const key = "enableNodeBindingAuth"

	raw, present := config[key]
	if !present {
		return false, fmt.Errorf(
			"%s is not set: it must be true or false. A ConfigMap without it "+
				"predates this platform-connector version and is missing AuthAudience and "+
				"AuthCrossNodeServiceAccounts as well; upgrade the chart rather than "+
				"relying on a default", key)
	}

	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		switch v {
		case True:
			return true, nil
		case "false":
			return false, nil
		}
	}

	return false, fmt.Errorf("%s must be true or false, got %#v", key, raw)
}

// newTokenValidator builds the TokenReview validator used to authenticate
// token-presenting publishers. It answers authentication only.
//
// The cross-node allowlist is deliberately not applied here: an authenticated
// identity not entitled to cross-node scope is pinned to this node by the
// interceptor rather than rejected, so it can still report the node it runs on.
// Authorization lives in platform-connectors/pkg/auth.
func newTokenValidator(audience string, kubeconfigPath string) (*grpcauth.Validator, error) {
	clientSet, err := newK8sClientset(kubeconfigPath)
	if err != nil {
		return nil, err
	}

	validator, err := grpcauth.NewValidator(clientSet, audience)
	if err != nil {
		return nil, fmt.Errorf("failed to build token validator: %w", err)
	}

	return validator, nil
}

// initializeAuthInterceptor builds the node-binding interceptor that keeps a
// publisher on one node from submitting health events naming another node. It
// returns nil when node binding is explicitly disabled, in which case any
// caller may name any node; that is not a supported production configuration.
func initializeAuthInterceptor(
	ctx context.Context,
	config map[string]any,
	kubeconfigPath string,
) (grpc.UnaryServerInterceptor, error) {
	enabled, err := nodeBindingEnabled(config)
	if err != nil {
		return nil, err
	}

	if !enabled {
		slog.WarnContext(ctx, "Node-binding authentication is DISABLED. Any caller able to reach the "+
			"platform-connector socket may submit health events naming any node in the cluster.")

		return nil, nil
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return nil, fmt.Errorf("NODE_NAME environment variable is required when node-binding auth is enabled")
	}

	crossNodeSAs, err := stringSliceFromConfig(config, "AuthCrossNodeServiceAccounts")
	if err != nil {
		return nil, err
	}

	// Every monitor may present a token, not only the cross-node ones, so the
	// audience is required whenever node binding is on: without it no token can
	// be verified and the node claims this check rests on are unreadable.
	audience, _ := config["AuthAudience"].(string)
	if audience == "" {
		return nil, fmt.Errorf("AuthAudience must be set when node-binding auth is enabled")
	}

	validator, err := newTokenValidator(audience, kubeconfigPath)
	if err != nil {
		return nil, err
	}

	mode, err := authMode(config)
	if err != nil {
		return nil, fmt.Errorf("parse AuthMode: %w", err)
	}

	failOpenOnUnavailable, err := boolFromConfig(config, "AuthFailOpenOnUnavailable", false)
	if err != nil {
		return nil, fmt.Errorf("parse AuthFailOpenOnUnavailable: %w", err)
	}

	interceptor, err := auth.NewNodeBindingInterceptor(auth.Config{
		NodeName:                 nodeName,
		Validator:                validator,
		CrossNodeServiceAccounts: crossNodeSAs,
		Mode:                     mode,
		FailOpenOnUnavailable:    failOpenOnUnavailable,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build node-binding interceptor: %w", err)
	}

	return interceptor, nil
}

// authMode reads the node-binding enforcement mode from config. Absent means
// auth.ModeEnforce, so that a ConfigMap that predates this setting keeps
// today's behavior rather than silently switching to audit-only.
func authMode(config map[string]any) (auth.Mode, error) {
	const key = "AuthMode"

	raw, present := config[key]
	if !present {
		return auth.ModeEnforce, nil
	}

	v, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string, got %#v", key, raw)
	}

	switch auth.Mode(v) {
	case auth.ModeEnforce, auth.ModeAudit:
		return auth.Mode(v), nil
	default:
		return "", fmt.Errorf("%s must be %q or %q, got %q", key, auth.ModeEnforce, auth.ModeAudit, v)
	}
}

// boolFromConfig reads a strict boolean config value, defaulting when absent.
// Values arrive as JSON, where the chart quotes them; an unquoted bool from a
// hand-edited ConfigMap is accepted too.
func boolFromConfig(config map[string]any, key string, def bool) (bool, error) {
	raw, present := config[key]
	if !present {
		return def, nil
	}

	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		switch v {
		case True:
			return true, nil
		case "false":
			return false, nil
		}
	}

	return false, fmt.Errorf("%s must be true or false, got %#v", key, raw)
}

func initializeGRPCSinkConnector(
	ctx context.Context,
	config map[string]any,
) (*grpcsink.GRPCSinkConnector, error) {
	ringBuffer := ringbuffer.NewRingBuffer("grpcSink", ctx)
	server.InitializeAndAttachRingBufferForConnectors(ringBuffer)

	target, ok := config["GRPCSinkTarget"].(string)
	if !ok || target == "" {
		return nil, fmt.Errorf("grpcSinkTarget not configured or empty")
	}

	maxRetriesInt64, ok := config["GRPCSinkConnectorMaxRetries"].(int64)
	if !ok {
		return nil, fmt.Errorf("failed to convert GRPCSinkConnectorMaxRetries to int: %v",
			config["GRPCSinkConnectorMaxRetries"])
	}

	maxRetries := int(maxRetriesInt64)

	// Optional SA token auth — empty string disables it
	tokenPath, _ := config["GRPCSinkTokenPath"].(string)

	connector, err := grpcsink.InitializeGRPCSinkConnector(ringBuffer, target, maxRetries, tokenPath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize gRPC sink connector: %w", err)
	}

	go connector.FetchAndProcessHealthMetric(ctx)

	return connector, nil
}

func initializeConnectors(
	ctx context.Context,
	config map[string]any,
	stopCh chan struct{},
	databaseClientCertMountPath string,
	kubeconfigPath string,
) (*ringbuffer.RingBuffer, *store.DatabaseStoreConnector, *grpcsink.GRPCSinkConnector, error) {
	var (
		k8sRingBuffer     *ringbuffer.RingBuffer
		storeConnector    *store.DatabaseStoreConnector
		grpcSinkConnector *grpcsink.GRPCSinkConnector
		err               error
	)

	if config["enableK8sPlatformConnector"] == True {
		k8sRingBuffer, err = initializeK8sConnector(ctx, config, stopCh, kubeconfigPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to initialize K8s connector: %w", err)
		}
	}

	// Keep the legacy config key name for backward compatibility with existing ConfigMaps
	if config["enableMongoDBStorePlatformConnector"] == True || config["enablePostgresDBStorePlatformConnector"] == True {
		storeConnector, err = initializeDatabaseStoreConnector(ctx, config, databaseClientCertMountPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to initialize database store connector: %w", err)
		}
	}

	if config["enableGRPCSinkConnector"] == True {
		grpcSinkConnector, err = initializeGRPCSinkConnector(ctx, config)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to initialize gRPC sink connector: %w", err)
		}
	}

	return k8sRingBuffer, storeConnector, grpcSinkConnector, nil
}

func cleanupResources(
	ctx context.Context,
	socket string,
	lis net.Listener,
	k8sRingBuffer *ringbuffer.RingBuffer,
	storeConnector *store.DatabaseStoreConnector,
	grpcSinkConnector *grpcsink.GRPCSinkConnector,
) error {
	if lis != nil {
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

	if storeConnector != nil {
		storeConnector.ShutdownRingBuffer(ctx)

		disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer disconnectCancel()

		if err := storeConnector.Disconnect(disconnectCtx); err != nil {
			return fmt.Errorf("error disconnecting database store connector: %w", err)
		}
	}

	if grpcSinkConnector != nil {
		grpcSinkConnector.ShutdownRingBuffer()

		if err := grpcSinkConnector.Close(); err != nil {
			slog.Error("Failed to close gRPC sink connector", "error", err)
		}
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
	k8sRingBuffer *ringbuffer.RingBuffer,
	storeConnector *store.DatabaseStoreConnector,
	grpcSinkConnector *grpcsink.GRPCSinkConnector,
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

	if err := cleanupResources(gCtx, cfg.socket, lis, k8sRingBuffer, storeConnector, grpcSinkConnector); err != nil {
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

func run() error {
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

	config, err := loadConfig(cfg.configFilePath)
	if err != nil {
		return err
	}

	k8sRingBuffer, storeConnector, grpcSinkConnector, err := initializeConnectors(ctx,
		config, stopCh, cfg.databaseClientCertMountPath, cfg.kubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to initialize connectors: %w", err)
	}

	pipeline, err := initializePipeline(ctx, config, pipeline.Options{
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

	lis, err := startGRPCServer(ctx, cfg.socket, pipeline, authInterceptor)
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
		return handleShutdown(gCtx, sigs, stopCh, cfg, lis, k8sRingBuffer, storeConnector, grpcSinkConnector, cancel)
	})

	return g.Wait()
}
