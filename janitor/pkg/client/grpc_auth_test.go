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

package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

func TestTokenInterceptor_AttachesToken(t *testing.T) {
	tokenPath := writeTestToken(t, "test-token-value")
	interceptor := TokenInterceptor(tokenPath)

	var capturedCtx context.Context

	fakeInvoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		capturedCtx = ctx
		return nil
	}

	err := interceptor(context.Background(), "/test.Method", nil, nil, nil, fakeInvoker)
	require.NoError(t, err)

	md, ok := metadata.FromOutgoingContext(capturedCtx)
	require.True(t, ok, "outgoing metadata should be present")

	authValues := md.Get("authorization")
	require.Len(t, authValues, 1)
	assert.Equal(t, "Bearer test-token-value", authValues[0])
}

func TestTokenInterceptor_ReReadsToken(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("token-v1"), 0o600))

	interceptor := TokenInterceptor(tokenPath)

	// First call reads token-v1
	var firstCtx context.Context

	fakeInvoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		firstCtx = ctx
		return nil
	}

	require.NoError(t, interceptor(context.Background(), "/test", nil, nil, nil, fakeInvoker))

	md1, _ := metadata.FromOutgoingContext(firstCtx)
	assert.Equal(t, "Bearer token-v1", md1.Get("authorization")[0])

	// Rotate token
	require.NoError(t, os.WriteFile(tokenPath, []byte("token-v2"), 0o600))

	// Second call should read token-v2
	var secondCtx context.Context

	fakeInvoker2 := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		secondCtx = ctx
		return nil
	}

	require.NoError(t, interceptor(context.Background(), "/test", nil, nil, nil, fakeInvoker2))

	md2, _ := metadata.FromOutgoingContext(secondCtx)
	assert.Equal(t, "Bearer token-v2", md2.Get("authorization")[0])
}

func TestTokenInterceptor_MissingTokenFile(t *testing.T) {
	interceptor := TokenInterceptor("/nonexistent/token")

	fakeInvoker := func(_ context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		t.Fatal("invoker should not be called when token file is missing")
		return nil
	}

	err := interceptor(context.Background(), "/test", nil, nil, nil, fakeInvoker)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "reading SA token")
}

func TestTokenInterceptor_TrimsWhitespace(t *testing.T) {
	tokenPath := writeTestToken(t, "  my-token\n")
	interceptor := TokenInterceptor(tokenPath)

	var capturedCtx context.Context

	fakeInvoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		capturedCtx = ctx
		return nil
	}

	require.NoError(t, interceptor(context.Background(), "/test", nil, nil, nil, fakeInvoker))

	md, _ := metadata.FromOutgoingContext(capturedCtx)
	assert.Equal(t, "Bearer my-token", md.Get("authorization")[0])
}

func writeTestToken(t *testing.T, content string) string {
	t.Helper()

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte(content), 0o600))

	return tokenPath
}
