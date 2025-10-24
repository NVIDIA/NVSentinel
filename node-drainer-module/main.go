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
	"os/signal"
	"syscall"

	"github.com/nvidia/nvsentinel/node-drainer-module/pkg/initializer"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Initialize klog flags to allow command-line control (e.g., -v=3)
	klog.InitFlags(nil)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var metricsPort = flag.String("metrics-port", "2112", "port to expose Prometheus metrics on")

	var mongoClientCertMountPath = flag.String("mongo-client-cert-mount-path", "/etc/ssl/mongo-client",
		"path where the mongodb client cert is mounted")

	var kubeconfigPath = flag.String("kubeconfig-path", "", "path to kubeconfig file")

	var tomlConfigPath = flag.String("config-path", "/etc/config/config.toml",
		"path where the node drainer config file is present")

	var dryRun = flag.Bool("dry-run", false, "flag to run node drainer module in dry-run mode")

	flag.Parse()

	logger := textlogger.NewLogger(textlogger.NewConfig()).WithValues(
		"version", version,
		"module", "node-drainer-module",
	)

	klog.SetLogger(logger)
	klog.InfoS("Starting node-drainer-module", "version", version, "commit", commit, "date", date)
	defer klog.Flush()

	klog.Infof("Mongo client cert path: %s", *mongoClientCertMountPath)

	params := initializer.InitializationParams{
		MongoClientCertMountPath: *mongoClientCertMountPath,
		KubeconfigPath:           *kubeconfigPath,
		TomlConfigPath:           *tomlConfigPath,
		MetricsPort:              *metricsPort,
		DryRun:                   *dryRun,
	}

	components, err := initializer.InitializeAll(ctx, params)
	if err != nil {
		klog.Fatalf("Initialization failed: %v", err)
	}

	// Informers must sync before processing events
	klog.Info("Starting Kubernetes informers")

	if err := components.Informers.Run(ctx); err != nil {
		klog.Fatalf("Failed to start informers: %v", err)
	}

	klog.Info("Kubernetes informers started and synced")

	klog.Info("Starting queue worker")
	components.QueueManager.Start(ctx)

	klog.Info("Starting MongoDB event watcher")

	criticalError := make(chan error)

	go func() {
		if err := components.EventWatcher.Start(ctx); err != nil {
			klog.Errorf("Event watcher failed: %v", err)

			criticalError <- err
		}
	}()

	klog.Info("All components started successfully")

	if err := initializer.StartMetricsServer(*metricsPort); err != nil {
		klog.Errorf("Failed to start metrics server: %v", err)
	}

	select {
	case <-ctx.Done():
	case err := <-criticalError:
		klog.Errorf("Critical component failure: %v", err)
		stop() // Cancel context to trigger shutdown
	}

	klog.Info("Shutting down node drainer")

	if err := components.EventWatcher.Stop(); err != nil {
		klog.Errorf("Failed to stop event watcher: %v", err)
	}

	components.QueueManager.Shutdown()

	klog.Info("Node drainer stopped")
}
