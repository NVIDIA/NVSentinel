// Copyright 2026 k8s-gpu-mcp-server contributors
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

package mcp

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	mcpgoserver "github.com/mark3labs/mcp-go/server"
)

// HTTPServer wraps the mcp-go MCPServer with NVSentinel's transport plumbing:
// streamable-HTTP at /mcp (optionally bearer-token protected), health/ready
// probes, a /version endpoint, and TLS with on-disk cert rotation. Prometheus
// metrics are exposed separately by commons/pkg/server on a different port —
// this server does not register /metrics.
type HTTPServer struct {
	mcpServer  *mcpgoserver.MCPServer
	httpServer *http.Server
	addr       string
	version    string
	ready      chan struct{}
	authToken  string
	tlsConfig  *TLSConfig
}

// NewHTTPServer constructs an HTTPServer for the given mcp-go MCPServer. The
// addr is the listen address (e.g. ":8080"); version is reported on /version.
func NewHTTPServer(mcpServer *mcpgoserver.MCPServer, addr, version string) *HTTPServer {
	return &HTTPServer{
		mcpServer: mcpServer,
		addr:      addr,
		version:   version,
		ready:     make(chan struct{}),
	}
}

// SetAuthToken sets the bearer token required for /mcp access. When empty
// (default), /mcp is unauthenticated — appropriate only for in-cluster
// deployments fronted by a NetworkPolicy.
func (h *HTTPServer) SetAuthToken(token string) {
	h.authToken = token
}

// SetTLSConfig configures TLS termination. When the supplied config is
// non-nil and Enabled(), the server serves HTTPS on addr instead of HTTP and
// reloads the cert/key periodically from disk.
func (h *HTTPServer) SetTLSConfig(cfg *TLSConfig) {
	h.tlsConfig = cfg
}

// ListenAndServe binds the listener, starts the HTTP server, and blocks until
// ctx is cancelled or Serve returns. On context cancellation it initiates a
// graceful Shutdown and returns its result.
func (h *HTTPServer) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()

	// MCP endpoint — streamable HTTP transport in stateless mode. Each
	// request is independent; this fits in-cluster routing where a gateway
	// fronts the server and clients do not retain sessions.
	streamableServer := mcpgoserver.NewStreamableHTTPServer(
		h.mcpServer,
		mcpgoserver.WithStateLess(true),
	)

	var mcpHandler http.Handler = streamableServer
	if h.authToken != "" {
		mcpHandler = h.requireBearerAuth(streamableServer)
	}

	mux.Handle("/mcp", mcpHandler)

	// Health, readiness, version probes — useful even when commons exposes
	// the canonical /healthz on a separate metrics port, because clients
	// connecting to /mcp can sanity-check the same socket they will use.
	mux.HandleFunc("/healthz", h.handleHealthz)
	mux.HandleFunc("/readyz", h.handleReadyz)
	mux.HandleFunc("/version", h.handleVersion)

	// WriteTimeout (90s) must exceed any tool exec timeout plus a marshaling
	// buffer to avoid spurious "socket hang up" on long reads. IdleTimeout
	// (120s) exceeds WriteTimeout so keep-alive connections survive
	// long-running tool calls.
	h.httpServer = &http.Server{
		Addr:              h.addr,
		Handler:           mux,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	useTLS := h.tlsConfig != nil && h.tlsConfig.Enabled()
	if useTLS {
		reloader, err := newCertReloader(h.tlsConfig.CertFile, h.tlsConfig.KeyFile)
		if err != nil {
			return fmt.Errorf("TLS setup: %w", err)
		}

		h.httpServer.TLSConfig = &tls.Config{
			GetCertificate: reloader.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		}
	}

	proto := "HTTP"
	if useTLS {
		proto = "HTTPS"
	}

	slog.Info("MCP "+proto+" server starting", "addr", h.addr, "tls", useTLS)

	// Bind the listener up front so close(h.ready) only fires when the
	// socket is actually reachable. Doing this inside the serving goroutine
	// would race with health probes.
	ln, err := net.Listen("tcp", h.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", h.addr, err)
	}

	if useTLS {
		ln = tls.NewListener(ln, h.httpServer.TLSConfig)
	}

	errCh := make(chan error, 1)

	go func() {
		close(h.ready)

		if serveErr := h.httpServer.Serve(ln); serveErr != http.ErrServerClosed {
			errCh <- serveErr
		}
	}()

	select {
	case <-ctx.Done():
		return h.Shutdown()
	case err := <-errCh:
		return err
	}
}

// Ready returns a channel that is closed once the listener is bound. Tests
// and readiness checks use it to synchronise with startup.
func (h *HTTPServer) Ready() <-chan struct{} {
	return h.ready
}

// Shutdown gracefully drains in-flight requests and closes the listener.
// Returns nil if ListenAndServe never ran.
func (h *HTTPServer) Shutdown() error {
	if h.httpServer == nil {
		return nil
	}

	slog.Info("MCP HTTP server shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return h.httpServer.Shutdown(ctx)
}

// handleHealthz is a minimal liveness probe — process is up.
func (h *HTTPServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(map[string]string{"status": "healthy"}); err != nil {
		slog.Error("failed to encode healthz response", "error", err)
	}
}

// handleReadyz is a minimal readiness probe — server is listening. Without
// NVML there are no degraded-mode considerations; tools advertise their own
// errors at call time.
func (h *HTTPServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ready"}); err != nil {
		slog.Error("failed to encode readyz response", "error", err)
	}
}

// handleVersion returns the build version. The "server" name distinguishes
// this from the donor's k8s-gpu-mcp-server.
func (h *HTTPServer) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := json.NewEncoder(w).Encode(map[string]string{
		"server":  "nvsentinel-mcp-server",
		"version": h.version,
	}); err != nil {
		slog.Error("failed to encode version response", "error", err)
	}
}

// requireBearerAuth wraps a handler with bearer-token auth. Returns 401 when
// the token is missing, malformed, or does not match. The compare uses
// constant-time comparison to avoid timing leaks.
func (h *HTTPServer) requireBearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"authorization required"}`, http.StatusUnauthorized)

			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")
		if token == auth {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"invalid authorization scheme"}`, http.StatusUnauthorized)

			return
		}

		if subtle.ConstantTimeCompare([]byte(token), []byte(h.authToken)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)

			return
		}

		next.ServeHTTP(w, r)
	})
}
