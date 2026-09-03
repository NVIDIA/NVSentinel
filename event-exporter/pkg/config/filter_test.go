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

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validConfig satisfies every non-filter requirement so Validate isolates the filter.
func validConfig(expression string) *Config {
	cfg := &Config{}
	cfg.Exporter.Sink.Endpoint = "https://sink.example.com"
	cfg.Exporter.OIDC.TokenURL = "https://idp.example.com/token"
	cfg.Exporter.OIDC.ClientID = "client"
	cfg.Exporter.OIDC.Scope = "scope"
	cfg.Exporter.ResumeToken.Collection = "resume_tokens"
	cfg.Exporter.ResumeToken.Database = "nvsentinel"
	cfg.Exporter.Filter.Expression = expression

	return cfg
}

// TestFilterConfig_Compile covers what this package owns: turning the configured string into
// a program, and treating a blank one as "export everything". The CEL rules themselves
// (which expressions are valid, why a bare field read is rejected) are commons/pkg/celevent's
// and are tested there, so only one invalid case appears here, to prove the error propagates.
func TestFilterConfig_Compile(t *testing.T) {
	tests := []struct {
		name           string
		expression     string
		wantProgram    bool
		wantErrContains string
	}{
		{name: "empty means export everything", expression: ""},
		{name: "spaces mean export everything", expression: "   "},
		{name: "tabs and newlines mean export everything", expression: "\t\n"},
		{
			name:        "a valid expression compiles",
			expression:  `!('45' in event.errorCode)`,
			wantProgram: true,
		},
		{
			name:            "an invalid expression propagates the error with the expression in it",
			expression:      `event.recommendedAction !=`,
			wantErrContains: `event.recommendedAction !=`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filter := FilterConfig{Expression: tc.expression}

			program, err := filter.Compile()

			if tc.wantErrContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErrContains)

				return
			}

			require.NoError(t, err)

			if tc.wantProgram {
				assert.NotNil(t, program)
			} else {
				assert.Nil(t, program, "a blank expression must not compile to a program")
			}
		})
	}
}

// TestConfig_Validate_FilterExpression checks the wiring rather than the CEL rules: a bad
// expression has to stop startup, because a filter can legitimately drop almost every event,
// so a typo discovered at runtime looks identical to "nothing is happening".
func TestConfig_Validate_FilterExpression(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		wantErr    bool
	}{
		{name: "no expression is valid", expression: ""},
		{name: "valid expression is valid", expression: `event.recommendedAction != 'NONE'`},
		{name: "malformed expression fails startup", expression: `event.recommendedAction !=`, wantErr: true},
		{name: "non-boolean expression fails startup", expression: `1 + 1`, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validConfig(tc.expression).Validate()

			if !tc.wantErr {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), "filter expression is invalid")
		})
	}
}
