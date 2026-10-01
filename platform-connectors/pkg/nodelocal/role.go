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

// Package nodelocal is the platform connector's node-local role: one pod per
// node, a DaemonSet, serving the PlatformConnector gRPC service on a Unix
// socket. Every accepted batch goes to queues, one ring buffer per enabled
// connector, and is acknowledged at once; the connectors process it from
// their queues and retry on their own. Callers that present a token are
// bound to this node by the node-binding interceptor.
package nodelocal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/bootstrap"
)

// TokenReview client rate limit for one node's callers. client-go's
// defaults (5 QPS, 10 burst) are meant for controllers that write
// occasionally, not for a call on the path of every cross-node health event:
// at 5 QPS a burst of events from the cluster-scoped publishers would queue
// in the client's rate limiter.
const (
	tokenReviewQPS   = 50
	tokenReviewBurst = 100
	// stopTimeout bounds the wait for requests in flight at shutdown: a
	// socket request is an enqueue behind at most one TokenReview.
	stopTimeout = 10 * time.Second
)

// Options are the node-local role's settings, from flags.
type Options struct {
	// Socket is the Unix socket path the publishers on this node dial.
	Socket string
}

type role struct {
	opts Options
}

// New returns the node-local role.
func New(opts Options) (bootstrap.Role, error) {
	if opts.Socket == "" {
		return nil, errors.New("-socket is required")
	}

	return &role{opts: opts}, nil
}

func (*role) Connectors(map[string]any) (bootstrap.ConnectorOptions, error) {
	return bootstrap.ConnectorOptions{Queued: true}, nil
}

func (r *role) Server(
	ctx context.Context, raw map[string]any, opts bootstrap.Options, _ *bootstrap.Connectors,
) (*bootstrap.Spec, error) {
	interceptor, err := authInterceptor(ctx, raw, opts.KubeconfigPath)
	if err != nil {
		return nil, err
	}

	lis, err := listen(ctx, r.opts.Socket)
	if err != nil {
		return nil, err
	}

	spec := &bootstrap.Spec{Listener: lis, StopTimeout: stopTimeout}
	if interceptor != nil {
		spec.Interceptors = []grpc.UnaryServerInterceptor{interceptor}
	}

	return spec, nil
}

// listen opens the Unix socket. The socket stays group/world accessible: the
// publishers that write to it run non-root at assorted UIDs, so tightening
// the mode here would require every publisher to change. Which node a caller
// may report on is decided by the node-binding interceptor, not by file
// permissions. Closing the listener at shutdown removes the socket file.
func listen(ctx context.Context, socket string) (net.Listener, error) {
	// A socket file left behind by a previous run would refuse the bind.
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to remove existing socket: %w", err)
	}

	var lc net.ListenConfig

	lis, err := lc.Listen(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on unix socket %s: %w", socket, err)
	}

	if err := os.Chmod(socket, 0o666); err != nil {
		_ = lis.Close()

		return nil, fmt.Errorf("failed to set socket permissions: %w", err)
	}

	return lis, nil
}

// authInterceptor builds the node-binding interceptor that keeps a publisher
// on one node from submitting health events naming another node, from the
// settings the shared config.json carries. It returns nil when node binding
// is explicitly disabled, in which case any caller may name any node; that
// is not a supported production configuration.
func authInterceptor(
	ctx context.Context, raw map[string]any, kubeconfigPath string,
) (grpc.UnaryServerInterceptor, error) {
	settings, err := auth.SettingsFromConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("node-binding auth settings: %w", err)
	}

	if !settings.Enabled {
		slog.WarnContext(ctx, "Node-binding authentication is DISABLED. Any caller able to reach the "+
			"platform-connector socket may submit health events naming any node in the cluster.")

		return nil, nil
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		return nil, errors.New("NODE_NAME environment variable is required when node-binding auth is enabled")
	}

	validator, err := auth.NewTokenReviewValidator(kubeconfigPath, settings.Audience, tokenReviewQPS, tokenReviewBurst)
	if err != nil {
		return nil, err
	}

	interceptor, err := auth.NewNodeBindingInterceptor(auth.Config{
		NodeName:                 nodeName,
		Validator:                validator,
		CrossNodeServiceAccounts: settings.CrossNodeServiceAccounts,
		Mode:                     settings.Mode,
		FailOpenOnUnavailable:    settings.FailOpenOnUnavailable,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build node-binding interceptor: %w", err)
	}

	return interceptor, nil
}
