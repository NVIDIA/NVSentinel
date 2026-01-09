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
	"log/slog"
	"net"
	"regexp"
	"sync"

	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
	"github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MockCSPBehavior defines how the mock CSP server should behave for each operation.
// This allows tests to configure success/failure per operation type.
type MockCSPBehavior struct {
	// SendTerminateSignal behavior
	TerminateError     error
	TerminateRequestID string

	// SendRebootSignal behavior
	RebootError     error
	RebootRequestID string

	// IsNodeReady behavior
	IsNodeReadyError error
	IsNodeReady      bool
}

// DefaultSuccessBehavior returns a MockCSPBehavior configured for all operations to succeed.
func DefaultSuccessBehavior() *MockCSPBehavior {
	return &MockCSPBehavior{
		TerminateRequestID: "test-terminate-request-ref",
		RebootRequestID:    "test-request-ref",
		IsNodeReady:        true,
	}
}

// DefaultFailureBehavior returns a MockCSPBehavior configured for all operations to fail.
func DefaultFailureBehavior() *MockCSPBehavior {
	return &MockCSPBehavior{
		TerminateError:   status.Errorf(codes.Internal, "failed to send terminate signal"),
		RebootError:      status.Errorf(codes.Internal, "failed to send reboot signal"),
		IsNodeReadyError: status.Errorf(codes.Internal, "failed to check if node is ready"),
	}
}

// MockCSPServer is a configurable mock implementation of CSPProviderServiceServer.
// Behavior can be changed at runtime via SetBehavior for per-test configuration.
type MockCSPServer struct {
	cspv1alpha1.UnimplementedCSPProviderServiceServer
	mu       sync.RWMutex
	behavior *MockCSPBehavior
}

// NewMockCSPServer creates a new MockCSPServer with default success behavior.
func NewMockCSPServer() *MockCSPServer {
	return &MockCSPServer{
		behavior: DefaultSuccessBehavior(),
	}
}

// SetBehavior updates the mock server's behavior. This is thread-safe and can be called
// from within tests to change behavior between reconciliations.
func (s *MockCSPServer) SetBehavior(behavior *MockCSPBehavior) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.behavior = behavior
}

// SetSuccess configures the server to succeed on all operations.
func (s *MockCSPServer) SetSuccess() {
	s.SetBehavior(DefaultSuccessBehavior())
}

// SetFailure configures the server to fail on all operations.
func (s *MockCSPServer) SetFailure() {
	s.SetBehavior(DefaultFailureBehavior())
}

// SetRebootSuccess configures only reboot operations to succeed.
func (s *MockCSPServer) SetRebootSuccess(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.behavior.RebootError = nil
	s.behavior.RebootRequestID = requestID
}

// SetRebootFailure configures only reboot operations to fail.
func (s *MockCSPServer) SetRebootFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.behavior.RebootError = err
}

// SetTerminateSuccess configures only terminate operations to succeed.
func (s *MockCSPServer) SetTerminateSuccess(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.behavior.TerminateError = nil
	s.behavior.TerminateRequestID = requestID
}

// SetTerminateFailure configures only terminate operations to fail.
func (s *MockCSPServer) SetTerminateFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.behavior.TerminateError = err
}

// SetNodeReady configures the IsNodeReady response.
func (s *MockCSPServer) SetNodeReady(ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.behavior.IsNodeReadyError = nil
	s.behavior.IsNodeReady = ready
}

// SetNodeReadyError configures IsNodeReady to return an error.
func (s *MockCSPServer) SetNodeReadyError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.behavior.IsNodeReadyError = err
}

func (s *MockCSPServer) SendTerminateSignal(
	ctx context.Context,
	req *cspv1alpha1.SendTerminateSignalRequest,
) (*cspv1alpha1.SendTerminateSignalResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.behavior.TerminateError != nil {
		return nil, s.behavior.TerminateError
	}

	return &cspv1alpha1.SendTerminateSignalResponse{
		RequestId: s.behavior.TerminateRequestID,
	}, nil
}

func (s *MockCSPServer) SendRebootSignal(
	ctx context.Context,
	req *cspv1alpha1.SendRebootSignalRequest,
) (*cspv1alpha1.SendRebootSignalResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.behavior.RebootError != nil {
		return nil, s.behavior.RebootError
	}

	return &cspv1alpha1.SendRebootSignalResponse{
		RequestId: s.behavior.RebootRequestID,
	}, nil
}

func (s *MockCSPServer) IsNodeReady(
	ctx context.Context,
	req *cspv1alpha1.IsNodeReadyRequest,
) (*cspv1alpha1.IsNodeReadyResponse, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.behavior.IsNodeReadyError != nil {
		return nil, s.behavior.IsNodeReadyError
	}

	return &cspv1alpha1.IsNodeReadyResponse{
		IsReady: s.behavior.IsNodeReady,
	}, nil
}

// MockCSPTestHelper manages the mock gRPC server lifecycle and provides a CSP client.
// This should be created once per test suite and used across all tests.
type MockCSPTestHelper struct {
	Server     *MockCSPServer
	Client     cspv1alpha1.CSPProviderServiceClient
	grpcServer *grpc.Server
	listener   *bufconn.Listener
	grpcConn   *grpc.ClientConn
}

// NewMockCSPTestHelper creates and starts a mock CSP gRPC server.
// The server runs in the background and the returned helper provides a ready-to-use client.
// Call Stop() when done (typically in AfterSuite).
func NewMockCSPTestHelper() *MockCSPTestHelper {
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	mockServer := NewMockCSPServer()

	cspv1alpha1.RegisterCSPProviderServiceServer(server, mockServer)

	go func() {
		if err := server.Serve(lis); err != nil {
			slog.Error("Mock CSP server exited with error", "error", err)
		}
	}()

	// Create client connection
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		slog.Error("Failed to create mock CSP client", "error", err)
	}

	return &MockCSPTestHelper{
		Server:     mockServer,
		Client:     cspv1alpha1.NewCSPProviderServiceClient(conn),
		grpcServer: server,
		listener:   lis,
		grpcConn:   conn,
	}
}

// Stop gracefully shuts down the mock gRPC server.
func (h *MockCSPTestHelper) Stop() {
	if h.grpcServer != nil {
		h.grpcServer.GracefulStop()
	}

	if h.grpcConn != nil {
		h.grpcConn.Close()
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
