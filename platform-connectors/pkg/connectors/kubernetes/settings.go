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

package kubernetes

import (
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/configfile"
)

// Settings are the connector's values from the shared config.json: the
// Kubernetes client rate limit and the node condition message limits.
type Settings struct {
	K8sConnectorConfig
	QPS   float32
	Burst int
}

// SettingsFromConfig reads Settings from a config.json map loaded by
// configfile.Load. Both roles of the binary build the connector from it. The
// two retry keys are optional: absent selects the connector's defaults
// (NewK8sConnector), and InitializeK8sConnector refuses a negative count or
// an over-long duration.
func SettingsFromConfig(raw map[string]any) (Settings, error) {
	qps, err := configfile.Float64(raw, "K8sConnectorQps")
	if err != nil {
		return Settings{}, err
	}

	burst, err := configfile.Int64(raw, "K8sConnectorBurst")
	if err != nil {
		return Settings{}, err
	}

	cfg := K8sConnectorConfig{}

	if cfg.MaxNodeConditionMessageLength, err = configfile.Int64(raw, "MaxNodeConditionMessageLength"); err != nil {
		return Settings{}, err
	}

	if cfg.CompactedHealthEventMsgLen, err = configfile.Int64(raw, "CompactedHealthEventMsgLen"); err != nil {
		return Settings{}, err
	}

	if _, ok := raw["K8sConnectorMaxRetries"]; ok {
		var maxRetries int64
		if maxRetries, err = configfile.Int64(raw, "K8sConnectorMaxRetries"); err != nil {
			return Settings{}, err
		}

		cfg.MaxRetries = int(maxRetries)
	}

	if _, ok := raw["K8sConnectorMaxRetryDuration"]; ok {
		if cfg.MaxRetryDuration, err = configfile.Duration(raw, "K8sConnectorMaxRetryDuration"); err != nil {
			return Settings{}, err
		}
	}

	return Settings{K8sConnectorConfig: cfg, QPS: float32(qps), Burst: int(burst)}, nil
}
