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

package auth_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
)

// The two halves of this change live in different packages and agree only by
// convention on the metadata key and the credential format. These tests drive a
// real gRPC call through both, so a change to one that the other does not
// follow shows up here instead of on a cluster.

const (
	wireNode  = "gpu-node-01"
	wireOther = "gpu-node-57"
	wireSA    = "system:serviceaccount:nvsentinel:csp-health-monitor"
)

// recordingServer captures the batch the handler actually received, which is
// what the rest of the pipeline would go on to act upon.
type recordingServer struct {
	pb.UnimplementedPlatformConnectorServer

	got *pb.HealthEvents
}

func (s *recordingServer) HealthEventOccurredV1(_ context.Context, in *pb.HealthEvents) (*emptypb.Empty, error) {
	s.got = in
	return &emptypb.Empty{}, nil
}

type wireValidator struct{ username string }

func (v wireValidator) Authenticate(_ context.Context, token string) (*grpcauth.Identity, error) {
	if token != "projected-token-contents" {
		return nil, status.Error(codes.Unauthenticated, "unknown token")
	}

	return &grpcauth.Identity{Username: v.username, PodName: "wire-pod", PodUID: "wire-pod-uid", NodeName: wireNode}, nil
}

// newWire starts an in-process platform-connector guarded by the real
// node-binding interceptor and returns a client dialled with the real token
// interceptor. tokenPath is empty for a node-local (credential-less) publisher.
func newWire(t *testing.T, tokenPath string) (pb.PlatformConnectorClient, *recordingServer) {
	t.Helper()

	interceptor, err := auth.NewNodeBindingInterceptor(auth.Config{
		NodeName:                 wireNode,
		Validator:                wireValidator{username: wireSA},
		CrossNodeServiceAccounts: []string{wireSA},
	})
	require.NoError(t, err)

	srv := &recordingServer{}
	grpcServer := grpc.NewServer(grpc.UnaryInterceptor(interceptor))
	pb.RegisterPlatformConnectorServer(grpcServer, srv)

	lis := bufconn.Listen(1024 * 1024)

	go func() { _ = grpcServer.Serve(lis) }()

	t.Cleanup(grpcServer.Stop)

	dialOpts := append(
		[]grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		},
		grpcclient.DialOptions(tokenPath)...,
	)

	conn, err := grpc.NewClient("passthrough:///bufnet", dialOpts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return pb.NewPlatformConnectorClient(conn), srv
}

// writeToken writes a projected-token-shaped file: the token bytes and nothing
// else. kubelet writes no trailing newline (verified on-cluster — the file's
// byte count is identical before and after stripping whitespace), and gRPC
// rejects a header value containing one outright, so a fixture that appended
// one would be testing a state that cannot occur.
func writeToken(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))

	return path
}

func wireEvents(nodeName string) *pb.HealthEvents {
	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{{NodeName: nodeName, Agent: "csp-health-monitor", CheckName: "maintenance"}},
	}
}

func TestWire_TokenlessPublisherIsPinnedToItsOwnNode(t *testing.T) {
	client, srv := newWire(t, "")

	_, err := client.HealthEventOccurredV1(context.Background(), wireEvents(wireOther))

	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Nil(t, srv.got, "the handler must never see a rejected batch")
}

func TestWire_TokenlessPublisherGetsItsNodeNameStamped(t *testing.T) {
	client, srv := newWire(t, "")

	_, err := client.HealthEventOccurredV1(context.Background(), wireEvents(""))

	require.NoError(t, err)
	require.NotNil(t, srv.got)
	assert.Equal(t, wireNode, srv.got.GetEvents()[0].GetNodeName(),
		"the stamp must survive marshalling, not just mutate the caller's copy")
}

func TestWire_AllowlistedTokenReachesAnotherNode(t *testing.T) {
	// The full path: the client reads the projected file, formats the header,
	// and the server parses it back out and matches the identity.
	client, srv := newWire(t, writeToken(t, "projected-token-contents"))

	_, err := client.HealthEventOccurredV1(context.Background(), wireEvents(wireOther))

	require.NoError(t, err)
	require.NotNil(t, srv.got)
	assert.Equal(t, wireOther, srv.got.GetEvents()[0].GetNodeName())
}

func TestWire_UnknownTokenIsRejected(t *testing.T) {
	client, srv := newWire(t, writeToken(t, "some-other-token"))

	_, err := client.HealthEventOccurredV1(context.Background(), wireEvents(wireOther))

	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.Nil(t, srv.got)
}

func TestWire_MissingTokenFileFailsTheCallLocally(t *testing.T) {
	// A publisher whose projected volume is missing must fail its own call
	// rather than silently fall through to the credential-less path, which
	// would leave it able to report only its own node while it believes it is
	// reporting cluster-wide.
	client, srv := newWire(t, filepath.Join(t.TempDir(), "absent"))

	_, err := client.HealthEventOccurredV1(context.Background(), wireEvents(wireOther))

	require.Error(t, err)
	assert.Nil(t, srv.got)
}

func TestWire_RotatedTokenIsPickedUpWithoutReconnecting(t *testing.T) {
	// The kubelet rewrites the projected token in place well before expiry. The
	// interceptor reads per call, so a long-lived connection must pick the new
	// contents up without a restart.
	path := writeToken(t, "stale-token")
	client, srv := newWire(t, path)

	_, err := client.HealthEventOccurredV1(context.Background(), wireEvents(wireOther))
	require.Error(t, err, "precondition: the stale token is not accepted")

	require.NoError(t, os.WriteFile(path, []byte("projected-token-contents"), 0o600))

	_, err = client.HealthEventOccurredV1(context.Background(), wireEvents(wireOther))

	require.NoError(t, err)
	require.NotNil(t, srv.got)
	assert.Equal(t, wireOther, srv.got.GetEvents()[0].GetNodeName())
}
