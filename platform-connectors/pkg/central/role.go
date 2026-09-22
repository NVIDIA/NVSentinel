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

// Package central is the deployment platform connector: the platform
// connector binary (-mode=deployment) serving the same PlatformConnector
// gRPC service as the node-local DaemonSet, but over TCP with TLS to the whole
// fleet, with a small fixed pool of datastore connections.
//
// It shares the start-up, the connectors, the pipeline and the request
// handler with the node-local role (pkg/bootstrap). What differs:
//   - callers authenticate with projected ServiceAccount tokens (TokenReview)
//     and every batch is pinned to the caller token's node claim;
//   - there is no queue: the connectors run inside the request (see
//     bootstrap.ConnectorOptions);
//   - every batch carries an idempotency key, so a resent batch is never
//     stored twice;
//   - connections are closed after a set age or idle time, and shutdown waits
//     a bounded time for open connections;
//   - a replica with a store connector becomes ready only once the
//     idempotency index is verified.
package central

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/bootstrap"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// AppName is the deployment platform connector's application name: main
// names the logger, the audit log and the tracing service with it, and the
// chart derives the Kubernetes objects and the TLS certificate from the same
// name.
const AppName = "platform-connector-deployment"

const (
	// indexVerifyInterval is how often an unready replica checks the
	// idempotency index while waiting for the datastore setup to create it;
	// indexVerifyTimeout bounds one check, so a datastore call that never
	// returns cannot stall the wait; indexVerifyBudget is how long a replica
	// waits in all before it exits, so an index that never appears shows up
	// as a crash loop instead of a pod that is quietly not ready.
	indexVerifyInterval = 5 * time.Second
	indexVerifyTimeout  = 30 * time.Second
	indexVerifyBudget   = 5 * time.Minute
	// stopTimeout bounds the wait for requests in flight at shutdown, so one
	// unresponsive client cannot block it. The chart's termination grace
	// period is sized from it (this wait, then the datastore disconnect and
	// the trace exporter, 5 s each).
	stopTimeout = 20 * time.Second
)

type role struct {
	opts     Options
	settings settings
	auth     auth.Settings
}

// New returns the deployment role.
func New(opts Options) (bootstrap.Role, error) {
	if opts.TLSCertDir == "" && !opts.InsecureDevelopmentMode {
		return nil, errors.New("-tls-cert-dir is required unless -tls-insecure-development-mode is set: " +
			"the caller token crosses the pod network")
	}

	return &role{opts: opts}, nil
}

// Connectors reads everything the role needs from config.json, so a bad
// setting is reported before the datastore is contacted.
func (r *role) Connectors(raw map[string]any) (bootstrap.ConnectorOptions, error) {
	var err error

	if r.settings, err = settingsFromConfig(raw); err != nil {
		return bootstrap.ConnectorOptions{}, err
	}

	if r.auth, err = deploymentAuthSettings(raw); err != nil {
		return bootstrap.ConnectorOptions{}, err
	}

	return bootstrap.ConnectorOptions{BestEffortTimeout: r.settings.conditionUpdateTimeout}, nil
}

func (r *role) Server(
	ctx context.Context, _ map[string]any, opts bootstrap.Options, conns *bootstrap.Connectors,
) (*bootstrap.Spec, error) {
	// The TokenReview client is sized for a call on the path of every batch
	// of the fleet, not for one node's callers, and a line per success would
	// be most of the log.
	validator, err := auth.NewTokenReviewValidator(opts.KubeconfigPath, r.auth.Audience,
		r.settings.tokenReviewQPS, r.settings.tokenReviewBurst,
		grpcauth.WithSuccessLogLevel(slog.LevelDebug), grpcauth.WithCacheSize(r.settings.tokenCacheSize))
	if err != nil {
		return nil, err
	}

	authInterceptor, err := newAuthInterceptor(ctx, r.auth, validator)
	if err != nil {
		return nil, err
	}

	gate := &indexGate{}

	spec := &bootstrap.Spec{
		// Authenticate and scope the caller, then key the batch, then refuse
		// it while the index is unverified; the handler validates, runs the
		// pipeline and hands the batch to the connectors.
		Interceptors:  []grpc.UnaryServerInterceptor{authInterceptor, idempotencyInterceptor, readinessInterceptor(gate)},
		ServerOptions: r.serverOptions(),
		Ready:         gate.Ready,
		StopTimeout:   stopTimeout,
	}

	if conns.Store == nil {
		// No index to verify.
		gate.verified.Store(true)
	} else {
		store := conns.Store

		spec.Background = append(spec.Background, func(ctx context.Context) error {
			return waitForIndex(ctx, store, gate, indexVerifyInterval, indexVerifyTimeout, indexVerifyBudget)
		})
	}

	if r.opts.TLSCertDir == "" {
		slog.WarnContext(ctx, "Serving PLAINTEXT: -tls-insecure-development-mode is set; never use this outside development")
	} else {
		cw, err := newCertWatcher(r.opts.TLSCertDir)
		if err != nil {
			return nil, err
		}

		spec.ServerOptions = append(spec.ServerOptions, grpc.Creds(credentials.NewTLS(&tls.Config{
			GetCertificate: cw.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		})))

		// The watcher's event and polling loops are what pick up certificate
		// rotations; without them the listener would serve the startup pair
		// forever.
		spec.Background = append(spec.Background, func(ctx context.Context) error {
			if err := cw.Start(ctx); err != nil && ctx.Err() == nil {
				return fmt.Errorf("certificate watcher: %w", err)
			}

			return nil
		})
	}

	var lc net.ListenConfig

	spec.Listener, err = lc.Listen(ctx, "tcp", r.opts.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", r.opts.ListenAddr, err)
	}

	slog.InfoContext(ctx, "Deployment platform connector configured", "tls", r.opts.TLSCertDir != "",
		"audience", r.auth.Audience, "conditionUpdateTimeout", r.settings.conditionUpdateTimeout)

	return spec, nil
}

// serverOptions are the connection lifetimes and per-connection buffers.
// grpc-go itself spreads MaxConnectionAge by plus or minus 10 percent per
// connection, so fleet connections do not expire in synchronized waves.
func (r *role) serverOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:  r.settings.maxConnAge,
			MaxConnectionIdle: r.settings.maxConnIdle,
		}),
		grpc.ReadBufferSize(r.settings.grpcReadBufferBytes),
		grpc.WriteBufferSize(r.settings.grpcWriteBufferBytes),
	}
}

// indexGate is the deployment role's readiness condition and its write gate:
// the idempotency index is verified, so no batch is acknowledged before the
// datastore can suppress its duplicates. A failed write leaves it alone:
// replicas stay in the Service through datastore outages, as the design
// requires.
type indexGate struct {
	verified atomic.Bool
}

func (g *indexGate) Ready() error {
	if !g.verified.Load() {
		return errors.New("idempotency index not verified yet")
	}

	return nil
}

// indexVerifier is the one store connector call the gate needs.
type indexVerifier interface {
	VerifyIdempotencyIndex(ctx context.Context) error
}

// waitForIndex opens the gate once the idempotency index matches its expected
// definition, checking every `every` until it does: this is what orders the
// datastore setup before any client traffic. Each check has its own deadline.
// The wait ends with ctx, or with an error once budget has passed without a
// verified index: the process then exits and Kubernetes restarts it, so a
// missing index is visible as a crash loop and the wait resumes with each
// restart. The index is not re-checked afterwards: it can only go missing
// through an operator action on the datastore, and the restore procedure
// re-runs the setup and restarts the replicas, which verify it again at
// start.
func waitForIndex(
	ctx context.Context, verifier indexVerifier, gate *indexGate, every, attemptTimeout, budget time.Duration,
) error {
	deadline := time.Now().Add(budget)

	for {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		err := verifier.VerifyIdempotencyIndex(attemptCtx)

		cancel()

		select {
		case <-ctx.Done():
			// Shutdown ended the wait; not a failure.
			return nil
		default:
		}

		if err == nil {
			gate.verified.Store(true)
			slog.InfoContext(ctx, "Idempotency index verified, replica is ready")

			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("idempotency index not verified within %s, exiting so the failure is visible "+
				"(the datastore setup creates the index; see the last check's error): %w", budget, err)
		}

		if errors.Is(err, datastore.ErrIndexMissing) || errors.Is(err, datastore.ErrIndexMismatch) {
			slog.WarnContext(ctx, "Idempotency index not there yet, waiting for the datastore setup to create it",
				"error", err, "retryIn", every)
		} else {
			slog.WarnContext(ctx, "Idempotency index check failed, retrying", "error", err, "retryIn", every)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}
