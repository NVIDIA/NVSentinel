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

package central

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/kubernetes"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/kubeconfig"
)

// Scope-violation classes for the scopeViolations metric: the check that
// rejected the batch, never the caller's identity (bounded cardinality).
const (
	violationClassNoClaim   = "no-node-claim"
	violationClassNodeScope = "node-scope"
)

// newValidator builds the TokenReview-backed token validator with a client
// rate limit and a verdict cache sized for the whole fleet rather than one
// node's callers.
func newValidator(cfg *config) (*grpcauth.Validator, error) {
	restConfig, err := kubeconfig.Load("")
	if err != nil {
		return nil, fmt.Errorf("loading in-cluster kubernetes config: %w", err)
	}

	restConfig.QPS = cfg.tokenReviewQPS
	restConfig.Burst = cfg.tokenReviewBurst
	restConfig.Timeout = 10 * time.Second

	clientSet, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes clientset: %w", err)
	}

	// One authenticated request must not cost an info log line at fleet
	// scale; the audit line moves to debug.
	opts := []grpcauth.ValidatorOption{grpcauth.WithSuccessLogLevel(slog.LevelDebug)}
	if cfg.tokenCacheSize > 0 {
		opts = append(opts, grpcauth.WithCacheSize(cfg.tokenCacheSize))
	}

	return grpcauth.NewValidator(clientSet, cfg.audience, opts...)
}

type callerCtxKey struct{}

func contextWithCaller(ctx context.Context, identity *grpcauth.Identity) context.Context {
	return context.WithValue(ctx, callerCtxKey{}, identity)
}

// callerFromContext returns the authenticated caller the interceptor stored
// for this request.
func callerFromContext(ctx context.Context) *grpcauth.Identity {
	identity, _ := ctx.Value(callerCtxKey{}).(*grpcauth.Identity)
	return identity
}

// rejectScopeViolation counts and builds a node-scope rejection.
func rejectScopeViolation(class string, format string, args ...any) error {
	scopeViolations.WithLabelValues(class).Inc()
	return status.Errorf(codes.PermissionDenied, format, args...)
}

// authInterceptor authenticates the caller token, then authorizes it: every
// caller must present a pod-bound projected token whose identity is on
// ALLOWED_PUBLISHERS, and its events are pinned to the token's own node claim.
// Identities on CROSS_NODE_PUBLISHERS may name any node.
func authInterceptor(cfg *config, callerValidator *grpcauth.Validator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {
		token, present, err := grpcauth.BearerTokenFromContext(ctx)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "malformed authorization metadata")
		}

		if !present {
			return nil, status.Error(codes.Unauthenticated, "caller token required")
		}

		identity, err := callerValidator.Authenticate(ctx, token)
		if err != nil {
			return nil, err
		}

		// Pod binding is what makes the node claim attested and what scopes the
		// derived idempotency keys; a token minted without a pod reference is
		// refused.
		if identity.PodUID == "" {
			return nil, status.Error(codes.PermissionDenied,
				"caller token is not pod-bound; a projected pod-bound token is required")
		}

		if !cfg.allowedPublishers[identity.Username] {
			slog.Warn("Caller not on the publisher allowlist",
				"username", identity.Username, "pod", identity.PodName)

			return nil, status.Errorf(codes.PermissionDenied,
				"identity %q is not an allowed publisher", identity.Username)
		}

		if he, isHealthEvents := req.(*pb.HealthEvents); isHealthEvents {
			if err := authorizePublisher(cfg, identity, he); err != nil {
				return nil, err
			}
		}

		return handler(contextWithCaller(ctx, identity), req)
	}
}

// authorizePublisher applies the node scope rule, as the socket does today: a
// plain publisher's events must all name the node its token is bound to, and
// an event that left the node name blank gets that node stamped onto it; a
// cross-node publisher may name any node but must name one.
//
// Either way the token must carry a node claim. A token bound to a pod that
// was never scheduled has a pod UID but no node, so the API server has not
// tied it to a running workload anywhere. The node-local connector refuses
// such a token for cross-node callers and pins plain callers to its own node;
// there is no local node here, so both kinds are refused: for a cross-node
// publisher the token would otherwise be a credential with cluster-wide reach
// and no verified origin.
func authorizePublisher(cfg *config, identity *grpcauth.Identity, he *pb.HealthEvents) error {
	if identity.NodeName == "" {
		return rejectScopeViolation(violationClassNoClaim,
			"caller token carries no node claim; a token bound to an unscheduled pod has no verified origin")
	}

	if cfg.crossNodePublishers[identity.Username] {
		for i, ev := range he.GetEvents() {
			if ev.GetNodeName() == "" {
				return status.Errorf(codes.InvalidArgument, "event %d has no nodeName", i)
			}
		}

		return nil
	}

	for _, ev := range he.GetEvents() {
		if ev.NodeName == "" {
			ev.NodeName = identity.NodeName

			continue
		}

		if ev.GetNodeName() != identity.NodeName {
			slog.Warn("Rejecting batch: event names node outside authorized scope",
				"scopeNode", identity.NodeName, "eventNode", ev.GetNodeName(), "caller", identity.Username)

			return rejectScopeViolation(violationClassNodeScope,
				"event names node %q outside authorized scope %q", ev.GetNodeName(), identity.NodeName)
		}
	}

	return nil
}
