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
	"net"
	"os"
	"strconv"
	"strings"
	"time"

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
	// envRetryWindow is the retry budget of a queued batch, counted from the
	// moment it is queued: waiting in the queue, attempts and backoff all
	// spend it (default 5m).
	envRetryWindow = "HEALTH_PUBLISH_RETRY_WINDOW"
	// envQueueMaxBatches bounds the direct-mode client queue in batches
	// (default 1024).
	envQueueMaxBatches = "HEALTH_PUBLISH_QUEUE_MAX_BATCHES"
	// envQueueMaxBytes bounds the direct-mode client queue in serialized bytes
	// (default 64 MiB).
	envQueueMaxBytes = "HEALTH_PUBLISH_QUEUE_MAX_BYTES"
)

const (
	defaultRetryWindow     = 5 * time.Minute
	defaultQueueMaxBatches = 1024
	defaultQueueMaxBytes   = 67108864
	// defaultRPCTimeout bounds one send. The server writes the batch to the
	// datastore and updates the node condition inside the request, so this
	// leaves room for both, and for a MongoDB primary election.
	defaultRPCTimeout = 30 * time.Second

	// defaultMaxSendBytes matches the gRPC server's default receive limit (4
	// MiB): a batch over it would be rejected by the server on every retry, so
	// the client refuses it at enqueue as rejected.
	defaultMaxSendBytes = 4194304

	// defaultFinalAttemptWindow is the least time the last attempt of a batch
	// gets: the last backoff sleep is cut so that attempt starts this long
	// before the retry window ends, and a batch with less than this left after
	// a failure is dropped rather than attempted with no time to succeed.
	defaultFinalAttemptWindow = time.Second
)

// DialFromEnvOr decides the publishing mode from the HEALTH_PUBLISH_*
// environment in one call.
//
// With HEALTH_PUBLISH_TARGET unset (socket mode) it runs fallback — the
// caller's legacy node-local dial, unchanged — and returns its connection
// wrapped in a client with a nil directOpt. New skips nil options, so call
// sites pass the returned values straight through in both modes.
//
// With HEALTH_PUBLISH_TARGET set (direct mode) it validates the direct-mode
// tuning environment (retry window and queue bounds, so a misconfigured value
// fails at startup instead of being silently defaulted later), dials the
// deployment platform connector with TLS verified against
// HEALTH_PUBLISH_TLS_CA_FILE (plaintext only with HEALTH_PUBLISH_INSECURE=true)
// and bearer-token authentication from HEALTH_PUBLISH_TOKEN_PATH, and returns
// a non-nil directOpt carrying the validated tuning and the conn. Passing
// directOpt to New hands the conn's lifecycle to the Publisher (Close closes
// it).
func DialFromEnvOr(fallback func() (*grpc.ClientConn, error)) (
	conn *grpc.ClientConn, client pb.PlatformConnectorClient, directOpt Option, err error,
) {
	target := os.Getenv(envTarget)
	if target == "" {
		conn, err = fallback()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("legacy platform-connector dial failed: %w", err)
		}

		return conn, pb.NewPlatformConnectorClient(conn), nil, nil
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

	conn, err = dialDirectFromEnv(target, tokenPath)
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
// the HEALTH_PUBLISH_* transport environment and the token at tokenPath.
func dialDirectFromEnv(target, tokenPath string) (*grpc.ClientConn, error) {
	opts, err := directDialOptionsFromEnv(target, tokenPath)
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
// (override or the target host), bearer-token authentication, and a send-size
// cap matching the server's default receive limit.
func directDialOptionsFromEnv(target, tokenPath string) ([]grpc.DialOption, error) {
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
	// hostname check would be skipped, and with a name that is not a host
	// (a resolver form such as "dns:host:port" left intact) every handshake
	// would fail. Both need the explicit override.
	if caFile != "" && serverName == "" {
		return nil, fmt.Errorf("cannot derive a TLS server name from %s %q; set %s", envTarget, target, envTLSServerName)
	}

	if caFile != "" && net.ParseIP(serverName) == nil && strings.ContainsAny(serverName, ":/") {
		return nil, fmt.Errorf("TLS server name %q derived from %s %q is not a host name; set %s",
			serverName, envTarget, target, envTLSServerName)
	}

	creds, err := buildTransportCredentials(caFile, serverName, allowInsecure)
	if err != nil {
		return nil, err
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithConnectParams(directConnectParams()),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(defaultMaxSendBytes)),
	}
	opts = append(opts, grpcclient.DialOptions(tokenPath)...)

	return opts, nil
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

// directTuning carries the direct-mode queue and retry configuration.
type directTuning struct {
	retryWindow time.Duration
	maxBatches  int
	maxBytes    int64
	rpcTimeout  time.Duration
	// maxMessageBytes is the largest batch accepted into the queue: the
	// server's receive limit, checked here so an oversize batch is refused at
	// once instead of failing on the wire. Zero disables the check.
	maxMessageBytes int64
	// finalAttemptWindow is the least time the last attempt of a batch gets
	// before its retry window ends; see defaultFinalAttemptWindow.
	finalAttemptWindow time.Duration
}

// defaultDirectTuning returns the contract defaults: a 5 minute retry window
// whose last attempt starts a second before it ends, a 1024-batch / 64 MiB
// queue and the 4 MiB message limit.
func defaultDirectTuning() directTuning {
	return directTuning{
		retryWindow:        defaultRetryWindow,
		maxBatches:         defaultQueueMaxBatches,
		maxBytes:           defaultQueueMaxBytes,
		rpcTimeout:         defaultRPCTimeout,
		maxMessageBytes:    defaultMaxSendBytes,
		finalAttemptWindow: defaultFinalAttemptWindow,
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

	maxBatches, err := positiveIntFromEnv(envQueueMaxBatches, int64(tune.maxBatches))
	if err != nil {
		return tune, err
	}

	tune.maxBatches = int(maxBatches)

	tune.maxBytes, err = positiveIntFromEnv(envQueueMaxBytes, tune.maxBytes)
	if err != nil {
		return tune, err
	}

	if tune.maxBytes < tune.maxMessageBytes {
		return tune, fmt.Errorf("invalid %s value %d: below the %d byte message limit, no full-size batch could be queued",
			envQueueMaxBytes, tune.maxBytes, tune.maxMessageBytes)
	}

	if tune.retryWindow <= tune.finalAttemptWindow {
		return tune, fmt.Errorf("invalid %s value %s: must exceed the %s final-attempt window, or no retry could ever run",
			envRetryWindow, tune.retryWindow, tune.finalAttemptWindow)
	}

	return tune, nil
}

// positiveIntFromEnv reads a positive integer from the named variable,
// returning fallback when it is unset.
func positiveIntFromEnv(name string, fallback int64) (int64, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("invalid %s value %q: must be a positive integer", name, raw)
	}

	return value, nil
}
