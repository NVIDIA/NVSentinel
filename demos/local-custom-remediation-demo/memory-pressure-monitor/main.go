// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func main() {
	slog.Info("Starting memory-pressure-monitor")

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	nodeName := envOrDefault("NODE_NAME", "")
	if nodeName == "" {
		return fmt.Errorf("NODE_NAME env var must be set")
	}

	thresholdMB, err := strconv.ParseUint(envOrDefault("MEM_THRESHOLD_MB", "500"), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid MEM_THRESHOLD_MB: %w", err)
	}

	pollSeconds, err := strconv.Atoi(envOrDefault("POLL_INTERVAL_SECONDS", "10"))
	if err != nil {
		return fmt.Errorf("invalid POLL_INTERVAL_SECONDS: %w", err)
	}

	socketPath := envOrDefault("SOCKET_PATH", "/var/run/nvsentinel.sock")
	procfsPath := envOrDefault("PROCFS_PATH", "/host/proc/meminfo")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	target := "unix://" + socketPath
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to create gRPC client: %w", err)
	}
	defer conn.Close()

	client := pb.NewPlatformConnectorClient(conn)

	slog.Info("Configuration",
		"node", nodeName,
		"thresholdMB", thresholdMB,
		"pollSeconds", pollSeconds,
		"socket", socketPath,
		"procfs", procfsPath,
	)

	ticker := time.NewTicker(time.Duration(pollSeconds) * time.Second)
	defer ticker.Stop()

	var lastHealthy *bool

	for {
		select {
		case <-ctx.Done():
			slog.Info("Shutting down")
			return nil
		case <-ticker.C:
			availMB, err := readMemAvailableMB(procfsPath)
			if err != nil {
				slog.Error("Failed to read meminfo", "error", err)
				continue
			}

			healthy := availMB >= thresholdMB
			slog.Info("Memory check", "availableMB", availMB, "thresholdMB", thresholdMB, "healthy", healthy)

			if lastHealthy != nil && *lastHealthy == healthy {
				continue
			}

			event := buildEvent(nodeName, availMB, thresholdMB, healthy)
			if err := sendEvent(ctx, client, event); err != nil {
				slog.Error("Failed to send health event", "error", err)
				continue
			}

			slog.Info("Sent health event", "healthy", healthy, "availableMB", availMB)
			lastHealthy = &healthy
		}
	}
}

func readMemAvailableMB(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected MemAvailable format: %s", line)
		}

		kB, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing MemAvailable value: %w", err)
		}

		return kB / 1024, nil
	}

	return 0, fmt.Errorf("MemAvailable not found in %s", path)
}

func buildEvent(nodeName string, availMB, thresholdMB uint64, healthy bool) *pb.HealthEvent {
	event := &pb.HealthEvent{
		Version:            1,
		Agent:              "memory-pressure-monitor",
		ComponentClass:     "Memory",
		CheckName:          "MemoryAvailableCheck",
		NodeName:           nodeName,
		GeneratedTimestamp: timestamppb.Now(),
		ProcessingStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		IsHealthy:          healthy,
		IsFatal:            !healthy,
	}

	if healthy {
		event.Message = fmt.Sprintf("Memory recovered: %d MB available (threshold: %d MB)", availMB, thresholdMB)
		event.RecommendedAction = pb.RecommendedAction_NONE
	} else {
		event.Message = fmt.Sprintf("Memory pressure: %d MB available, below threshold of %d MB", availMB, thresholdMB)
		event.RecommendedAction = pb.RecommendedAction_CUSTOM
		event.CustomRecommendedAction = "RECLAIM_MEMORY"
	}

	return event
}

func sendEvent(ctx context.Context, client pb.PlatformConnectorClient, event *pb.HealthEvent) error {
	_, err := client.HealthEventOccurredV1(ctx, &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	})
	return err
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
