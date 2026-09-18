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

package grpcsink

import (
	"errors"

	"github.com/nvidia/nvsentinel/platform-connectors/pkg/configfile"
)

// Settings are the connector's values from the shared config.json.
type Settings struct {
	// Target is the address the connector dials.
	Target string
	// TokenPath is the ServiceAccount token file attached to every RPC;
	// empty sends none.
	TokenPath string
}

// SettingsFromConfig reads Settings from a config.json map loaded by
// configfile.Load. Both roles of the binary build the connector from it. The
// retry count is not here: only the node-local role's queue loop retries, so
// it reads GRPCSinkConnectorMaxRetries itself.
func SettingsFromConfig(raw map[string]any) (Settings, error) {
	target, err := configfile.String(raw, "GRPCSinkTarget")
	if err != nil {
		return Settings{}, err
	}

	if target == "" {
		return Settings{}, errors.New("GRPCSinkTarget not configured or empty")
	}

	tokenPath, err := configfile.String(raw, "GRPCSinkTokenPath")
	if err != nil {
		return Settings{}, err
	}

	return Settings{Target: target, TokenPath: tokenPath}, nil
}
