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

package nvgrpc

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
)

func TestDialOptions(t *testing.T) {
	testLogger := logr.Discard()

	dummyUnary := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(ctx, method, req, reply, cc, opts...)
	}
	dummyStream := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(ctx, desc, cc, method, opts...)
	}

	opts := []DialOption{
		WithLogger(testLogger),
		WithUnaryInterceptor(dummyUnary),
		WithStreamInterceptor(dummyStream),
		WithUnaryInterceptor(dummyUnary), // Test multiple appends
		WithStreamInterceptor(dummyStream),
	}

	dOpts := &dialOptions{}
	for _, opt := range opts {
		opt(dOpts)
	}

	t.Run("Logger is correctly assigned", func(t *testing.T) {
		if dOpts.logger != testLogger {
			t.Errorf("expected logger to be set, got %v", dOpts.logger)
		}
	})

	t.Run("Unary interceptors are correctly appended", func(t *testing.T) {
		if len(dOpts.unaryInterceptors) != 2 {
			t.Errorf("expected 2 unary interceptors, got %d", len(dOpts.unaryInterceptors))
		}
	})

	t.Run("Stream interceptors are correctly appended", func(t *testing.T) {
		if len(dOpts.streamInterceptors) != 2 {
			t.Errorf("expected 2 stream interceptors, got %d", len(dOpts.streamInterceptors))
		}
	})
}
