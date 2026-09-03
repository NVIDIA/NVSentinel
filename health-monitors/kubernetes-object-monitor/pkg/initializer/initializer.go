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
	"maps"
	"net/http"
	"os"
	"slices"
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
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
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
	CacheSyncTimeout        time.Duration
	MaxConcurrentReconciles int
	PlatformConnectorSocket string
	// PlatformConnectorToken is the path to a projected ServiceAccount token
	// presented to platform-connector. This monitor watches cluster-wide
	// objects and therefore reports on nodes other than its own, which
	// platform-connector only permits for an allowlisted, token-authenticated
	// identity. Empty disables token authentication.
	PlatformConnectorToken string
	ProcessingStrategy     string
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

	conn, err := dialPlatformConnector(params.PlatformConnectorSocket, params.PlatformConnectorToken)
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

	mgr, lookupGVKs, err := createManager(params, cfg.Policies)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	if err := setupHealthChecks(mgr); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to setup health checks: %w", err)
	}

	// The API reader is what lookup() falls back to. A GVK it names that the
	// cache has no pruned entry for must not be read through the cached client,
	// which would start a cluster-wide informer for it and hold it in full.
	celEnv, err := celenv.NewEnvironment(mgr.GetAPIReader())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	celEnv.UseCacheForLookups(mgr.GetCache(), lookupGVKs)

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

func createManager(params Params, policies []config.Policy) (ctrl.Manager, []schema.GroupVersionKind, error) {
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("getting kubeconfig: %w", err)
	}

	plan, err := buildCachePlan(restConfig, policies, params.ResyncPeriod)
	if err != nil {
		return nil, nil, err
	}

	mgrOpts := buildManagerOptions(params, plan.options)

	mgr, err := ctrl.NewManager(restConfig, mgrOpts)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create manager: %w", err)
	}

	return mgr, plan.lookupGVKs, nil
}

func buildManagerOptions(params Params, cacheOptions cache.Options) ctrl.Options {
	return ctrl.Options{
		Metrics: server.Options{
			BindAddress: params.MetricsBindAddress,
		},
		HealthProbeBindAddress: params.HealthProbeBindAddress,
		Cache:                  cacheOptions,
		// Serve the watched objects from the informer cache. Unstructured reads
		// bypass it by default, which would put a live GET on the API server in
		// every reconcile of every object.
		Client: client.Options{
			Cache: &client.CacheOptions{Unstructured: true},
		},
		Controller: ctrlconfig.Controller{
			CacheSyncTimeout: params.CacheSyncTimeout,
		},
	}
}

// cachePlan is what the enabled policies imply for the cache: the options to
// build the manager with, and the GVKs a lookup() may read from it.
type cachePlan struct {
	options cache.Options
	// lookupGVKs each have an entry retaining every field the policies read off
	// a looked-up object of that GVK, and are cached cluster-wide so that
	// whichever namespace a lookup names is present.
	lookupGVKs []schema.GroupVersionKind
}

func buildCachePlan(
	restConfig *rest.Config,
	policies []config.Policy,
	resyncPeriod time.Duration,
) (cachePlan, error) {
	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		return cachePlan{}, fmt.Errorf("failed to create Kubernetes HTTP client: %w", err)
	}

	restMapper, err := apiutil.NewDynamicRESTMapper(restConfig, httpClient)
	if err != nil {
		return cachePlan{}, fmt.Errorf("failed to create Kubernetes REST mapper: %w", err)
	}

	return buildCachePlanWithRESTMapper(restMapper, policies, resyncPeriod)
}

func buildCachePlanWithRESTMapper(
	restMapper meta.RESTMapper,
	policies []config.Policy,
	resyncPeriod time.Duration,
) (cachePlan, error) {
	opts := cache.Options{
		SyncPeriod: &resyncPeriod,
	}

	scopes, err := collectWatchScopes(restMapper, policies)
	if err != nil {
		return cachePlan{}, err
	}

	if len(scopes.watched) == 0 {
		return cachePlan{options: opts}, nil
	}

	compiler, err := celenv.NewCompilerEnvironment()
	if err != nil {
		return cachePlan{}, fmt.Errorf("failed to create CEL environment for cache field derivation: %w", err)
	}

	entries := buildCacheEntries(compiler, policies)

	// Every GVK the policies reach gets an entry, cluster-scoped ones included,
	// because the entry is where the transform lives. Namespaces stays nil to
	// cache cluster-wide, which controller-runtime also requires of an entry for
	// a cluster-scoped kind.
	opts.ByObject = make(map[client.Object]cache.ByObject, len(entries))

	var lookupGVKs []schema.GroupVersionKind

	for gvk, entry := range entries {
		namespaces := scopes.namespaces[gvk]

		opts.ByObject[newUnstructuredForGVK(gvk)] = cache.ByObject{
			Namespaces: namespaces,
			Transform:  entry.transform,
		}

		if !entry.servesLookups {
			continue
		}

		// A lookup names any namespace it likes, and reading an entry that
		// holds only some of them fails for the rest.
		if namespaces != nil {
			slog.Info("Reading lookup() through the API: the GVK is cached for named namespaces only",
				"gvk", gvk.String(), "namespaces", slices.Sorted(maps.Keys(namespaces)))

			continue
		}

		lookupGVKs = append(lookupGVKs, gvk)
	}

	return cachePlan{options: opts, lookupGVKs: lookupGVKs}, nil
}

// watchScopes is the namespace scope each watched GVK is cached at.
type watchScopes struct {
	watched map[schema.GroupVersionKind]bool
	// namespaces holds the cache configuration per namespace for a GVK watched
	// in named namespaces only. A GVK cached cluster-wide is absent from it.
	namespaces map[schema.GroupVersionKind]map[string]cache.Config
}

// collectWatchScopes reads the resource stanza of every enabled policy to work
// out which GVKs are watched and at which namespace scope. One policy naming no
// namespace caches the GVK cluster-wide whatever the others ask for, since a
// wider cache answers their reads too.
func collectWatchScopes(restMapper meta.RESTMapper, policies []config.Policy) (watchScopes, error) {
	scopes := watchScopes{
		watched:    make(map[schema.GroupVersionKind]bool),
		namespaces: make(map[schema.GroupVersionKind]map[string]cache.Config),
	}

	clusterWide := make(map[schema.GroupVersionKind]bool)

	for _, p := range policies {
		if !p.Enabled {
			continue
		}

		gvk := policyGVK(p)
		scopes.watched[gvk] = true

		if p.Resource.Namespace == "" {
			clusterWide[gvk] = true
			delete(scopes.namespaces, gvk)

			continue
		}

		if err := validateResourceNamespaceScope(restMapper, p, gvk); err != nil {
			return watchScopes{}, err
		}

		if clusterWide[gvk] {
			continue
		}

		if scopes.namespaces[gvk] == nil {
			scopes.namespaces[gvk] = make(map[string]cache.Config)
		}

		scopes.namespaces[gvk][p.Resource.Namespace] = cache.Config{}
	}

	return scopes, nil
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

func dialPlatformConnector(socket, tokenPath string) (*grpc.ClientConn, error) {
	socketPath := strings.TrimPrefix(socket, "unix://")

	dialOpts := append(
		[]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
		grpcclient.DialOptions(tokenPath)...,
	)

	slog.Info("Dialing platform connector", "socket", socket, "tokenAuthEnabled", tokenPath != "")

	for attempt := 1; attempt <= 10; attempt++ {
		if _, err := os.Stat(socketPath); err != nil {
			slog.Warn("Platform connector socket not found", "attempt", attempt, "path", socketPath)

			if attempt < 10 {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}

			return nil, fmt.Errorf("socket not found after retries: %w", err)
		}

		conn, err := grpc.NewClient(socket, dialOpts...)
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
