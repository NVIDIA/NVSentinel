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

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/commons/pkg/server"
)

func TestNewInitializationParams_ConfiguredRateLimits_ForwardsValues(t *testing.T) {
	rateLimits := kubeclient.RateLimitConfig{QPS: 40, Burst: 80}

	params := newInitializationParams("", "", "", "", false, rateLimits)

	assert.Equal(t, rateLimits, params.KubernetesClientRateLimits)
}

type fakeLagProvider struct {
	lastEmptyBatch time.Time
	lastEventRead  time.Time
}

func (f fakeLagProvider) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return f.lastEmptyBatch, f.lastEventRead
}

func getFreePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defer listener.Close()

	return listener.Addr().(*net.TCPAddr).Port
}

func waitForServer(t *testing.T, port int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()

			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("server did not start on port %d within %v", port, timeout)
}

func TestCreateMetricsServer_DatastoreReadinessProbe(t *testing.T) {
	checker := server.NewDatastoreReadinessChecker(prometheus.NewRegistry())

	port := getFreePort(t)

	srv, err := createMetricsServer(strconv.Itoa(port), checker)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = srv.Serve(ctx)
	}()

	waitForServer(t, port, 5*time.Second)

	client := &http.Client{Timeout: 2 * time.Second}

	// Liveness probe should succeed immediately
	respHealthz, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	require.NoError(t, err)
	defer respHealthz.Body.Close()
	assert.Equal(t, http.StatusOK, respHealthz.StatusCode)

	// Readiness probe should fail before initial batch
	respReadyz, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
	require.NoError(t, err)
	defer respReadyz.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, respReadyz.StatusCode)

	// After initial batch completes, readiness probe returns 200 OK
	checker.SetLagProvider(fakeLagProvider{lastEmptyBatch: time.Now()})

	respReadyz2, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
	require.NoError(t, err)
	defer respReadyz2.Body.Close()
	assert.Equal(t, http.StatusOK, respReadyz2.StatusCode)
}
