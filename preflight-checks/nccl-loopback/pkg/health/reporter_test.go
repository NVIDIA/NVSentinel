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

// Tests for the health event reporter: bearer-token call metadata attached to
// HealthEventOccurredV1 publishes, exercised against a real gRPC server on a
// Unix socket.
package health

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/emptypb"
)

// capturingConnector is a PlatformConnector implementation that records the
// "authorization" metadata of every HealthEventOccurredV1 call it receives.
type capturingConnector struct {
	pb.UnimplementedPlatformConnectorServer

	mu          sync.Mutex
	authHeaders [][]string
}

func (c *capturingConnector) HealthEventOccurredV1(
	ctx context.Context,
	_ *pb.HealthEvents,
) (*emptypb.Empty, error) {
	md, _ := metadata.FromIncomingContext(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.authHeaders = append(c.authHeaders, md.Get("authorization"))

	return &emptypb.Empty{}, nil
}

func (c *capturingConnector) calls(t *testing.T) [][]string {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	captured := make([][]string, len(c.authHeaders))
	copy(captured, c.authHeaders)

	return captured
}

func (c *capturingConnector) lastAuth(t *testing.T) []string {
	t.Helper()

	captured := c.calls(t)
	if len(captured) == 0 {
		t.Fatal("no HealthEventOccurredV1 calls were captured")
	}

	return captured[len(captured)-1]
}

// startTestConnector serves a capturingConnector on a Unix socket in a
// temporary directory and returns the socket path.
func startTestConnector(t *testing.T) (string, *capturingConnector) {
	t.Helper()

	socketPath := filepath.Join(t.TempDir(), "pc.sock")

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to listen on %s: %v", socketPath, err)
	}

	connector := &capturingConnector{}
	server := grpc.NewServer()
	pb.RegisterPlatformConnectorServer(server, connector)

	go func() {
		_ = server.Serve(lis)
	}()

	t.Cleanup(server.Stop)

	return socketPath, connector
}

func writeToken(t *testing.T, contents string) string {
	t.Helper()

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("failed to write token file: %v", err)
	}

	return tokenPath
}

func sendTestEvent(t *testing.T, socketPath, tokenPath string) error {
	t.Helper()

	reporter := NewReporter(socketPath, "test-node", pb.ProcessingStrategy_STORE_ONLY, tokenPath)

	return reporter.SendEvent(context.Background(), true, false, "test event", "")
}

func TestSendEventAttachesBearerToken(t *testing.T) {
	socketPath, connector := startTestConnector(t)
	tokenPath := writeToken(t, "projected-token")

	if err := sendTestEvent(t, socketPath, tokenPath); err != nil {
		t.Fatalf("SendEvent failed: %v", err)
	}

	auth := connector.lastAuth(t)
	if len(auth) != 1 || auth[0] != "Bearer projected-token" {
		t.Errorf("got authorization %v, want [Bearer projected-token]", auth)
	}
}

func TestSendEventRereadsTokenOnEveryCall(t *testing.T) {
	socketPath, connector := startTestConnector(t)
	tokenPath := writeToken(t, "token-one")

	if err := sendTestEvent(t, socketPath, tokenPath); err != nil {
		t.Fatalf("first SendEvent failed: %v", err)
	}

	// The kubelet rewrites the projected token file, so every call must
	// read it fresh instead of caching the first contents.
	if err := os.WriteFile(tokenPath, []byte("token-two"), 0o600); err != nil {
		t.Fatalf("failed to rotate token file: %v", err)
	}

	if err := sendTestEvent(t, socketPath, tokenPath); err != nil {
		t.Fatalf("second SendEvent failed: %v", err)
	}

	captured := connector.calls(t)
	if len(captured) != 2 {
		t.Fatalf("got %d calls, want 2", len(captured))
	}

	// Length-checked before indexing: md.Get returns an empty slice when the
	// header is absent, and indexing it would panic instead of reporting the
	// assertion failure.
	if len(captured[0]) == 0 || len(captured[1]) == 0 {
		t.Fatalf("got authorization %v, want one value per call", captured)
	}

	if captured[0][0] != "Bearer token-one" || captured[1][0] != "Bearer token-two" {
		t.Errorf("got authorization %v, want [Bearer token-one] then [Bearer token-two]", captured)
	}
}

func TestSendEventRejectsPaddedTokenInsteadOfRepairingIt(t *testing.T) {
	// A configured credential is forwarded verbatim. A token file containing
	// whitespace is a broken mount, not something to silently repair: trimming
	// it here would mean this client, rather than whatever wrote the file,
	// decides what the credential is. grpc-go forwards the value unchanged and
	// the receiving HTTP/2 transport refuses the newline, so SendEvent fails and
	// the request never reaches the RPC handler.
	socketPath, connector := startTestConnector(t)
	tokenPath := writeToken(t, "  padded-token\n")

	err := sendTestEvent(t, socketPath, tokenPath)
	if err == nil {
		t.Fatalf("SendEvent succeeded with a padded token; want failure")
	}

	if captured := connector.calls(t); len(captured) != 0 {
		t.Errorf("a request reached the handler (%v); want none delivered", captured)
	}
}

func TestSendEventWithoutTokenPathSendsNoAuthorization(t *testing.T) {
	socketPath, connector := startTestConnector(t)

	if err := sendTestEvent(t, socketPath, ""); err != nil {
		t.Fatalf("SendEvent failed: %v", err)
	}

	if auth := connector.lastAuth(t); len(auth) != 0 {
		t.Errorf("got authorization %v, want none", auth)
	}
}

func TestSendEventMissingTokenFileFailsWithoutSending(t *testing.T) {
	socketPath, connector := startTestConnector(t)
	missingPath := filepath.Join(t.TempDir(), "does-not-exist")

	if err := sendTestEvent(t, socketPath, missingPath); err == nil {
		t.Fatal("expected SendEvent to fail when the token file is unreadable")
	}

	if captured := connector.calls(t); len(captured) != 0 {
		t.Errorf("got %d calls, want 0: a configured token must not be skipped silently", len(captured))
	}
}
