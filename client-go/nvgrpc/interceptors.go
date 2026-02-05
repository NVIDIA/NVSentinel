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
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NewLatencyUnaryInterceptor returns an interceptor that logs the latency and status of unary RPCs.
func NewLatencyUnaryInterceptor(logger logr.Logger) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{},
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		duration := time.Since(start)

		s := status.Convert(err)
		code := s.Code()
		kv := []interface{}{
			"grpc.method", method,
			"duration", duration,
			"code", int(code),
		}

		if err != nil {
			if code == codes.Canceled || code == codes.DeadlineExceeded {
				logger.V(4).Info("RPC finished with context error", kv...)
				return err
			}

			logger.Error(err, "RPC failed", kv...)

			return err
		}

		if logger.V(6).Enabled() {
			logger.V(6).Info("RPC succeeded", kv...)
		}

		return nil
	}
}

// NewLatencyStreamInterceptor returns an interceptor that logs the latency and status of stream establishment.
func NewLatencyStreamInterceptor(logger logr.Logger) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn,
		method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		start := time.Now()
		stream, err := streamer(ctx, desc, cc, method, opts...)
		duration := time.Since(start)

		s := status.Convert(err)
		code := s.Code()
		kv := []interface{}{
			"grpc.method", method,
			"duration", duration,
			"code", int(code),
		}

		if err != nil {
			if code == codes.Canceled || code == codes.DeadlineExceeded {
				logger.V(4).Info("Stream establishment canceled", kv...)
				return stream, err
			}

			logger.Error(err, "Stream establishment failed", kv...)

			return stream, err
		}

		if logger.V(4).Enabled() {
			logger.V(4).Info("Stream started", kv...)
		}

		return stream, nil
	}
}
