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
	"errors"
	"testing"
	"time"

	logr "github.com/go-logr/logr/testing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLatencyUnaryInterceptor(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		log          bool
		invokerErr   error
		expectedCode codes.Code
	}{
		{"Returns OK code for successful call", "/svc/success", true, nil, codes.OK},
		{"Returns internal status error on internal error", "/svc/internal_error", true, status.Error(codes.Internal, "fail"), codes.Internal},
		{"Returns canceled status error when canceled", "/svc/cancel", true, status.Error(codes.Canceled, ""), codes.Canceled},
		{"Returns deadline exceeded status error on timeout", "/svc/timeout", true, status.Error(codes.DeadlineExceeded, ""), codes.DeadlineExceeded},
		{"Does not log if level too low", "/svc/skip_log", false, nil, codes.OK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := logr.NewTestLogger(t)
			if tt.log {
				logger = logger.V(4)
			}

			interceptor := NewLatencyUnaryInterceptor(logger)

			invoker := func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
				if !tt.log {
					time.Sleep(1 * time.Millisecond)
				}
				return tt.invokerErr
			}

			err := interceptor(context.Background(), tt.method, nil, nil, nil, invoker)
			if !errors.Is(err, tt.invokerErr) {
				t.Fatalf("Returned error mismatch. Got %v, want %v", err, tt.invokerErr)
			}
		})
	}
}

func TestLatencyStreamInterceptor(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		log         bool
		streamerErr error
	}{
		{"Returns nil on successful start of stream", "/svc/start_stream", true, nil},
		{"Returns internal status error for failed stream", "/svc/stream_fail", true, status.Error(codes.Internal, "fail")},
		{"Does not log if level too low", "/svc/skip_log", false, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := logr.NewTestLogger(t)
			if tt.log {
				logger = logger.V(4)
			}

			interceptor := NewLatencyStreamInterceptor(logger)
			desc := &grpc.StreamDesc{}

			streamer := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
				if !tt.log {
					time.Sleep(1 * time.Millisecond)
				}
				return nil, tt.streamerErr
			}

			_, err := interceptor(context.Background(), desc, nil, tt.method, streamer)
			if !errors.Is(err, tt.streamerErr) {
				t.Fatalf("Returned error mismatch. Got %v, want %v", err, tt.streamerErr)
			}
		})
	}
}
