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

// Package main implements the janitor-provider gRPC service that provides
// cloud service provider operations for node lifecycle management including
// reboot signals, readiness checks, and termination signals.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"

	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/janitor-provider/pkg/auth"
	"github.com/nvidia/nvsentinel/janitor-provider/pkg/csp"
	"github.com/nvidia/nvsentinel/janitor-provider/pkg/model"
)

var (
	// These variables will be populated during the build process
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type janitorProviderServer struct {
	cspv1alpha1.UnimplementedCSPProviderServiceServer
	cspClient model.CSPClient
	k8sClient kubernetes.Interface
}

func (s *janitorProviderServer) SendRebootSignal(
	ctx context.Context, req *cspv1alpha1.SendRebootSignalRequest,
) (*cspv1alpha1.SendRebootSignalResponse, error) {
	slog.Info("Sending reboot signal", "node", req.NodeName)

	node, err := s.k8sClient.CoreV1().Nodes().Get(ctx, req.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get node: %v", err)
	}

	requestID, err := s.cspClient.SendRebootSignal(ctx, *node)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to send reboot signal: %v", err)
	}

	return &cspv1alpha1.SendRebootSignalResponse{
		RequestId: string(requestID),
	}, nil
}

func (s *janitorProviderServer) IsNodeReady(
	ctx context.Context, req *cspv1alpha1.IsNodeReadyRequest,
) (*cspv1alpha1.IsNodeReadyResponse, error) {
	slog.Info("Checking if node is ready", "node", req.NodeName)

	node, err := s.k8sClient.CoreV1().Nodes().Get(ctx, req.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get node: %v", err)
	}

	isReady, err := s.cspClient.IsNodeReady(ctx, *node, req.RequestId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to check if node is ready: %v", err)
	}

	return &cspv1alpha1.IsNodeReadyResponse{
		IsReady: isReady,
	}, nil
}

func (s *janitorProviderServer) SendTerminateSignal(
	ctx context.Context, req *cspv1alpha1.SendTerminateSignalRequest,
) (*cspv1alpha1.SendTerminateSignalResponse, error) {
	slog.Info("Sending terminate signal", "node", req.NodeName)

	node, err := s.k8sClient.CoreV1().Nodes().Get(ctx, req.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get node: %v", err)
	}

	requestID, err := s.cspClient.SendTerminateSignal(ctx, *node)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to send terminate signal: %v", err)
	}

	return &cspv1alpha1.SendTerminateSignalResponse{
		RequestId: string(requestID),
	}, nil
}

func main() {
	logger.SetDefaultStructuredLogger("janitor-provider", version)
	slog.Info("Starting janitor-provider", "version", version, "commit", commit, "date", date)

	if err := run(); err != nil {
		slog.Error("Failed to run", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig

	lis, err := lc.Listen(ctx, "tcp", fmt.Sprintf(":%s", os.Getenv("JANITOR_PROVIDER_PORT")))
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	k8sClient, err := newK8sClient()
	if err != nil {
		return err
	}

	metricsPort, err := strconv.Atoi(os.Getenv("METRICS_PORT"))
	if err != nil {
		return fmt.Errorf("failed to convert metrics port to int: %w", err)
	}

	srv := server.NewServer(
		server.WithPort(metricsPort),
		server.WithPrometheusMetrics(),
		server.WithSimpleHealth(),
	)

	cspClient, err := csp.New(ctx)
	if err != nil {
		return fmt.Errorf("failed to create csp client: %w", err)
	}

	serverOpts, certWatcher, err := buildServerOpts(k8sClient)
	if err != nil {
		return err
	}

	svr := grpc.NewServer(serverOpts...)
	cspv1alpha1.RegisterCSPProviderServiceServer(svr, &janitorProviderServer{
		cspClient: cspClient,
		k8sClient: k8sClient,
	})

	g, gCtx := errgroup.WithContext(ctx)

	if certWatcher != nil {
		g.Go(func() error {
			if err := certWatcher.Start(gCtx); err != nil {
				return fmt.Errorf("cert watcher failed: %w", err)
			}

			return nil
		})
	}

	// Metrics server failures are logged but do NOT terminate the service
	g.Go(func() error {
		slog.Info("Starting metrics server", "port", metricsPort)

		if err := srv.Serve(gCtx); err != nil {
			slog.Error("Metrics server failed - continuing without metrics", "error", err)
		}

		return nil
	})

	g.Go(func() error {
		slog.Info("Starting gRPC server", "port", os.Getenv("JANITOR_PROVIDER_PORT"))

		if err := svr.Serve(lis); err != nil {
			return fmt.Errorf("failed to serve gRPC: %w", err)
		}

		return nil
	})

	// Graceful shutdown on context cancellation
	g.Go(func() error {
		<-gCtx.Done()
		slog.Info("Shutting down gRPC server")
		svr.GracefulStop()

		return nil
	})

	return g.Wait()
}

func newK8sClient() (kubernetes.Interface, error) {
	k8sRestConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	k8sClient, err := kubernetes.NewForConfig(k8sRestConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return k8sClient, nil
}

// buildServerOpts constructs gRPC server options for TLS and auth,
// returning any cert watcher that needs to be started.
func buildServerOpts(
	k8sClient kubernetes.Interface,
) ([]grpc.ServerOption, *certwatcher.CertWatcher, error) {
	var serverOpts []grpc.ServerOption

	certPath, keyPath := os.Getenv("TLS_CERT_PATH"), os.Getenv("TLS_KEY_PATH")
	tlsEnabled := certPath != "" && keyPath != ""

	if certPath != "" != (keyPath != "") {
		return nil, nil, fmt.Errorf(
			"both TLS_CERT_PATH and TLS_KEY_PATH must be set, got cert=%q key=%q",
			certPath, keyPath,
		)
	}

	var certWatcher *certwatcher.CertWatcher

	if tlsEnabled {
		var err error

		certWatcher, err = certwatcher.New(certPath, keyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create cert watcher: %w", err)
		}

		tlsCfg := &tls.Config{
			GetCertificate: certWatcher.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		}

		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))

		slog.Info("gRPC TLS enabled with cert hot-reload", "certPath", certPath)
	}

	if audiences := os.Getenv("AUTH_AUDIENCES"); audiences != "" {
		parts := strings.Split(audiences, ",")
		auds := make([]string, 0, len(parts))

		for _, a := range parts {
			if trimmed := strings.TrimSpace(a); trimmed != "" {
				auds = append(auds, trimmed)
			}
		}

		serverOpts = append(serverOpts,
			grpc.UnaryInterceptor(
				auth.TokenReviewInterceptor(k8sClient, auds)))

		slog.Info("gRPC TokenReview auth enabled", "audiences", auds)
	}

	return serverOpts, certWatcher, nil
}
