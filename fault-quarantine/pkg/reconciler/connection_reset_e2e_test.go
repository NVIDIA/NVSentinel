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

package reconciler

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/config"
)

// connResetProxy stands between fault-quarantine and the envtest API server.
// It resets the TCP connection under the first resets PATCH requests to one
// node, the way an API server restart or a load balancer cuts a request off,
// and passes every other request through.
type connResetProxy struct {
	server  *httptest.Server
	forward *httputil.ReverseProxy
	path    string
	resets  int32
	// applyFirst forwards a PATCH before resetting its connection, so the write
	// lands and only the response is lost.
	applyFirst bool
	patches    atomic.Int32
}

type tcpConnKey struct{}

func newConnResetProxy(t *testing.T, nodeName string, resets int32, applyFirst bool) *connResetProxy {
	t.Helper()

	apiServer, err := url.Parse(e2eTestEnv.Config.Host)
	require.NoError(t, err)

	transport, err := rest.TransportFor(e2eTestEnv.Config)
	require.NoError(t, err)

	p := &connResetProxy{
		forward: &httputil.ReverseProxy{
			Rewrite:   func(r *httputil.ProxyRequest) { r.SetURL(apiServer) },
			Transport: transport,
		},
		path:       "/api/v1/nodes/" + nodeName,
		resets:     resets,
		applyFirst: applyFirst,
	}

	// HTTP/2, as client-go speaks to the API server.
	p.server = httptest.NewUnstartedServer(p)
	p.server.EnableHTTP2 = true
	p.server.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		return context.WithValue(ctx, tcpConnKey{}, conn.(*tls.Conn).NetConn())
	}
	p.server.StartTLS()
	t.Cleanup(p.server.Close)

	return p
}

func (p *connResetProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch || r.URL.Path != p.path || p.patches.Add(1) > p.resets {
		p.forward.ServeHTTP(w, r)
		return
	}

	if p.applyFirst {
		p.forward.ServeHTTP(httptest.NewRecorder(), r)
	} else {
		_, _ = io.Copy(io.Discard, r.Body)
	}

	// Closing with linger 0 sends a RST, so the client reads ECONNRESET.
	conn := r.Context().Value(tcpConnKey{}).(*net.TCPConn)
	_ = conn.SetLinger(0)
	_ = conn.Close()
}

func (p *connResetProxy) clientset(t *testing.T) kubernetes.Interface {
	t.Helper()

	clientset, err := kubernetes.NewForConfig(&rest.Config{
		Host: p.server.URL,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.server.Certificate().Raw}),
		},
	})
	require.NoError(t, err)

	return clientset
}

func cordonOnFatalXidRules() config.TomlConfig {
	return config.TomlConfig{
		LabelPrefix: "k8s.nvidia.com/",
		RuleSets: []config.QuarantineRuleSet{{
			Enabled:  true,
			Name:     "fatal-xid",
			Version:  "1",
			Priority: 10,
			Match: config.Match{Any: []config.Rule{{
				Kind: "HealthEvent", Expression: "event.checkName == 'GpuXidError' && event.isFatal == true",
			}}},
			Cordon: config.Cordon{ShouldCordon: true},
		}},
	}
}

// A fatal event whose cordon PATCH loses its connection, as when the API server
// restarts, still cordons the node because the patch is retried. When the lost
// PATCH had landed, the retry fails its resourceVersion precondition, reads the
// node again and finds nothing left to write.
func TestE2E_CordonPatchConnectionReset_RetriesAndCordons(t *testing.T) {
	tests := []struct {
		name        string
		applyFirst  bool
		wantPatches int32
		wantStatus  model.Status
	}{
		{
			name:        "reset before the API server saw the patch",
			wantPatches: 2,
			wantStatus:  model.Quarantined,
		},
		{
			name:        "reset after the API server applied the patch",
			applyFirst:  true,
			wantPatches: 2,
			// The retry reads back a node that already carries this event's quarantine.
			wantStatus: model.AlreadyQuarantined,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(e2eTestContext, 30*time.Second)
			defer cancel()

			nodeName := "e2e-cordon-reset-" + generateShortTestID()
			createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
			defer func() {
				_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
			}()

			proxy := newConnResetProxy(t, nodeName, 1, tt.applyFirst)
			_, mockWatcher, getStatus, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
				TomlConfig: cordonOnFatalXidRules(),
				Clientset:  proxy.clientset(t),
			})

			eventID := generateTestID()
			mockWatcher.EventsChan <- &TestEvent{Data: createHealthEventBSON(eventID, nodeName, "GpuXidError",
				false, true, []*protos.Entity{{EntityType: "GPU", EntityValue: "0"}}, model.StatusInProgress)}

			assert.Eventually(t, func() bool { return getStatus(eventID) != nil },
				statusCheckTimeout, statusCheckPollInterval, "the event should be processed")

			node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			require.NoError(t, err)
			assert.True(t, node.Spec.Unschedulable, "the node should be cordoned")
			assert.Equal(t, tt.wantPatches, proxy.patches.Load(), "PATCH requests for the node")

			status := getStatus(eventID)
			require.NotNil(t, status)
			assert.Equal(t, tt.wantStatus, *status)
		})
	}
}

// When every attempt loses its connection, the cordon gives up once the
// NodePatchBackoff budget is spent instead of retrying forever, and returns
// the connection error.
func TestE2E_CordonPatchConnectionAlwaysReset_GivesUpAfterBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(e2eTestContext, 60*time.Second)
	defer cancel()

	nodeName := "e2e-cordon-reset-" + generateShortTestID()
	createE2ETestNode(ctx, t, nodeName, nil, nil, nil, false)
	defer func() {
		_ = e2eTestClient.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	}()

	proxy := newConnResetProxy(t, nodeName, math.MaxInt32, false)
	r, _, _, _ := setupE2EReconcilerWithOptions(t, ctx, E2EReconcilerConfig{
		TomlConfig: cordonOnFatalXidRules(),
		Clientset:  proxy.clientset(t),
	})

	// budget is the sleep between the attempts before jitter, which adds up to
	// Jitter times as much again.
	backoff := kubeclient.NodePatchBackoff()
	sleeps := backoff
	sleeps.Jitter = 0

	var budget time.Duration
	for sleeps.Steps > 1 {
		budget += sleeps.Step()
	}

	start := time.Now()
	_, err := r.k8sClient.QuarantineNodeAndSetAnnotations(ctx, nodeName, nil, true, nil, nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, utilnet.IsConnectionReset(err), "the connection error should be returned, got %v", err)
	assert.EqualValues(t, backoff.Steps, proxy.patches.Load(), "PATCH requests for the node")
	assert.GreaterOrEqual(t, elapsed, budget)
	assert.Less(t, elapsed, time.Duration(float64(budget)*(1+backoff.Jitter))+5*time.Second)

	node, err := e2eTestClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.False(t, node.Spec.Unschedulable, "the node should not be cordoned")
}
