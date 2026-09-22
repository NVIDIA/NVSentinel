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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/kubernetes"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

// stubKubeconfig points at nothing; building a clientset from it never
// contacts an API server, which is all a connector's construction does.
func stubKubeconfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster:
      server: https://127.0.0.1:1
contexts:
  - name: test
    context:
      cluster: test
      user: test
current-context: test
users:
  - name: test
    user: {}
`), 0o600))

	return path
}

var k8sSettings = kubernetes.Settings{
	K8sConnectorConfig: kubernetes.K8sConnectorConfig{MaxNodeConditionMessageLength: 1024, CompactedHealthEventMsgLen: 256},
	QPS:                5,
	Burst:              10,
}

// TestBuildConnectors_QueuedAndDirect: the same enable flags build the same
// connectors in both modes. Queued, every member is a ring buffer and its
// connector drains it; direct, the connectors themselves are the members.
// Shutdown drains the queues and returns.
func TestBuildConnectors_QueuedAndDirect(t *testing.T) {
	ctx := context.Background()
	raw := map[string]any{
		"enableK8sPlatformConnector":  "true",
		"enablePromPlatformConnector": "true",
		"enableGRPCSinkConnector":     "false",
	}

	queued, err := BuildConnectors(ctx, raw, k8sSettings, Options{KubeconfigPath: stubKubeconfig(t)}, ConnectorOptions{Queued: true})
	require.NoError(t, err)
	require.Len(t, queued.Set, 2)
	require.Nil(t, queued.Store, "no store enabled")

	for _, member := range queued.Set {
		_, isQueue := member.(*ringbuffer.RingBuffer)
		require.True(t, isQueue, "queued members are ring buffers")
	}

	done := make(chan struct{})

	go func() {
		queued.Shutdown(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown of the queued connectors did not return")
	}

	direct, err := BuildConnectors(ctx, raw, k8sSettings,
		Options{KubeconfigPath: stubKubeconfig(t)}, ConnectorOptions{BestEffortTimeout: time.Second})
	require.NoError(t, err)
	require.Len(t, direct.Set, 2)

	for _, member := range direct.Set {
		_, isQueue := member.(*ringbuffer.RingBuffer)
		require.False(t, isQueue, "direct members are the connectors themselves")
	}

	direct.Shutdown(ctx)
}

// TestBuildConnectors_SinkSettings: the sink needs a target; the retry count
// config.json carries is read in both modes, as the chart always writes it.
func TestBuildConnectors_SinkSettings(t *testing.T) {
	ctx := context.Background()

	_, err := BuildConnectors(ctx, map[string]any{"enableGRPCSinkConnector": "true", "GRPCSinkTarget": ""},
		k8sSettings, Options{}, ConnectorOptions{})
	require.ErrorContains(t, err, "GRPCSinkTarget")

	_, err = BuildConnectors(ctx, map[string]any{"enableGRPCSinkConnector": "true", "GRPCSinkTarget": "sink.example:9000"},
		k8sSettings, Options{}, ConnectorOptions{})
	require.ErrorContains(t, err, "GRPCSinkConnectorMaxRetries")

	direct, err := BuildConnectors(ctx, map[string]any{
		"enableGRPCSinkConnector": "true", "GRPCSinkTarget": "sink.example:9000", "GRPCSinkConnectorMaxRetries": json.Number("3"),
	}, k8sSettings, Options{}, ConnectorOptions{BestEffortTimeout: time.Second})
	require.NoError(t, err)
	require.Len(t, direct.Set, 1)

	direct.Shutdown(ctx)
}

// failing is a connector whose every batch fails.
type failing struct{}

func (failing) ProcessBatch(context.Context, *pb.HealthEvents) error { return errors.New("boom") }
func (failing) FetchAndProcessHealthMetric(context.Context)          {}

// TestAdd_QueuedDirectAndBestEffort pins how a member enters the set: as its
// ring buffer when queued, as itself when it must decide the reply, and
// wrapped in BestEffort when its failure must not fail the batch.
func TestAdd_QueuedDirectAndBestEffort(t *testing.T) {
	ctx := context.Background()
	batch := &pb.HealthEvents{}

	queued := &Connectors{mode: ConnectorOptions{Queued: true}}
	queued.add(ctx, queued.queue(ctx, "test"), "test", failing{}, true)
	_, isQueue := queued.Set[0].(*ringbuffer.RingBuffer)
	require.True(t, isQueue)
	queued.Shutdown(ctx)

	direct := &Connectors{mode: ConnectorOptions{BestEffortTimeout: time.Second}}
	direct.add(ctx, nil, "bare", failing{}, false)
	direct.add(ctx, nil, "best-effort", failing{}, true)
	require.EqualError(t, direct.Set[0].ProcessBatch(ctx, batch), "boom", "a member that decides the reply fails the batch")
	require.NoError(t, direct.Set[1].ProcessBatch(ctx, batch), "a best-effort member never fails the batch")
}

// TestReadiness_ShuttingDownWinsOverReady: /readyz follows the role's
// condition until shutdown begins, then answers unready whatever the role
// says, so the probes take the replica out of the Service first.
func TestReadiness_ShuttingDownWinsOverReady(t *testing.T) {
	roleReady := errors.New("not yet")
	probe := &readiness{ready: func() error { return roleReady }}

	require.ErrorIs(t, probe.Ready(context.Background()), roleReady)

	roleReady = nil
	require.NoError(t, probe.Ready(context.Background()))

	probe.shuttingDown.Store(true)
	require.ErrorContains(t, probe.Ready(context.Background()), "shutting down")
}
