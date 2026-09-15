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
)

const testAudience = "platform-connector-deployment.nvsentinel.nvidia.com"

func batchNaming(nodes ...string) *pb.HealthEvents {
	events := make([]*pb.HealthEvent, 0, len(nodes))
	for _, node := range nodes {
		events = append(events, &pb.HealthEvent{NodeName: node, CheckName: "check"})
	}

	return &pb.HealthEvents{Events: events}
}

func identityFor(username, nodeName string) *grpcauth.Identity {
	return &grpcauth.Identity{Username: username, PodUID: "pod-uid-1", NodeName: nodeName}
}

func TestAuthorizePublisher(t *testing.T) {
	cfg := &config{
		allowedPublishers:   map[string]bool{testPublisher: true, testCrossNode: true},
		crossNodePublishers: map[string]bool{testCrossNode: true},
	}

	t.Run("plain publisher pinned to its node claim", func(t *testing.T) {
		require.NoError(t, authorizePublisher(cfg, identityFor(testPublisher, "gpu-node"),
			batchNaming("gpu-node", "gpu-node")))
	})

	t.Run("blank node name is stamped with the token's node", func(t *testing.T) {
		he := batchNaming("gpu-node", "")
		require.NoError(t, authorizePublisher(cfg, identityFor(testPublisher, "gpu-node"), he))
		require.Equal(t, "gpu-node", he.Events[1].NodeName)
	})

	t.Run("cross-node publisher must name a node", func(t *testing.T) {
		err := authorizePublisher(cfg, identityFor(testCrossNode, "system-node"), batchNaming("node-a", ""))
		require.Error(t, err)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("plain publisher naming another node is rejected", func(t *testing.T) {
		err := authorizePublisher(cfg, identityFor(testPublisher, "gpu-node"), batchNaming("gpu-node", "other-node"))
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})

	t.Run("plain publisher without a node claim is rejected", func(t *testing.T) {
		err := authorizePublisher(cfg, identityFor(testPublisher, ""), batchNaming("gpu-node"))
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})

	t.Run("cross-node identity may name any node", func(t *testing.T) {
		require.NoError(t, authorizePublisher(cfg, identityFor(testCrossNode, "system-node"),
			batchNaming("node-a", "node-b")))
	})

	// A token bound to a pod that never scheduled has a pod UID and no node
	// claim: the API server has not tied it to a running workload, so it must
	// not get cluster-wide reach, as on the node-local connector.
	t.Run("cross-node identity without a node claim is rejected", func(t *testing.T) {
		err := authorizePublisher(cfg, identityFor(testCrossNode, ""), batchNaming("node-a", "node-b"))
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})
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
	cfg := &config{allowedPublishers: map[string]bool{testPublisher: true}}

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

	t.Run("missing authorization metadata is Unauthenticated", func(t *testing.T) {
		interceptor := authInterceptor(cfg,
			validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a"))))

		_, err := interceptor(context.Background(), batchNaming("node-a"), unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("malformed authorization metadata is Unauthenticated", func(t *testing.T) {
		interceptor := authInterceptor(cfg,
			validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a"))))

		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("authorization", "Basic not-a-bearer-token"))

		_, err := interceptor(ctx, batchNaming("node-a"), unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("a token without a pod binding is PermissionDenied", func(t *testing.T) {
		interceptor := authInterceptor(cfg, validatorReturning(t, authenticatedAs(testPublisher, nil)))

		_, err := interceptor(bearerContext("tok-unbound"), batchNaming("node-a"), unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
		require.Contains(t, status.Convert(err).Message(), "pod-bound")
	})

	t.Run("an authenticated identity off the allowlist is PermissionDenied", func(t *testing.T) {
		interceptor := authInterceptor(cfg,
			validatorReturning(t, authenticatedAs(
				"system:serviceaccount:nvsentinel:not-a-publisher", podBoundExtras("node-a"))))

		_, err := interceptor(bearerContext("tok-denied"), batchNaming("node-a"), unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
		require.Contains(t, status.Convert(err).Message(), "not an allowed publisher")
	})

	t.Run("a batch naming another node is PermissionDenied", func(t *testing.T) {
		interceptor := authInterceptor(cfg,
			validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a"))))

		_, err := interceptor(bearerContext("tok-scope"), batchNaming("node-b"), unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})

	t.Run("happy path reaches the handler with the caller identity", func(t *testing.T) {
		interceptor := authInterceptor(cfg,
			validatorReturning(t, authenticatedAs(testPublisher, podBoundExtras("node-a"))))

		var got *grpcauth.Identity

		handler := func(ctx context.Context, req interface{}) (interface{}, error) {
			got = callerFromContext(ctx)

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

	crossCfg := &config{
		allowedPublishers:   map[string]bool{testPublisher: true, testCrossNode: true},
		crossNodePublishers: map[string]bool{testCrossNode: true},
	}

	t.Run("a cross-node publisher naming other nodes reaches the handler", func(t *testing.T) {
		interceptor := authInterceptor(crossCfg,
			validatorReturning(t, authenticatedAs(testCrossNode, podBoundExtras("system-node"))))

		var got *grpcauth.Identity

		handler := func(ctx context.Context, req interface{}) (interface{}, error) {
			got = callerFromContext(ctx)

			return &empty.Empty{}, nil
		}

		_, err := interceptor(bearerContext("tok-cross"), batchNaming("node-a", "node-b"), unaryInfo, handler)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, testCrossNode, got.Username)
		require.Equal(t, "system-node", got.NodeName)
	})

	t.Run("a cross-node publisher whose token has no node claim is PermissionDenied", func(t *testing.T) {
		interceptor := authInterceptor(crossCfg,
			validatorReturning(t, authenticatedAs(testCrossNode, podBoundExtras(""))))

		_, err := interceptor(bearerContext("tok-cross-unbound"), batchNaming("node-a"), unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})

	t.Run("the allowlist still gates non-HealthEvents requests", func(t *testing.T) {
		interceptor := authInterceptor(cfg,
			validatorReturning(t, authenticatedAs(
				"system:serviceaccount:nvsentinel:not-a-publisher", podBoundExtras("node-a"))))

		_, err := interceptor(bearerContext("tok-denied-passthrough"), &empty.Empty{}, unaryInfo, unreachableHandler(t))
		require.Error(t, err)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})
}
