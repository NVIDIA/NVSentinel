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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/envutil"
	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
)

// Environment defaults for the deployment platform connector. These are
// tuning values; the chart exposes the ones an operator may need to change.
const (
	defaultListenAddr             = ":50051"
	defaultMetricsPort            = 2112
	defaultMaxConnAge             = 10 * time.Minute
	defaultMaxConnIdle            = 5 * time.Minute
	defaultConditionUpdateTimeout = 10 * time.Second
	defaultShutdownTimeout        = 20 * time.Second
	// defaultPipelineTimeout bounds the pipeline for one batch. It must leave
	// room for at least one node metadata read (the metadata transformer's
	// lookupTimeout, 3s by default), and it keeps a cross-node batch naming
	// many uncached nodes from spending one lookup timeout per node during an
	// API server outage; once spent, the remaining lookups fail open at once.
	defaultPipelineTimeout = 5 * time.Second
	// The ADR's worst case is about 830 TokenReviews per second per replica
	// (every monitor pod of a 100,000-node fleet writing in every cache
	// window); the default leaves headroom above it.
	defaultTokenReviewQPS   = 1000
	defaultTokenReviewBurst = 2000
	defaultConfigPath       = "/etc/config/config.json"
)

type config struct {
	listenAddr  string
	metricsPort int
	audience    string
	// allowedPublishers is every identity that may publish health events.
	// Every batch is pinned to the caller token's node claim unless the
	// identity is also on crossNodePublishers.
	allowedPublishers map[string]bool
	// crossNodePublishers is the subset of allowedPublishers permitted to name
	// any node.
	crossNodePublishers map[string]bool
	certMountPath       string
	tlsCertDir          string
	maxConnAge          time.Duration
	maxConnIdle         time.Duration
	// conditionUpdateTimeout bounds the node condition update (and the
	// Kubernetes Event write) of one request; the write alone decides the
	// reply, so this only bounds how long a slow API server can delay it.
	conditionUpdateTimeout time.Duration
	// pipelineTimeout bounds the pipeline run of one request as a whole.
	pipelineTimeout time.Duration
	// shutdownTimeout bounds the wait for open connections at shutdown, so one
	// unresponsive client cannot block it.
	shutdownTimeout time.Duration
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

func parseIdentityList(raw string) (map[string]bool, error) {
	out := map[string]bool{}

	for _, w := range strings.Split(raw, ",") {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}

		if err := grpcauth.ValidateServiceAccountUsername(w); err != nil {
			return nil, fmt.Errorf("invalid identity %q: %w", w, err)
		}

		out[w] = true
	}

	return out, nil
}

func envInt(key string, def int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}

	return v, nil
}

// envPositiveInt is envInt for values where zero or a negative number would
// make the server useless.
func envPositiveInt(key string, def int) (int, error) {
	v, err := envInt(key, def)
	if err != nil {
		return 0, err
	}

	if v <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %d", key, v)
	}

	return v, nil
}

// envFloat reads a floating point value, as the shared config.json expresses
// the Kubernetes client QPS.
func envFloat(key string, def float64) (float64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}

	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}

	return v, nil
}

func envPositiveDuration(key string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}

	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
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
		certMountPath: DatastoreCertMountPath(),
		configPath:    envutil.GetEnvString("CONFIG_PATH", defaultConfigPath),
	}

	var err error

	if cfg.metricsPort, err = envInt("METRICS_PORT", defaultMetricsPort); err != nil {
		return nil, err
	}

	if err = loadPublisherAuthEnv(cfg); err != nil {
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

// DatastoreCertMountPath returns the client certificate mount path of the
// configured datastore: the chart sets exactly one of the two variables. The
// index Job (PC_MODE=ensure-idempotency-index) reads the same variables.
func DatastoreCertMountPath() string {
	if path := os.Getenv("POSTGRESQL_CLIENT_CERT_MOUNT_PATH"); path != "" {
		return path
	}

	return os.Getenv("MONGODB_CLIENT_CERT_MOUNT_PATH")
}

// loadPublisherAuthEnv reads the token audience and the two publisher lists.
func loadPublisherAuthEnv(cfg *config) error {
	cfg.audience = os.Getenv("AUTH_AUDIENCE")
	if cfg.audience == "" {
		return fmt.Errorf("AUTH_AUDIENCE is required")
	}

	var err error

	if cfg.allowedPublishers, err = parseIdentityList(os.Getenv("ALLOWED_PUBLISHERS")); err != nil {
		return fmt.Errorf("ALLOWED_PUBLISHERS: %w", err)
	}

	if len(cfg.allowedPublishers) == 0 {
		return fmt.Errorf("ALLOWED_PUBLISHERS is required (comma-separated ServiceAccount usernames)")
	}

	if cfg.crossNodePublishers, err = parseIdentityList(os.Getenv("CROSS_NODE_PUBLISHERS")); err != nil {
		return fmt.Errorf("CROSS_NODE_PUBLISHERS: %w", err)
	}

	// Cross-node scope only lifts the pinning for an identity that can publish
	// at all; an entry outside ALLOWED_PUBLISHERS is a configuration error, not
	// a silently dead one.
	for identity := range cfg.crossNodePublishers {
		if !cfg.allowedPublishers[identity] {
			return fmt.Errorf("CROSS_NODE_PUBLISHERS entry %q is not in ALLOWED_PUBLISHERS", identity)
		}
	}

	return nil
}

// loadTokenReviewEnv reads the TokenReview client rate limit and the verdict
// cache size.
func loadTokenReviewEnv(cfg *config) error {
	var err error

	if cfg.tokenCacheSize, err = envInt("TOKEN_CACHE_SIZE", 0); err != nil {
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
	qps, err := envFloat("K8S_CLIENT_QPS", 0)
	if err != nil {
		return err
	}

	cfg.k8sClientQPS = float32(qps)

	if cfg.k8sClientBurst, err = envInt("K8S_CLIENT_BURST", 0); err != nil {
		return err
	}

	if cfg.nodeMetadataCacheSize, err = envInt("NODE_METADATA_CACHE_SIZE", 0); err != nil {
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

	if cfg.shutdownTimeout, err = envPositiveDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout); err != nil {
		return err
	}

	if cfg.pipelineTimeout, err = envPositiveDuration("PIPELINE_TIMEOUT", defaultPipelineTimeout); err != nil {
		return err
	}

	if cfg.grpcReadBufferBytes, err = envInt("GRPC_READ_BUFFER_BYTES", 0); err != nil {
		return err
	}

	if cfg.grpcWriteBufferBytes, err = envInt("GRPC_WRITE_BUFFER_BYTES", 0); err != nil {
		return err
	}

	if cfg.grpcReadBufferBytes < 0 || cfg.grpcWriteBufferBytes < 0 {
		return fmt.Errorf("GRPC_READ_BUFFER_BYTES and GRPC_WRITE_BUFFER_BYTES must not be negative")
	}

	return nil
}

// loadJSONConfig reads the platform connector config.json shared with the
// DaemonSet. Numbers are decoded as json.Number so callers can coerce without
// caring how the file was rendered.
func loadJSONConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	result := map[string]any{}
	if err := dec.Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config %s: %w", path, err)
	}

	return result, nil
}

// cfgBool reads a toggle from a loadJSONConfig map, accepting both the chart's
// quoted form and a raw bool.
func cfgBool(m map[string]any, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return v == trueValue
	default:
		return false
	}
}

// cfgInt64 and cfgFloat64 coerce values out of a loadJSONConfig map, whose
// numbers are always json.Number (UseNumber).
func cfgInt64(m map[string]any, key string) (int64, error) {
	v, ok := m[key].(json.Number)
	if !ok {
		return 0, fmt.Errorf("config key %q missing or not a number (got %T)", key, m[key])
	}

	return v.Int64()
}

func cfgFloat64(m map[string]any, key string) (float64, error) {
	v, ok := m[key].(json.Number)
	if !ok {
		return 0, fmt.Errorf("config key %q missing or not a number (got %T)", key, m[key])
	}

	return v.Float64()
}
