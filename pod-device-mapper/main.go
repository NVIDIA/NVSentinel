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

// pod-device-mapper writes the dgxc.nvidia.com/devices annotation, which lists the devices of each pod on its node.
// node-drainer reads the annotation to find the pods that use a device in a partial drain (ADR-071, ADR-072).
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
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/pod-device-mapper/pkg/config"
	"github.com/nvidia/nvsentinel/pod-device-mapper/pkg/mapper"
)

const (
	defaultAgentName              = "pod-device-mapper"
	defaultPodDeviceMonitorPeriod = time.Second * 30

	// At the 30s poll period this is 5 minutes of a mapper that cannot do its job: long
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

	kubeconfigPath = flag.String("kubeconfig", "",
		"Kubeconfig for Kubernetes API access; empty uses in-cluster auth")
	kubeletKubeconfigPath = flag.String("kubelet-kubeconfig", "",
		"Kubeconfig for the kubelet HTTPS endpoint; empty uses KUBELET_HOST and the pod ServiceAccount token")

	configPath = flag.String("config", "", "TOML configuration file. Required.")

	// The flag keeps the name it had in metadata-collector, so a host-native setup only changes the binary.
	maxConsecutivePodMapperFailures = flag.Int("pod-mapper-max-consecutive-failures",
		defaultMaxConsecutivePodMapperFailures,
		"Consecutive poll failures tolerated before exiting non-zero. Minimum 1.")

	metricsPort = flag.Int("metrics-port", defaultMetricsPort,
		"Port for the Prometheus metrics endpoint. 0 disables it.")
)

func main() {
	flag.Parse()

	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting pod-device-mapper", "version", version, "commit", commit, "date", date)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	entityTypes, err := loadMapperConfig(*configPath, os.Getenv("NODE_NAME"))
	if err != nil {
		slog.Error("Invalid pod device mapper configuration", "error", err)
		cancel()
		os.Exit(1)
	}

	group, groupCtx := errgroup.WithContext(ctx)

	if *metricsPort > 0 {
		group.Go(func() error {
			// Not returned: losing metrics is worth less than losing the annotations that partial drains need.
			if err := server.NewServer(server.WithPort(*metricsPort), server.WithPrometheusMetrics()).
				Serve(groupCtx); err != nil {
				slog.Error("Metrics endpoint stopped; continuing without it", "error", err)
			}

			return nil
		})
	}

	group.Go(func() error {
		return runMapper(groupCtx, newPodMapperMetrics(prometheus.DefaultRegisterer), entityTypes)
	})

	if err := group.Wait(); err != nil {
		slog.Error("Pod device mapper failed", "error", err)
		cancel()
		os.Exit(1)
	}

	cancel()
	slog.Info("Pod device mapper stopped")
}

// loadMapperConfig reads the devices that the mapper records, and checks that the node name is set when the
// mapper needs it.
func loadMapperConfig(path, nodeName string) (model.PartialRemediationEntityTypes, error) {
	entityTypes, err := config.LoadPartialRemediationEntityTypes(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load the pod device mapper configuration: %w", err)
	}

	if err := mapper.CheckNodeName(entityTypes, nodeName); err != nil {
		return nil, err
	}

	return entityTypes, nil
}

func runMapper(ctx context.Context, metrics *podMapperMetrics, entityTypes model.PartialRemediationEntityTypes) error {
	podDeviceMapper, err := mapper.NewPodDeviceMapper(ctx, entityTypes,
		mapper.WithKubeconfigs(*kubeconfigPath, *kubeletKubeconfigPath),
		mapper.WithNodeName(os.Getenv("NODE_NAME")))
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
