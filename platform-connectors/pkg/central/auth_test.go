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

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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
	extra := map[string]authv1.ExtraValue{
		"authentication.kubernetes.io/pod-name": {"monitor-pod"},
		"authentication.kubernetes.io/pod-uid":  {"pod-uid-1"},
	}

	if nodeName != "" {
		extra["authentication.kubernetes.io/node-name"] = authv1.ExtraValue{nodeName}
	}

	return extra
}

func bearerContext(token string) context.Context {
	return metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))
}

func TestAuthInterceptor(t *testing.T) {
	settings := auth.Settings{Enabled: true, Audience: testAudience}

	unaryInfo := &grpc.UnaryServerInfo{FullMethod: "/PlatformConnector/HealthEventOccurredV1"}

	// unreachableHandler fails the test if the interceptor lets a rejected
	// request through.
	unreachableHandler := func(t *testing.T) grpc.UnaryHandler {
		t.Helper()

		return func(context.Context, interface{}) (interface{}, error) {
			t.Fatal("handler must not be reached")

			return nil, nil
		}
	}

	// build wires the deployment configuration into the shared interceptor.
	build := func(t *testing.T, settings auth.Settings, v *grpcauth.Validator) grpc.UnaryServerInterceptor {
		t.Helper()

		interceptor, err := newAuthInterceptor(context.Background(), settings, v)
		require.NoError(t, err)

		return interceptor
	}

	t.Run("missing authorization metadata is Unauthenticated", func(t *testing.T) {
		interceptor := build(t, settings,
			validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a"))))

		_, err := interceptor(context.Background(), batchNaming("node-a"), unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("malformed authorization metadata is Unauthenticated", func(t *testing.T) {
		interceptor := build(t, settings,
			validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a"))))

		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("authorization", "Basic not-a-bearer-token"))

		_, err := interceptor(ctx, batchNaming("node-a"), unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("a token without a pod binding is PermissionDenied", func(t *testing.T) {
		interceptor := build(t, settings, validatorReturning(t, authenticatedAs(testPublisher, nil)))

		_, err := interceptor(bearerContext("tok-unbound"), batchNaming("node-a"), unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
		require.Contains(t, status.Convert(err).Message(), "pod-bound")
	})

	t.Run("a batch naming another node is PermissionDenied", func(t *testing.T) {
		interceptor := build(t, settings,
			validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a"))))

		_, err := interceptor(bearerContext("tok-scope"), batchNaming("node-b"), unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})

	t.Run("happy path reaches the handler with the caller identity", func(t *testing.T) {
		interceptor := build(t, settings,
			validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a"))))

		var got *grpcauth.Identity

		handler := func(ctx context.Context, req interface{}) (interface{}, error) {
			got = auth.CallerFromContext(ctx)

			return &empty.Empty{}, nil
		}

		resp, err := interceptor(bearerContext("tok-ok"), batchNaming("node-a", "node-a"), unaryInfo, handler)
		require.NoError(t, err)
		require.NotNil(t, resp)

		require.NotNil(t, got, "the handler must see the caller identity in its context")
		require.Equal(t, testPublisher, got.Username)
		require.Equal(t, "pod-uid-1", got.PodUID)
		require.Equal(t, "node-a", got.NodeName)
	})

	crossSettings := auth.Settings{
		Enabled:                  true,
		Audience:                 testAudience,
		CrossNodeServiceAccounts: []string{testCrossNode},
	}

	t.Run("a cross-node publisher naming other nodes reaches the handler", func(t *testing.T) {
		interceptor := build(t, crossSettings,
			validatorReturning(t, authenticatedAs(testCrossNode, podBoundExtras("system-node"))))

		var got *grpcauth.Identity

		handler := func(ctx context.Context, req interface{}) (interface{}, error) {
			got = auth.CallerFromContext(ctx)

			return &empty.Empty{}, nil
		}

		_, err := interceptor(bearerContext("tok-cross"), batchNaming("node-a", "node-b"), unaryInfo, handler)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, testCrossNode, got.Username)
		require.Equal(t, "system-node", got.NodeName)
	})

	t.Run("a cross-node publisher whose token has no node claim is PermissionDenied", func(t *testing.T) {
		interceptor := build(t, crossSettings,
			validatorReturning(t, authenticatedAs(testCrossNode, podBoundExtras(""))))

		_, err := interceptor(bearerContext("tok-cross-unbound"), batchNaming("node-a"), unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})

	t.Run("a non-HealthEvents request without a token is Unauthenticated", func(t *testing.T) {
		interceptor := build(t, settings,
			validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a"))))

		_, err := interceptor(context.Background(), &empty.Empty{}, unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})
}

// TestDeploymentAuthSettings: the deployment role reads the same config.json
// keys as the DaemonSet and refuses to start without node binding, since it
// has no local node to fall back on.
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
		func(context.Context, interface{}) (interface{}, error) { return &empty.Empty{}, nil })
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}
