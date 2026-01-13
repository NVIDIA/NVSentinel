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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid/parser"
)

const (
	testMetadataJSON = `
{
  "version": "1.0",
  "timestamp": "2025-12-10T18:12:55Z",
  "node_name": "test-node",
  "chassis_serial": null,
  "gpus": [
    {
      "gpu_id": 0,
      "uuid": "GPU-123",
      "pci_address": "0000:00:08.0",
      "serial_number": "1655322020697",
      "device_name": "NVIDIA H100 80GB HBM3",
      "nvlinks": [
        {
          "link_id": 0,
          "remote_pci_address": "ffff:ff:ff.0",
          "remote_link_id": 0
        },
        {
          "link_id": 1,
          "remote_pci_address": "ffff:ff:ff.0",
          "remote_link_id": 0
        }
      ]
    }
  ]
}`
	testMetadataMissingPCIJSON = `
{
  "version": "1.0",
  "timestamp": "2025-12-10T18:12:55Z",
  "node_name": "test-node",
  "chassis_serial": null,
  "gpus": [
    {
      "gpu_id": 0,
      "uuid": "GPU-123",
      "serial_number": "1655322020697",
      "device_name": "NVIDIA H100 80GB HBM3",
      "nvlinks": [
        {
          "link_id": 0,
          "remote_pci_address": "ffff:ff:ff.0",
          "remote_link_id": 0
        },
        {
          "link_id": 1,
          "remote_pci_address": "ffff:ff:ff.0",
          "remote_link_id": 0
        }
      ]
    }
  ]
}`
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

func TestParseGPUResetLine(t *testing.T) {
	xidHandler := &XIDHandler{}

	testCases := []struct {
		name  string
		line  string
		gpuId string
	}{
		{
			name:  "Valid GPU Reset Line",
			line:  "GPU reset executed: GPU-123",
			gpuId: "GPU-123",
		},
		{
			name:  "Invalid GPU Reset Line",
			line:  "GPU reset executed:",
			gpuId: "",
		},
		{
			name:  "XID Log Line GPU",
			line:  "NVRM: GPU at PCI:0000:00:08.0: GPU-123",
			gpuId: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gpuId := xidHandler.parseGPUResetLine(tc.line)
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
		"test-agent", "test-component", "test-check", "http://localhost:8080", "/tmp/metadata.json")
	assert.Nil(t, err)

	testCases := []struct {
		name     string
		code     pb.RecommendedAction
		fatality bool
	}{
		{
			name:     "Fatal XID",
			code:     pb.RecommendedAction_CONTACT_SUPPORT,
			fatality: true,
		},
		{
			name:     "Non-Fatal XID",
			code:     pb.RecommendedAction_NONE,
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
	tmpDir := t.TempDir()
	metadataFile := filepath.Join(tmpDir, "gpu_metadata.json")
	require.NoError(t, os.WriteFile(metadataFile, []byte(testMetadataJSON), 0600))
	metadataFileMissingPCI := filepath.Join(tmpDir, "gpu_metadata_missing_pci.json")
	require.NoError(t, os.WriteFile(metadataFileMissingPCI, []byte(testMetadataMissingPCIJSON), 0600))

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
				h, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", "/tmp/metadata.json")
				h.parser = &mockParser{
					parseFunc: func(msg string) (*parser.Response, error) {
						return nil, nil
					},
				}
				return h
			},
			expectEvent: false,
			expectError: false,
		},
		{
			name:    "Valid XID Message",
			message: "NVRM: Xid (PCI:0000:00:08.0): 79, pid=12345, name=test-process",
			setupHandler: func() *XIDHandler {
				h, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", "/tmp/metadata.json")
				h.parser = &mockParser{
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
				}
				return h
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
				assert.Equal(t, pb.RecommendedAction_CONTACT_SUPPORT, event.RecommendedAction)
				assert.Equal(t, "NVRM: Xid (PCI:0000:00:08.0): 79, pid=12345, name=test-process", event.Message)
				require.Len(t, event.ErrorCode, 1)
				assert.Equal(t, "Xid 79", event.ErrorCode[0])
				require.Len(t, event.EntitiesImpacted, 1)
				assert.Equal(t, "PCI", event.EntitiesImpacted[0].EntityType)
				assert.Equal(t, "0000:00:08", event.EntitiesImpacted[0].EntityValue)
				// Issue #197: Message field stores full journal, no Metadata duplication
				assert.Empty(t, event.Metadata)
			},
		},
		{
			name:    "Valid GPU Reset Message",
			message: "GPU reset executed: GPU-123",
			setupHandler: func() *XIDHandler {
				h, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", metadataFile)
				return h
			},
			expectEvent: true,
			expectError: false,
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)
				event := events.Events[0]
				expectedEvent := &pb.HealthEvent{
					Version:        1,
					Agent:          "test-agent",
					CheckName:      "xid-check",
					ComponentClass: "GPU",
					EntitiesImpacted: []*pb.Entity{
						{
							EntityType:  "PCI",
							EntityValue: "0000:00:08",
						},
						{
							EntityType:  "GPU_UUID",
							EntityValue: "GPU-123",
						},
					},
					Message:           healthyHealthEventMessage,
					IsFatal:           false,
					IsHealthy:         true,
					NodeName:          "test-node",
					RecommendedAction: pb.RecommendedAction_NONE,
				}
				assert.NotNil(t, event.GeneratedTimestamp)
				event.GeneratedTimestamp = nil
				assert.Equal(t, expectedEvent, event)
			},
		},
		{
			name:    "Valid GPU Reset Message with Metadata Collector not Initialized",
			message: "GPU reset executed: GPU-123",
			setupHandler: func() *XIDHandler {
				h, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", "/tmp/metadata.json")
				return h
			},
			expectEvent: false,
			expectError: true,
		},
		{
			name:    "Valid GPU Reset Message with Metadata Collector missing GPU UUID",
			message: "GPU reset executed: GPU-456",
			setupHandler: func() *XIDHandler {
				h, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", metadataFile)
				return h
			},
			expectEvent: false,
			expectError: true,
		},
		{
			name:    "Valid GPU Reset Message with Metadata Collector not containing PCI",
			message: "GPU reset executed: GPU-123",
			setupHandler: func() *XIDHandler {
				h, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", metadataFileMissingPCI)
				return h
			},
			expectEvent: false,
			expectError: true,
		},
		{
			name:    "Valid XID with GPU UUID from NVRM: RESET_GPU overridden to RESTART_VM",
			message: "NVRM: Xid (PCI:0000:00:08.0): 79, pid=12345, name=test-process",
			setupHandler: func() *XIDHandler {
				h, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", "/tmp/metadata.json")
				h.parser = &mockParser{
					parseFunc: func(msg string) (*parser.Response, error) {
						return &parser.Response{
							Success: true,
							Result: parser.XIDDetails{
								DecodedXIDStr: "Xid 79",
								PCIE:          "0000:00:08.0",
								Mnemonic:      "GPU has fallen off the bus",
								Resolution:    "RESET_GPU",
								Number:        79,
							},
						}, nil
					},
				}
				h.pciToGPUUUID["0000:00:08"] = "GPU-12345678-1234-1234-1234-123456789012"
				return h
			},
			expectEvent: true,
			expectError: false,
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)
				event := events.Events[0]
				require.Len(t, event.EntitiesImpacted, 2)
				assert.Equal(t, "PCI", event.EntitiesImpacted[0].EntityType)
				assert.Equal(t, "GPU_UUID", event.EntitiesImpacted[1].EntityType)
				assert.Equal(t, "GPU-12345678-1234-1234-1234-123456789012", event.EntitiesImpacted[1].EntityValue)
				assert.Equal(t, "NVRM: Xid (PCI:0000:00:08.0): 79, pid=12345, name=test-process", event.Message)
				assert.Equal(t, pb.RecommendedAction_RESTART_VM, event.RecommendedAction)
				assert.Empty(t, event.Metadata)
			},
		},
		{
			name:    "Valid XID with GPU UUID from Metadata Collector: RESET_GPU kept",
			message: "NVRM: Xid (PCI:0000:00:08.0): 79, pid=12345, name=test-process",
			setupHandler: func() *XIDHandler {
				h, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", metadataFile)
				h.parser = &mockParser{
					parseFunc: func(msg string) (*parser.Response, error) {
						return &parser.Response{
							Success: true,
							Result: parser.XIDDetails{
								DecodedXIDStr: "Xid 79",
								PCIE:          "0000:00:08.0",
								Mnemonic:      "GPU has fallen off the bus",
								Resolution:    "RESET_GPU",
								Number:        79,
							},
						}, nil
					},
				}
				return h
			},
			expectEvent: true,
			expectError: false,
			validateEvent: func(t *testing.T, events *pb.HealthEvents) {
				require.NotNil(t, events)
				require.Len(t, events.Events, 1)
				event := events.Events[0]
				require.Len(t, event.EntitiesImpacted, 2)
				assert.Equal(t, "PCI", event.EntitiesImpacted[0].EntityType)
				assert.Equal(t, "GPU_UUID", event.EntitiesImpacted[1].EntityType)
				assert.Equal(t, "GPU-123", event.EntitiesImpacted[1].EntityValue)
				assert.Equal(t, "NVRM: Xid (PCI:0000:00:08.0): 79, pid=12345, name=test-process", event.Message)
				assert.Equal(t, pb.RecommendedAction_COMPONENT_RESET, event.RecommendedAction)
				assert.Empty(t, event.Metadata)
			},
		},
		{
			name:    "Parser Returns Error",
			message: "Some random message",
			setupHandler: func() *XIDHandler {
				h, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", "/tmp/metadata.json")
				h.parser = &mockParser{
					parseFunc: func(msg string) (*parser.Response, error) {
						return nil, errors.New("parse error")
					},
				}
				return h
			},
			expectEvent: false,
			expectError: false,
		},
		{
			name:    "Parser Returns Nil Response",
			message: "Some random message",
			setupHandler: func() *XIDHandler {
				h, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", "/tmp/metadata.json")
				h.parser = &mockParser{
					parseFunc: func(msg string) (*parser.Response, error) {
						return nil, nil
					},
				}
				return h
			},
			expectEvent: false,
			expectError: false,
		},
		{
			name:    "Parser Returns Success=false",
			message: "Some random message",
			setupHandler: func() *XIDHandler {
				h, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", "/tmp/metadata.json")
				h.parser = &mockParser{
					parseFunc: func(msg string) (*parser.Response, error) {
						return &parser.Response{
							Success: false,
						}, nil
					},
				}
				return h
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
	handler, _ := NewXIDHandler("test-node", "test-agent", "GPU", "xid-check", "", "/tmp/metadata.json")

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
				assert.Equal(t, pb.RecommendedAction_CONTACT_SUPPORT, event.RecommendedAction)
				assert.Equal(t, "Test XID message", event.Message)
				require.Len(t, event.ErrorCode, 1)
				assert.Equal(t, "Xid 79", event.ErrorCode[0])
				assert.Equal(t, "test-node", event.NodeName)
				assert.NotNil(t, event.GeneratedTimestamp)
				assert.Empty(t, event.Metadata)
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
				assert.Equal(t, "0000:00:09", event.EntitiesImpacted[0].EntityValue)
				assert.Equal(t, "GPU_UUID", event.EntitiesImpacted[1].EntityType)
				assert.Equal(t, "GPU-ABCDEF12-3456-7890-ABCD-EF1234567890", event.EntitiesImpacted[1].EntityValue)
				assert.Equal(t, "Test XID message", event.Message)
				assert.Empty(t, event.Metadata)
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
			handler, err := NewXIDHandler(tc.nodeName, tc.agentName, tc.componentClass, tc.checkName, tc.xidAnalyserEndpoint, "/tmp/metadata.json")

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
				assert.NotNil(t, handler.metadataReader)
			}
		})
	}
}
