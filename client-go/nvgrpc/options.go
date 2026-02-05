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
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
)

// DialOption configures the gRPC client connection.
type DialOption func(*dialOptions)

type dialOptions struct {
	logger             logr.Logger
	unaryInterceptors  []grpc.UnaryClientInterceptor
	streamInterceptors []grpc.StreamClientInterceptor
}

// WithLogger sets the logger to be used by the client.
func WithLogger(logger logr.Logger) DialOption {
	return func(opts *dialOptions) {
		opts.logger = logger
	}
}

// WithUnaryInterceptor adds a unary client interceptor to the chain.
func WithUnaryInterceptor(interceptor grpc.UnaryClientInterceptor) DialOption {
	return func(opts *dialOptions) {
		opts.unaryInterceptors = append(opts.unaryInterceptors, interceptor)
	}
}

// WithStreamInterceptor adds a stream client interceptor to the chain.
func WithStreamInterceptor(interceptor grpc.StreamClientInterceptor) DialOption {
	return func(opts *dialOptions) {
		opts.streamInterceptors = append(opts.streamInterceptors, interceptor)
	}
}
