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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/configfile"
)

const (
	testPublisher = "system:serviceaccount:nvsentinel:gpu-health-monitor"
	testCrossNode = "system:serviceaccount:nvsentinel:health-events-analyzer"
)

// configFromJSON decodes config.json text the way configfile.Load does
// (numbers as json.Number).
func configFromJSON(t *testing.T, raw string) map[string]any {
	t.Helper()

	m, err := configfile.Decode([]byte(raw))
	require.NoError(t, err)

	return m
}

const deploymentConfig = `{"deployment": {
	"TokenReviewQps": 1000, "TokenReviewBurst": 2000, "TokenCacheSize": 500000,
	"ConditionUpdateTimeout": "10s", "MaxConnectionAge": "10m", "MaxConnectionIdle": "5m",
	"GrpcReadBufferBytes": 8192, "GrpcWriteBufferBytes": 4096
}}`

func TestSettingsFromConfig(t *testing.T) {
	s, err := settingsFromConfig(configFromJSON(t, deploymentConfig))
	require.NoError(t, err)
	require.Equal(t, settings{
		tokenReviewQPS:         1000,
		tokenReviewBurst:       2000,
		tokenCacheSize:         500000,
		conditionUpdateTimeout: 10 * time.Second,
		maxConnAge:             10 * time.Minute,
		maxConnIdle:            5 * time.Minute,
		grpcReadBufferBytes:    8192,
		grpcWriteBufferBytes:   4096,
	}, s)
}

func TestSettingsFromConfig_Rejections(t *testing.T) {
	cases := []struct {
		name, raw, wantErr string
	}{
		{"no deployment object", `{"K8sConnectorQps": 5}`, `"deployment" missing`},
		{"deployment not an object", `{"deployment": 3}`, "not an object"},
		{"missing key", `{"deployment": {"TokenReviewQps": 1000}}`, `"TokenReviewBurst" missing`},
		{"zero", strings.Replace(deploymentConfig, `"TokenCacheSize": 500000`, `"TokenCacheSize": 0`, 1), "TokenCacheSize must be positive"},
		{"negative", strings.Replace(deploymentConfig, `"GrpcReadBufferBytes": 8192`, `"GrpcReadBufferBytes": -1`, 1), "GrpcReadBufferBytes must be positive"},
		{"fraction", strings.Replace(deploymentConfig, `"TokenReviewQps": 1000`, `"TokenReviewQps": 1.5`, 1), "TokenReviewQps"},
		{"malformed duration", strings.Replace(deploymentConfig, `"MaxConnectionAge": "10m"`, `"MaxConnectionAge": "soon"`, 1), "MaxConnectionAge"},
		{"zero duration", strings.Replace(deploymentConfig, `"ConditionUpdateTimeout": "10s"`, `"ConditionUpdateTimeout": "0s"`, 1), "ConditionUpdateTimeout must be positive"},
		{"duration as number", strings.Replace(deploymentConfig, `"MaxConnectionIdle": "5m"`, `"MaxConnectionIdle": 300`, 1), "not a duration string"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := settingsFromConfig(configFromJSON(t, tc.raw))
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestNew_RequiresTLSUnlessTheDevelopmentModeIsNamed(t *testing.T) {
	_, err := New(Options{ListenAddr: ":50051"})
	require.ErrorContains(t, err, "-tls-cert-dir is required")

	_, err = New(Options{ListenAddr: ":50051", InsecureDevelopmentMode: true})
	require.NoError(t, err)

	_, err = New(Options{ListenAddr: ":50051", TLSCertDir: "/etc/tls"})
	require.NoError(t, err)
}
