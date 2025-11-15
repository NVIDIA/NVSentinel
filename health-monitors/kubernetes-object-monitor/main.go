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
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nvidia/nvsentinel/commons/pkg/logger"
)

const (
	defaultAgentName = "kubernetes-object-monitor"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	logger.SetDefaultStructuredLogger(defaultAgentName, version)
	slog.Info("Starting kubernetes-object-monitor", "version",
		version, "commit", commit, "date", date, "log_level", logLevel)

	if err := run(); err != nil {
		slog.Error("Fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("Kubernetes Object Monitor started successfully")
	slog.Info("TODO: Implement controller-runtime based monitor with CEL policy evaluation")

	<-ctx.Done()

	slog.Info("Shutdown signal received, exiting gracefully")

	return nil
}
