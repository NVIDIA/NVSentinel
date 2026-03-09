// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

	"github.com/nvidia/nvsentinel/health-monitors/slurm-drain-monitor/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParser_Parse_singleCheck(t *testing.T) {
	patterns := []config.Pattern{
		{
			Name:              "slurm-healthcheck",
			Regex:             `^\[HC\]`,
			CheckName:         "SlurmHealthCheck",
			ComponentClass:    "NODE",
			RecommendedAction: "CONTACT_SUPPORT",
		},
	}
	p, err := New("; ", patterns)
	require.NoError(t, err)

	reasons := p.Parse("[HC] GPU ECC")
	require.Len(t, reasons, 1)
	assert.Equal(t, "slurm-healthcheck", reasons[0].PatternName)
	assert.Equal(t, "[HC] GPU ECC", reasons[0].Segment)
	assert.Equal(t, "SlurmHealthCheck", reasons[0].CheckName)
}

func TestParser_Parse_multiCheck(t *testing.T) {
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
		{Name: "notresp", Regex: `Not responding`, CheckName: "SlurmNotResponding", ComponentClass: "NODE"},
	}
	p, err := New("; ", patterns)
	require.NoError(t, err)

	reasons := p.Parse("[HC] GPU ECC; Not responding")
	require.Len(t, reasons, 2)
	assert.Equal(t, "[HC] GPU ECC", reasons[0].Segment)
	assert.Equal(t, "SlurmHC", reasons[0].CheckName)
	assert.Equal(t, "Not responding", reasons[1].Segment)
	assert.Equal(t, "SlurmNotResponding", reasons[1].CheckName)
}

func TestParser_Parse_emptyString(t *testing.T) {
	patterns := []config.Pattern{
		{Name: "any", Regex: `.*`, CheckName: "Any", ComponentClass: "NODE"},
	}
	p, err := New("; ", patterns)
	require.NoError(t, err)

	reasons := p.Parse("")
	assert.Nil(t, reasons)
	reasons = p.Parse("   ")
	assert.Nil(t, reasons)
}

func TestParser_Parse_operatorPrefixed_noMatch(t *testing.T) {
	// Parser does not filter by prefix; reconciler does. Parser just matches regex.
	// If we pass "slurm-operator: cordon" and have a pattern that matches "slurm-operator", we get a match.
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
	}
	p, err := New("; ", patterns)
	require.NoError(t, err)

	reasons := p.Parse("slurm-operator: cordon")
	assert.Len(t, reasons, 0)
}

func TestParser_Parse_noMatch(t *testing.T) {
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
	}
	p, err := New("; ", patterns)
	require.NoError(t, err)

	reasons := p.Parse("Some other reason")
	assert.Len(t, reasons, 0)
}

func TestParser_Parse_emptyDelimiter(t *testing.T) {
	patterns := []config.Pattern{
		{Name: "hc", Regex: `^\[HC\]`, CheckName: "SlurmHC", ComponentClass: "NODE"},
	}
	p, err := New("", patterns)
	require.NoError(t, err)

	reasons := p.Parse("[HC] GPU ECC")
	require.Len(t, reasons, 1)
	assert.Equal(t, "[HC] GPU ECC", reasons[0].Segment)
}

func TestParser_Parse_messageOverride(t *testing.T) {
	patterns := []config.Pattern{
		{
			Name:              "hc",
			Regex:             `^\[HC\]`,
			CheckName:         "SlurmHC",
			ComponentClass:    "NODE",
			Message:           "Health check reported issue",
		},
	}
	p, err := New("; ", patterns)
	require.NoError(t, err)

	reasons := p.Parse("[HC] GPU ECC")
	require.Len(t, reasons, 1)
	assert.Equal(t, "Health check reported issue", reasons[0].Message)
}

func TestNew_invalidRegex(t *testing.T) {
	patterns := []config.Pattern{
		{Name: "bad", Regex: `[invalid`, CheckName: "X", ComponentClass: "NODE"},
	}
	_, err := New("; ", patterns)
	require.Error(t, err)
}
