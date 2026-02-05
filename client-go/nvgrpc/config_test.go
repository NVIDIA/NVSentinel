// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
package nvgrpc

import (
	"testing"
)

func TestConfig_Default_TargetPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		argTarget  string
		envTarget  string
		wantTarget string
	}{
		{
			name:       "Explicit target is preserved",
			argTarget:  "unix:///arg.sock",
			envTarget:  "unix:///env.sock",
			wantTarget: "unix:///arg.sock",
		},
		{
			name:       "Env var used when target is empty",
			argTarget:  "",
			envTarget:  "unix:///env.sock",
			wantTarget: "unix:///env.sock",
		},
		{
			name:       "Default used when both are empty",
			argTarget:  "",
			envTarget:  "",
			wantTarget: DefaultNvidiaDeviceAPISocket,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(NvidiaDeviceAPITargetEnvVar, tt.envTarget)

			cfg := &Config{Target: tt.argTarget}
			cfg.Default()

			if cfg.Target != tt.wantTarget {
				t.Errorf("Target = %q, want %q", cfg.Target, tt.wantTarget)
			}
		})
	}
}

func TestConfig_Default_UserAgent(t *testing.T) {
	t.Run("Populates default UserAgent if empty", func(t *testing.T) {
		cfg := &Config{}
		cfg.Default()

		if cfg.UserAgent == "" {
			t.Error("UserAgent should have been populated with version-based default")
		}
	})

	t.Run("Preserves custom UserAgent", func(t *testing.T) {
		custom := "my-custom-agent/1.0"
		cfg := &Config{UserAgent: custom}
		cfg.Default()

		if cfg.UserAgent != custom {
			t.Errorf("UserAgent = %q, want %q", cfg.UserAgent, custom)
		}
	})
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "Valid config",
			cfg: Config{
				Target:    "unix:///var/run/test.sock",
				UserAgent: "test/1.0",
			},
			wantErr: false,
		},
		{
			name: "Missing target",
			cfg: Config{
				UserAgent: "test/1.0",
			},
			wantErr: true,
		},
		{
			name: "Missing user agent",
			cfg: Config{
				Target: "unix:///var/run/test.sock",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
