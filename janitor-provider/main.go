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
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
	"github.com/nvidia/nvsentinel/commons/pkg/logger"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
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

func (s *janitorProviderServer) SendRebootSignal(ctx context.Context, req *cspv1alpha1.SendRebootSignalRequest) (*cspv1alpha1.SendRebootSignalResponse, error) {
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

func (s *janitorProviderServer) IsNodeReady(ctx context.Context, req *cspv1alpha1.IsNodeReadyRequest) (*cspv1alpha1.IsNodeReadyResponse, error) {
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

func (s *janitorProviderServer) SendTerminateSignal(ctx context.Context, req *cspv1alpha1.SendTerminateSignalRequest) (*cspv1alpha1.SendTerminateSignalResponse, error) {
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

	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", os.Getenv("JANITOR_PROVIDER_PORT")))
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	k8sRestConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	k8sClient, err := kubernetes.NewForConfig(k8sRestConfig)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
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

	svr := grpc.NewServer()
	cspv1alpha1.RegisterCSPProviderServiceServer(svr, &janitorProviderServer{
		cspClient: cspClient,
		k8sClient: k8sClient,
	})

	g, gCtx := errgroup.WithContext(ctx)

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
