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

// Package initializer wires the NVCRE Certification Monitor together: the
// policy configuration, the controller-runtime manager and clients, the
// platform-connector publisher, and the reconciler that runs on the manager.
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
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"

	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/controller"
	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/publisher"
	"github.com/nvidia/nvsentinel/health-monitors/nvcre-certification-monitor/pkg/state"
)

// Params holds all command-line parameters for initialization.
type Params struct {
	MetricsBindAddress      string
	HealthProbeBindAddress  string
	ResyncInterval          time.Duration
	PlatformConnectorSocket string
	// PlatformConnectorToken is the path to a projected ServiceAccount token
	// presented to platform-connector. This monitor reads cluster-wide
	// Certification CRs and therefore reports on nodes other than its own,
	// which platform-connector only permits for an allowlisted,
	// token-authenticated identity. Empty disables token authentication.
	PlatformConnectorToken string
	ProcessingStrategy     string
	ConfigPath             string
}

// Components holds the initialized runtime components.
type Components struct {
	Manager   ctrl.Manager
	GRPCConn  *grpc.ClientConn
	Publisher *publisher.Publisher
}

// InitializeAll wires up all components required by the monitor.
func InitializeAll(ctx context.Context, params Params) (*Components, error) {
	slogHandler := slog.Default().Handler()
	logrLogger := logr.FromSlogHandler(slogHandler)
	ctrllog.SetLogger(logrLogger)

	conn, err := dialPlatformConnector(ctx, params.PlatformConnectorSocket, params.PlatformConnectorToken)
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

	cfg, err := config.Load(params.ConfigPath)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to load policy config from %q: %w", params.ConfigPath, err)
	}

	evaluator, err := config.NewEvaluator(cfg.Policies)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to build policy evaluator: %w", err)
	}

	mgr, err := createManager(params)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	if err := setupHealthChecks(mgr); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to setup health checks: %w", err)
	}

	directClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create direct client: %w", err)
	}

	annotator := state.NewAnnotationManager(directClient)
	// AddRecovered reads the error-recovered list and writes it back whole. A
	// cached read can return a list that predates the previous patch in the
	// same sweep and drop that entry, so cert writes also use the direct client.
	certAnnotator := state.NewCertAnnotationHelper(directClient)

	if err := registerReconciler(mgr, pub, evaluator, annotator, certAnnotator, params); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to register reconciler: %w", err)
	}

	return &Components{
		Manager:   mgr,
		GRPCConn:  conn,
		Publisher: pub,
	}, nil
}

func createManager(params Params) (ctrl.Manager, error) {
	cfg := ctrl.GetConfigOrDie()

	mgrOpts := ctrl.Options{
		Metrics: server.Options{
			BindAddress: params.MetricsBindAddress,
		},
		HealthProbeBindAddress: params.HealthProbeBindAddress,
		// Result ConfigMaps are read a few at a time, once per sweep. Reading them
		// through the cache would start a cluster-wide ConfigMap informer, so they
		// are fetched directly from the API server instead. Certifications and
		// Nodes stay cached.
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.ConfigMap{}},
			},
		},
		// The Node informer holds every Node in the cluster. The reconciler only
		// reads the failure label and annotation from cached Nodes (all writes go
		// through the uncached direct client), so everything else is dropped
		// before it enters the cache. Same approach as fault-quarantine, labeler
		// and node-drainer.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Node{}: {Transform: stripNodeForCache},
			},
		},
	}

	mgr, err := ctrl.NewManager(cfg, mgrOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	return mgr, nil
}

// stripNodeForCache keeps only the Node fields the reconciler reads from the
// cache: identity, labels, and the cert-failures annotation.
func stripNodeForCache(obj any) (any, error) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil, fmt.Errorf("node transform: expected Node object, got %T", obj)
	}

	var annotations map[string]string
	if value, exists := node.Annotations[state.AnnotationKey]; exists {
		annotations = map[string]string{state.AnnotationKey: value}
	}

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:            node.Name,
			UID:             node.UID,
			ResourceVersion: node.ResourceVersion,
			Labels:          node.Labels,
			Annotations:     annotations,
		},
	}, nil
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

func registerReconciler(
	mgr ctrl.Manager,
	pub *publisher.Publisher,
	evaluator *config.Evaluator,
	annotator *state.AnnotationManager,
	certAnnotator *state.CertAnnotationHelper,
	params Params,
) error {
	reconciler := controller.NewReconciler(
		mgr.GetClient(),
		pub,
		evaluator,
		annotator,
		certAnnotator,
		params.ResyncInterval,
	)

	if err := mgr.Add(reconciler); err != nil {
		return fmt.Errorf("failed to add reconciler: %w", err)
	}

	slog.Info("Registered reconciler", "resyncInterval", params.ResyncInterval)

	return nil
}

const (
	// maxDialAttempts bounds the platform-connector connection retries at startup.
	maxDialAttempts = 10
	// readyTimeout bounds how long a single attempt waits for the gRPC
	// connection to reach the Ready state.
	readyTimeout = 10 * time.Second
)

func dialPlatformConnector(ctx context.Context, socket, tokenPath string) (*grpc.ClientConn, error) {
	socketPath := strings.TrimPrefix(socket, "unix://")

	dialOpts := append(
		[]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
		grpcclient.DialOptions(tokenPath)...,
	)

	slog.Info("Dialing platform connector", "socket", socket, "tokenAuthEnabled", tokenPath != "")

	var lastErr error

	for attempt := 1; attempt <= maxDialAttempts; attempt++ {
		conn, err := connectPlatformConnector(ctx, socket, socketPath, dialOpts)
		if err == nil {
			slog.Info("Connected to platform connector", "attempt", attempt)

			return conn, nil
		}

		lastErr = err

		slog.Warn("Platform connector not ready", "attempt", attempt, "error", err)

		if attempt == maxDialAttempts {
			break
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled while connecting to platform connector: %w", ctx.Err())
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}

	return nil, fmt.Errorf("failed to connect to platform connector after %d attempts: %w", maxDialAttempts, lastErr)
}

// connectPlatformConnector performs one connection attempt: the socket file
// must exist, the gRPC client must be created, and the connection must reach
// the Ready state. grpc.NewClient does no I/O on its own, so without the last
// step a client could be returned that never reaches the platform connector.
func connectPlatformConnector(
	ctx context.Context, socket, socketPath string, dialOpts []grpc.DialOption,
) (*grpc.ClientConn, error) {
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("socket not found: %w", err)
	}

	conn, err := grpc.NewClient(socket, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC client: %w", err)
	}

	if err := waitUntilReady(ctx, conn, readyTimeout); err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("connection not ready: %w", err)
	}

	return conn, nil
}

// waitUntilReady triggers connection establishment and blocks until the
// ClientConn reaches connectivity.Ready or the timeout/context expires.
func waitUntilReady(parent context.Context, conn *grpc.ClientConn, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	conn.Connect()

	for {
		state := conn.GetState()
		if state == connectivity.Ready {
			return nil
		}

		if !conn.WaitForStateChange(ctx, state) {
			return ctx.Err()
		}
	}
}
