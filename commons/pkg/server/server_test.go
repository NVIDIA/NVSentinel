// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
)

// getFreePort gets an available port on localhost for testing.
func getFreePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	defer listener.Close()

	return listener.Addr().(*net.TCPAddr).Port
}

// waitForServer waits for the server to start accepting connections.
func waitForServer(t *testing.T, port int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("server did not start within %v", timeout)
}

func TestNewServer(t *testing.T) {
	t.Run("creates server with defaults", func(t *testing.T) {
		srv := NewServer()
		if srv == nil {
			t.Fatal("NewServer returned nil")
		}

		s := srv.(*server)
		if s.port != DefaultPort {
			t.Errorf("expected port %d, got %d", DefaultPort, s.port)
		}
		if s.readTimeout != DefaultReadTimeout {
			t.Errorf("expected readTimeout %v, got %v", DefaultReadTimeout, s.readTimeout)
		}
		if s.writeTimeout != DefaultWriteTimeout {
			t.Errorf("expected writeTimeout %v, got %v", DefaultWriteTimeout, s.writeTimeout)
		}
		if s.idleTimeout != DefaultIdleTimeout {
			t.Errorf("expected idleTimeout %v, got %v", DefaultIdleTimeout, s.idleTimeout)
		}
		if s.shutdownTimeout != DefaultShutdownTimeout {
			t.Errorf("expected shutdownTimeout %v, got %v", DefaultShutdownTimeout, s.shutdownTimeout)
		}
		if s.maxHeaderBytes != DefaultMaxHeaderBytes {
			t.Errorf("expected maxHeaderBytes %d, got %d", DefaultMaxHeaderBytes, s.maxHeaderBytes)
		}
	})

	t.Run("applies functional options", func(t *testing.T) {
		port := 9999
		readTimeout := 5 * time.Second
		writeTimeout := 15 * time.Second
		idleTimeout := 120 * time.Second
		shutdownTimeout := 10 * time.Second
		maxHeaderBytes := 2 << 20

		srv := NewServer(
			WithPort(port),
			WithReadTimeout(readTimeout),
			WithWriteTimeout(writeTimeout),
			WithIdleTimeout(idleTimeout),
			WithShutdownTimeout(shutdownTimeout),
			WithMaxHeaderBytes(maxHeaderBytes),
		)

		s := srv.(*server)
		if s.port != port {
			t.Errorf("expected port %d, got %d", port, s.port)
		}
		if s.readTimeout != readTimeout {
			t.Errorf("expected readTimeout %v, got %v", readTimeout, s.readTimeout)
		}
		if s.writeTimeout != writeTimeout {
			t.Errorf("expected writeTimeout %v, got %v", writeTimeout, s.writeTimeout)
		}
		if s.idleTimeout != idleTimeout {
			t.Errorf("expected idleTimeout %v, got %v", idleTimeout, s.idleTimeout)
		}
		if s.shutdownTimeout != shutdownTimeout {
			t.Errorf("expected shutdownTimeout %v, got %v", shutdownTimeout, s.shutdownTimeout)
		}
		if s.maxHeaderBytes != maxHeaderBytes {
			t.Errorf("expected maxHeaderBytes %d, got %d", maxHeaderBytes, s.maxHeaderBytes)
		}
	})
}

func TestServerServe(t *testing.T) {
	t.Run("starts and serves HTTP requests", func(t *testing.T) {
		port := getFreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		srv := NewServer(
			WithPort(port),
			WithSimpleHealth(),
		)

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return srv.Serve(gCtx)
		})

		// Wait for server to start
		waitForServer(t, port, 2*time.Second)

		// Test health endpoint
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err != nil {
			t.Fatalf("failed to GET /healthz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read response body: %v", err)
		}

		if string(body) != "ok" {
			t.Errorf("expected body 'ok', got '%s'", string(body))
		}

		// Trigger shutdown
		cancel()

		// Wait for graceful shutdown
		if err := g.Wait(); err != nil {
			t.Errorf("server returned error: %v", err)
		}
	})

	t.Run("gracefully shuts down with timeout", func(t *testing.T) {
		port := getFreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		srv := NewServer(
			WithPort(port),
			WithShutdownTimeout(1*time.Second),
		)

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return srv.Serve(gCtx)
		})

		waitForServer(t, port, 2*time.Second)

		// Trigger shutdown
		start := time.Now()
		cancel()

		err := g.Wait()
		duration := time.Since(start)

		if err != nil {
			t.Errorf("server returned error: %v", err)
		}

		if duration > 2*time.Second {
			t.Errorf("shutdown took too long: %v", duration)
		}
	})
}

func TestServerHealthEndpoints(t *testing.T) {
	t.Run("simple health check returns ok", func(t *testing.T) {
		port := getFreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		srv := NewServer(
			WithPort(port),
			WithSimpleHealth(),
		)

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return srv.Serve(gCtx)
		})
		defer cancel()

		waitForServer(t, port, 2*time.Second)

		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err != nil {
			t.Fatalf("failed to GET /healthz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
		}

		if contentType := resp.Header.Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
			t.Errorf("expected Content-Type 'text/plain; charset=utf-8', got '%s'", contentType)
		}
	})
}

func TestHealthChecker(t *testing.T) {
	t.Run("health check passes when checker returns nil", func(t *testing.T) {
		port := getFreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		checker := &mockHealthChecker{healthy: true}
		srv := NewServer(
			WithPort(port),
			WithHealthCheck(checker),
		)

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return srv.Serve(gCtx)
		})
		defer cancel()

		waitForServer(t, port, 2*time.Second)

		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err != nil {
			t.Fatalf("failed to GET /healthz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
		}
	})

	t.Run("health check fails when checker returns error", func(t *testing.T) {
		port := getFreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		checker := &mockHealthChecker{healthy: false, err: fmt.Errorf("service unhealthy")}
		srv := NewServer(
			WithPort(port),
			WithHealthCheck(checker),
		)

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return srv.Serve(gCtx)
		})
		defer cancel()

		waitForServer(t, port, 2*time.Second)

		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err != nil {
			t.Fatalf("failed to GET /healthz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected status %d, got %d", http.StatusServiceUnavailable, resp.StatusCode)
		}
	})
}

// mockHealthChecker implements HealthChecker for testing.
type mockHealthChecker struct {
	healthy bool
	err     error
}

func (m *mockHealthChecker) Healthy(ctx context.Context) error {
	if !m.healthy {
		return m.err
	}
	return nil
}

func TestReadinessChecker(t *testing.T) {
	t.Run("readiness check passes when checker returns nil", func(t *testing.T) {
		port := getFreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		checker := &mockReadinessChecker{ready: true}
		srv := NewServer(
			WithPort(port),
			WithReadinessCheck(checker),
		)

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return srv.Serve(gCtx)
		})
		defer cancel()

		waitForServer(t, port, 2*time.Second)

		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
		if err != nil {
			t.Fatalf("failed to GET /readyz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
		}
	})

	t.Run("readiness check fails when checker returns error", func(t *testing.T) {
		port := getFreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		checker := &mockReadinessChecker{ready: false, err: fmt.Errorf("dependencies not ready")}
		srv := NewServer(
			WithPort(port),
			WithReadinessCheck(checker),
		)

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return srv.Serve(gCtx)
		})
		defer cancel()

		waitForServer(t, port, 2*time.Second)

		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
		if err != nil {
			t.Fatalf("failed to GET /readyz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("expected status %d, got %d", http.StatusServiceUnavailable, resp.StatusCode)
		}
	})
}

// mockReadinessChecker implements ReadinessChecker for testing.
type mockReadinessChecker struct {
	ready bool
	err   error
}

func (m *mockReadinessChecker) Ready(ctx context.Context) error {
	if !m.ready {
		return m.err
	}
	return nil
}

func TestCustomHandlers(t *testing.T) {
	t.Run("custom handler is registered and served", func(t *testing.T) {
		port := getFreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		customHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("custom response"))
		})

		srv := NewServer(
			WithPort(port),
			WithHandler("/custom", customHandler),
		)

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return srv.Serve(gCtx)
		})
		defer cancel()

		waitForServer(t, port, 2*time.Second)

		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/custom", port))
		if err != nil {
			t.Fatalf("failed to GET /custom: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read response body: %v", err)
		}

		if string(body) != "custom response" {
			t.Errorf("expected body 'custom response', got '%s'", string(body))
		}
	})

	t.Run("multiple handlers can be registered", func(t *testing.T) {
		port := getFreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handler1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("handler1"))
		})

		handler2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("handler2"))
		})

		srv := NewServer(
			WithPort(port),
			WithHandler("/path1", handler1),
			WithHandler("/path2", handler2),
		)

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return srv.Serve(gCtx)
		})
		defer cancel()

		waitForServer(t, port, 2*time.Second)

		// Test first handler
		resp1, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/path1", port))
		if err != nil {
			t.Fatalf("failed to GET /path1: %v", err)
		}
		defer resp1.Body.Close()

		body1, err := io.ReadAll(resp1.Body)
		if err != nil {
			t.Fatalf("failed to read response body: %v", err)
		}

		if string(body1) != "handler1" {
			t.Errorf("expected 'handler1', got '%s'", string(body1))
		}

		// Test second handler
		resp2, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/path2", port))
		if err != nil {
			t.Fatalf("failed to GET /path2: %v", err)
		}
		defer resp2.Body.Close()

		body2, err := io.ReadAll(resp2.Body)
		if err != nil {
			t.Fatalf("failed to read response body: %v", err)
		}

		if string(body2) != "handler2" {
			t.Errorf("expected 'handler2', got '%s'", string(body2))
		}
	})
}

func TestPrometheusMetrics(t *testing.T) {
	t.Run("prometheus metrics endpoint is registered", func(t *testing.T) {
		port := getFreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		srv := NewServer(
			WithPort(port),
			WithPrometheusMetrics(),
		)

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return srv.Serve(gCtx)
		})
		defer cancel()

		waitForServer(t, port, 2*time.Second)

		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
		if err != nil {
			t.Fatalf("failed to GET /metrics: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
		}

		// Verify it's actually Prometheus metrics format
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read response body: %v", err)
		}

		bodyStr := string(body)
		if len(bodyStr) == 0 {
			t.Error("metrics endpoint returned empty body")
		}

		// Prometheus metrics should contain HELP or TYPE comments
		if !containsAny(bodyStr, []string{"# HELP", "# TYPE", "go_"}) {
			t.Error("response doesn't look like Prometheus metrics format")
		}
	})
}

func TestServerConcurrency(t *testing.T) {
	t.Run("handles concurrent requests", func(t *testing.T) {
		port := getFreePort(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var requestCount atomic.Int32
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount.Add(1)
			time.Sleep(10 * time.Millisecond) // Simulate work
			w.WriteHeader(http.StatusOK)
		})

		srv := NewServer(
			WithPort(port),
			WithHandler("/test", handler),
		)

		g, gCtx := errgroup.WithContext(ctx)
		g.Go(func() error {
			return srv.Serve(gCtx)
		})
		defer cancel()

		waitForServer(t, port, 2*time.Second)

		// Make concurrent requests
		numRequests := 10
		eg := errgroup.Group{}
		for i := 0; i < numRequests; i++ {
			eg.Go(func() error {
				resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/test", port))
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				return nil
			})
		}

		if err := eg.Wait(); err != nil {
			t.Errorf("concurrent requests failed: %v", err)
		}

		count := int(requestCount.Load())
		if count != numRequests {
			t.Errorf("expected %d requests, got %d", numRequests, count)
		}
	})
}

func TestWithTLS(t *testing.T) {
	t.Run("TLS config is stored", func(t *testing.T) {
		cfg := TLSConfig{
			CertFile: "/path/to/cert.pem",
			KeyFile:  "/path/to/key.pem",
		}

		srv := NewServer(WithTLS(cfg))
		s := srv.(*server)

		if s.tlsConfig == nil {
			t.Fatal("TLS config is nil")
		}

		if s.tlsConfig.CertFile != cfg.CertFile {
			t.Errorf("expected CertFile %s, got %s", cfg.CertFile, s.tlsConfig.CertFile)
		}

		if s.tlsConfig.KeyFile != cfg.KeyFile {
			t.Errorf("expected KeyFile %s, got %s", cfg.KeyFile, s.tlsConfig.KeyFile)
		}
	})
}

// TestIsRunning verifies that the IsRunning method correctly reports server state.
func TestIsRunning(t *testing.T) {
	t.Run("server not running initially", func(t *testing.T) {
		port := getFreePort(t)
		srv := NewServer(
			WithPort(port),
			WithSimpleHealth(),
		)

		s := srv.(*server)

		if s.IsRunning() {
			t.Error("expected IsRunning() to return false before server starts")
		}
	})

	t.Run("server running after start", func(t *testing.T) {
		port := getFreePort(t)
		srv := NewServer(
			WithPort(port),
			WithSimpleHealth(),
		)

		s := srv.(*server)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start server in goroutine
		errCh := make(chan error, 1)
		go func() {
			errCh <- srv.Serve(ctx)
		}()

		// Wait for server to start
		waitForServer(t, port, 2*time.Second)

		if !s.IsRunning() {
			t.Error("expected IsRunning() to return true after server starts")
		}

		// Stop server
		cancel()

		// Wait for server to stop
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("unexpected error from Serve: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for server to stop")
		}

		// Give server time to clean up
		time.Sleep(100 * time.Millisecond)

		if s.IsRunning() {
			t.Error("expected IsRunning() to return false after server stops")
		}
	})

	t.Run("concurrent access to IsRunning", func(t *testing.T) {
		port := getFreePort(t)
		srv := NewServer(
			WithPort(port),
			WithSimpleHealth(),
		)

		s := srv.(*server)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start server in goroutine
		go func() {
			_ = srv.Serve(ctx)
		}()

		// Wait for server to start
		waitForServer(t, port, 2*time.Second)

		// Spawn multiple goroutines that concurrently check IsRunning
		var readCount atomic.Int32
		g := errgroup.Group{}

		for i := 0; i < 100; i++ {
			g.Go(func() error {
				for j := 0; j < 100; j++ {
					if s.IsRunning() {
						readCount.Add(1)
					}
					time.Sleep(time.Microsecond)
				}
				return nil
			})
		}

		// Wait for all goroutines
		if err := g.Wait(); err != nil {
			t.Errorf("unexpected error from concurrent reads: %v", err)
		}

		// Verify we got many successful reads
		if readCount.Load() == 0 {
			t.Error("expected at least some IsRunning() calls to return true")
		}

		// Stop server
		cancel()

		// Give server time to stop
		time.Sleep(200 * time.Millisecond)

		if s.IsRunning() {
			t.Error("expected IsRunning() to return false after server stops")
		}
	})
}

// Helper function to check if a string contains any of the given substrings.
func containsAny(s string, substrings []string) bool {
	for _, substr := range substrings {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}
