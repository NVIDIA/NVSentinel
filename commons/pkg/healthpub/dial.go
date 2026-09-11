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
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcclient"
)

// Environment variables read by the shared publishing client. The same
// names are used by the Python client.
const (
	// envTarget switches the publisher to direct mode: when set it is the
	// host:port of the deployment platform connector Service; when unset the
	// publisher keeps the node-local socket behavior unchanged.
	envTarget = "HEALTH_PUBLISH_TARGET"
	// envTLSCAFile is the CA bundle the server certificate is verified
	// against. Required in direct mode unless envInsecure is "true".
	envTLSCAFile = "HEALTH_PUBLISH_TLS_CA_FILE"
	// envTLSServerName overrides the TLS ServerName; default is the host part
	// of the target.
	envTLSServerName = "HEALTH_PUBLISH_TLS_SERVER_NAME"
	// envTokenPath points at a projected ServiceAccount token minted for the
	// deployment platform connector audience; read fresh per send so kubelet
	// rotation is picked up.
	envTokenPath = "HEALTH_PUBLISH_TOKEN_PATH"
	// envInsecure permits a plaintext direct connection; development only.
	envInsecure = "HEALTH_PUBLISH_INSECURE"
	// envRetryWindow is the retry budget of a batch, counted from the Publish
	// call: waiting for the send slot, attempts and backoff all spend it
	// (default 5m).
	envRetryWindow = "HEALTH_PUBLISH_RETRY_WINDOW"
)

const (
	defaultRetryWindow = 5 * time.Minute
	// defaultRPCTimeout bounds one send. The server writes the batch to the
	// datastore and updates the node condition inside the request, so this
	// leaves room for both, and for a MongoDB primary election.
	defaultRPCTimeout = 30 * time.Second

	// maxRetryBackoff caps the pause between a batch's attempts. The pause
	// starts at defaultInitialBackoff and doubles after every failure, with
	// defaultBackoffJitter of jitter, until it reaches the cap.
	maxRetryBackoff = 30 * time.Second
)

// DialFromEnvOr decides the publishing mode from the HEALTH_PUBLISH_*
// environment in one call and returns the connection, a client on it and the
// Option that hands the connection to the Publisher, which closes it in Close.
//
// With HEALTH_PUBLISH_TARGET unset (socket mode) it runs fallback, the
// caller's legacy node-local dial, unchanged.
//
// With HEALTH_PUBLISH_TARGET set (direct mode) it validates the direct-mode
// tuning environment (the retry window, so a misconfigured value fails at
// startup instead of being silently defaulted later), dials the deployment
// platform connector with TLS verified against HEALTH_PUBLISH_TLS_CA_FILE
// (plaintext only with HEALTH_PUBLISH_INSECURE=true) and bearer-token
// authentication from HEALTH_PUBLISH_TOKEN_PATH, and the Option also switches
// the Publisher to direct mode with the validated tuning.
func DialFromEnvOr(fallback func() (*grpc.ClientConn, error)) (
	conn *grpc.ClientConn, client pb.PlatformConnectorClient, opt Option, err error,
) {
	target := os.Getenv(envTarget)
	if target == "" {
		conn, err = fallback()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("legacy platform-connector dial failed: %w", err)
		}

		return conn, pb.NewPlatformConnectorClient(conn), withOwnedConn(conn), nil
	}

	tune, err := directTuningFromEnv()
	if err != nil {
		return nil, nil, nil, err
	}

	// The server accepts no batch without a token, so a missing token path is
	// a configuration error to fail on at startup, not per send.
	tokenPath := strings.TrimSpace(os.Getenv(envTokenPath))
	if tokenPath == "" {
		return nil, nil, nil, fmt.Errorf("%s is required when %s is set: every direct send must carry a token",
			envTokenPath, envTarget)
	}

	conn, err = dialDirectFromEnv(target, tokenPath, tune)
	if err != nil {
		return nil, nil, nil, err
	}

	slog.Info("Dialing deployment platform connector directly",
		"target", target,
		"tlsEnabled", os.Getenv(envTLSCAFile) != "",
		"tokenPath", tokenPath)

	return conn, pb.NewPlatformConnectorClient(conn), withDirect(conn, tune), nil
}

// dialDirectFromEnv creates a direct-mode client connection to target using
// the HEALTH_PUBLISH_* transport environment, the token at tokenPath and the
// retry policy from tune.
func dialDirectFromEnv(target, tokenPath string, tune directTuning) (*grpc.ClientConn, error) {
	opts, err := directDialOptionsFromEnv(target, tokenPath, tune)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC client for deployment platform connector %s: %w", target, err)
	}

	return conn, nil
}

// directDialOptionsFromEnv builds the dial options for a direct connection:
// TLS from the CA file (with the per-handshake reload), the pinned ServerName
// (override or the target host), the retry policy every RPC on the connection
// runs under, and bearer-token authentication inside it, so every attempt
// re-reads the projected token file and a token the kubelet rotated during a
// call's retry window is picked up by the next attempt.
func directDialOptionsFromEnv(target, tokenPath string, tune directTuning) ([]grpc.DialOption, error) {
	allowInsecure := false

	if raw := os.Getenv(envInsecure); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid %s value %q: %w", envInsecure, raw, err)
		}

		allowInsecure = parsed
	}

	caFile := os.Getenv(envTLSCAFile)

	serverName := strings.TrimSpace(os.Getenv(envTLSServerName))
	if serverName == "" {
		serverName = serverNameFromTarget(target)
	}

	// The server certificate is verified against this name; with none the
	// host name check would be skipped, so a target the name cannot be derived
	// from needs the explicit override.
	if caFile != "" && serverName == "" {
		return nil, fmt.Errorf("cannot derive a TLS server name from %s %q; set %s", envTarget, target, envTLSServerName)
	}

	creds, err := buildTransportCredentials(caFile, serverName, allowInsecure)
	if err != nil {
		return nil, err
	}

	// Chained interceptors run in order, so the token interceptor runs once
	// per attempt, inside the retries.
	return []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithConnectParams(directConnectParams()),
		grpc.WithChainUnaryInterceptor(retryInterceptor(tune), grpcclient.TokenInterceptor(tokenPath)),
	}, nil
}

// maxReconnectDelay caps the pause between the channel's attempts to reconnect
// to the Service. gRPC's default grows to two minutes over an outage, and a
// publish attempt made while the channel waits fails at once without touching
// the network, so after the server was back a batch could still sit out most
// of a minute. Capped, the channel is connected within seconds of the server
// returning and the retry cadence alone decides when the batch is resent.
const maxReconnectDelay = 10 * time.Second

// directConnectMinTimeout is gRPC's own default minimum connection timeout,
// restated because WithConnectParams replaces it.
const directConnectMinTimeout = 20 * time.Second

// directConnectParams is gRPC's default reconnect backoff with the delay cap
// lowered to maxReconnectDelay.
func directConnectParams() grpc.ConnectParams {
	cfg := backoff.DefaultConfig
	cfg.MaxDelay = maxReconnectDelay

	return grpc.ConnectParams{Backoff: cfg, MinConnectTimeout: directConnectMinTimeout}
}

// retryAttemptLimit is the attempt count the retry interceptor requires. A
// batch's retries end with its retry window, which publish sets as the call's
// deadline, so the count only has to stay out of reach at the production pace
// of at least about two seconds between attempts. It still bounds a call made
// without a deadline, which no caller of this connection makes.
const retryAttemptLimit = 10_000

// retryInterceptor is the retry policy of every direct-mode RPC, installed on
// the connection: a failed attempt is repeated after a jittered exponential
// pause (tune.backoffInitial, doubling up to tune.backoffMax), each attempt
// bounded by tune.rpcTimeout, until the call's deadline ends the retries.
// Rejections the server would repeat on every attempt are not retried. The
// per-call option publish adds meters and logs the retries.
func retryInterceptor(tune directTuning) grpc.UnaryClientInterceptor {
	return retry.UnaryClientInterceptor(
		retry.WithMax(retryAttemptLimit),
		retry.WithBackoff(retry.BackoffExponentialWithJitterBounded(
			tune.backoffInitial, tune.backoffJitter, tune.backoffMax)),
		retry.WithPerRetryTimeout(tune.rpcTimeout),
		retry.WithRetriable(retriable),
	)
}

// directTuning carries the direct-mode retry configuration: the window a
// batch may spend, the bound of one attempt and the pacing between attempts.
type directTuning struct {
	retryWindow time.Duration
	rpcTimeout  time.Duration
	// backoffInitial is the pause after the first failed attempt; it doubles
	// after every further failure, jittered by the backoffJitter fraction, up
	// to backoffMax.
	backoffInitial time.Duration
	backoffMax     time.Duration
	backoffJitter  float64
}

// defaultDirectTuning returns the contract defaults: a 5 minute retry window
// and 30 second attempts paced from 2 seconds up to 30 seconds apart.
func defaultDirectTuning() directTuning {
	return directTuning{
		retryWindow:    defaultRetryWindow,
		rpcTimeout:     defaultRPCTimeout,
		backoffInitial: defaultInitialBackoff,
		backoffMax:     maxRetryBackoff,
		backoffJitter:  defaultBackoffJitter,
	}
}

// directTuningFromEnv reads the tuning environment, rejecting values that do
// not parse or are not positive; unset values take the contract defaults.
func directTuningFromEnv() (directTuning, error) {
	tune := defaultDirectTuning()

	if raw := os.Getenv(envRetryWindow); raw != "" {
		window, err := time.ParseDuration(raw)
		if err != nil || window <= 0 {
			return tune, fmt.Errorf("invalid %s value %q: must be a positive duration", envRetryWindow, raw)
		}

		tune.retryWindow = window
	}

	return tune, nil
}
