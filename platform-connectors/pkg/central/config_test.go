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

package central

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testPublisher = "system:serviceaccount:nvsentinel:gpu-health-monitor"
	testCrossNode = "system:serviceaccount:nvsentinel:health-events-analyzer"
)

// setRequiredEnv sets the minimum environment a config load needs.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AUTH_AUDIENCE", "platform-connector-deployment.nvsentinel.nvidia.com")
	t.Setenv("ALLOWED_PUBLISHERS", testPublisher)
	t.Setenv("TLS_INSECURE_DEVELOPMENT_MODE", "true")
}

func TestLoadConfigDefaults(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := loadConfigFromEnv()
	require.NoError(t, err)

	require.Equal(t, ":50051", cfg.listenAddr)
	require.Equal(t, 2112, cfg.metricsPort)
	require.True(t, cfg.allowedPublishers[testPublisher])
	require.Empty(t, cfg.crossNodePublishers)
	require.Equal(t, 10*time.Minute, cfg.maxConnAge)
	require.Equal(t, 5*time.Minute, cfg.maxConnIdle)
	require.Equal(t, 10*time.Second, cfg.conditionUpdateTimeout)
	require.Equal(t, 20*time.Second, cfg.shutdownTimeout)
	require.Equal(t, 5*time.Second, cfg.pipelineTimeout)
	require.Zero(t, cfg.tokenCacheSize, "zero keeps the grpcauth default")
	require.EqualValues(t, 1000, cfg.tokenReviewQPS)
	require.Equal(t, 2000, cfg.tokenReviewBurst)
	require.Empty(t, cfg.tlsCertDir, "plaintext is allowed only because the insecure development mode is set")
	require.Equal(t, "/etc/config/config.json", cfg.configPath)
	require.Zero(t, cfg.k8sClientQPS, "zero keeps the shared config value")
	require.Zero(t, cfg.nodeMetadataCacheSize)
	require.Zero(t, cfg.nodeMetadataCacheTTL)
	require.Zero(t, cfg.grpcReadBufferBytes, "zero keeps the grpc-go default")
	require.Zero(t, cfg.grpcWriteBufferBytes)
}

func TestLoadConfigOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LISTEN_ADDR", ":9999")
	t.Setenv("ALLOWED_PUBLISHERS", testPublisher+", "+testCrossNode)
	t.Setenv("CROSS_NODE_PUBLISHERS", testCrossNode)
	t.Setenv("MAX_CONNECTION_AGE", "1m")
	t.Setenv("MAX_CONNECTION_IDLE", "30s")
	t.Setenv("CONDITION_UPDATE_TIMEOUT", "3s")
	t.Setenv("SHUTDOWN_TIMEOUT", "7s")
	t.Setenv("TOKEN_CACHE_SIZE", "450000")
	t.Setenv("TOKENREVIEW_QPS", "200")
	t.Setenv("TOKENREVIEW_BURST", "400")
	t.Setenv("POSTGRESQL_CLIENT_CERT_MOUNT_PATH", "/etc/ssl/client-certs")
	t.Setenv("K8S_CLIENT_QPS", "100.5")
	t.Setenv("K8S_CLIENT_BURST", "200")
	t.Setenv("NODE_METADATA_CACHE_SIZE", "200000")
	t.Setenv("NODE_METADATA_CACHE_TTL", "10m")
	t.Setenv("GRPC_READ_BUFFER_BYTES", "8192")
	t.Setenv("GRPC_WRITE_BUFFER_BYTES", "4096")

	cfg, err := loadConfigFromEnv()
	require.NoError(t, err)
	require.EqualValues(t, 100.5, cfg.k8sClientQPS)
	require.Equal(t, 200, cfg.k8sClientBurst)
	require.Equal(t, 200000, cfg.nodeMetadataCacheSize)
	require.Equal(t, 10*time.Minute, cfg.nodeMetadataCacheTTL)
	require.Equal(t, 8192, cfg.grpcReadBufferBytes)
	require.Equal(t, 4096, cfg.grpcWriteBufferBytes)

	require.Equal(t, ":9999", cfg.listenAddr)
	require.True(t, cfg.allowedPublishers[testPublisher])
	require.True(t, cfg.allowedPublishers[testCrossNode])
	require.True(t, cfg.crossNodePublishers[testCrossNode])
	require.Equal(t, time.Minute, cfg.maxConnAge)
	require.Equal(t, 30*time.Second, cfg.maxConnIdle)
	require.Equal(t, 3*time.Second, cfg.conditionUpdateTimeout)
	require.Equal(t, 7*time.Second, cfg.shutdownTimeout)
	require.Equal(t, 450000, cfg.tokenCacheSize)
	require.EqualValues(t, 200, cfg.tokenReviewQPS)
	require.Equal(t, 400, cfg.tokenReviewBurst)
	require.Equal(t, "/etc/ssl/client-certs", cfg.certMountPath)
}

func TestLoadConfigRejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T)
		wantErr string
	}{
		{"missing audience", func(t *testing.T) { t.Setenv("AUTH_AUDIENCE", "") }, "AUTH_AUDIENCE is required"},
		{"missing publishers", func(t *testing.T) { t.Setenv("ALLOWED_PUBLISHERS", "") }, "ALLOWED_PUBLISHERS is required"},
		{"non-canonical publisher", func(t *testing.T) { t.Setenv("ALLOWED_PUBLISHERS", "not-a-username") }, "ALLOWED_PUBLISHERS"},
		{
			"cross-node identity outside the publisher allowlist",
			func(t *testing.T) { t.Setenv("CROSS_NODE_PUBLISHERS", testCrossNode) },
			"not in ALLOWED_PUBLISHERS",
		},
		{
			"plaintext without insecure development mode",
			func(t *testing.T) { t.Setenv("TLS_INSECURE_DEVELOPMENT_MODE", "") },
			"TLS_CERT_DIR is required",
		},
		{"malformed duration", func(t *testing.T) { t.Setenv("CONDITION_UPDATE_TIMEOUT", "soon") }, "invalid CONDITION_UPDATE_TIMEOUT"},
		{"zero condition timeout", func(t *testing.T) { t.Setenv("CONDITION_UPDATE_TIMEOUT", "0s") }, "must be positive"},
		{"zero pipeline timeout", func(t *testing.T) { t.Setenv("PIPELINE_TIMEOUT", "0s") }, "must be positive"},
		{"negative shutdown timeout", func(t *testing.T) { t.Setenv("SHUTDOWN_TIMEOUT", "-5s") }, "must be positive"},
		{"malformed connection age", func(t *testing.T) { t.Setenv("MAX_CONNECTION_AGE", "soon") }, "invalid MAX_CONNECTION_AGE"},
		{"negative token cache", func(t *testing.T) { t.Setenv("TOKEN_CACHE_SIZE", "-1") }, "TOKEN_CACHE_SIZE must not be negative"},
		{"zero tokenreview qps", func(t *testing.T) { t.Setenv("TOKENREVIEW_QPS", "0") }, "TOKENREVIEW_QPS must be positive"},
		{"malformed metrics port", func(t *testing.T) { t.Setenv("METRICS_PORT", "p") }, "invalid METRICS_PORT"},
		{"negative node cache", func(t *testing.T) { t.Setenv("NODE_METADATA_CACHE_SIZE", "-1") }, "must not be negative"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			tc.mutate(t)

			_, err := loadConfigFromEnv()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestDatastoreCertMountPath(t *testing.T) {
	t.Setenv("POSTGRESQL_CLIENT_CERT_MOUNT_PATH", "")
	t.Setenv("MONGODB_CLIENT_CERT_MOUNT_PATH", "")
	require.Empty(t, DatastoreCertMountPath(), "no certificate mount means TLS off")

	t.Setenv("MONGODB_CLIENT_CERT_MOUNT_PATH", "/etc/ssl/mongo-client")
	require.Equal(t, "/etc/ssl/mongo-client", DatastoreCertMountPath())

	t.Setenv("POSTGRESQL_CLIENT_CERT_MOUNT_PATH", "/etc/ssl/client-certs")
	require.Equal(t, "/etc/ssl/client-certs", DatastoreCertMountPath(), "the PostgreSQL path wins when set")
}

func TestCfgBool(t *testing.T) {
	m := map[string]any{"quoted": "true", "raw": true, "off": "false", "number": json.Number("1")}
	require.True(t, cfgBool(m, "quoted"))
	require.True(t, cfgBool(m, "raw"))
	require.False(t, cfgBool(m, "off"))
	require.False(t, cfgBool(m, "number"))
	require.False(t, cfgBool(m, "missing"))
}
