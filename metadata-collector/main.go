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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/collector"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/mapper"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/nvml"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/writer"
)

const (
	defaultAgentName              = "metadata-collector"
	defaultOutputPath             = "/var/lib/nvsentinel/gpu_metadata.json"
	defaultPodDeviceMonitorPeriod = time.Second * 30
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"

	outputPath = flag.String("output-path", defaultOutputPath, "Path to write the GPU metadata JSON file")
)

func main() {
	flag.Parse()

	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting metadata-collector", "version", version, "commit", commit, "date", date)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	if err := runCollector(ctx); err != nil {
		slog.Error("Metadata collector failed", "error", err)
		cancel()
		os.Exit(1)
	}

	slog.Info("Metadata collector completed successfully, starting pod device mapper")

	if err := runMapper(ctx); err != nil {
		slog.Error("Pod device mapper failed", "error", err)
		cancel()
		os.Exit(1)
	}

	slog.Info("Pod device mapper completed successfully")
}

func runMapper(ctx context.Context) error {
	podDeviceMapper, err := mapper.NewPodDeviceMapper(ctx)
	if err != nil {
		return fmt.Errorf("could not create mapper: %w", err)
	}

	ticker := time.NewTicker(defaultPodDeviceMonitorPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			numUpdates, err := podDeviceMapper.UpdatePodDevicesAnnotations()
			if err != nil {
				return fmt.Errorf("could not update mapper pod devices annotations: %w", err)
			}

			slog.Info("Device mapper pod annotation updates", "podCount", numUpdates)
		}
	}
}

func runCollector(ctx context.Context) error {
	slog.Info("Initializing NVML")

	nvmlWrapper := &nvml.NVMLWrapper{}
	if err := nvmlWrapper.Init(); err != nil {
		return fmt.Errorf("failed to initialize NVML: %w", err)
	}

	defer func() {
		if err := nvmlWrapper.Shutdown(); err != nil {
			slog.Error("Failed to shutdown NVML", "error", err)
		}
	}()

	slog.Info("Collecting GPU metadata")

	metadataCollector := collector.NewCollector(nvmlWrapper)

	metadata, err := metadataCollector.Collect(ctx)
	if err != nil {
		return fmt.Errorf("failed to collect GPU metadata: %w", err)
	}

	slog.Info("GPU metadata collected",
		"node", metadata.NodeName,
		"gpu_count", len(metadata.GPUs),
		"nvswitch_count", len(metadata.NVSwitches),
	)

	slog.Info("Writing metadata to file", "output_path", *outputPath)

	metadataWriter, err := writer.NewWriter(*outputPath)
	if err != nil {
		return fmt.Errorf("failed to create metadata writer: %w", err)
	}

	if err := metadataWriter.Write(metadata); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	slog.Info("Successfully wrote GPU metadata", "output_path", *outputPath)

	return nil
}
