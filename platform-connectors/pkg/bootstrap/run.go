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

package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	srv "github.com/nvidia/nvsentinel/commons/pkg/server"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/configfile"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/server"
)

// Role is one of the two shapes of the platform connector: the node-local
// DaemonSet serving a Unix socket, or the deployment platform connector
// serving the fleet over TLS.
type Role interface {
	// Connectors says how the connectors run, once the shared config.json
	// is loaded.
	Connectors(raw map[string]any) (ConnectorOptions, error)
	// Server builds the role's server pieces from the loaded config, the
	// process options and the built connectors.
	Server(ctx context.Context, raw map[string]any, opts Options, conns *Connectors) (*Spec, error)
}

// Spec is what a role contributes to the shared server.
type Spec struct {
	Listener net.Listener
	// ServerOptions secure and tune the connection: transport credentials,
	// keepalive, buffer sizes. The interceptor chain is added by Run.
	ServerOptions []grpc.ServerOption
	// Interceptors run in this order in front of the shared handler.
	Interceptors []grpc.UnaryServerInterceptor
	// Ready, when set, is the role's readiness condition, answered on
	// /readyz together with "not shutting down". The node-local role has
	// none: its probes use /healthz.
	Ready func() error
	// Background tasks run until shutdown begins, when their context ends;
	// one returning an error stops the server.
	Background []func(ctx context.Context) error
	// StopTimeout bounds the wait for requests in flight at shutdown; both
	// roles set a positive value.
	StopTimeout time.Duration
}

// Options are the process settings both roles take from flags.
type Options struct {
	ConfigPath  string
	MetricsPort int
	// KubeconfigPath is empty for the in-cluster configuration.
	KubeconfigPath string
	// CertMountPath is the datastore client certificate directory, empty
	// when the datastore connection is not TLS.
	CertMountPath string
}

// Run brings a role up and serves until a signal or a failure, then shuts
// down in order. It returns nil after a clean shutdown.
func Run(ctx context.Context, role Role, opts Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	raw, err := configfile.Load(opts.ConfigPath)
	if err != nil {
		return err
	}

	k8s, err := kubernetes.SettingsFromConfig(raw)
	if err != nil {
		return fmt.Errorf("kubernetes connector settings: %w", err)
	}

	mode, err := role.Connectors(raw)
	if err != nil {
		return err
	}

	conns, err := BuildConnectors(ctx, raw, k8s, opts, mode)
	if err != nil {
		return err
	}

	// The metadata transformer's Kubernetes client shares the connector's
	// rate limit: one setting covers the role's node reads and writes.
	pipe, err := pipeline.NewFromRawConfig(ctx, raw, pipeline.Options{
		KubeconfigPath:  opts.KubeconfigPath,
		KubeClientQPS:   k8s.QPS,
		KubeClientBurst: k8s.Burst,
	})
	if err != nil {
		conns.Shutdown(ctx)

		return fmt.Errorf("failed to initialize pipeline: %w", err)
	}

	defer pipe.Close()

	spec, err := role.Server(ctx, raw, opts, conns)
	if err != nil {
		conns.Shutdown(ctx)

		return err
	}

	grpcServer := grpc.NewServer(append(spec.ServerOptions, grpc.ChainUnaryInterceptor(spec.Interceptors...))...)
	pb.RegisterPlatformConnectorServer(grpcServer, &server.PlatformConnectorServer{Pipeline: pipe, Connector: conns.Set})

	return serve(ctx, cancel, spec, grpcServer, conns, opts.MetricsPort)
}

// readiness answers /readyz: the role's condition, and not shutting down.
type readiness struct {
	ready        func() error
	shuttingDown atomic.Bool
}

func (r *readiness) Ready(context.Context) error {
	if r.shuttingDown.Load() {
		return errors.New("shutting down")
	}

	return r.ready()
}

// serve runs the gRPC server, the metrics and probe server, the role's
// background tasks and the signal handler until one of them ends the group.
func serve(
	ctx context.Context, cancel context.CancelFunc, spec *Spec,
	grpcServer *grpc.Server, conns *Connectors, metricsPort int,
) error {
	g, gCtx := errgroup.WithContext(ctx)

	backgroundCtx, stopBackground := context.WithCancel(gCtx)
	defer stopBackground()

	for _, task := range spec.Background {
		g.Go(func() error { return task(backgroundCtx) })
	}

	g.Go(func() error {
		slog.InfoContext(gCtx, "gRPC server listening", "addr", spec.Listener.Addr().String())

		// Serve returns nil once Stop or GracefulStop has run, or
		// ErrServerStopped when they ran first: the normal end of a shutdown,
		// not an error.
		if err := grpcServer.Serve(spec.Listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("gRPC server: %w", err)
		}

		return nil
	})

	metricsOpts := []srv.Option{srv.WithPort(metricsPort), srv.WithPrometheusMetrics(), srv.WithSimpleHealth()}

	var probe *readiness

	if spec.Ready != nil {
		probe = &readiness{ready: spec.Ready}
		metricsOpts = append(metricsOpts, srv.WithReadinessCheck(probe))
	}

	metrics := srv.NewServer(metricsOpts...)

	// This server also answers the probes, so its failure stops the process
	// instead of leaving it serving unobserved.
	g.Go(func() error {
		if err := metrics.Serve(gCtx); err != nil {
			return fmt.Errorf("metrics and probe server: %w", err)
		}

		return nil
	})

	g.Go(func() error {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

		defer signal.Stop(sigs)

		select {
		case sig := <-sigs:
			slog.InfoContext(gCtx, "Received signal, shutting down", "signal", sig)
		case <-gCtx.Done():
			slog.InfoContext(gCtx, "Shutting down", "cause", context.Cause(gCtx))
		}

		// Unready first, so the probes take the replica out of the Service
		// while requests in flight finish; the background tasks end before
		// the connectors they may use are shut down.
		if probe != nil {
			probe.shuttingDown.Store(true)
		}

		stopBackground()
		shutdown(gCtx, spec, grpcServer, conns)
		cancel()

		return nil
	})

	return g.Wait()
}

// shutdown stops accepting, lets requests in flight finish for a bounded
// time, then stops the connectors. Closing the listener also removes a Unix
// socket file.
func shutdown(ctx context.Context, spec *Spec, grpcServer *grpc.Server, conns *Connectors) {
	// GracefulStop waits for every open connection; one wedged client would
	// otherwise block shutdown forever, so it is bounded.
	done := make(chan struct{})

	go func() {
		grpcServer.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(spec.StopTimeout):
		slog.WarnContext(ctx, "GracefulStop timed out, forcing Stop", "timeout", spec.StopTimeout)
		grpcServer.Stop()
	}

	conns.Shutdown(ctx)
}
