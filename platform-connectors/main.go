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

	"github.com/nvidia/nvsentinel/commons/pkg/flags"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	srv "github.com/nvidia/nvsentinel/commons/pkg/server"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/nodemetadata"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"
	"golang.org/x/sync/errgroup"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/json"
	k8s "k8s.io/client-go/kubernetes"
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
	logger.SetDefaultStructuredLogger("platform-connectors", version)
	slog.Info("Starting platform-connectors", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Platform connectors exited with error", "error", err)
		os.Exit(1)
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
) (*ringbuffer.RingBuffer, nodemetadata.Processor, error) {
	k8sRingBuffer := ringbuffer.NewRingBuffer("kubernetes", ctx)
	server.InitializeAndAttachRingBufferForConnectors(k8sRingBuffer)

	qpsTemp, ok := config["K8sConnectorQps"].(float64)
	if !ok {
		return nil, nil, fmt.Errorf("failed to convert K8sConnectorQps to float: %v", config["K8sConnectorQps"])
	}

	qps := float32(qpsTemp)

	burst, ok := config["K8sConnectorBurst"].(int64)
	if !ok {
		return nil, nil, fmt.Errorf("failed to convert K8sConnectorBurst to int: %v", config["K8sConnectorBurst"])
	}

	k8sConnector, clientset, err := kubernetes.InitializeK8sConnector(ctx, k8sRingBuffer, qps, int(burst), stopCh)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize K8sConnector: %w", err)
	}

	go k8sConnector.FetchAndProcessHealthMetric(ctx)

	// Node metadata enrichment is optional - failures are logged but don't abort startup
	processor, err := initializeNodeMetadataProcessor(ctx, config, clientset)
	if err != nil {
		slog.Warn("Failed to initialize node metadata processor, continuing without enrichment", "error", err)
	}

	return k8sRingBuffer, processor, nil
}

func initializeDatabaseStoreConnector(
	ctx context.Context,
	databaseClientCertMountPath string,
) (*store.DatabaseStoreConnector, error) {
	ringBuffer := ringbuffer.NewRingBuffer("databaseStore", ctx)
	server.InitializeAndAttachRingBufferForConnectors(ringBuffer)

	storeConnector, err := store.InitializeDatabaseStoreConnector(ctx, ringBuffer, databaseClientCertMountPath)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database store connector: %w", err)
	}

	go storeConnector.FetchAndProcessHealthMetric(ctx)

	return storeConnector, nil
}

func initializeNodeMetadataProcessor(
	ctx context.Context,
	config map[string]interface{},
	clientset k8s.Interface,
) (nodemetadata.Processor, error) {
	cfg, err := nodemetadata.NewConfigFromMap(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create node metadata config: %w", err)
	}

	if !cfg.Enabled {
		slog.Info("Node metadata enrichment is disabled")

		return nil, nil
	}

	processor, err := nodemetadata.NewProcessor(ctx, nodemetadata.PlatformKubernetes, cfg, clientset)
	if err != nil {
		return nil, fmt.Errorf("failed to create node metadata processor: %w", err)
	}

	slog.Info("Node metadata processor initialized successfully")

	return processor, nil
}

func startGRPCServer(ctx context.Context, socket string, processor nodemetadata.Processor) (net.Listener, error) {
	err := os.Remove(socket)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove existing socket: %w", err)
	}

	lc := &net.ListenConfig{}

	lis, err := lc.Listen(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on unix socket %s: %w", socket, err)
	}

	var opts []grpc.ServerOption

	grpcServer := grpc.NewServer(opts...)
	pb.RegisterPlatformConnectorServer(grpcServer, &server.PlatformConnectorServer{
		Processor: processor,
	})

	go func() {
		err = grpcServer.Serve(lis)
		if err != nil {
			slog.Error("Not able to accept incoming connections", "error", err)
			os.Exit(1)
		}
	}()

	return lis, nil
}

func initializeConnectors(
	ctx context.Context,
	config map[string]interface{},
	stopCh chan struct{},
	databaseClientCertMountPath string,
) (*ringbuffer.RingBuffer, *store.DatabaseStoreConnector, nodemetadata.Processor, error) {
	var (
		k8sRingBuffer  *ringbuffer.RingBuffer
		storeConnector *store.DatabaseStoreConnector
		processor      nodemetadata.Processor
		err            error
	)

	if config["enableK8sPlatformConnector"] == True {
		k8sRingBuffer, processor, err = initializeK8sConnector(ctx, config, stopCh)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to initialize K8s connector: %w", err)
		}
	}

	// Keep the legacy config key name for backward compatibility with existing ConfigMaps
	if config["enableMongoDBStorePlatformConnector"] == True {
		storeConnector, err = initializeDatabaseStoreConnector(ctx, databaseClientCertMountPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to initialize database store connector: %w", err)
		}
	}

	return k8sRingBuffer, storeConnector, processor, nil
}

func cleanupResources(
	socket string,
	lis net.Listener,
	k8sRingBuffer *ringbuffer.RingBuffer,
	storeConnector *store.DatabaseStoreConnector,
) error {
	if lis != nil {
		if k8sRingBuffer != nil {
			k8sRingBuffer.ShutDownHealthMetricQueue()
		}

		if err := lis.Close(); err != nil {
			slog.Error("Failed to close listener", "error", err)
		}

		if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
			slog.Error("Failed to remove socket file", "error", err)
		}
	}

	if storeConnector != nil {
		disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer disconnectCancel()

		if err := storeConnector.Disconnect(disconnectCtx); err != nil {
			return fmt.Errorf("error disconnecting database store connector: %w", err)
		}
	}

	return nil
}

func run() error {
	socket := flag.String("socket", "", "unix socket path")
	configFilePath := flag.String("config", "/etc/config/config.json", "path to the config file")
	metricsPort := flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	// Register database certificate flags using common package
	certConfig := flags.RegisterDatabaseCertFlags()

	flag.Parse()

	// Resolve the certificate path using common logic
	databaseClientCertMountPath := certConfig.ResolveCertPath()

	if *socket == "" {
		return fmt.Errorf("socket is not present")
	}

	sigs := make(chan os.Signal, 1)
	stopCh := make(chan struct{})

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	defer cancel()

	config, err := loadConfig(*configFilePath)
	if err != nil {
		return err
	}

	k8sRingBuffer, storeConnector, processor, err := initializeConnectors(ctx, config, stopCh, databaseClientCertMountPath)
	if err != nil {
		return err
	}

	lis, err := startGRPCServer(ctx, *socket, processor)
	if err != nil {
		return err
	}

	portInt, err := strconv.Atoi(*metricsPort)
	if err != nil {
		return fmt.Errorf("invalid metrics port: %w", err)
	}

	srv := srv.NewServer(
		srv.WithPort(portInt),
		srv.WithPrometheusMetrics(),
		srv.WithSimpleHealth(),
	)

	g, gCtx := errgroup.WithContext(ctx)

	// Metrics server failures are logged but do NOT terminate the service
	g.Go(func() error {
		slog.Info("Starting metrics server", "port", portInt)

		if err := srv.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	g.Go(func() error {
		slog.Info("Waiting for SIGINT/SIGTERM or context cancellation")
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

		defer func() {
			// Always stop signal delivery and close channel to avoid leaks.
			signal.Stop(sigs)
			close(sigs)
		}()

		select {
		case sig := <-sigs:
			slog.Info("Received signal", "signal", sig)
		case <-gCtx.Done():
			slog.Info("Context cancelled, initiating shutdown")
		}

		close(stopCh)

		if err := cleanupResources(*socket, lis, k8sRingBuffer, storeConnector); err != nil {
			return err
		}

		// Also cancel the root to propagate shutdown to any other goroutines.
		cancel()

		return nil
	})

	return g.Wait()
}
