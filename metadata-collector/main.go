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

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/collector"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/mapper"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/nvml"
	"github.com/nvidia/nvsentinel/metadata-collector/pkg/writer"
)

const (
	defaultAgentName              = "metadata-collector"
	defaultOutputPath             = "/var/lib/nvsentinel/gpu_metadata.json"
	defaultPodDeviceMonitorPeriod = time.Second * 30

	// At the 30s poll period this is 5 minutes of a collector that cannot do its job: long
	// enough to ride out a credential rotation or a kubelet restart, short enough to still be
	// a loud failure.
	defaultMaxConsecutivePodMapperFailures = 10

	// Matches every other NVSentinel component, and the chart's PodMonitor scrapes a container
	// port named "metrics" rather than a number.
	defaultMetricsPort = 2112
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"

	outputPath = flag.String("output-path", defaultOutputPath, "Path to write the GPU metadata JSON file")

	maxConsecutivePodMapperFailures = flag.Int("pod-mapper-max-consecutive-failures",
		defaultMaxConsecutivePodMapperFailures,
		"Consecutive pod device mapper poll failures tolerated before exiting non-zero. Minimum 1.")

	metricsPort = flag.Int("metrics-port", defaultMetricsPort,
		"Port for the Prometheus metrics endpoint. 0 disables it, which matters here because this "+
			"runs with hostNetwork and so binds on the node.")
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

	// The mapper loops until the context ends, so the metrics endpoint has to run alongside it
	// rather than after it. Either goroutine returning cancels the other, which keeps the
	// threshold's non-zero exit intact.
	group, groupCtx := errgroup.WithContext(ctx)

	if *metricsPort > 0 {
		group.Go(func() error {
			err := server.NewServer(
				server.WithPort(*metricsPort),
				server.WithPrometheusMetrics(),
			).Serve(groupCtx)
			// Deliberately not returned. The port is on the host network here, so a collision
			// with a node service (kube-vip also uses 2112) would otherwise crash-loop this
			// DaemonSet fleet-wide — the failure this component's whole issue is about. Losing
			// metrics is worth strictly less than losing the collector.
			if err != nil {
				slog.Error("Metrics endpoint stopped; continuing without it", "error", err)
			}

			return nil
		})
	}

	group.Go(func() error {
		return runMapper(groupCtx, newPodMapperMetrics(prometheus.DefaultRegisterer))
	})

	if err := group.Wait(); err != nil {
		slog.Error("Pod device mapper failed", "error", err)
		cancel()
		os.Exit(1)
	}

	slog.Info("Pod device mapper completed successfully")
}

func runMapper(ctx context.Context, metrics *podMapperMetrics) error {
	podDeviceMapper, err := mapper.NewPodDeviceMapper(ctx)
	if err != nil {
		return fmt.Errorf("could not create mapper: %w", err)
	}

	ticker := time.NewTicker(defaultPodDeviceMonitorPeriod)
	defer ticker.Stop()

	return pollPodDevices(ctx, podDeviceMapper, ticker.C, *maxConsecutivePodMapperFailures, metrics)
}

// pollPodDevices updates pod device annotations on every tick, returning only when ctx is done
// or maxConsecutiveFailures polls have failed in a row.
//
// The threshold is the point: a single failed poll used to return, which main turns into
// os.Exit(1), so one rotated credential restarted every pod in the fleet at once. Failing
// loudly is still wanted, just once the failures look permanent rather than transient.
func pollPodDevices(ctx context.Context, podDeviceMapper mapper.PodDeviceMapper,
	ticks <-chan time.Time, maxConsecutiveFailures int, metrics *podMapperMetrics) error {
	if maxConsecutiveFailures < 1 {
		return fmt.Errorf("pod-mapper-max-consecutive-failures must be at least 1, got %d", maxConsecutiveFailures)
	}

	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			numUpdates, err := podDeviceMapper.UpdatePodDevicesAnnotations()
			if err != nil {
				consecutiveFailures++
				// Recorded before the threshold check, so the final streak is visible on the
				// scrape that precedes the exit rather than being lost with the process.
				metrics.recordFailure(consecutiveFailures)

				if consecutiveFailures >= maxConsecutiveFailures {
					return fmt.Errorf("could not update mapper pod devices annotations, %d consecutive failures: %w",
						consecutiveFailures, err)
				}

				slog.Error("Could not update mapper pod devices annotations, retrying on the next tick",
					"consecutiveFailures", consecutiveFailures,
					"threshold", maxConsecutiveFailures,
					"error", err)

				continue
			}

			consecutiveFailures = 0

			metrics.recordSuccess()

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

	metadataCollector := collector.NewCollector(nvmlWrapper, collector.NewDefaultTopoCollector())

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
