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
	"fmt"
	"os"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/envutil"
)

// Environment defaults for the deployment platform connector. These are
// tuning values; the chart exposes the ones an operator may need to change.
const (
	defaultListenAddr             = ":50051"
	defaultMetricsPort            = 2112
	defaultMaxConnAge             = 10 * time.Minute
	defaultMaxConnIdle            = 5 * time.Minute
	defaultConditionUpdateTimeout = 10 * time.Second
	// The ADR's worst case is about 830 TokenReviews per second per replica
	// (every monitor pod of a 100,000-node fleet writing in every cache
	// window); the default leaves headroom above it.
	defaultTokenReviewQPS   = 1000
	defaultTokenReviewBurst = 2000
	defaultConfigPath       = "/etc/config/config.json"
)

type config struct {
	listenAddr    string
	metricsPort   int
	certMountPath string
	tlsCertDir    string
	maxConnAge    time.Duration
	maxConnIdle   time.Duration
	// conditionUpdateTimeout bounds the node condition update (and the
	// Kubernetes Event write) of one request; the write alone decides the
	// reply, so this only bounds how long a slow API server can delay it.
	conditionUpdateTimeout time.Duration
	// tokenCacheSize is the number of caller tokens whose TokenReview verdict
	// is remembered; zero keeps the grpcauth default.
	tokenCacheSize   int
	tokenReviewQPS   float32
	tokenReviewBurst int
	// k8sClientQPS, k8sClientBurst, nodeMetadataCacheSize and
	// nodeMetadataCacheTTL, when set, override the values of the shared
	// config.json, which are sized for one node's platform connector; the
	// central replicas serve the whole fleet.
	k8sClientQPS          float32
	k8sClientBurst        int
	nodeMetadataCacheSize int
	nodeMetadataCacheTTL  time.Duration
	// grpcReadBufferBytes and grpcWriteBufferBytes size the two buffers gRPC
	// keeps per connection; zero keeps the grpc-go default of 32 KiB each.
	// Smaller buffers cut the memory each connected monitor pod costs.
	grpcReadBufferBytes  int
	grpcWriteBufferBytes int
	configPath           string
}

// envPositiveInt reads an integer whose zero or negative value would make the
// server useless.
func envPositiveInt(key string, def int) (int, error) {
	v, err := envutil.ParseEnvInt(key, def)
	if err != nil {
		return 0, err
	}

	if v <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %d", key, v)
	}

	return v, nil
}

// envPositiveDuration reads a duration whose zero or negative value would
// make the server useless. Unset keeps the default, which may be zero where
// zero means "keep the shared config.json value".
func envPositiveDuration(key string, def time.Duration) (time.Duration, error) {
	if os.Getenv(key) == "" {
		return def, nil
	}

	v, err := envutil.ParseEnvDuration(key, def)
	if err != nil {
		return 0, err
	}

	if v <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", key, v)
	}

	return v, nil
}

// trueValue is the string form of an enabled boolean toggle, as both the
// chart's quoted env values and the shared JSON config use it.
const trueValue = "true"

func loadConfigFromEnv() (*config, error) {
	cfg := &config{
		listenAddr:    envutil.GetEnvString("LISTEN_ADDR", defaultListenAddr),
		certMountPath: datastoreCertMountPath(),
		configPath:    envutil.GetEnvString("CONFIG_PATH", defaultConfigPath),
	}

	var err error

	if cfg.metricsPort, err = envutil.ParseEnvInt("METRICS_PORT", defaultMetricsPort); err != nil {
		return nil, err
	}

	if err = loadTokenReviewEnv(cfg); err != nil {
		return nil, err
	}

	if err = loadFleetSizingEnv(cfg); err != nil {
		return nil, err
	}

	if err = loadTLSEnv(cfg); err != nil {
		return nil, err
	}

	if err = loadTuningEnv(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// datastoreCertMountPath returns the client certificate mount path of the
// configured datastore: the chart sets exactly one of the two variables.
func datastoreCertMountPath() string {
	if path := os.Getenv("POSTGRESQL_CLIENT_CERT_MOUNT_PATH"); path != "" {
		return path
	}

	return os.Getenv("MONGODB_CLIENT_CERT_MOUNT_PATH")
}

// loadTokenReviewEnv reads the TokenReview client rate limit and the verdict
// cache size.
func loadTokenReviewEnv(cfg *config) error {
	var err error

	if cfg.tokenCacheSize, err = envutil.ParseEnvInt("TOKEN_CACHE_SIZE", 0); err != nil {
		return err
	}

	if cfg.tokenCacheSize < 0 {
		return fmt.Errorf("TOKEN_CACHE_SIZE must not be negative, got %d", cfg.tokenCacheSize)
	}

	qps, err := envPositiveInt("TOKENREVIEW_QPS", defaultTokenReviewQPS)
	if err != nil {
		return err
	}

	cfg.tokenReviewQPS = float32(qps)

	if cfg.tokenReviewBurst, err = envPositiveInt("TOKENREVIEW_BURST", defaultTokenReviewBurst); err != nil {
		return err
	}

	return nil
}

// loadFleetSizingEnv reads the overrides for values the shared config.json
// sizes for one node. Unset (zero) keeps the config.json value.
func loadFleetSizingEnv(cfg *config) error {
	qps, err := envutil.ParseEnvFloat64("K8S_CLIENT_QPS", 0)
	if err != nil {
		return err
	}

	cfg.k8sClientQPS = float32(qps)

	if cfg.k8sClientBurst, err = envutil.ParseEnvInt("K8S_CLIENT_BURST", 0); err != nil {
		return err
	}

	if cfg.nodeMetadataCacheSize, err = envutil.ParseEnvInt("NODE_METADATA_CACHE_SIZE", 0); err != nil {
		return err
	}

	if cfg.k8sClientQPS < 0 || cfg.k8sClientBurst < 0 || cfg.nodeMetadataCacheSize < 0 {
		return fmt.Errorf("K8S_CLIENT_QPS, K8S_CLIENT_BURST and NODE_METADATA_CACHE_SIZE must not be negative")
	}

	if cfg.nodeMetadataCacheTTL, err = envPositiveDuration("NODE_METADATA_CACHE_TTL", 0); err != nil {
		return err
	}

	return nil
}

// loadTLSEnv reads the listener TLS settings. The token crosses the pod
// network in gRPC metadata, so a plaintext listener is refused unless the
// explicitly named insecure development mode is set.
func loadTLSEnv(cfg *config) error {
	cfg.tlsCertDir = os.Getenv("TLS_CERT_DIR")
	insecureDev := os.Getenv("TLS_INSECURE_DEVELOPMENT_MODE") == trueValue

	if cfg.tlsCertDir == "" && !insecureDev {
		return fmt.Errorf("TLS_CERT_DIR is required unless TLS_INSECURE_DEVELOPMENT_MODE=true")
	}

	return nil
}

// loadTuningEnv reads the connection lifetime and timeout knobs.
func loadTuningEnv(cfg *config) error {
	var err error

	if cfg.maxConnAge, err = envPositiveDuration("MAX_CONNECTION_AGE", defaultMaxConnAge); err != nil {
		return err
	}

	if cfg.maxConnIdle, err = envPositiveDuration("MAX_CONNECTION_IDLE", defaultMaxConnIdle); err != nil {
		return err
	}

	if cfg.conditionUpdateTimeout, err = envPositiveDuration(
		"CONDITION_UPDATE_TIMEOUT", defaultConditionUpdateTimeout); err != nil {
		return err
	}

	if cfg.grpcReadBufferBytes, err = envutil.ParseEnvInt("GRPC_READ_BUFFER_BYTES", 0); err != nil {
		return err
	}

	if cfg.grpcWriteBufferBytes, err = envutil.ParseEnvInt("GRPC_WRITE_BUFFER_BYTES", 0); err != nil {
		return err
	}

	if cfg.grpcReadBufferBytes < 0 || cfg.grpcWriteBufferBytes < 0 {
		return fmt.Errorf("GRPC_READ_BUFFER_BYTES and GRPC_WRITE_BUFFER_BYTES must not be negative")
	}

	return nil
}
