// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package initializer wires nvhv-health-monitor together: the NVHV client, the
// platform connector publisher, the monitor, the poller and the metrics server.
package initializer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/monitor"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/nvhvclient"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/poller"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/publisher"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdict"
)

// Params holds the command-line parameters.
type Params struct {
	NVHVAddress            string
	NodeName               string
	PollInterval           time.Duration
	RequestTimeout         time.Duration // zero means the poll interval
	UnreachableAfter       time.Duration
	CollectionFailureAfter time.Duration
	MetricsPort            int
	// StateFile keeps the raised outage events across restarts; empty disables it.
	StateFile string
	// ProcessingStrategy is set on every event: a name of pb.ProcessingStrategy,
	// STORE_ONLY by default (the flag's default).
	ProcessingStrategy string

	PlatformConnectorSocket string
	PlatformConnectorToken  string
}

// Components holds the initialized runtime components.
type Components struct {
	Poller    *poller.Poller
	Server    server.Server
	Publisher *publisher.Publisher

	nvhvConn *grpc.ClientConn
}

// Close releases the publisher and the NVHV connection.
func (c *Components) Close() {
	c.Publisher.Close()

	if err := c.nvhvConn.Close(); err != nil {
		slog.Warn("Close NVHV connection", "error", err)
	}
}

// InitializeAll validates the parameters and builds every component.
func InitializeAll(params Params) (*Components, error) {
	timeout, err := validate(params)
	if err != nil {
		return nil, err
	}

	strategyValue, ok := pb.ProcessingStrategy_value[params.ProcessingStrategy]
	if !ok {
		return nil, fmt.Errorf("unexpected processingStrategy value: %q", params.ProcessingStrategy)
	}

	slog.Info("Event handling strategy configured", "processingStrategy", params.ProcessingStrategy)

	nvhvConn, err := grpc.NewClient(params.NVHVAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to create the gRPC client for NVHV at %s: %w", params.NVHVAddress, err)
	}

	// With HEALTH_PUBLISH_TARGET set the client talks to the deployment platform
	// connector directly; otherwise it dials the node-local socket. The option
	// hands the connection to the publisher, which closes it in Close.
	_, pcClient, pubOpt, err := healthpub.DialFromEnvOr(func() (*grpc.ClientConn, error) {
		return grpc.NewClient(params.PlatformConnectorSocket, grpcclient.InsecureDialOptions(params.PlatformConnectorToken)...)
	})
	if err != nil {
		_ = nvhvConn.Close()

		return nil, fmt.Errorf("failed to connect to the platform connector at %s: %w", params.PlatformConnectorSocket, err)
	}

	pub := publisher.New(pcClient, params.PlatformConnectorSocket, params.NodeName, pubOpt)

	stateFile := statefile.File{Path: params.StateFile}

	restored, err := stateFile.Load()
	if err != nil {
		slog.Warn("Read state file; starting without it", "path", params.StateFile, "error", err)
	}

	m := monitor.New(monitor.Config{
		Agent:                  publisher.AgentName,
		NodeName:               params.NodeName,
		UnreachableAfter:       params.UnreachableAfter,
		CollectionFailureAfter: params.CollectionFailureAfter,
		Publish:                pub.Publish,
		ProcessingStrategy:     pb.ProcessingStrategy(strategyValue),
		Restore:                restored,
		Save:                   stateFile.Save,
	})

	// Healthy while polls keep completing. A failed call to NVHV still counts:
	// NVHV being down is reported by the NVHVUnreachable event and the
	// unreachable metric, and restarting this container would not help. A poll
	// blocked on the central platform connector is waiting, not hung.
	healthChecker := server.NewPollingHealthChecker(3*params.PollInterval + timeout)
	healthChecker.AllowWaitingOn(pub.WaitingOnServer)

	return &Components{
		Poller: &poller.Poller{
			Evaluator: nvhvclient.New(nvhvConn),
			Handle: func(ctx context.Context, v *verdict.Verdict, err error) {
				m.Handle(ctx, v, err)
				healthChecker.MarkAlive()
			},
			Request:  m.Request,
			Interval: params.PollInterval,
			Timeout:  timeout,
		},
		Server: server.NewServer(
			server.WithPort(params.MetricsPort),
			server.WithPrometheusMetrics(),
			server.WithHealthCheck(healthChecker),
		),
		Publisher: pub,
		nvhvConn:  nvhvConn,
	}, nil
}

// validate checks the parameters and returns the effective request timeout.
func validate(params Params) (time.Duration, error) {
	if params.NodeName == "" {
		return 0, errors.New("node name is required (--node-name or NODE_NAME)")
	}

	if params.PollInterval <= 0 {
		return 0, errors.New("poll-interval must be positive")
	}

	timeout := params.RequestTimeout
	if timeout == 0 {
		timeout = params.PollInterval
	}

	if timeout < 0 || timeout > params.PollInterval {
		return 0, fmt.Errorf("request-timeout must be positive and at most the poll interval (%s)", params.PollInterval)
	}

	return timeout, nil
}
