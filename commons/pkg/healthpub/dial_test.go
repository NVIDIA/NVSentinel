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

package healthpub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// clearPublishEnv resets every HEALTH_PUBLISH_* variable for the test so
// leakage between cases cannot flip modes.
func clearPublishEnv(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		envTarget, envTLSCAFile, envTLSServerName, envTokenPath,
		envInsecure, envRetryWindow,
	} {
		t.Setenv(key, "")
	}
}

// fallbackNotCalled fails the test if the legacy fallback dial runs; direct
// mode must never fall back.
func fallbackNotCalled(t *testing.T) func() (*grpc.ClientConn, error) {
	t.Helper()

	return func() (*grpc.ClientConn, error) {
		t.Error("fallback must not run when HEALTH_PUBLISH_TARGET is set")

		return nil, errors.New("fallback called in direct mode")
	}
}

// socketFallback stands in for a caller's legacy node-local dial: a
// lazily-connecting plaintext client against the daemonset socket.
func socketFallback(t *testing.T, called *atomic.Bool) func() (*grpc.ClientConn, error) {
	t.Helper()

	return func() (*grpc.ClientConn, error) {
		called.Store(true)

		// The connection is handed to the publisher through the returned
		// Option, and Publisher.Close is its only owner: no cleanup here.
		return grpc.NewClient("unix:///run/nvsentinel/nvsentinel.sock",
			grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
}

// TestDialFromEnvOr_SocketModeRunsFallback: with HEALTH_PUBLISH_TARGET unset,
// DialFromEnvOr must run the caller's legacy dial and return its connection
// with the Option that hands the connection to the Publisher, whose Close then
// closes it.
func TestDialFromEnvOr_SocketModeRunsFallback(t *testing.T) {
	clearPublishEnv(t)

	var called atomic.Bool

	conn, client, opt, err := DialFromEnvOr(socketFallback(t, &called))
	require.NoError(t, err)

	assert.True(t, called.Load(), "no HEALTH_PUBLISH_TARGET must mean the legacy dial runs")
	assert.NotNil(t, conn, "socket mode must return the fallback's connection")
	assert.NotNil(t, client, "socket mode must wrap the fallback's connection in a client")
	require.NotNil(t, opt, "socket mode hands the connection to the publisher")

	p := New(client, "unix:///tmp/nvsentinel.sock", "test-socket-owned-conn", opt)
	assert.Nil(t, p.direct, "socket mode is not direct mode")
	require.NoError(t, p.Close())
	assert.Equal(t, connectivity.Shutdown, conn.GetState(), "Close closes the connection in socket mode too")
	require.NoError(t, p.Close(), "Close is idempotent")
}

// TestDialFromEnvOr_SocketModeFallbackError: a failing legacy dial must
// surface wrapped, with nothing else returned.
func TestDialFromEnvOr_SocketModeFallbackError(t *testing.T) {
	clearPublishEnv(t)

	fallbackErr := errors.New("legacy dial exploded")

	conn, client, opt, err := DialFromEnvOr(func() (*grpc.ClientConn, error) {
		return nil, fallbackErr
	})
	require.ErrorIs(t, err, fallbackErr)
	assert.Nil(t, conn)
	assert.Nil(t, client)
	assert.Nil(t, opt)
}

// TestDialFromEnvOr_DirectRefusesPlaintext: a direct target without a CA file
// must be refused unless the insecure mode is explicitly named.
func TestDialFromEnvOr_DirectRefusesPlaintext(t *testing.T) {
	clearPublishEnv(t)
	t.Setenv(envTarget, "platform-connector-deployment.nvsentinel.svc:50051")
	t.Setenv(envTokenPath, testTokenFile(t))

	_, _, _, err := DialFromEnvOr(fallbackNotCalled(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), envTLSCAFile)
	assert.Contains(t, err.Error(), envInsecure)
}

// testTokenFile writes a projected-token stand-in and returns its path.
func testTokenFile(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("test-token"), 0o600))

	return path
}

// TestDialFromEnvOr_DirectRequiresTokenPath: the server rejects every batch
// without a token, so a direct-mode publisher without a token path must fail
// at startup rather than on every send.
func TestDialFromEnvOr_DirectRequiresTokenPath(t *testing.T) {
	clearPublishEnv(t)
	t.Setenv(envTarget, "127.0.0.1:50051")
	t.Setenv(envInsecure, "true")

	_, _, _, err := DialFromEnvOr(fallbackNotCalled(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), envTokenPath)
}

// TestDialFromEnvOr_DirectInsecureDevelopment: HEALTH_PUBLISH_INSECURE=true
// permits plaintext and returns a non-nil direct option.
func TestDialFromEnvOr_DirectInsecureDevelopment(t *testing.T) {
	clearPublishEnv(t)
	t.Setenv(envTarget, "127.0.0.1:50051")
	t.Setenv(envInsecure, "true")
	t.Setenv(envTokenPath, testTokenFile(t))

	conn, client, directOpt, err := DialFromEnvOr(fallbackNotCalled(t))
	require.NoError(t, err)

	t.Cleanup(func() { _ = conn.Close() })

	assert.NotNil(t, directOpt)
	assert.NotNil(t, client)
}

// TestDialFromEnvOr_InvalidEnvRejected: malformed insecure/tuning values must
// fail the dial rather than being silently defaulted.
func TestDialFromEnvOr_InvalidEnvRejected(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"bad_insecure", envInsecure, "notabool"},
		{"bad_retry_window", envRetryWindow, "5minutes"},
		{"negative_retry_window", envRetryWindow, "-1m"},
		{"zero_retry_window", envRetryWindow, "0s"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearPublishEnv(t)
			t.Setenv(envTarget, "127.0.0.1:50051")
			t.Setenv(envInsecure, "true")
			t.Setenv(envTokenPath, testTokenFile(t))
			t.Setenv(tc.key, tc.value)

			_, _, _, err := DialFromEnvOr(fallbackNotCalled(t))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.key)
		})
	}
}

// TestDialFromEnvOr_RefusesTargetWithoutServerName: with TLS the server
// certificate is checked against the target's host name; a target no name can
// be derived from must name it explicitly, or the check would be skipped.
func TestDialFromEnvOr_RefusesTargetWithoutServerName(t *testing.T) {
	clearPublishEnv(t)
	t.Setenv(envTarget, ":50051")
	t.Setenv(envTLSCAFile, "/nonexistent/ca.crt")
	t.Setenv(envTokenPath, testTokenFile(t))

	_, _, _, err := DialFromEnvOr(fallbackNotCalled(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), envTLSServerName)

	// With the override the dial gets as far as the CA file, which is absent.
	t.Setenv(envTLSServerName, "platform-connector-deployment.nvsentinel.svc")

	_, _, _, err = DialFromEnvOr(fallbackNotCalled(t))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), envTLSServerName)
	assert.Contains(t, err.Error(), "/nonexistent/ca.crt")

	// Plaintext development mode verifies nothing, so it needs no name.
	clearPublishEnv(t)
	t.Setenv(envTarget, ":50051")
	t.Setenv(envInsecure, "true")
	t.Setenv(envTokenPath, testTokenFile(t))

	conn, _, directOpt, err := DialFromEnvOr(fallbackNotCalled(t))
	require.NoError(t, err)
	require.NotNil(t, directOpt)
	require.NoError(t, conn.Close())
}

// TestDirectTuningFromEnv_ValidOverrides: set values must override the
// contract defaults.
func TestDirectTuningFromEnv_ValidOverrides(t *testing.T) {
	clearPublishEnv(t)
	t.Setenv(envRetryWindow, "90s")

	tune, err := directTuningFromEnv()
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, tune.retryWindow)

	clearPublishEnv(t)

	tune, err = directTuningFromEnv()
	require.NoError(t, err)
	assert.Equal(t, defaultDirectTuning(), tune)
}

// TestServerNameFromTarget covers scheme, authority and port stripping.
func TestServerNameFromTarget(t *testing.T) {
	cases := []struct {
		target string
		want   string
	}{
		{"platform-connector-deployment.nvsentinel.svc:50051", "platform-connector-deployment.nvsentinel.svc"},
		{"dns:///platform-connector-deployment.nvsentinel.svc:50051", "platform-connector-deployment.nvsentinel.svc"},
		{"passthrough:///127.0.0.1:50051", "127.0.0.1"},
		{"127.0.0.1:50051", "127.0.0.1"},
		{"no-port-host", "no-port-host"},
	}

	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			assert.Equal(t, tc.want, serverNameFromTarget(tc.target))
		})
	}
}

// capturingServer is a real PlatformConnector gRPC service that records the
// metadata of the first call it receives.
type capturingServer struct {
	pb.UnimplementedPlatformConnectorServer

	got chan metadata.MD
}

func (s *capturingServer) HealthEventOccurredV1(
	ctx context.Context, _ *pb.HealthEvents,
) (*emptypb.Empty, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	select {
	case s.got <- md:
	default:
	}

	return &emptypb.Empty{}, nil
}

// generateTestCA produces a CA (written as a PEM file) and a server
// certificate for "localhost"/127.0.0.1 signed by it.
func generateTestCA(t *testing.T) (caPEMPath string, serverCert tls.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "healthpub-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	caPEMPath = filepath.Join(t.TempDir(), "ca.crt")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	require.NoError(t, os.WriteFile(caPEMPath, caPEM, 0o600))

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	require.NoError(t, err)

	return caPEMPath, tls.Certificate{
		Certificate: [][]byte{serverDER},
		PrivateKey:  serverKey,
	}
}

// TestDialFromEnvOr_DirectTLSEndToEnd: the full direct path against a real
// TLS gRPC server: CA verification with the pinned ServerName, the bearer
// token read from HEALTH_PUBLISH_TOKEN_PATH, and the idempotency-key header,
// all observed server-side.
func TestDialFromEnvOr_DirectTLSEndToEnd(t *testing.T) {
	caPEMPath, serverCert := generateTestCA(t)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS12,
	})))
	capture := &capturingServer{got: make(chan metadata.MD, 1)}
	pb.RegisterPlatformConnectorServer(server, capture)

	go func() { _ = server.Serve(lis) }()

	t.Cleanup(server.Stop)

	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("e2e-token"), 0o600))

	clearPublishEnv(t)
	t.Setenv(envTarget, lis.Addr().String())
	t.Setenv(envTLSCAFile, caPEMPath)
	t.Setenv(envTLSServerName, "localhost")
	t.Setenv(envTokenPath, tokenPath)

	_, client, directOpt, err := DialFromEnvOr(fallbackNotCalled(t))
	require.NoError(t, err)
	require.NotNil(t, directOpt)

	p := New(client, lis.Addr().String(), "test-e2e-tls", directOpt)

	require.NoError(t, p.Publish(context.Background(), sampleEvents()))

	select {
	case md := <-capture.got:
		require.Equal(t, []string{"Bearer e2e-token"}, md.Get("authorization"),
			"the projected token must arrive as a Bearer credential")

		keys := md.Get(IdempotencyKeyHeader)
		require.Len(t, keys, 1, "every direct send must carry exactly one idempotency key")
		assert.Regexp(t, idempotencyKeyFormat, keys[0])
	case <-time.After(10 * time.Second):
		t.Fatal("server never received the published batch over TLS")
	}

	closePublisher(t, p)
}

// TestDialFromEnvOr_DirectPlaintextEndToEnd: the insecure development path
// must deliver against a plaintext server, still carrying the idempotency key
// and the caller's W3C trace context captured at Publish time.
func TestDialFromEnvOr_DirectPlaintextEndToEnd(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := grpc.NewServer()
	capture := &capturingServer{got: make(chan metadata.MD, 1)}
	pb.RegisterPlatformConnectorServer(server, capture)

	go func() { _ = server.Serve(lis) }()

	t.Cleanup(server.Stop)

	clearPublishEnv(t)
	t.Setenv(envTarget, lis.Addr().String())
	t.Setenv(envInsecure, "true")
	t.Setenv(envTokenPath, testTokenFile(t))

	_, client, directOpt, err := DialFromEnvOr(fallbackNotCalled(t))
	require.NoError(t, err)
	require.NotNil(t, directOpt)

	p := New(client, lis.Addr().String(), "test-e2e-plaintext", directOpt)

	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:     trace.SpanID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11},
		TraceFlags: trace.FlagsSampled,
	})
	publishCtx := trace.ContextWithSpanContext(context.Background(), spanCtx)

	require.NoError(t, p.Publish(publishCtx, sampleEvents()))

	select {
	case md := <-capture.got:
		require.Len(t, md.Get(IdempotencyKeyHeader), 1)

		traceparent := md.Get("traceparent")
		require.Len(t, traceparent, 1, "direct sends must propagate the caller's trace context")
		assert.Contains(t, traceparent[0], spanCtx.TraceID().String(),
			"the propagated traceparent must carry the Publish-time trace ID")
	case <-time.After(10 * time.Second):
		t.Fatal("server never received the published batch over plaintext")
	}

	closePublisher(t, p)
}

// tokenCheckingServer accepts a batch only with the rotated token and records
// the authorization header of every attempt.
type tokenCheckingServer struct {
	pb.UnimplementedPlatformConnectorServer

	seen chan string
}

func (s *tokenCheckingServer) HealthEventOccurredV1(
	ctx context.Context, _ *pb.HealthEvents,
) (*emptypb.Empty, error) {
	md, _ := metadata.FromIncomingContext(ctx)

	auth := ""
	if values := md.Get("authorization"); len(values) > 0 {
		auth = values[0]
	}

	s.seen <- auth

	if auth != "Bearer fresh" {
		return nil, status.Error(codes.Unauthenticated, "stale token")
	}

	return &emptypb.Empty{}, nil
}

// TestDialFromEnvOr_TokenIsReadPerAttempt: the kubelet rewrites the projected
// token file, so an attempt refused with the old token must be retried with
// the new one inside the same call, which needs the token interceptor to run
// per attempt, inside the retry interceptor.
func TestDialFromEnvOr_TokenIsReadPerAttempt(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := grpc.NewServer()
	check := &tokenCheckingServer{seen: make(chan string, 8)}
	pb.RegisterPlatformConnectorServer(server, check)

	go func() { _ = server.Serve(lis) }()

	t.Cleanup(server.Stop)

	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("stale"), 0o600))

	clearPublishEnv(t)
	t.Setenv(envTarget, lis.Addr().String())
	t.Setenv(envInsecure, "true")
	t.Setenv(envTokenPath, tokenPath)

	_, client, opt, err := DialFromEnvOr(fallbackNotCalled(t))
	require.NoError(t, err)

	p := New(client, lis.Addr().String(), "test-e2e-token-rotation", opt)

	result := publishAsync(context.Background(), p, sampleEvents())

	select {
	case auth := <-check.seen:
		assert.Equal(t, "Bearer stale", auth, "the first attempt carries the token as it was")
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the first attempt")
	}

	require.NoError(t, os.WriteFile(tokenPath, []byte("fresh"), 0o600))

	require.NoError(t, awaitPublish(t, result), "the retry carries the rotated token and is accepted")

	last := ""
	for len(check.seen) > 0 {
		last = <-check.seen
	}

	assert.Equal(t, "Bearer fresh", last, "the retry re-read the token file")

	closePublisher(t, p)
}

// TestDialFromEnvOr_DirectTLSWrongCARefused: with the client trusting CA-A
// and the server presenting a certificate signed by CA-B, every send must be
// refused at the handshake: the server never receives a batch, retries climb,
// and the batch eventually drops when the retry window expires.
func TestDialFromEnvOr_DirectTLSWrongCARefused(t *testing.T) {
	trustedCAPath, _ := generateTestCA(t) // CA-A: what the client trusts.
	_, serverCert := generateTestCA(t)    // CA-B: what actually signed the server.

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS12,
	})))
	capture := &capturingServer{got: make(chan metadata.MD, 1)}
	pb.RegisterPlatformConnectorServer(server, capture)

	go func() { _ = server.Serve(lis) }()

	t.Cleanup(server.Stop)

	clearPublishEnv(t)
	t.Setenv(envTarget, lis.Addr().String())
	t.Setenv(envTLSCAFile, trustedCAPath)
	t.Setenv(envTLSServerName, "localhost")
	t.Setenv(envTokenPath, testTokenFile(t))
	// Long enough for a retry at the production pace (the first pause is about
	// 2 s), short enough for a test.
	t.Setenv(envRetryWindow, "5s")

	_, client, directOpt, err := DialFromEnvOr(fallbackNotCalled(t))
	require.NoError(t, err, "the dial is lazy; the handshake failure surfaces per send")
	require.NotNil(t, directOpt)

	monitor := "test-e2e-wrong-ca"
	retriesBefore := testutil.ToFloat64(sendRetries.WithLabelValues(monitor))
	droppedBefore := testutil.ToFloat64(
		sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted))

	p := New(client, lis.Addr().String(), monitor, directOpt)

	require.ErrorIs(t, p.Publish(context.Background(), sampleEvents()), ErrPublishDropped,
		"a server certificate the client does not trust never delivers; the batch drops when its window ends")

	assert.GreaterOrEqual(t, testutil.ToFloat64(sendRetries.WithLabelValues(monitor)), retriesBefore+1,
		"handshake failures must surface as retries, not deliveries")
	assert.Equal(t, droppedBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted)),
		"the batch must drop when the retry window expires")

	select {
	case <-capture.got:
		t.Fatal("a batch crossed a TLS session the client must have refused")
	default:
	}

	closePublisher(t, p)
}

// TestDirectConnectParams_CapsTheReconnectDelay: the channel's reconnect
// backoff keeps gRPC's defaults except for the cap, so a client that saw an
// outage is connected again within seconds of the server returning instead
// of waiting out a delay that gRPC lets grow to two minutes.
func TestDirectConnectParams_CapsTheReconnectDelay(t *testing.T) {
	params := directConnectParams()

	assert.Equal(t, maxReconnectDelay, params.Backoff.MaxDelay)
	assert.Less(t, params.Backoff.MaxDelay, maxRetryBackoff,
		"the channel must reconnect faster than the publisher's retry cadence")
	assert.Equal(t, backoff.DefaultConfig.BaseDelay, params.Backoff.BaseDelay)
	assert.Equal(t, backoff.DefaultConfig.Multiplier, params.Backoff.Multiplier)
	assert.Equal(t, backoff.DefaultConfig.Jitter, params.Backoff.Jitter)
	assert.Equal(t, directConnectMinTimeout, params.MinConnectTimeout)
}
