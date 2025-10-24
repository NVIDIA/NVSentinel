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

package xid

import (
	"errors"
	"testing"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseNVRMGPUMapLine(t *testing.T) {
	xidHandler := &XIDHandler{}

	testCases := []struct {
		name  string
		line  string
		pciId string
		gpuId string
	}{
		{
			name:  "Valid XID Error",
			line:  "NVRM: GPU at PCI:0000:00:08.0: GPU-123",
			pciId: "0000:00:08.0",
			gpuId: "GPU-123",
		},
		{
			name:  "Invalid XID Error",
			line:  "NVRM: Some other error message",
			pciId: "",
			gpuId: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pciId, gpuId := xidHandler.parseNVRMGPUMapLine(tc.line)
			assert.Equal(t, tc.pciId, pciId)
			assert.Equal(t, tc.gpuId, gpuId)
		})
	}
}

func TestNormalizePCI(t *testing.T) {
	xidHandler := &XIDHandler{}

	testCases := []struct {
		name          string
		pci           string
		normalizedPCI string
	}{
		{
			name:          "Valid PCI",
			pci:           "0000:00:08.0",
			normalizedPCI: "0000:00:08",
		},
		{
			name:          "PCI without dot",
			pci:           "0000:00:08",
			normalizedPCI: "0000:00:08",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			normalizedPCI := xidHandler.normalizePCI(tc.pci)
			assert.Equal(t, tc.normalizedPCI, normalizedPCI)
		})
	}
}

func TestDetermineFatality(t *testing.T) {
	xidHandler, err := NewXIDHandler("test-node",
		"test-agent", "test-component", "test-check", "http://localhost:8080")
	assert.Nil(t, err)

	testCases := []struct {
		name     string
		code     pb.RecommenedAction
		fatality bool
	}{
		{
			name:     "Fatal XID",
			code:     pb.RecommenedAction_CONTACT_SUPPORT,
			fatality: true,
		},
		{
			name:     "Non-Fatal XID",
			code:     pb.RecommenedAction_NONE,
			fatality: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fatality := xidHandler.determineFatality(tc.code)
			assert.Equal(t, tc.fatality, fatality)
		})
	}
}

// mockParser is a mock implementation of parser.Parser for testing
type mockParser struct {
	parseFunc func(message string) (*parser.Response, error)
}

func (m *mockParser) Parse(message string) (*parser.Response, error) {
	if m.parseFunc != nil {
		return m.parseFunc(message)
	}
	return nil, nil
}

func TestProcessLine(t *testing.T) {
	testCases := []struct {
		name          string
		message       string
		setupHandler  func() *XIDHandler
		expectEvent   bool
		expectError   bool
		validateEvent func(t *testing.T, events *pb.HealthEvents)
	}{
		{
			name:    "NVRM GPU Map Line",
			message: "NVRM: GPU at PCI:0000:00:08.0: GPU-12345678-1234-1234-1234-123456789012",
			setupHandler: func() *XIDHandler {
				return &XIDHandler{
					nodeName:              "test-node",
					defaultAgentName:      "test-agent",
					defaultComponentClass: "GPU",
					checkName:             "xid-check",
					pciToGPUUUID:          make(map[string]string),
					parser: &mockParser{
						parseFunc: func(msg string) (*parser.Response, error) {
							return nil, nil
						},
					},
				}
			},
			expectEvent: false,
			expectError: false,
		},
		{
			name:    "Valid XID Message",
			message: "NVRM: Xid (PCI:0000:00:08.0): 79, pid=12345, name=test-process",
			setupHandler: func() *XIDHandler {
				return &XIDHandler{
					nodeName:              "test-node",
					defaultAgentName:      "test-agent",
					defaultComponentClass: "GPU",
					checkName:             "xid-check",
					pciToGPUUUID:          make(map[string]string),
					parser: &mockParser{
						parseFunc: func(msg string) (*parser.Response, error) {
							return &parser.Response{
								Success: true,
								Result: parser.XIDDetails{
									DecodedXIDStr: "Xid 79",
									PCIE:          "0000:00:08.0",
									Mnemonic:      "GPU has fallen off the bus",
									Resolution:    "CONTACT_SUPPORT",
									Number:        79,
								},
							}, nil
						},
					},
				}
			},
			expectEvent: true,
			expectError: false,
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)
				event := events.Events[0]
				assert.Equal(t, "test-agent", event.Agent)
				assert.Equal(t, "xid-check", event.CheckName)
				assert.Equal(t, "GPU", event.ComponentClass)
				assert.False(t, event.IsHealthy)
				assert.True(t, event.IsFatal)
				assert.Equal(t, pb.RecommenedAction_CONTACT_SUPPORT, event.RecommendedAction)
				assert.Contains(t, event.Message, "GPU has fallen off the bus")
				assert.Contains(t, event.ErrorCode, "Xid 79")
				require.Len(t, event.EntitiesImpacted, 1)
				assert.Equal(t, "PCI", event.EntitiesImpacted[0].EntityType)
				assert.Equal(t, "0000:00:08.0", event.EntitiesImpacted[0].EntityValue)
			},
		},
		{
			name:    "Valid XID with GPU UUID",
			message: "NVRM: Xid (PCI:0000:00:08.0): 79, pid=12345, name=test-process",
			setupHandler: func() *XIDHandler {
				handler := &XIDHandler{
					nodeName:              "test-node",
					defaultAgentName:      "test-agent",
					defaultComponentClass: "GPU",
					checkName:             "xid-check",
					pciToGPUUUID:          make(map[string]string),
					parser: &mockParser{
						parseFunc: func(msg string) (*parser.Response, error) {
							return &parser.Response{
								Success: true,
								Result: parser.XIDDetails{
									DecodedXIDStr: "Xid 79",
									PCIE:          "0000:00:08.0",
									Mnemonic:      "GPU has fallen off the bus",
									Resolution:    "CONTACT_SUPPORT",
									Number:        79,
								},
							}, nil
						},
					},
				}
				handler.pciToGPUUUID["0000:00:08"] = "GPU-12345678-1234-1234-1234-123456789012"
				return handler
			},
			expectEvent: true,
			expectError: false,
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)
				event := events.Events[0]
				require.Len(t, event.EntitiesImpacted, 2)
				assert.Equal(t, "PCI", event.EntitiesImpacted[0].EntityType)
				assert.Equal(t, "GPU", event.EntitiesImpacted[1].EntityType)
				assert.Equal(t, "GPU-12345678-1234-1234-1234-123456789012", event.EntitiesImpacted[1].EntityValue)
			},
		},
		{
			name:    "Parser Returns Error",
			message: "Some random message",
			setupHandler: func() *XIDHandler {
				return &XIDHandler{
					nodeName:              "test-node",
					defaultAgentName:      "test-agent",
					defaultComponentClass: "GPU",
					checkName:             "xid-check",
					pciToGPUUUID:          make(map[string]string),
					parser: &mockParser{
						parseFunc: func(msg string) (*parser.Response, error) {
							return nil, errors.New("parse error")
						},
					},
				}
			},
			expectEvent: false,
			expectError: false,
		},
		{
			name:    "Parser Returns Nil Response",
			message: "Some random message",
			setupHandler: func() *XIDHandler {
				return &XIDHandler{
					nodeName:              "test-node",
					defaultAgentName:      "test-agent",
					defaultComponentClass: "GPU",
					checkName:             "xid-check",
					pciToGPUUUID:          make(map[string]string),
					parser: &mockParser{
						parseFunc: func(msg string) (*parser.Response, error) {
							return nil, nil
						},
					},
				}
			},
			expectEvent: false,
			expectError: false,
		},
		{
			name:    "Parser Returns Success=false",
			message: "Some random message",
			setupHandler: func() *XIDHandler {
				return &XIDHandler{
					nodeName:              "test-node",
					defaultAgentName:      "test-agent",
					defaultComponentClass: "GPU",
					checkName:             "xid-check",
					pciToGPUUUID:          make(map[string]string),
					parser: &mockParser{
						parseFunc: func(msg string) (*parser.Response, error) {
							return &parser.Response{
								Success: false,
							}, nil
						},
					},
				}
			},
			expectEvent: false,
			expectError: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler := tc.setupHandler()
			events, err := handler.ProcessLine(tc.message)

			if tc.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			if tc.expectEvent {
				require.NotNil(t, events)
				if tc.validateEvent != nil {
					tc.validateEvent(t, events)
				}
			} else {
				assert.Nil(t, events)
			}
		})
	}
}

func TestCreateHealthEventFromResponse(t *testing.T) {
	handler := &XIDHandler{
		nodeName:              "test-node",
		defaultAgentName:      "test-agent",
		defaultComponentClass: "GPU",
		checkName:             "xid-check",
		pciToGPUUUID:          make(map[string]string),
	}

	testCases := []struct {
		name          string
		xidResp       *parser.Response
		message       string
		setupHandler  func()
		validateEvent func(t *testing.T, events *pb.HealthEvents)
	}{
		{
			name: "Basic XID Event",
			xidResp: &parser.Response{
				Success: true,
				Result: parser.XIDDetails{
					DecodedXIDStr: "Xid 79",
					PCIE:          "0000:00:08.0",
					Mnemonic:      "GPU has fallen off the bus",
					Resolution:    "CONTACT_SUPPORT",
					Number:        79,
				},
			},
			message:      "Test XID message",
			setupHandler: func() {},
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				assert.Equal(t, uint32(1), events.Version)
				require.Len(t, events.Events, 1)
				event := events.Events[0]
				assert.Equal(t, uint32(1), event.Version)
				assert.Equal(t, "test-agent", event.Agent)
				assert.Equal(t, "xid-check", event.CheckName)
				assert.Equal(t, "GPU", event.ComponentClass)
				assert.False(t, event.IsHealthy)
				assert.True(t, event.IsFatal)
				assert.Equal(t, pb.RecommenedAction_CONTACT_SUPPORT, event.RecommendedAction)
				assert.Contains(t, event.Message, "GPU has fallen off the bus")
				assert.Contains(t, event.Message, "CONTACT_SUPPORT")
				assert.Contains(t, event.ErrorCode, "Xid 79")
				assert.Equal(t, "test-node", event.NodeName)
				assert.NotNil(t, event.GeneratedTimestamp)
				assert.Equal(t, "Test XID message", event.Metadata["JOURNAL_MESSAGE"])
			},
		},
		{
			name: "XID Event with GPU UUID",
			xidResp: &parser.Response{
				Success: true,
				Result: parser.XIDDetails{
					DecodedXIDStr: "Xid 13",
					PCIE:          "0000:00:09.0",
					Mnemonic:      "Graphics Engine Exception",
					Resolution:    "IGNORE",
					Number:        13,
				},
			},
			message: "Test XID message",
			setupHandler: func() {
				handler.pciToGPUUUID["0000:00:09"] = "GPU-ABCDEF12-3456-7890-ABCD-EF1234567890"
			},
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)
				event := events.Events[0]
				require.Len(t, event.EntitiesImpacted, 2)
				assert.Equal(t, "PCI", event.EntitiesImpacted[0].EntityType)
				assert.Equal(t, "0000:00:09.0", event.EntitiesImpacted[0].EntityValue)
				assert.Equal(t, "GPU", event.EntitiesImpacted[1].EntityType)
				assert.Equal(t, "GPU-ABCDEF12-3456-7890-ABCD-EF1234567890", event.EntitiesImpacted[1].EntityValue)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler.pciToGPUUUID = make(map[string]string)
			tc.setupHandler()

			events := handler.createHealthEventFromResponse(tc.xidResp, tc.message)

			if tc.validateEvent != nil {
				tc.validateEvent(t, events)
			}
		})
	}
}

func TestNewXIDHandler(t *testing.T) {
	testCases := []struct {
		name                string
		nodeName            string
		agentName           string
		componentClass      string
		checkName           string
		xidAnalyserEndpoint string
		expectError         bool
	}{
		{
			name:                "With XID Analyser Endpoint",
			nodeName:            "test-node",
			agentName:           "test-agent",
			componentClass:      "GPU",
			checkName:           "xid-check",
			xidAnalyserEndpoint: "http://localhost:8080",
			expectError:         false,
		},
		{
			name:                "Without XID Analyser Endpoint",
			nodeName:            "test-node",
			agentName:           "test-agent",
			componentClass:      "GPU",
			checkName:           "xid-check",
			xidAnalyserEndpoint: "",
			expectError:         false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewXIDHandler(tc.nodeName, tc.agentName, tc.componentClass, tc.checkName, tc.xidAnalyserEndpoint)

			if tc.expectError {
				assert.Error(t, err)
				assert.Nil(t, handler)
			} else {
				assert.NoError(t, err)
				require.NotNil(t, handler)
				assert.Equal(t, tc.nodeName, handler.nodeName)
				assert.Equal(t, tc.agentName, handler.defaultAgentName)
				assert.Equal(t, tc.componentClass, handler.defaultComponentClass)
				assert.Equal(t, tc.checkName, handler.checkName)
				assert.NotNil(t, handler.pciToGPUUUID)
				assert.NotNil(t, handler.parser)
			}
		})
	}
}
