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

package controller

import (
	"context"
	"log"
	"net"
	"regexp"

	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
	"github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	lis *bufconn.Listener
)

func bufDialer(context.Context, string) (net.Conn, error) {
	return lis.Dial()
}

type cspProviderServer struct {
	cspv1alpha1.UnimplementedCSPProviderServiceServer
}

func (s *cspProviderServer) SendTerminateSignal(ctx context.Context, req *cspv1alpha1.SendTerminateSignalRequest) (*cspv1alpha1.SendTerminateSignalResponse, error) {
	return &cspv1alpha1.SendTerminateSignalResponse{
		RequestId: "value",
	}, nil
}

func (s *cspProviderServer) SendRebootSignal(ctx context.Context, req *cspv1alpha1.SendRebootSignalRequest) (*cspv1alpha1.SendRebootSignalResponse, error) {
	return &cspv1alpha1.SendRebootSignalResponse{
		RequestId: "test-request-ref",
	}, nil
}

func (s *cspProviderServer) IsNodeReady(ctx context.Context, req *cspv1alpha1.IsNodeReadyRequest) (*cspv1alpha1.IsNodeReadyResponse, error) {
	return &cspv1alpha1.IsNodeReadyResponse{
		IsReady: true,
	}, nil
}

func (r *RebootNodeReconciler) setupGRPCServer() {
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	cspv1alpha1.RegisterCSPProviderServiceServer(server, &cspProviderServer{})
	go func() {
		if err := server.Serve(lis); err != nil {
			log.Fatalf("Server exited with error: %v", err)
		}
	}()

	client, err := grpc.NewClient("passthrough://bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	r.CSPClient = cspv1alpha1.NewCSPProviderServiceClient(client)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
}

type cspProviderFailingServer struct {
	cspv1alpha1.UnimplementedCSPProviderServiceServer
}

func (s *cspProviderFailingServer) SendTerminateSignal(ctx context.Context, req *cspv1alpha1.SendTerminateSignalRequest) (*cspv1alpha1.SendTerminateSignalResponse, error) {
	return nil, status.Errorf(codes.Internal, "failed to send terminate signal")
}

func (s *cspProviderFailingServer) SendRebootSignal(ctx context.Context, req *cspv1alpha1.SendRebootSignalRequest) (*cspv1alpha1.SendRebootSignalResponse, error) {
	return nil, status.Errorf(codes.Internal, "failed to send reboot signal")
}

func (s *cspProviderFailingServer) IsNodeReady(ctx context.Context, req *cspv1alpha1.IsNodeReadyRequest) (*cspv1alpha1.IsNodeReadyResponse, error) {
	return nil, status.Errorf(codes.Internal, "failed to check if node is ready")
}

func (r *RebootNodeReconciler) setupFailingGRPCServer() {
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	cspv1alpha1.RegisterCSPProviderServiceServer(server, &cspProviderFailingServer{})
	go func() {
		if err := server.Serve(lis); err != nil {
			log.Fatalf("Server exited with error: %v", err)
		}
	}()

	client, err := grpc.NewClient("passthrough://bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	r.CSPClient = cspv1alpha1.NewCSPProviderServiceClient(client)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
}

func (r *TerminateNodeReconciler) setupFailingGRPCServer() {
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	cspv1alpha1.RegisterCSPProviderServiceServer(server, &cspProviderFailingServer{})
	go func() {
		if err := server.Serve(lis); err != nil {
			log.Fatalf("Server exited with error: %v", err)
		}
	}()

	client, err := grpc.NewClient("passthrough://bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	r.CSPClient = cspv1alpha1.NewCSPProviderServiceClient(client)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
}

func (r *TerminateNodeReconciler) setupGRPCServer() {
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	cspv1alpha1.RegisterCSPProviderServiceServer(server, &cspProviderServer{})
	go func() {
		if err := server.Serve(lis); err != nil {
			log.Fatalf("Server exited with error: %v", err)
		}
	}()

	client, err := grpc.NewClient("passthrough://bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
	r.CSPClient = cspv1alpha1.NewCSPProviderServiceClient(client)
	if err != nil {
		log.Fatalf("Failed to create client: %v", err)
	}
}

// nolint:gochecknoglobals,lll,unused // test pattern
var conditionReasonPattern = regexp.MustCompile("^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$")

// Validates that the given condition is properly formed. This function validates
// that the reason field matches the regex included by kubebuilder in the given CRD:
// +kubebuilder:validation:Pattern=`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`
//
//nolint:unused // used by ginkgo tests
func checkStatusConditions(conditions []metav1.Condition) {
	for _, condition := range conditions {
		gomega.Expect(conditionReasonPattern.MatchString(condition.Reason)).To(gomega.BeTrue())
	}
}
