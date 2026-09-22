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

package central

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
)

const testAudience = "platform-connector-deployment.nvsentinel.nvidia.com"

func batchNaming(nodes ...string) *pb.HealthEvents {
	events := make([]*pb.HealthEvent, 0, len(nodes))
	for _, node := range nodes {
		events = append(events, &pb.HealthEvent{NodeName: node, CheckName: "check"})
	}

	return &pb.HealthEvents{Events: events}
}

func validatorReturning(t *testing.T, st authv1.TokenReviewStatus) *grpcauth.Validator {
	t.Helper()

	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			tr := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
			tr.Status = st

			return true, tr, nil
		})

	v, err := grpcauth.NewValidator(client, testAudience)
	require.NoError(t, err)

	return v
}

func authenticatedAs(username string, extra map[string]authv1.ExtraValue) authv1.TokenReviewStatus {
	return authv1.TokenReviewStatus{
		Authenticated: true,
		User:          authv1.UserInfo{Username: username, UID: "sa-uid-1", Extra: extra},
		Audiences:     []string{testAudience},
	}
}

func podBoundExtras(nodeName string) map[string]authv1.ExtraValue {
	return map[string]authv1.ExtraValue{
		"authentication.kubernetes.io/pod-name":  {"monitor-pod"},
		"authentication.kubernetes.io/pod-uid":   {"pod-uid-1"},
		"authentication.kubernetes.io/node-name": {nodeName},
	}
}

func bearerContext(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))
}

// TestNewAuthInterceptor_ForwardsCrossNodePublishers: the deployment wiring
// hands the cross-node list to the shared interceptor, so a listed publisher
// naming other nodes reaches the handler with its identity. The rest of the
// no-local-node behaviour is pkg/auth's and tested there.
func TestNewAuthInterceptor_ForwardsCrossNodePublishers(t *testing.T) {
	interceptor, err := newAuthInterceptor(context.Background(), auth.Settings{
		Enabled:                  true,
		Audience:                 testAudience,
		CrossNodeServiceAccounts: []string{testCrossNode},
	}, validatorReturning(t, authenticatedAs(testCrossNode, podBoundExtras("system-node"))))
	require.NoError(t, err)

	var got *grpcauth.Identity

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		got = auth.CallerFromContext(ctx)

		return &emptypb.Empty{}, nil
	}

	_, err = interceptor(bearerContext("tok-cross"), batchNaming("node-a", "node-b"),
		&grpc.UnaryServerInfo{FullMethod: "/PlatformConnector/HealthEventOccurredV1"}, handler)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, testCrossNode, got.Username)
	require.Equal(t, "system-node", got.NodeName)
}

func TestDeploymentAuthSettings(t *testing.T) {
	_, err := deploymentAuthSettings(map[string]any{"enableNodeBindingAuth": "false"})
	require.ErrorContains(t, err, "enableNodeBindingAuth must be true")

	_, err = deploymentAuthSettings(map[string]any{"enableNodeBindingAuth": "maybe"})
	require.ErrorContains(t, err, "node-binding auth settings: enableNodeBindingAuth")

	settings, err := deploymentAuthSettings(map[string]any{
		"enableNodeBindingAuth":        "true",
		"AuthAudience":                 testAudience,
		"AuthCrossNodeServiceAccounts": []any{testCrossNode},
	})
	require.NoError(t, err)
	require.Equal(t, testAudience, settings.Audience)
	require.Equal(t, []string{testCrossNode}, settings.CrossNodeServiceAccounts)
}

// TestNewAuthInterceptor_SocketOnlySettingsStillEnforce: audit mode and
// fail-open are settings of the socket role; the deployment role built from
// the same config.json still rejects a node-scoped caller naming another node.
func TestNewAuthInterceptor_SocketOnlySettingsStillEnforce(t *testing.T) {
	interceptor, err := newAuthInterceptor(context.Background(), auth.Settings{
		Enabled:               true,
		Audience:              testAudience,
		Mode:                  auth.ModeAudit,
		FailOpenOnUnavailable: true,
	}, validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a"))))
	require.NoError(t, err)

	_, err = interceptor(bearerContext("tok-audit"), batchNaming("node-b"),
		&grpc.UnaryServerInfo{FullMethod: "/PlatformConnector/HealthEventOccurredV1"},
		func(context.Context, interface{}) (interface{}, error) { return &emptypb.Empty{}, nil })
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}
