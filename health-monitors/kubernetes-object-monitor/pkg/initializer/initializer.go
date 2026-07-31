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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
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

	pub := publisher.New(pcClient, params.PlatformConnectorSocket, pb.ProcessingStrategy(strategyValue))

	mgr, err := createManager(params, cfg.Policies)
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

func createManager(params Params, policies []config.Policy) (ctrl.Manager, error) {
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("getting kubeconfig: %w", err)
	}

	cacheOptions, err := buildCacheOptions(restConfig, policies, params.ResyncPeriod)
	if err != nil {
		return nil, err
	}

	mgrOpts := ctrl.Options{
		Metrics: server.Options{
			BindAddress: params.MetricsBindAddress,
		},
		HealthProbeBindAddress: params.HealthProbeBindAddress,
		Cache:                  cacheOptions,
	}

	mgr, err := ctrl.NewManager(restConfig, mgrOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	return mgr, nil
}

func buildCacheOptions(
	restConfig *rest.Config,
	policies []config.Policy,
	resyncPeriod time.Duration,
) (cache.Options, error) {
	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return cache.Options{}, fmt.Errorf("failed to create Kubernetes HTTP client: %w", err)
	}

	restMapper, err := apiutil.NewDynamicRESTMapper(restConfig, httpClient)
	if err != nil {
		return cache.Options{}, fmt.Errorf("failed to create Kubernetes REST mapper: %w", err)
	}

	return buildCacheOptionsWithRESTMapper(restMapper, policies, resyncPeriod)
}

func buildCacheOptionsWithRESTMapper(
	restMapper meta.RESTMapper,
	policies []config.Policy,
	resyncPeriod time.Duration,
) (cache.Options, error) {
	opts := cache.Options{
		SyncPeriod: &resyncPeriod,
	}

	namespacesByGVK := make(map[schema.GroupVersionKind]map[string]cache.Config)
	allNamespacesByGVK := make(map[schema.GroupVersionKind]bool)

	for _, p := range policies {
		if !p.Enabled {
			continue
		}

		gvk := policyGVK(p)
		if p.Resource.Namespace == "" {
			allNamespacesByGVK[gvk] = true
			delete(namespacesByGVK, gvk)

			continue
		}

		if err := validateResourceNamespaceScope(restMapper, p, gvk); err != nil {
			return cache.Options{}, err
		}

		if allNamespacesByGVK[gvk] {
			continue
		}

		if namespacesByGVK[gvk] == nil {
			namespacesByGVK[gvk] = make(map[string]cache.Config)
		}

		namespacesByGVK[gvk][p.Resource.Namespace] = cache.Config{}
	}

	if len(namespacesByGVK) == 0 {
		return opts, nil
	}

	opts.ByObject = make(map[client.Object]cache.ByObject, len(namespacesByGVK))
	for gvk, namespaces := range namespacesByGVK {
		opts.ByObject[newUnstructuredForGVK(gvk)] = cache.ByObject{
			Namespaces: namespaces,
		}
	}

	return opts, nil
}

func validateResourceNamespaceScope(
	restMapper meta.RESTMapper,
	p config.Policy,
	gvk schema.GroupVersionKind,
) error {
	mapping, err := restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("policy %q: failed to resolve resource scope for %s: %w", p.Name, gvk.String(), err)
	}

	if mapping.Scope.Name() == meta.RESTScopeNameRoot {
		return fmt.Errorf(
			"policy %q: resource.namespace cannot be set for cluster-scoped resource %s",
			p.Name,
			gvk.String(),
		)
	}

	return nil
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
	pub controller.HealthEventPublisher,
	policies []config.Policy,
	maxConcurrentReconciles int,
) error {
	apiReader := mgr.GetAPIReader()
	annotationMgr := annotations.NewManagerWithReader(apiReader, mgr.GetClient())
	gvkPolicies := groupPoliciesByGVK(policies)
	// The shared barrier prevents workers from consuming initial informer events
	// before every controller has restored its persisted match state.
	stateBarrier := newStateLoadBarrier()
	stateLoader := &controllerStateLoader{
		barrier: stateBarrier,
	}
	enableWarmup := true

	for gvk, policies := range gvkPolicies {
		mapping, err := mgr.GetRESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return fmt.Errorf("failed to resolve resource scope for %s: %w", gvk.String(), err)
		}

		var resourceNamespaced bool

		switch mapping.Scope.Name() {
		case meta.RESTScopeNameNamespace:
			resourceNamespaced = true
		case meta.RESTScopeNameRoot:
			resourceNamespaced = false
		default:
			return fmt.Errorf(
				"unsupported resource scope %q for %s",
				mapping.Scope.Name(),
				gvk.String(),
			)
		}

		reconciler := controller.NewResourceReconcilerWithReader(
			mgr.GetClient(),
			apiReader,
			evaluator,
			pub,
			annotationMgr,
			policies,
			gvk,
		)

		gatedReconciler := &stateLoadingReconciler{
			reconciler: reconciler,
			barrier:    stateBarrier,
		}

		builtController, err := ctrl.NewControllerManagedBy(mgr).
			For(newUnstructuredForGVK(gvk)).
			WithOptions(ctrlcontroller.Options{
				MaxConcurrentReconciles: maxConcurrentReconciles,
				EnableWarmup:            &enableWarmup,
			}).
			Build(gatedReconciler)
		if err != nil {
			return fmt.Errorf("failed to create controller for %s: %w", gvk.String(), err)
		}

		warmupController, ok := builtController.(interface {
			Warmup(context.Context) error
		})
		if !ok {
			return fmt.Errorf("controller for %s does not support source warmup", gvk.String())
		}

		stateLoader.controllers = append(stateLoader.controllers, controllerState{
			gvk:                gvk,
			resourceNamespaced: resourceNamespaced,
			reconciler:         reconciler,
			controller:         warmupController,
		})

		slog.Info("Registered controller", "gvk", gvk.String(), "policies", len(policies))
	}

	if err := mgr.Add(stateLoader); err != nil {
		return fmt.Errorf("failed to register controller state loader: %w", err)
	}

	return nil
}

type stateLoadBarrier struct {
	done chan struct{}
	err  error
}

func newStateLoadBarrier() *stateLoadBarrier {
	return &stateLoadBarrier{done: make(chan struct{})}
}

func (b *stateLoadBarrier) complete(err error) {
	b.err = err
	close(b.done)
}

func (b *stateLoadBarrier) wait(ctx context.Context) error {
	select {
	case <-b.done:
		return b.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type stateLoadingReconciler struct {
	reconciler *controller.ResourceReconciler
	barrier    *stateLoadBarrier
}

func (r *stateLoadingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if err := r.barrier.wait(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("initial state load failed: %w", err)
	}

	return r.reconciler.Reconcile(ctx, req)
}

type controllerState struct {
	gvk                schema.GroupVersionKind
	resourceNamespaced bool
	reconciler         *controller.ResourceReconciler
	controller         interface {
		Warmup(context.Context) error
	}
}

type controllerStateLoader struct {
	barrier     *stateLoadBarrier
	controllers []controllerState
}

func (l *controllerStateLoader) Start(ctx context.Context) error {
	for _, state := range l.controllers {
		// Controller warmup is serialized and idempotent. Calling it here waits
		// for the source handler and cache to be ready before the final API scan,
		// even when the manager's warmup phase is still finishing concurrently.
		if err := state.controller.Warmup(ctx); err != nil {
			loadErr := fmt.Errorf("failed to warm up controller source for %s: %w", state.gvk.String(), err)
			l.barrier.complete(loadErr)

			return loadErr
		}

		if err := state.reconciler.LoadStateWithScope(ctx, state.resourceNamespaced); err != nil {
			loadErr := fmt.Errorf("failed to load state for controller %s: %w", state.gvk.String(), err)
			l.barrier.complete(loadErr)

			return loadErr
		}
	}

	l.barrier.complete(nil)
	<-ctx.Done()

	return nil
}

func (*controllerStateLoader) NeedLeaderElection() bool {
	return true
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

		gvk := policyGVK(p)
		result[gvk] = append(result[gvk], p)
	}

	return result
}

func policyGVK(p config.Policy) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   p.Resource.Group,
		Version: p.Resource.Version,
		Kind:    p.Resource.Kind,
	}
}

func newUnstructuredForGVK(gvk schema.GroupVersionKind) client.Object {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)

	return obj
}
