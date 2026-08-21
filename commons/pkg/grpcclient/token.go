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

// Package grpcclient holds the shared client half of NVSentinel's gRPC
// ServiceAccount-token authentication (ADR-030): a unary interceptor that
// attaches a projected SA token as a Bearer credential on every call.
//
// The server half lives in commons/pkg/grpcauth.
package grpcclient

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// TokenInterceptor returns a gRPC unary client interceptor that reads a
// ServiceAccount token from tokenPath on every call and attaches it as a
// Bearer token in the "authorization" gRPC metadata header.
//
// The token is re-read on each invocation so that kubelet's rotation of a
// projected token is picked up without a restart.
func TokenInterceptor(tokenPath string) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		//nolint:gosec // G304: tokenPath is operator-controlled config, not user input.
		tokenBytes, err := os.ReadFile(tokenPath)
		if err != nil {
			return fmt.Errorf("reading SA token from %q: %w", tokenPath, err)
		}

		token := string(tokenBytes)

		// An empty file is a broken mount, not a credential. Sending "Bearer "
		// would get a generic "token not authenticated" back from the server and
		// send whoever debugs it looking at RBAC and audiences; failing here
		// names the actual problem.
		if token == "" {
			return fmt.Errorf("SA token file %q is empty", tokenPath)
		}

		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// DialOptions returns the dial options needed to authenticate to an NVSentinel
// gRPC service with a projected SA token, or nil when tokenPath is empty
// (authentication disabled). Appending a nil slice is a no-op, so callers can
// use it unconditionally:
//
//	opts = append(opts, grpcclient.DialOptions(tokenPath)...)
func DialOptions(tokenPath string) []grpc.DialOption {
	if tokenPath == "" {
		return nil
	}

	return []grpc.DialOption{grpc.WithUnaryInterceptor(TokenInterceptor(tokenPath))}
}
