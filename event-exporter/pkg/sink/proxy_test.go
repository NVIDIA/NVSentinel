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

package sink

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/event-exporter/pkg/auth"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/transformer"
)

const proxiedSinkHost = "sink.example.invalid"

var proxiedSinkRequests atomic.Int32

// http.ProxyFromEnvironment reads the proxy env vars once per process, so the
// proxy must be configured before any test runs. Requests to localhost are
// never proxied, so the other tests in this package are unaffected.
func TestMain(m *testing.M) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == proxiedSinkHost {
			proxiedSinkRequests.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))

	os.Setenv("HTTP_PROXY", proxy.URL)
	os.Unsetenv("NO_PROXY")
	os.Unsetenv("no_proxy")

	code := m.Run()

	proxy.Close()
	os.Exit(code)
}

func TestPublish_UsesProxyFromEnvironment(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "test-token-123",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	tokenProvider := auth.NewTokenProvider(tokenServer.URL, "client-id", "client-secret", "scope", false)
	sink := NewHTTPSink("http://"+proxiedSinkHost+"/events", 10*time.Second, tokenProvider, false, 1)

	event := &transformer.CloudEvent{
		SpecVersion: "1.0",
		Type:        "com.nvidia.nvsentinel.health.v1",
		Source:      "nvsentinel://test-cluster/healthevents",
		ID:          "test-event-id",
		Time:        "2025-01-15T10:30:00Z",
		Data:        map[string]any{},
	}

	before := proxiedSinkRequests.Load()

	if err := sink.Publish(context.Background(), event); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	if got := proxiedSinkRequests.Load() - before; got != 1 {
		t.Errorf("proxy received %d requests for %s, want 1", got, proxiedSinkHost)
	}
}
