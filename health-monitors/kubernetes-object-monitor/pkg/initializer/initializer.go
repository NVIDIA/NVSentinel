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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/annotations"
	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/controller"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/policy"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/publisher"
)

type Params struct {
	PolicyConfigPath        string
	MetricsBindAddress      string
	HealthProbeBindAddress  string
	ResyncPeriod            time.Duration
	MaxConcurrentReconciles int
	PlatformConnectorSocket string
	ProcessingStrategy      string
}

type Components struct {
	Manager   ctrl.Manager
	GRPCConn  *grpc.ClientConn
	Publisher *publisher.Publisher
	Evaluator *policy.Evaluator
	Config    *config.Config
}

func InitializeAll(ctx context.Context, params Params) (*Components, error) {
	slogHandler := slog.Default().Handler()
	logrLogger := logr.FromSlogHandler(slogHandler)
	ctrllog.SetLogger(logrLogger)

	cfg, err := config.Load(params.PolicyConfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load policy config: %w", err)
	}

	slog.Info("Loaded policy configuration", "policies", len(cfg.Policies))

	conn, err := dialPlatformConnector(params.PlatformConnectorSocket)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to platform connector: %w", err)
	}

	pcClient := pb.NewPlatformConnectorClient(conn)

	strategyValue, ok := pb.ProcessingStrategy_value[params.ProcessingStrategy]
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("unexpected processingStrategy value: %q", params.ProcessingStrategy)
	}

	slog.Info("Event handling strategy configured", "processingStrategy", params.ProcessingStrategy)

	pub := publisher.New(pcClient, pb.ProcessingStrategy(strategyValue))

	mgr, err := createManager(params)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	if err := setupHealthChecks(mgr); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to setup health checks: %w", err)
	}

	celEnv, err := celenv.NewEnvironment(mgr.GetClient())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	evaluator, err := policy.NewEvaluator(celEnv, cfg.Policies)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create policy evaluator: %w", err)
	}

	if err := registerControllers(mgr, evaluator, pub, cfg.Policies,
		params.MaxConcurrentReconciles); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to register controllers: %w", err)
	}

	return &Components{
		Manager:   mgr,
		GRPCConn:  conn,
		Publisher: pub,
		Evaluator: evaluator,
		Config:    cfg,
	}, nil
}

func createManager(params Params) (ctrl.Manager, error) {
	config := ctrl.GetConfigOrDie()

	mgrOpts := ctrl.Options{
		Metrics: server.Options{
			BindAddress: params.MetricsBindAddress,
		},
		HealthProbeBindAddress: params.HealthProbeBindAddress,
		Cache: cache.Options{
			SyncPeriod: &params.ResyncPeriod,
		},
	}

	mgr, err := ctrl.NewManager(config, mgrOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	return mgr, nil
}

func setupHealthChecks(mgr ctrl.Manager) error {
	if err := mgr.AddHealthzCheck("ping", func(req *http.Request) error { return nil }); err != nil {
		return fmt.Errorf("failed to add health check: %w", err)
	}

	if err := mgr.AddReadyzCheck("ping", func(req *http.Request) error { return nil }); err != nil {
		return fmt.Errorf("failed to add ready check: %w", err)
	}

	return nil
}

func registerControllers(
	mgr ctrl.Manager,
	evaluator *policy.Evaluator,
	pub *publisher.Publisher,
	policies []config.Policy,
	maxConcurrentReconciles int,
) error {
	annotationMgr := annotations.NewManager(mgr.GetClient())
	gvkPolicies := groupPoliciesByGVK(policies)

	for gvk, policies := range gvkPolicies {
		reconciler := controller.NewResourceReconciler(mgr.GetClient(), evaluator, pub, annotationMgr, policies, gvk)

		if err := reconciler.LoadState(context.Background()); err != nil {
			slog.Warn("Failed to load state for controller, starting fresh", "gvk", gvk.String(), "error", err)
		}

		if err := ctrl.NewControllerManagedBy(mgr).
			For(newUnstructuredForGVK(gvk)).
			WithOptions(ctrlcontroller.Options{
				MaxConcurrentReconciles: maxConcurrentReconciles,
			}).
			Complete(reconciler); err != nil {
			return fmt.Errorf("failed to create controller for %s: %w", gvk.String(), err)
		}

		slog.Info("Registered controller", "gvk", gvk.String(), "policies", len(policies))
	}

	return nil
}

func dialPlatformConnector(socket string) (*grpc.ClientConn, error) {
	socketPath := strings.TrimPrefix(socket, "unix://")

	for attempt := 1; attempt <= 10; attempt++ {
		if _, err := os.Stat(socketPath); err != nil {
			slog.Warn("Platform connector socket not found", "attempt", attempt, "path", socketPath)

			if attempt < 10 {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			return nil, fmt.Errorf("socket not found after retries: %w", err)
		}

		conn, err := grpc.NewClient(socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			slog.Warn("Failed to create gRPC client", "attempt", attempt, "error", err)

			if attempt < 10 {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			return nil, fmt.Errorf("failed to create client after retries: %w", err)
		}

		slog.Info("Connected to platform connector", "attempt", attempt)

		return conn, nil
	}

	return nil, fmt.Errorf("exhausted retries")
}

func groupPoliciesByGVK(policies []config.Policy) map[schema.GroupVersionKind][]config.Policy {
	result := make(map[schema.GroupVersionKind][]config.Policy)

	for _, p := range policies {
		if !p.Enabled {
			continue
		}

		gvk := schema.GroupVersionKind{
			Group:   p.Resource.Group,
			Version: p.Resource.Version,
			Kind:    p.Resource.Kind,
		}
		result[gvk] = append(result[gvk], p)
	}

	return result
}

func newUnstructuredForGVK(gvk schema.GroupVersionKind) client.Object {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	return obj
}
