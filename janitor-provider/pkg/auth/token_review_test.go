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

package auth

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
)

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		name      string
		ctx       context.Context
		wantToken string
		wantCode  codes.Code
	}{
		{
			name:     "missing metadata",
			ctx:      context.Background(),
			wantCode: codes.Unauthenticated,
		},
		{
			name: "missing authorization header",
			ctx: metadata.NewIncomingContext(
				context.Background(),
				metadata.Pairs("other", "value")),
			wantCode: codes.Unauthenticated,
		},
		{
			name: "non-bearer scheme",
			ctx: metadata.NewIncomingContext(
				context.Background(),
				metadata.Pairs("authorization", "Basic abc123")),
			wantCode: codes.Unauthenticated,
		},
		{
			name: "empty bearer token",
			ctx: metadata.NewIncomingContext(
				context.Background(),
				metadata.Pairs("authorization", "Bearer ")),
			wantCode: codes.Unauthenticated,
		},
		{
			name: "valid bearer token",
			ctx: metadata.NewIncomingContext(
				context.Background(),
				metadata.Pairs("authorization", "Bearer my-token")),
			wantToken: "my-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := extractBearerToken(tt.ctx)
			if tt.wantCode != codes.OK {
				require.Error(t, err)
				st, ok := status.FromError(err)
				require.True(t, ok)
				assert.Equal(t, tt.wantCode, st.Code())
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantToken, token)
			}
		})
	}
}

func TestValidateToken_Authenticated(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor(
		"create", "tokenreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			tr := action.(k8stesting.CreateAction).
				GetObject().(*authv1.TokenReview)
			tr.Status = authv1.TokenReviewStatus{
				Authenticated: true,
				User: authv1.UserInfo{
					Username: "system:serviceaccount:ns:sa",
				},
				Audiences: tr.Spec.Audiences,
			}

			return true, tr, nil
		},
	)

	err := validateToken(
		context.Background(), client,
		"good-token", []string{"nvsentinel-csp-provider"})
	require.NoError(t, err)
}

func TestValidateToken_Unauthenticated(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor(
		"create", "tokenreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			tr := action.(k8stesting.CreateAction).
				GetObject().(*authv1.TokenReview)
			tr.Status = authv1.TokenReviewStatus{
				Authenticated: false,
				Error:         "token expired",
			}

			return true, tr, nil
		},
	)

	err := validateToken(
		context.Background(), client,
		"bad-token", []string{"nvsentinel-csp-provider"})
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
	assert.Contains(t, st.Message(), "token expired")
}

func TestTokenReviewInterceptor_Success(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor(
		"create", "tokenreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			tr := action.(k8stesting.CreateAction).
				GetObject().(*authv1.TokenReview)
			tr.Status = authv1.TokenReviewStatus{
				Authenticated: true,
				User: authv1.UserInfo{
					Username: "system:serviceaccount:ns:sa",
				},
				Audiences: tr.Spec.Audiences,
			}

			return true, tr, nil
		},
	)

	interceptor := TokenReviewInterceptor(
		client, []string{"nvsentinel-csp-provider"})

	ctx := metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs("authorization", "Bearer valid-token"))

	handlerCalled := false
	handler := func(
		ctx context.Context, req any,
	) (any, error) {
		handlerCalled = true

		return "ok", nil
	}

	resp, err := interceptor(
		ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/test"},
		handler)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp)
	assert.True(t, handlerCalled)
}

func TestTokenReviewInterceptor_MissingToken(t *testing.T) {
	client := fake.NewSimpleClientset()

	interceptor := TokenReviewInterceptor(
		client, []string{"nvsentinel-csp-provider"})

	// No authorization metadata
	ctx := metadata.NewIncomingContext(
		context.Background(), metadata.Pairs())

	handler := func(
		ctx context.Context, req any,
	) (any, error) {
		t.Fatal("handler should not be called")

		return nil, nil
	}

	_, err := interceptor(
		ctx, nil,
		&grpc.UnaryServerInfo{FullMethod: "/test"},
		handler)
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}
