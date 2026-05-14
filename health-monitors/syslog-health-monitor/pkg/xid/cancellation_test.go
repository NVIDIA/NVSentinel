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

package xid

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/cancellation"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid/parser"
)

// xid162Mock returns a parser response that mimics syslog-monitor's view of an
// XID 162 line: success=true with DecodedXIDStr "162" and PCI 0000:00:08.0.
func xid162Mock() *parser.Response {
	return &parser.Response{
		Success: true,
		Result: parser.XIDDetails{
			DecodedXIDStr: "162",
			PCIE:          "0000:00:08.0",
			Mnemonic:      "Recovery",
			Resolution:    "NONE",
			Number:        162,
		},
	}
}

func newHandlerWithMetadata(t *testing.T) *XIDHandler {
	t.Helper()

	tmpDir := t.TempDir()
	metadataFile := filepath.Join(tmpDir, "gpu_metadata.json")
	require.NoError(t, os.WriteFile(metadataFile, []byte(testMetadataJSON), 0o600))

	h, err := NewXIDHandler("test-node", "test-agent", "GPU", "SysLogsXIDError",
		"", metadataFile, pb.ProcessingStrategy_EXECUTE_REMEDIATION)
	require.NoError(t, err)

	return h
}

func TestProcessLine_NoCancellationsConfiguredEmitsOnlySource(t *testing.T) {
	h := newHandlerWithMetadata(t)
	h.parser = &mockParser{parseFunc: func(string) (*parser.Response, error) { return xid162Mock(), nil }}

	events, err := h.ProcessLine("ignored")
	require.NoError(t, err)
	require.NotNil(t, events)
	require.Len(t, events.Events, 1)
	assert.Equal(t, []string{"162"}, events.Events[0].ErrorCode)
	assert.False(t, events.Events[0].IsHealthy)
}

func TestProcessLine_EmitsCancellationEventForMatchingRule(t *testing.T) {
	h := newHandlerWithMetadata(t)
	h.parser = &mockParser{parseFunc: func(string) (*parser.Response, error) { return xid162Mock(), nil }}

	h.SetCancellationResolver(cancellation.NewResolver(&cancellation.CheckCancellations{
		Name:    "SysLogsXIDError",
		Enabled: true,
		Rules: []cancellation.CancellationRule{
			{OnErrorCode: "162", CancelErrorCodes: []string{"163"}},
		},
	}))

	events, err := h.ProcessLine("ignored")
	require.NoError(t, err)
	require.NotNil(t, events)
	require.Len(t, events.Events, 2)

	source := events.Events[0]
	assert.Equal(t, []string{"162"}, source.ErrorCode)
	assert.False(t, source.IsHealthy)
	assert.False(t, source.IsFatal) // NONE → non-fatal
	assert.Empty(t, source.Metadata, "source event must not be mutated by cancellation logic")

	cancel := events.Events[1]
	assert.Equal(t, []string{"163"}, cancel.ErrorCode)
	assert.True(t, cancel.IsHealthy)
	assert.False(t, cancel.IsFatal)
	assert.Equal(t, pb.RecommendedAction_NONE, cancel.RecommendedAction)
	assert.Equal(t, "Cancelled by SysLogsXIDError error code 162", cancel.Message)
	assert.Equal(t, "162", cancel.Metadata["nvsentinel.nvidia.com/cancel-source-error-code"])
	// Identifying fields propagate so downstream entity-and-error-code matching
	// can clear the prior fault.
	assert.Equal(t, source.Agent, cancel.Agent)
	assert.Equal(t, source.CheckName, cancel.CheckName)
	assert.Equal(t, source.ComponentClass, cancel.ComponentClass)
	assert.Equal(t, source.NodeName, cancel.NodeName)
	assert.Equal(t, source.Version, cancel.Version)
	assert.Equal(t, source.ProcessingStrategy, cancel.ProcessingStrategy)
	require.Equal(t, len(source.EntitiesImpacted), len(cancel.EntitiesImpacted))

	for i, srcEnt := range source.EntitiesImpacted {
		assert.Equal(t, srcEnt.EntityType, cancel.EntitiesImpacted[i].EntityType)
		assert.Equal(t, srcEnt.EntityValue, cancel.EntitiesImpacted[i].EntityValue)
	}
}

func TestProcessLine_FansOutToMultipleCancellationTargets(t *testing.T) {
	h := newHandlerWithMetadata(t)
	h.parser = &mockParser{parseFunc: func(string) (*parser.Response, error) { return xid162Mock(), nil }}

	h.SetCancellationResolver(cancellation.NewResolver(&cancellation.CheckCancellations{
		Name:    "SysLogsXIDError",
		Enabled: true,
		Rules: []cancellation.CancellationRule{
			{OnErrorCode: "162", CancelErrorCodes: []string{"163", "164", "165"}},
		},
	}))

	events, err := h.ProcessLine("ignored")
	require.NoError(t, err)
	require.Len(t, events.Events, 4) // source + 3 cancellations

	gotTargets := []string{
		events.Events[1].ErrorCode[0],
		events.Events[2].ErrorCode[0],
		events.Events[3].ErrorCode[0],
	}
	assert.Equal(t, []string{"163", "164", "165"}, gotTargets)
}

func TestProcessLine_NoMatchingRuleEmitsOnlySource(t *testing.T) {
	h := newHandlerWithMetadata(t)
	h.parser = &mockParser{parseFunc: func(string) (*parser.Response, error) { return xid162Mock(), nil }}

	h.SetCancellationResolver(cancellation.NewResolver(&cancellation.CheckCancellations{
		Name:    "SysLogsXIDError",
		Enabled: true,
		Rules: []cancellation.CancellationRule{
			// Rule for a different source code; XID 162 must not be cancelled.
			{OnErrorCode: "999", CancelErrorCodes: []string{"998"}},
		},
	}))

	events, err := h.ProcessLine("ignored")
	require.NoError(t, err)
	require.Len(t, events.Events, 1)
	assert.Equal(t, []string{"162"}, events.Events[0].ErrorCode)
}

// MutatingTheCancellationEvent must not affect the source — the synthetic event
// owns its own EntitiesImpacted slice.
func TestProcessLine_CancellationEntityCloneIsIndependent(t *testing.T) {
	h := newHandlerWithMetadata(t)
	h.parser = &mockParser{parseFunc: func(string) (*parser.Response, error) { return xid162Mock(), nil }}

	h.SetCancellationResolver(cancellation.NewResolver(&cancellation.CheckCancellations{
		Name:    "SysLogsXIDError",
		Enabled: true,
		Rules: []cancellation.CancellationRule{
			{OnErrorCode: "162", CancelErrorCodes: []string{"163"}},
		},
	}))

	events, err := h.ProcessLine("ignored")
	require.NoError(t, err)
	require.Len(t, events.Events, 2)

	cancel := events.Events[1]
	cancel.EntitiesImpacted[0].EntityValue = "MUTATED"

	source := events.Events[0]
	assert.NotEqual(t, "MUTATED", source.EntitiesImpacted[0].EntityValue)
}
