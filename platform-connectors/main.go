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

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/flags"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	srv "github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/grpcsink"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"
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

func loadConfig(configFilePath string) (map[string]interface{}, error) {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read platform-connector-configmap with err %w", err)
	}

	result := make(map[string]interface{})

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
	config map[string]interface{},
	stopCh chan struct{},
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

	k8sConnectorCfg := kubernetes.K8sConnectorConfig{
		MaxNodeConditionMessageLength: maxNodeConditionMessageLength,
		CompactedHealthEventMsgLen:    compactedEventMsgLen,
	}

	k8sConnector, _, err := kubernetes.InitializeK8sConnector(ctx, k8sRingBuffer, qps, int(burst),
		stopCh, k8sConnectorCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize K8sConnector: %w", err)
	}

	go k8sConnector.FetchAndProcessHealthMetric(ctx)

	return k8sRingBuffer, nil
}

func initializeDatabaseStoreConnector(
	ctx context.Context,
	config map[string]interface{},
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

func initializePipeline(ctx context.Context, config map[string]any) (*pipeline.Pipeline, error) {
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

	return pipeline.NewFromConfigs(ctx, transformerConfigs)
}

func startGRPCServer(
	ctx context.Context,
	socket string,
	pipeline *pipeline.Pipeline,
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

	// Set socket permissions to allow other processes to connect (0666 = rw-rw-rw-)
	if err := os.Chmod(socket, 0o666); err != nil {
		return nil, fmt.Errorf("failed to set socket permissions: %w", err)
	}

	slog.InfoContext(ctx, "gRPC server socket created successfully", "socket", socket, "permissions", "0666")

	var opts []grpc.ServerOption

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

func initializeGRPCSinkConnector(
	ctx context.Context,
	config map[string]interface{},
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
	config map[string]interface{},
	stopCh chan struct{},
	databaseClientCertMountPath string,
) (*ringbuffer.RingBuffer, *store.DatabaseStoreConnector, *grpcsink.GRPCSinkConnector, error) {
	var (
		k8sRingBuffer     *ringbuffer.RingBuffer
		storeConnector    *store.DatabaseStoreConnector
		grpcSinkConnector *grpcsink.GRPCSinkConnector
		err               error
	)

	if config["enableK8sPlatformConnector"] == True {
		k8sRingBuffer, err = initializeK8sConnector(ctx, config, stopCh)
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
}

func parseFlags() (*platformConnectorConfig, error) {
	socket := flag.String("socket", "", "unix socket path")
	configFilePath := flag.String("config", "/etc/config/config.json", "path to the config file")
	metricsPort := flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

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

	config, err := loadConfig(cfg.configFilePath)
	if err != nil {
		return err
	}

	k8sRingBuffer, storeConnector, grpcSinkConnector, err := initializeConnectors(ctx,
		config, stopCh, cfg.databaseClientCertMountPath)
	if err != nil {
		return fmt.Errorf("failed to initialize connectors: %w", err)
	}

	pipeline, err := initializePipeline(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to initialize pipeline: %w", err)
	}

	lis, err := startGRPCServer(ctx, cfg.socket, pipeline)
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
