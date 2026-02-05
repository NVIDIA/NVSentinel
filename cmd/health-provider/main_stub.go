//go:build !nvml

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

// Command health-provider stub for building without NVML support.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/klog/v2"

	"github.com/nvidia/nvsentinel/pkg/healthprovider"
)

func main() {
	klog.InitFlags(nil)

	cfg, nvmlCfg := parseFlags()
	flag.Parse()

	logger := klog.Background()
	logger.Info("Starting health-provider (stub build - no NVML support)",
		"serverAddress", cfg.ServerAddress,
		"providerID", cfg.ProviderID,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("Received signal, shutting down", "signal", sig)
		cancel()
	}()

	provider := healthprovider.New(cfg, logger)

	// Register stub NVML source (will fail to start but won't crash)
	if nvmlCfg.HealthCheckEnabled {
		nvmlSource := healthprovider.NewNVMLSource(nvmlCfg, logger)
		provider.RegisterSource(nvmlSource)
	}

	if err := provider.Run(ctx); err != nil {
		logger.Error(err, "Provider failed")
		os.Exit(1)
	}

	logger.Info("Health provider shutdown complete")
}

func parseFlags() (healthprovider.Config, healthprovider.NVMLSourceConfig) {
	cfg := healthprovider.DefaultConfig()
	nvmlCfg := healthprovider.DefaultNVMLSourceConfig()

	flag.StringVar(&cfg.ServerAddress, "server-address", cfg.ServerAddress,
		"Address of device-api-server gRPC endpoint")
	flag.StringVar(&cfg.ProviderID, "provider-id", cfg.ProviderID,
		"Unique identifier for this provider instance")
	flag.StringVar(&cfg.NodeName, "node-name", cfg.NodeName,
		"Kubernetes node name")
	flag.IntVar(&cfg.HealthCheckPort, "health-port", cfg.HealthCheckPort,
		"HTTP port for health probes")
	flag.StringVar(&nvmlCfg.DriverRoot, "driver-root", nvmlCfg.DriverRoot,
		"Root path for NVIDIA driver libraries")
	flag.BoolVar(&nvmlCfg.HealthCheckEnabled, "enable-nvml", nvmlCfg.HealthCheckEnabled,
		"Enable NVML GPU health monitoring")

	flag.Parse()

	if addr := os.Getenv("PROVIDER_SERVER_ADDRESS"); addr != "" {
		cfg.ServerAddress = addr
	}
	if id := os.Getenv("PROVIDER_ID"); id != "" {
		cfg.ProviderID = id
	}
	if name := os.Getenv("NODE_NAME"); name != "" && cfg.NodeName == "" {
		cfg.NodeName = name
	}
	if root := os.Getenv("NVML_DRIVER_ROOT"); root != "" {
		nvmlCfg.DriverRoot = root
	}

	return cfg, nvmlCfg
}
