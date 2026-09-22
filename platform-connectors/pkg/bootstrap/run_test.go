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

package bootstrap_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/bootstrap"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/nodelocal"
)

// testConfig is a config.json with every connector off and node binding
// disabled, enough for Run to come up without a cluster.
const testConfig = `{
	"enableNodeBindingAuth": "false",
	"enableK8sPlatformConnector": "false", "enableGRPCSinkConnector": "false", "enablePromPlatformConnector": "false",
	"enableMongoDBStorePlatformConnector": "false", "enablePostgresDBStorePlatformConnector": "false",
	"K8sConnectorQps": 5.00, "K8sConnectorBurst": 10,
	"MaxNodeConditionMessageLength": 1024, "CompactedHealthEventMsgLen": 256,
	"pipeline": [{"name": "Deduplicator", "enabled": false, "config": "/nonexistent/dedup.toml"}]
}`

func writeTestConfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(testConfig), 0o600))

	return path
}

// tcpRole is the smallest deployment-shaped role: a TCP listener, no
// interceptors, and whatever readiness condition and background tasks the
// test hands it.
type tcpRole struct {
	ready      func() error
	background []func(context.Context) error
}

func (tcpRole) Connectors(map[string]any) (bootstrap.ConnectorOptions, error) {
	return bootstrap.ConnectorOptions{BestEffortTimeout: time.Second}, nil
}

func (r tcpRole) Server(context.Context, map[string]any, bootstrap.Options, *bootstrap.Connectors) (*bootstrap.Spec, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	return &bootstrap.Spec{Listener: lis, Ready: r.ready, Background: r.background, StopTimeout: time.Second}, nil
}

func freePort(t *testing.T) int {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	port := lis.Addr().(*net.TCPAddr).Port
	require.NoError(t, lis.Close())

	return port
}

// TestRun_ServesAndStopsCleanly: Run brings the node-local role up from
// config.json, answers a batch on its socket and the probes on the metrics port,
// and returns nil once its context ends, with the socket file gone.
func TestRun_ServesAndStopsCleanly(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "pc.sock")
	configPath := writeTestConfig(t)

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	role, err := nodelocal.New(nodelocal.Options{Socket: socket})
	require.NoError(t, err)

	done := make(chan error, 1)

	go func() {
		done <- bootstrap.Run(ctx, role, bootstrap.Options{ConfigPath: configPath, MetricsPort: port})
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(socket)

		return err == nil
	}, 10*time.Second, 20*time.Millisecond, "the socket appears")

	conn, err := grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
	defer callCancel()

	_, err = pb.NewPlatformConnectorClient(conn).HealthEventOccurredV1(callCtx, &pb.HealthEvents{Version: 1})
	require.NoError(t, err, "an empty batch is accepted with no connector enabled")

	require.Eventually(t, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err != nil {
			return false
		}

		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond, "the probe answers on the metrics port")

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a shutdown by context is a clean exit")
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}

	_, err = os.Stat(socket)
	require.True(t, os.IsNotExist(err), "closing the listener removes the socket file")
}

// TestRun_BackgroundFailureEndsTheRun: a background task's error (the index
// wait giving up after its budget) stops the server, shuts down and comes
// back from Run, so the process exits and Kubernetes restarts it.
func TestRun_BackgroundFailureEndsTheRun(t *testing.T) {
	role := tcpRole{background: []func(context.Context) error{
		func(context.Context) error { return errors.New("index budget spent") },
	}}

	err := bootstrap.Run(context.Background(), role, bootstrap.Options{ConfigPath: writeTestConfig(t), MetricsPort: freePort(t)})

	require.ErrorContains(t, err, "index budget spent")
}

// TestRun_MetricsServerFailureEndsTheRun: the metrics and probe server not
// coming up is fatal, since the probes live on it.
func TestRun_MetricsServerFailureEndsTheRun(t *testing.T) {
	port := freePort(t)
	taken, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	require.NoError(t, err)
	t.Cleanup(func() { _ = taken.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	err = bootstrap.Run(ctx, tcpRole{}, bootstrap.Options{ConfigPath: writeTestConfig(t), MetricsPort: port})

	require.ErrorContains(t, err, "metrics and probe server")
}

// TestRun_ReadyzFollowsTheRole: /readyz answers the role's condition while
// serving, so a replica whose index is unverified stays out of the Service.
func TestRun_ReadyzFollowsTheRole(t *testing.T) {
	var unready atomic.Bool

	unready.Store(true)

	role := tcpRole{ready: func() error {
		if unready.Load() {
			return errors.New("index not verified yet")
		}

		return nil
	}}

	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- bootstrap.Run(ctx, role, bootstrap.Options{ConfigPath: writeTestConfig(t), MetricsPort: port})
	}()

	readyz := fmt.Sprintf("http://127.0.0.1:%d/readyz", port)
	require.Eventually(t, func() bool { return httpStatus(readyz) == http.StatusServiceUnavailable }, 10*time.Second, 50*time.Millisecond)

	unready.Store(false)
	require.Eventually(t, func() bool { return httpStatus(readyz) == http.StatusOK }, 10*time.Second, 50*time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

func httpStatus(url string) int {
	resp, err := http.Get(url) //nolint:gosec,noctx // a probe of the test's own server
	if err != nil {
		return 0
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}
