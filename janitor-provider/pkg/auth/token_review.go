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
	"fmt"
	"log/slog"
	"strings"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TokenReviewInterceptor returns a gRPC unary server interceptor that
// validates incoming requests using the Kubernetes TokenReview API.
//
// It extracts the Bearer token from the "authorization" metadata header,
// submits a TokenReview to the K8s API server, and rejects requests
// where the token is missing, invalid, or does not match the expected
// audience.
func TokenReviewInterceptor(
	k8sClient kubernetes.Interface,
	audiences []string,
) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		token, err := extractBearerToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("extracting bearer token: %w", err)
		}

		if err := validateToken(ctx, k8sClient, token, audiences); err != nil {
			return nil, fmt.Errorf("validating token: %w", err)
		}

		return handler(ctx, req)
	}
}

// extractBearerToken extracts the Bearer token from gRPC metadata.
func extractBearerToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing metadata")
	}

	authHeaders := md.Get("authorization")
	if len(authHeaders) == 0 {
		return "", status.Error(codes.Unauthenticated,
			"missing authorization header")
	}

	val := authHeaders[0]
	if !strings.HasPrefix(val, "Bearer ") {
		return "", status.Error(codes.Unauthenticated,
			"authorization header must use Bearer scheme")
	}

	token := strings.TrimPrefix(val, "Bearer ")
	if token == "" {
		return "", status.Error(codes.Unauthenticated,
			"empty bearer token")
	}

	return token, nil
}

// validateToken submits a TokenReview to the Kubernetes API server
// and checks authentication status and audience.
func validateToken(
	ctx context.Context,
	k8sClient kubernetes.Interface,
	token string,
	audiences []string,
) error {
	review := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     token,
			Audiences: audiences,
		},
	}

	result, err := k8sClient.AuthenticationV1().TokenReviews().Create(
		ctx, review, metav1.CreateOptions{},
	)
	if err != nil {
		slog.Error("TokenReview API call failed", "error", err)

		return status.Errorf(codes.Internal,
			"token validation failed: %v", err)
	}

	if !result.Status.Authenticated {
		slog.Warn("Token authentication failed",
			"error", result.Status.Error)

		return status.Error(codes.Unauthenticated,
			fmt.Sprintf("token not authenticated: %s",
				result.Status.Error))
	}

	slog.Info("Request authenticated",
		"user", result.Status.User.Username,
		"audiences", result.Status.Audiences)

	return nil
}
