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

package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

const proxiedTokenHost = "token.example.invalid"

var proxiedTokenRequests atomic.Int32

// http.ProxyFromEnvironment reads the proxy env vars once per process, so the
// proxy must be configured before any test runs. Requests to localhost are
// never proxied, so the other tests in this package are unaffected.
func TestMain(m *testing.M) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == proxiedTokenHost {
			proxiedTokenRequests.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken: "proxied-token",
			ExpiresIn:   3600,
		})
	}))

	os.Setenv("HTTP_PROXY", proxy.URL)
	os.Unsetenv("NO_PROXY")
	os.Unsetenv("no_proxy")

	code := m.Run()

	proxy.Close()
	os.Exit(code)
}

func TestGetToken_UsesProxyFromEnvironment(t *testing.T) {
	provider := NewTokenProvider("http://"+proxiedTokenHost+"/token", "test-client", "test-secret", "test-scope", false)

	before := proxiedTokenRequests.Load()

	token, err := provider.GetToken(context.Background())
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}

	if token != "proxied-token" {
		t.Errorf("GetToken() = %v, want proxied-token", token)
	}

	if got := proxiedTokenRequests.Load() - before; got != 1 {
		t.Errorf("proxy received %d requests for %s, want 1", got, proxiedTokenHost)
	}
}
