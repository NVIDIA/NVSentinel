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
	"testing"

	"github.com/stretchr/testify/assert"
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

// These exercise the whole server-side stack together — the real TokenReview
// validator, its verdict cache, and the node-binding interceptor — rather than
// the interceptor against a stub. The seam they cover is the one that only
// showed up on a live cluster otherwise: whether the node claim survives
// TokenReview extraction and caching intact, and is still enforced on a cache
// hit.

const (
	stackNode     = "gpu-node-01"
	stackOther    = "gpu-node-57"
	stackAudience = "nvsentinel-platform-connector"
	stackCrossSA  = "system:serviceaccount:nvsentinel:csp-health-monitor"
	stackLocalSA  = "system:serviceaccount:nvsentinel:gpu-health-monitor"
)

// tokenReviewFor returns a clientset that authenticates any token as username,
// attaching nodeName as the bound pod's node claim when non-empty. It also
// counts calls, so cache hits are observable.
func tokenReviewFor(username, nodeName string) (*fake.Clientset, *int) {
	calls := 0
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			calls++

			// A projected token is always pod-bound; cross-node scope now
			// requires that binding, so the fixture must carry the pod UID.
			extra := map[string]authv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"publisher-abc"},
				"authentication.kubernetes.io/pod-uid":  {"publisher-abc-uid"},
			}
			if nodeName != "" {
				extra["authentication.kubernetes.io/node-name"] = authv1.ExtraValue{nodeName}
			}

			tr := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
			tr.Status = authv1.TokenReviewStatus{
				Authenticated: true,
				Audiences:     []string{stackAudience},
				User:          authv1.UserInfo{Username: username, Extra: extra},
			}

			return true, tr, nil
		})

	return client, &calls
}

func stackInterceptor(t *testing.T, client *fake.Clientset) grpc.UnaryServerInterceptor {
	t.Helper()

	validator, err := grpcauth.NewValidator(client, stackAudience)
	require.NoError(t, err)

	interceptor, err := auth.NewNodeBindingInterceptor(auth.Config{
		NodeName:                 stackNode,
		Validator:                validator,
		CrossNodeServiceAccounts: []string{stackCrossSA},
	})
	require.NoError(t, err)

	return interceptor
}

func callStack(interceptor grpc.UnaryServerInterceptor, token string, nodeNames ...string) (*pb.HealthEvents, error) {
	evts := make([]*pb.HealthEvent, 0, len(nodeNames))
	for _, n := range nodeNames {
		evts = append(evts, &pb.HealthEvent{NodeName: n, Agent: "test", CheckName: "test"})
	}

	batch := &pb.HealthEvents{Version: 1, Events: evts}
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+token))

	_, err := interceptor(ctx, batch, &grpc.UnaryServerInfo{FullMethod: "/test/M"},
		func(context.Context, any) (any, error) { return nil, nil })

	return batch, err
}

func TestStack_NodeClaimSurvivesTokenReviewAndCache(t *testing.T) {
	// A node-local publisher whose token attests this node: accepted, and the
	// second call must be served from cache while still enforcing the claim.
	client, calls := tokenReviewFor(stackLocalSA, stackNode)
	interceptor := stackInterceptor(t, client)

	_, err := callStack(interceptor, "tok-a", stackNode)
	require.NoError(t, err)

	batch, err := callStack(interceptor, "tok-a", "")
	require.NoError(t, err)
	assert.Equal(t, stackNode, batch.GetEvents()[0].GetNodeName(), "blank name stamped with the connector's node")

	assert.Equal(t, 1, *calls, "the second call is served from the verdict cache")
}

func TestStack_ReplayedTokenRejectedIncludingFromCache(t *testing.T) {
	// The token attests a different node than the connector runs on. It must be
	// refused on the first call and, just as importantly, on the cached second
	// call — caching authentication must not quietly skip the claim check.
	client, calls := tokenReviewFor(stackLocalSA, stackOther)
	interceptor := stackInterceptor(t, client)

	for range 2 {
		_, err := callStack(interceptor, "tok-replay", stackNode)

		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "bound to node")
	}

	assert.Equal(t, 1, *calls, "the replayed token is cached as an identity but still rejected")
}

func TestStack_AllowlistedCrossNodePublisher(t *testing.T) {
	client, _ := tokenReviewFor(stackCrossSA, stackNode)
	interceptor := stackInterceptor(t, client)

	batch, err := callStack(interceptor, "tok-csp", stackOther, "gpu-node-99")

	require.NoError(t, err, "an allowlisted identity may name any node")
	assert.Equal(t, stackOther, batch.GetEvents()[0].GetNodeName(), "names are forwarded exactly as sent")
}

func TestStack_ClaimlessTokenFallsBackToPinning(t *testing.T) {
	// An older cluster issues tokens with no node claim. The publisher still
	// authenticates and keeps working, scoped to this node.
	client, _ := tokenReviewFor(stackLocalSA, "")
	interceptor := stackInterceptor(t, client)

	_, err := callStack(interceptor, "tok-old", stackNode)
	require.NoError(t, err)

	_, err = callStack(interceptor, "tok-old", stackOther)
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "still pinned to its own node")
}

func TestStack_TokenReviewOutageIsRetryableNotRejection(t *testing.T) {
	// The API server is unreachable. The publisher must get a code its retry
	// policy acts on, never a rejection of its credential.
	//
	// This costs the real retry window (see grpcauth.tokenReviewRetryWindow)
	// because that window is not configurable — which is the point: a sustained
	// outage is meant to be absorbed for that long before anyone hears about
	// it. Do not "fix" the runtime by making it a knob.
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, context.DeadlineExceeded
		})

	interceptor := stackInterceptor(t, client)

	_, err := callStack(interceptor, "tok-any", stackNode)

	assert.Equal(t, codes.Unavailable, status.Code(err))
}
