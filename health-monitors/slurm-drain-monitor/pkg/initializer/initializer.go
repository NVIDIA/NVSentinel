// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package initializer

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/controller"
	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/parser"
	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/publisher"
)

// Params holds initialization parameters.
type Params struct {
	ConfigPath              string
	MetricsBindAddress      string
	HealthProbeBindAddress  string
	ResyncPeriod            time.Duration
	MaxConcurrentReconciles int
	PlatformConnectorSocket string
	ProcessingStrategy      string
}

// Components holds initialized components.
type Components struct {
	Manager  ctrl.Manager
	GRPCConn *grpc.ClientConn
	Config   *config.Config
}

// InitializeAll loads config, dials platform connector, creates parser and publisher,
// and registers the drain reconciler.
func InitializeAll(ctx context.Context, params Params) (*Components, error) {
	slogHandler := slog.Default().Handler()
	logrLogger := logr.FromSlogHandler(slogHandler)
	ctrllog.SetLogger(logrLogger)

	conn, pr, pub, cfg, err := initConnAndClients(ctx, params)
	if err != nil {
		return nil, err
	}

	cacheOpts, err := buildCacheOptions(cfg, params.ResyncPeriod)
	if err != nil {
		conn.Close()

		return nil, err
	}

	mgrOpts := ctrl.Options{
		Metrics:                server.Options{BindAddress: params.MetricsBindAddress},
		HealthProbeBindAddress: params.HealthProbeBindAddress,
		Cache:                  cacheOpts,
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		conn.Close()

		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	if err := setupHealthProbes(mgr); err != nil {
		conn.Close()

		return nil, err
	}

	reconciler := controller.NewDrainReconciler(mgr.GetClient(), pr, pub)
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		WithOptions(ctrlcontroller.Options{
			MaxConcurrentReconciles: params.MaxConcurrentReconciles,
		}).
		Complete(reconciler); err != nil {
		conn.Close()

		return nil, fmt.Errorf("failed to create controller: %w", err)
	}

	slog.Info("Registered drain reconciler")

	return &Components{
		Manager:  mgr,
		GRPCConn: conn,
		Config:   cfg,
	}, nil
}

func initConnAndClients(ctx context.Context, params Params) (
	*grpc.ClientConn, *parser.Parser, *publisher.Publisher, *config.Config, error,
) {
	cfg, err := config.Load(params.ConfigPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to load config: %w", err)
	}

	if err := validateRecommendedActions(cfg); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("config validation failed: %w", err)
	}

	slog.Info("Loaded slurm-drain-monitor config", "namespace", cfg.Namespace, "patterns", len(cfg.Patterns))

	conn, err := dialPlatformConnector(ctx, params.PlatformConnectorSocket)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to connect to platform connector: %w", err)
	}

	pcClient := pb.NewPlatformConnectorClient(conn)
	strategyValue, ok := pb.ProcessingStrategy_value[params.ProcessingStrategy]

	if !ok {
		conn.Close()

		return nil, nil, nil, nil, fmt.Errorf("unexpected processingStrategy value: %q", params.ProcessingStrategy)
	}

	slog.Info("Event handling strategy configured", "processingStrategy", params.ProcessingStrategy)

	pr, err := parser.New(cfg.ReasonDelimiter, cfg.Patterns)
	if err != nil {
		conn.Close()

		return nil, nil, nil, nil, fmt.Errorf("failed to create parser: %w", err)
	}

	pub := publisher.New(pcClient, pb.ProcessingStrategy(strategyValue))

	return conn, pr, pub, cfg, nil
}

func validateRecommendedActions(cfg *config.Config) error {
	for _, p := range cfg.Patterns {
		if p.RecommendedAction == "" {
			continue
		}

		if _, exists := pb.RecommendedAction_value[p.RecommendedAction]; !exists {
			valid := make([]string, 0, len(pb.RecommendedAction_value))
			for name := range pb.RecommendedAction_value {
				valid = append(valid, name)
			}

			return fmt.Errorf("pattern %q: invalid recommendedAction %q (valid: %v)", p.Name, p.RecommendedAction, valid)
		}
	}

	return nil
}

func buildCacheOptions(cfg *config.Config, resyncPeriod time.Duration) (cache.Options, error) {
	opts := cache.Options{
		SyncPeriod: &resyncPeriod,
	}

	sel, err := parseLabelSelector(cfg.LabelSelector)
	if err != nil {
		return cache.Options{}, err
	}

	if sel != nil || cfg.Namespace != "" {
		byObj := cache.ByObject{}
		if sel != nil {
			byObj.Label = sel
		}

		if cfg.Namespace != "" {
			byObj.Namespaces = map[string]cache.Config{cfg.Namespace: {}}
		}

		opts.ByObject = map[client.Object]cache.ByObject{
			&corev1.Pod{}: byObj,
		}
	}

	return opts, nil
}

func parseLabelSelector(s string) (labels.Selector, error) {
	if s == "" {
		return nil, nil
	}

	sel, err := labels.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid labelSelector %q: %w", s, err)
	}

	return sel, nil
}

func setupHealthProbes(mgr ctrl.Manager) error {
	if err := mgr.AddHealthzCheck("ping", func(req *http.Request) error { return nil }); err != nil {
		return fmt.Errorf("failed to add healthz check: %w", err)
	}

	if err := mgr.AddReadyzCheck("ping", func(req *http.Request) error { return nil }); err != nil {
		return fmt.Errorf("failed to add readyz check: %w", err)
	}

	return nil
}

func dialPlatformConnector(ctx context.Context, socket string) (*grpc.ClientConn, error) {
	socketPath := strings.TrimPrefix(socket, "unix://")

	for attempt := 1; attempt <= 10; attempt++ {
		if _, err := os.Stat(socketPath); err != nil {
			slog.Warn("Platform connector socket not found", "attempt", attempt, "path", socketPath)

			if attempt < 10 {
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("context cancelled while waiting for socket: %w", ctx.Err())
				case <-time.After(time.Duration(attempt) * time.Second):
				}

				continue
			}

			return nil, fmt.Errorf("socket not found after retries: %w", err)
		}

		conn, err := grpc.NewClient(socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			slog.Warn("Failed to create gRPC client", "attempt", attempt, "error", err)

			if attempt < 10 {
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("context cancelled while connecting: %w", ctx.Err())
				case <-time.After(time.Duration(attempt) * time.Second):
				}

				continue
			}

			return nil, fmt.Errorf("failed to create client after retries: %w", err)
		}

		slog.Info("Connected to platform connector", "attempt", attempt)

		return conn, nil
	}

	return nil, fmt.Errorf("exhausted retries")
}
