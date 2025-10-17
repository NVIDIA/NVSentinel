// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateParser(t *testing.T) {
	testCases := []struct {
		name        string
		config      ParserConfig
		expectError bool
		parserType  string
	}{
		{
			name: "Create CSV Parser (Sidecar Disabled)",
			config: ParserConfig{
				NodeName:            "test-node",
				XidAnalyserEndpoint: "",
				SidecarEnabled:      false,
			},
			expectError: false,
			parserType:  "*parser.CSVParser",
		},
		{
			name: "Create Sidecar Parser (Sidecar Enabled with Endpoint)",
			config: ParserConfig{
				NodeName:            "test-node",
				XidAnalyserEndpoint: "http://localhost:8080",
				SidecarEnabled:      true,
			},
			expectError: false,
			parserType:  "*parser.SidecarParser",
		},
		{
			name: "Error: Sidecar Enabled without Endpoint",
			config: ParserConfig{
				NodeName:            "test-node",
				XidAnalyserEndpoint: "",
				SidecarEnabled:      true,
			},
			expectError: true,
			parserType:  "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			parser, err := CreateParser(tc.config)

			if tc.expectError {
				assert.Error(t, err)
				assert.Nil(t, parser)
				assert.Contains(t, err.Error(), "XidAnalyserEndpoint is required")
			} else {
				require.NoError(t, err)
				require.NotNil(t, parser)
				assert.IsType(t, getParserTypeInstance(tc.parserType), parser)
			}
		})
	}
}

func getParserTypeInstance(typeName string) interface{} {
	switch typeName {
	case "*parser.CSVParser":
		return &CSVParser{}
	case "*parser.SidecarParser":
		return &SidecarParser{}
	default:
		return nil
	}
}
