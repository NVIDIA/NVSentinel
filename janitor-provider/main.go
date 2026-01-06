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

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"

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
	isReady, err := s.cspClient.IsNodeReady(ctx, *node, "Node is ready")
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

	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", os.Getenv("JANITOR_PROVIDER_PORT")))
	if err != nil {
		slog.Error("Failed to listen", "error", err)
		os.Exit(1)
	}

	k8sRestConfig, err := rest.InClusterConfig()
	if err != nil {
		slog.Error("Failed to create kubernetes clientset", "error", err)
		os.Exit(1)
	}

	k8sClient, err := kubernetes.NewForConfig(k8sRestConfig)
	if err != nil {
		slog.Error("Failed to create kubernetes client", "error", err)
		os.Exit(1)
	}

	metricsPort, err := strconv.Atoi(os.Getenv("METRICS_PORT"))
	if err != nil {
		slog.Error("Failed to convert metrics port to int", "error", err)
		os.Exit(1)
	}

	srv := server.NewServer(
		server.WithPort(metricsPort),
		server.WithPrometheusMetrics(),
		server.WithSimpleHealth(),
	)
	if err := srv.Serve(context.Background()); err != nil {
		slog.Error("Failed to serve metrics server", "error", err)
		os.Exit(1)
	}

	svr := grpc.NewServer()
	cspClient, err := csp.New(context.Background())
	if err != nil {
		slog.Error("Failed to create csp client", "error", err)
		os.Exit(1)
	}
	cspv1alpha1.RegisterCSPProviderServiceServer(svr, &janitorProviderServer{
		cspClient: cspClient,
		k8sClient: k8sClient,
	})
	if err := svr.Serve(lis); err != nil {
		slog.Error("Failed to serve", "error", err)
		os.Exit(1)
	}
}
