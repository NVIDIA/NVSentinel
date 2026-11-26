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
	"slices"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/collector"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/nvml"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/writer"
)

const (
	defaultAgentName  = "metadata-collector"
	defaultOutputPath = "/var/lib/nvsentinel/gpu_metadata.json"

	// Matches every other NVSentinel component, and the chart's PodMonitor scrapes a container
	// port named "metrics" rather than a number.
	defaultMetricsPort = 2112
)

// movedFlags belonged to the pod device mapper, which runs in pod-device-mapper since ADR-072. metadata-collector
// still accepts them, so that an older pod spec or host setup keeps starting, and ignores them.
var movedFlags = []string{
	"kubeconfig", "kubelet-kubeconfig", "config", "pod-mapper-max-consecutive-failures",
}

// movedBoolFlag is registered as a boolean, so that a bare --pod-mapper-enabled still parses.
const movedBoolFlag = "pod-mapper-enabled"

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"

	outputPath = flag.String("output-path", defaultOutputPath, "Path to write the GPU metadata JSON file")

	metricsPort = flag.Int("metrics-port", defaultMetricsPort,
		"Port for the Prometheus metrics endpoint. 0 disables it, which matters here because this "+
			"runs with hostNetwork and so binds on the node.")
)

func init() {
	for _, name := range movedFlags {
		flag.String(name, "", "Ignored. The pod device mapper runs in pod-device-mapper.")
	}

	flag.Bool(movedBoolFlag, true, "Ignored. The pod device mapper runs in pod-device-mapper.")
}

func main() {
	flag.Parse()

	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting metadata-collector", "version", version, "commit", commit, "date", date)

	if set := setMovedFlags(flag.CommandLine); len(set) > 0 {
		slog.Warn("Ignoring the flags of the pod device mapper, which runs in pod-device-mapper", "flags", set)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	collectionCompleted := newCollectionCompletedGauge(prometheus.DefaultRegisterer)

	// The metrics endpoint starts before collection, not after it. Collection calls into the GPU
	// driver, which can block indefinitely when the driver is wedged, and a pod stuck there should
	// still be scraped and show the stall through collectionCompleted rather than go dark.
	group, groupCtx := errgroup.WithContext(ctx)

	if *metricsPort > 0 {
		group.Go(func() error {
			err := server.NewServer(
				server.WithPort(*metricsPort),
				server.WithPrometheusMetrics(),
			).Serve(groupCtx)
			// Deliberately not returned. The port is on the host network here, so a collision
			// with a node service (kube-vip also uses 2112) would otherwise crash-loop this
			// DaemonSet fleet-wide. Losing metrics is worth strictly less than losing the collector.
			if err != nil {
				slog.Error("Metrics endpoint stopped; continuing without it", "error", err)
			}

			return nil
		})
	}

	if _, err := runCollector(ctx); err != nil {
		slog.Error("Metadata collector failed", "error", err)
		cancel()
		os.Exit(1)
	}

	collectionCompleted.SetToCurrentTime()

	slog.Info("Metadata collector completed successfully")

	// The process stays up after the collection: it runs as a DaemonSet, so returning would restart it in a loop.
	<-ctx.Done()

	if err := group.Wait(); err != nil {
		slog.Error("Metrics endpoint failed", "error", err)
	}

	cancel()
}

// setMovedFlags returns the moved flags that the command line sets.
func setMovedFlags(flags *flag.FlagSet) []string {
	var set []string

	flags.Visit(func(f *flag.Flag) {
		if f.Name == movedBoolFlag || slices.Contains(movedFlags, f.Name) {
			set = append(set, f.Name)
		}
	})

	return set
}

func runCollector(ctx context.Context) (*model.GPUMetadata, error) {
	slog.Info("Initializing NVML")

	nvmlWrapper := &nvml.NVMLWrapper{}
	if err := nvmlWrapper.Init(); err != nil {
		return nil, fmt.Errorf("failed to initialize NVML: %w", err)
	}

	defer func() {
		if err := nvmlWrapper.Shutdown(); err != nil {
			slog.Error("Failed to shutdown NVML", "error", err)
		}
	}()

	slog.Info("Collecting GPU metadata")

	metadataCollector := collector.NewCollector(nvmlWrapper, collector.NewDefaultTopoCollector())

	metadata, err := metadataCollector.Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to collect GPU metadata: %w", err)
	}

	slog.Info("GPU metadata collected",
		"node", metadata.NodeName,
		"gpu_count", len(metadata.GPUs),
		"nvswitch_count", len(metadata.NVSwitches),
	)

	slog.Info("Writing metadata to file", "output_path", *outputPath)

	metadataWriter, err := writer.NewWriter(*outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create metadata writer: %w", err)
	}

	if err := metadataWriter.Write(metadata); err != nil {
		return nil, fmt.Errorf("failed to write metadata: %w", err)
	}

	slog.Info("Successfully wrote GPU metadata", "output_path", *outputPath)

	return metadata, nil
}
