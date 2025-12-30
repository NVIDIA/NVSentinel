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
	"log/slog"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
)

// janitorProviderServer is the server implementation for the janitor provider.
type janitorProviderServer struct {
	cspv1alpha1.UnimplementedCSPProviderServiceServer
}

func (s *janitorProviderServer) SendRebootSignal(ctx context.Context, req *cspv1alpha1.SendRebootSignalRequest) (*cspv1alpha1.SendRebootSignalResponse, error) {
	slog.Info("Sending reboot signal", "node", req.NodeName)
	return &cspv1alpha1.SendRebootSignalResponse{
		RequestId: "1234567890",
	}, nil
}

func (s *janitorProviderServer) IsNodeReady(ctx context.Context, req *cspv1alpha1.IsNodeReadyRequest) (*cspv1alpha1.IsNodeReadyResponse, error) {
	slog.Info("Checking if node is ready", "node", req.NodeName)
	return &cspv1alpha1.IsNodeReadyResponse{
		IsReady: true,
	}, nil
}

func (s *janitorProviderServer) SendTerminateSignal(ctx context.Context, req *cspv1alpha1.SendTerminateSignalRequest) (*cspv1alpha1.SendTerminateSignalResponse, error) {
	slog.Info("Sending terminate signal", "node", req.NodeName)
	return nil, status.Errorf(codes.Unimplemented, "method SendTerminateSignal not implemented")
}

func main() {
	slog.Info("Starting janitor provider")

	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		slog.Error("Failed to listen", "error", err)
		os.Exit(1)
	}

	svr := grpc.NewServer()
	cspv1alpha1.RegisterCSPProviderServiceServer(svr, &janitorProviderServer{})
	if err := svr.Serve(lis); err != nil {
		slog.Error("Failed to serve", "error", err)
		os.Exit(1)
	}
}
