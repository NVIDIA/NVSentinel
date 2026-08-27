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

package grpcauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authv1 "k8s.io/api/authentication/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	wantAudience = "nvsentinel-platform-connector"
	cspSA        = "system:serviceaccount:nvsentinel:csp-health-monitor"
)

// fakeClientWithStatus returns a clientset whose TokenReview responds with the
// given status, letting each test model one authenticator behaviour.
func fakeClientWithStatus(st authv1.TokenReviewStatus) *fake.Clientset {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			tr := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
			tr.Status = st

			return true, tr, nil
		})

	return client
}

func TestValidator_Authenticate(t *testing.T) {
	tests := []struct {
		name     string
		st       authv1.TokenReviewStatus
		wantCode codes.Code
		wantUser string
	}{
		{
			name: "authenticated with the requested audience",
			st: authv1.TokenReviewStatus{
				Authenticated: true,
				User:          authv1.UserInfo{Username: cspSA, UID: "uid-1"},
				Audiences:     []string{wantAudience},
			},
			wantCode: codes.OK,
			wantUser: cspSA,
		},
		{
			name: "not authenticated",
			st: authv1.TokenReviewStatus{
				Authenticated: false,
				Error:         "token expired",
			},
			wantCode: codes.Unauthenticated,
		},
		{
			name: "authenticated for a different service's audience",
			st: authv1.TokenReviewStatus{
				Authenticated: true,
				User:          authv1.UserInfo{Username: cspSA},
				Audiences:     []string{"nvsentinel-csp-provider"},
			},
			wantCode: codes.Unauthenticated,
		},
		{
			name: "authenticator returned no audiences",
			// An authenticator that does not understand audiences authenticates
			// the generic API-server token every pod already has mounted, and
			// echoes nothing back. Accepting that would make the audience
			// meaningless.
			st: authv1.TokenReviewStatus{
				Authenticated: true,
				User:          authv1.UserInfo{Username: cspSA},
			},
			wantCode: codes.Unauthenticated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := NewValidator(fakeClientWithStatus(tt.st), wantAudience)
			require.NoError(t, err)

			identity, err := v.Authenticate(context.Background(), "some-token")

			assert.Equal(t, tt.wantCode, status.Code(err))

			if tt.wantCode != codes.OK {
				assert.Nil(t, identity)
				return
			}

			require.NotNil(t, identity)
			assert.Equal(t, tt.wantUser, identity.Username)
		})
	}
}

func TestValidator_RequestsTheConfiguredAudience(t *testing.T) {
	var got []string

	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			tr := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
			got = tr.Spec.Audiences
			tr.Status = authv1.TokenReviewStatus{
				Authenticated: true,
				User:          authv1.UserInfo{Username: cspSA},
				Audiences:     tr.Spec.Audiences,
			}

			return true, tr, nil
		})

	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	_, err = v.Authenticate(context.Background(), "some-token")
	require.NoError(t, err)
	assert.Equal(t, []string{wantAudience}, got)
}

func TestValidator_APIErrorIsRetryableNotUnauthenticated(t *testing.T) {
	// The token may well be valid and the API server merely unreachable, so the
	// caller must see a code its retry policy actually retries on. Unavailable
	// is the only such code in commons/pkg/healthpub.isRetryable; anything else
	// would drop a health event on a transient control-plane blip.
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "tokenreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("connection refused")
		})

	v, err := NewValidator(client, wantAudience)
	require.NoError(t, err)

	fastRetries(v)

	identity, err := v.Authenticate(context.Background(), "some-token")

	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Nil(t, identity)
}

func TestNewValidator_Validation(t *testing.T) {
	t.Run("client is required", func(t *testing.T) {
		_, err := NewValidator(nil, wantAudience)
		require.Error(t, err)
	})

	t.Run("audience is required", func(t *testing.T) {
		// An audience-less TokenReview accepts the default SA token that every
		// pod already has, which is not an authentication decision.
		_, err := NewValidator(fake.NewSimpleClientset(), "")
		require.Error(t, err)
	})
}

func TestValidator_ClassifiesAPIErrors(t *testing.T) {
	// Only a fault that might clear on its own may be reported as Unavailable,
	// because that is the one code the publishers retry on. Reporting a missing
	// RBAC rule that way turns a fixable deployment error into an endless retry
	// loop with no diagnosis in sight.
	gr := schema.GroupResource{Group: "authentication.k8s.io", Resource: "tokenreviews"}

	tests := []struct {
		name     string
		apiErr   error
		wantCode codes.Code
	}{
		{
			name:     "missing RBAC is permanent",
			apiErr:   k8serrors.NewForbidden(gr, "", errors.New("cannot create tokenreviews")),
			wantCode: codes.Internal,
		},
		{
			name:     "our own credential rejected is permanent",
			apiErr:   k8serrors.NewUnauthorized("invalid bearer token"),
			wantCode: codes.Internal,
		},
		{
			name:     "malformed request is permanent",
			apiErr:   k8serrors.NewBadRequest("malformed tokenreview"),
			wantCode: codes.Internal,
		},
		{
			name:     "api server 500 is retryable",
			apiErr:   k8serrors.NewInternalError(errors.New("etcd unreachable")),
			wantCode: codes.Unavailable,
		},
		{
			name:     "throttling is retryable",
			apiErr:   k8serrors.NewTooManyRequestsError("slow down"),
			wantCode: codes.Unavailable,
		},
		{
			name:     "server timeout is retryable",
			apiErr:   k8serrors.NewTimeoutError("timed out", 1),
			wantCode: codes.Unavailable,
		},
		{
			name:     "connection failure is retryable",
			apiErr:   errors.New("connection refused"),
			wantCode: codes.Unavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "tokenreviews",
				func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.apiErr
				})

			v, err := NewValidator(client, wantAudience)
			require.NoError(t, err)

			fastRetries(v)

			identity, err := v.Authenticate(context.Background(), "some-token")

			assert.Equal(t, tt.wantCode, status.Code(err))
			assert.Nil(t, identity)
		})
	}
}

func TestValidator_PreservesCallerCancellation(t *testing.T) {
	// A caller that walked away is not an API-server outage, and reporting it as
	// one would have the publisher retry a request nobody is waiting for.
	tests := []struct {
		name     string
		ctx      func() (context.Context, context.CancelFunc)
		wantCode codes.Code
	}{
		{
			name: "canceled",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				return ctx, func() {}
			},
			wantCode: codes.Canceled,
		},
		{
			name: "deadline exceeded",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))

				return ctx, cancel
			},
			wantCode: codes.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := tt.ctx()
			defer cancel()

			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "tokenreviews",
				func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, context.Canceled
				})

			v, err := NewValidator(client, wantAudience)
			require.NoError(t, err)

			identity, err := v.Authenticate(ctx, "some-token")

			assert.Equal(t, tt.wantCode, status.Code(err))
			assert.Nil(t, identity)
		})
	}
}

func TestBearerTokenFromContext(t *testing.T) {
	tests := []struct {
		name        string
		ctx         context.Context
		wantToken   string
		wantPresent bool
		wantErr     bool
	}{
		{
			name:        "no metadata is absent, not an error",
			ctx:         context.Background(),
			wantPresent: false,
		},
		{
			name:        "no authorization header is absent, not an error",
			ctx:         metadata.NewIncomingContext(context.Background(), metadata.Pairs("other", "v")),
			wantPresent: false,
		},
		{
			name: "bearer token is extracted",
			ctx: metadata.NewIncomingContext(context.Background(),
				metadata.Pairs("authorization", "Bearer abc123")),
			wantToken:   "abc123",
			wantPresent: true,
		},
		{
			name: "a non-bearer scheme is a broken client, not an anonymous call",
			ctx: metadata.NewIncomingContext(context.Background(),
				metadata.Pairs("authorization", "Basic abc123")),
			wantErr: true,
		},
		{
			name: "an empty bearer token is an error",
			ctx: metadata.NewIncomingContext(context.Background(),
				metadata.Pairs("authorization", "Bearer ")),
			wantErr: true,
		},
		{
			// RFC 7235: the scheme token is case-insensitive.
			name: "the scheme is matched case-insensitively",
			ctx: metadata.NewIncomingContext(context.Background(),
				metadata.Pairs("authorization", "bearer abc123")),
			wantToken:   "abc123",
			wantPresent: true,
		},
		{
			name: "a scheme with no credential is an error",
			ctx: metadata.NewIncomingContext(context.Background(),
				metadata.Pairs("authorization", "Bearer")),
			wantErr: true,
		},
		{
			// Two headers means two candidate credentials. Taking the first would
			// let a caller attach a credential that is carried but never checked.
			name: "several authorization headers are rejected, not resolved",
			ctx: metadata.NewIncomingContext(context.Background(),
				metadata.MD{"authorization": []string{"Bearer first", "Bearer second"}}),
			wantErr: true,
		},
		{
			name: "an empty authorization header is an error",
			ctx: metadata.NewIncomingContext(context.Background(),
				metadata.Pairs("authorization", "")),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, present, err := BearerTokenFromContext(tt.ctx)

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, codes.Unauthenticated, status.Code(err))

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantToken, token)
			assert.Equal(t, tt.wantPresent, present)
		})
	}
}

// A destructive RPC can only be retried when the failure provably happened
// before the handler ran. Unavailable alone does not prove that.
func TestIsAuthBackendUnavailable(t *testing.T) {
	t.Run("tagged pre-handler outage is retryable", func(t *testing.T) {
		err := withAuthUnavailableDetail(
			status.New(codes.Unavailable, "token validation unavailable: boom"))
		require.True(t, IsAuthBackendUnavailable(err))
	})

	t.Run("a bare Unavailable is ambiguous and must not be", func(t *testing.T) {
		require.False(t, IsAuthBackendUnavailable(
			status.Error(codes.Unavailable, "connection refused")))
	})

	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "nil", err: nil},
		{name: "not a status", err: errors.New("plain error")},
		{name: "permission denied", err: status.Error(codes.PermissionDenied, "nope")},
		{name: "internal", err: status.Error(codes.Internal, "nope")},
		{name: "deadline exceeded", err: status.Error(codes.DeadlineExceeded, "nope")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, IsAuthBackendUnavailable(tc.err))
		})
	}
}
