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

func TestValidate_NoFilterExpression_IsValid(t *testing.T) {
	require.NoError(t, validConfig("").Validate())
}

func TestValidate_ValidFilterExpression_IsValid(t *testing.T) {
	require.NoError(t, validConfig(`event.recommendedAction != 'NONE'`).Validate())
}

func TestValidate_MalformedFilterExpression_FailsAtStartup(t *testing.T) {
	// The point of validating here: a filter can legitimately drop almost every event, so
	// a typo discovered at runtime looks identical to "nothing is happening".
	err := validConfig(`event.recommendedAction !=`).Validate()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "filter expression is invalid")
}

func TestValidate_NonBooleanFilterExpression_FailsAtStartup(t *testing.T) {
	err := validConfig(`1 + 1`).Validate()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "filter expression is invalid")
}

func TestCompile_BlankExpression_YieldsNoProgram(t *testing.T) {
	for _, expression := range []string{"", "   ", "\t\n"} {
		filter := FilterConfig{Expression: expression}

		program, err := filter.Compile()

		require.NoError(t, err)
		assert.Nil(t, program, "a blank expression means export everything")
	}
}

func TestCompile_ValidExpression_YieldsAProgram(t *testing.T) {
	filter := FilterConfig{Expression: `!('45' in event.errorCode)`}

	program, err := filter.Compile()

	require.NoError(t, err)
	assert.NotNil(t, program)
}
