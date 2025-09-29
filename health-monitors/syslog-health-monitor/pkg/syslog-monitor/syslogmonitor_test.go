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
// limitations under the License.package faultdiagnostics
package syslogmonitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/protos"
	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	TEST_NODE                   = "test-node"
	TEST_AGENT                  = "test-agent"
	TEST_COMPONENT              = "test-component"
	TEST_JOURNAL_PATH           = "/fake/journal/path"
	TEST_LOG_WITH_MATCH_IN_IT   = "Log with match in it"
	TEST_LOG_WITH_MATCH_IN_IT_2 = "Another log with match in it"
)

// LogCheckConfig represents the YAML configuration structure
type LogCheckConfig struct {
	Checks []CheckDefinition `yaml:"checks"`
}

// Mock PlatformConnectorClient
type mockPlatformConnectorClient struct {
	RecordedHealthEvents []*pb.HealthEvents
}

func (m *mockPlatformConnectorClient) HealthEventOccuredV1(ctx context.Context, events *pb.HealthEvents, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	m.RecordedHealthEvents = append(m.RecordedHealthEvents, events)
	return &emptypb.Empty{}, nil
}

func TestNewSyslogMonitor(t *testing.T) {
	sxidFilePath, actionMappingPath, err := moveSXIDAndActionMappingToTempDir()
	assert.NoError(t, err)
	defer func() {
		if err := os.Remove(sxidFilePath); err != nil {
			t.Logf("Failed to remove SXID file %s: %v", sxidFilePath, err)
		}
		if err := os.Remove(actionMappingPath); err != nil {
			t.Logf("Failed to remove action mapping file %s: %v", actionMappingPath, err)
		}
	}()
	// Define test arguments
	args := struct {
		NodeName              string
		Checks                []CheckDefinition
		PcClient              pb.PlatformConnectorClient
		DefaultAgentName      string
		DefaultComponentClass string
		PollingInterval       string
	}{
		NodeName: TEST_NODE,
		Checks: []CheckDefinition{
			{Name: "check1", JournalPath: "/some/path"}, // JournalPath is still relevant for the CheckDefinition
		},
		PcClient:              &mockPlatformConnectorClient{},
		DefaultAgentName:      TEST_AGENT,
		DefaultComponentClass: TEST_COMPONENT,
		PollingInterval:       "60s",
	}

	// Test case 1: Valid configuration with default factory
	testStateFile := "/tmp/test-syslog-monitor-state.json"
	defer os.Remove(testStateFile) // Cleanup

	filePath := FilePaths{
		StateFilePath:     testStateFile,
		SxidMappingPath:   sxidFilePath,
		ActionMappingPath: actionMappingPath,
	}

	monitor, err := NewSyslogMonitor(args.NodeName,
		args.Checks, args.PcClient, args.DefaultAgentName, args.DefaultComponentClass, args.PollingInterval, filePath, "http://localhost:8080")
	assert.NoError(t, err)
	assert.NotNil(t, monitor)
	assert.Equal(t, args.NodeName, monitor.nodeName)
	assert.Equal(t, args.Checks, monitor.checks)
	assert.NotNil(t, monitor.journalFactory, "Journal factory should not be nil")
	assert.Equal(t, testStateFile, monitor.filePaths.StateFilePath)

	// Test case 2: With specific fake factory
	fakeJournalFactory := NewFakeJournalFactory()
	fakeJournal := NewFakeJournal()
	fakeJournal.Path = "/fake/journal"
	fakeJournalFactory.AddJournal("/some/path", fakeJournal)

	testStateFile2 := "/tmp/test-syslog-monitor-state2.json"
	defer os.Remove(testStateFile2) // Cleanup

	filePath = FilePaths{
		StateFilePath:     testStateFile2,
		ActionMappingPath: actionMappingPath,
		SxidMappingPath:   sxidFilePath,
	}
	monitor, err = NewSyslogMonitorWithFactory(args.NodeName,
		args.Checks, args.PcClient, args.DefaultAgentName, args.DefaultComponentClass, args.PollingInterval, filePath, fakeJournalFactory, "http://localhost:8080")
	assert.NoError(t, err)
	assert.NotNil(t, monitor)
	assert.Equal(t, fakeJournalFactory, monitor.journalFactory)
}

func TestPrepareHealthEvent(t *testing.T) {
	check := CheckDefinition{
		Name: "test_check",
	}

	fd := &SyslogMonitor{
		nodeName:              TEST_NODE,
		defaultAgentName:      TEST_AGENT,
		defaultComponentClass: TEST_COMPONENT,
	}

	message := "test message"
	errRes := types.ErrorResolution{
		RecommendedAction: pb.RecommenedAction_NODE_REBOOT,
	}
	healthEvents := fd.prepareHealthEventWithAction(check, message, false, errRes)

	assert.NotNil(t, healthEvents)
	assert.Equal(t, uint32(1), healthEvents.Version)
	assert.Len(t, healthEvents.Events, 1)

	event := healthEvents.Events[0]
	assert.Equal(t, uint32(1), event.Version)
	assert.Equal(t, TEST_AGENT, event.Agent)
	assert.Equal(t, "test_check", event.CheckName)
	assert.Equal(t, TEST_COMPONENT, event.ComponentClass)
	assert.Equal(t, TEST_NODE, event.NodeName)
	assert.Equal(t, message, event.Message)
	assert.False(t, event.IsHealthy)
	assert.False(t, event.IsFatal)
	assert.Equal(t, errRes.RecommendedAction, event.RecommendedAction)
}

// TestJournalProcessingLogic tests specific journal cursor handling logic
func TestJournalProcessingLogic(t *testing.T) {
	sxidFilePath, actionMappingPath, err := moveSXIDAndActionMappingToTempDir()
	assert.NoError(t, err)
	defer func() {
		if err := os.Remove(sxidFilePath); err != nil {
			t.Logf("Failed to remove SXID file %s: %v", sxidFilePath, err)
		}
		if err := os.Remove(actionMappingPath); err != nil {
			t.Logf("Failed to remove action mapping file %s: %v", actionMappingPath, err)
		}
	}()
	// Create a check definition
	check := CheckDefinition{
		Name:        "mockCheck",
		JournalPath: TEST_JOURNAL_PATH,
	}

	// Create fake journal with some entries
	fakeJournal := NewFakeJournal()
	fakeJournal.AddEntryWithMessage("nothing", "cursor-1")
	fakeJournal.AddEntryWithMessage("sxid123", "cursor-2")
	fakeJournal.AddEntryWithMessage("Another error message", "cursor-3")

	// Create factory and add journal
	fakeJournalFactory := NewFakeJournalFactory()
	fakeJournalFactory.AddJournal(check.JournalPath, fakeJournal)

	// Create SyslogMonitor
	testStateFile := "/tmp/test-syslog-monitor-state-4.json"
	defer os.Remove(testStateFile)

	filePath := FilePaths{
		StateFilePath:     testStateFile,
		ActionMappingPath: actionMappingPath,
		SxidMappingPath:   sxidFilePath,
	}

	sm, err := NewSyslogMonitorWithFactory(
		TEST_NODE,
		[]CheckDefinition{check},
		&mockPlatformConnectorClient{},
		TEST_AGENT,
		TEST_COMPONENT,
		"60s",
		filePath,
		fakeJournalFactory,
		"http://localhost:8080",
	)
	assert.NoError(t, err)

	sm.checkToHandlerMap["mockCheck"] = &mockHandler{
		nodeName:              "test",
		defaultAgentName:      "syslog-health-monitor",
		defaultComponentClass: "GPU",
		checkName:             "mockCheck",
	}
	// First run should initialize cursor
	err = sm.executeCheck(check)
	assert.NoError(t, err)

	// Verify cursor was stored
	cursor, exists := sm.checkLastCursors[check.Name]
	assert.True(t, exists, "Cursor should be stored after first run")
	assert.NotEmpty(t, cursor)

	// Add new entries that would exceed threshold
	fakeJournal.AddEntryWithMessage("New error message 1", "cursor-4")
	fakeJournal.AddEntryWithMessage("New error message 2", "cursor-5")

	// Create new mock client to track events clearly
	mockPCClient := &mockPlatformConnectorClient{}
	sm.pcClient = mockPCClient

	// Next execution should process only new entries since the last cursor
	err = sm.executeCheck(check)
	assert.NoError(t, err)

	// Since we have new entries with errors and count=0, we should get a health event
	assert.NotEmpty(t, mockPCClient.RecordedHealthEvents, "Health event should be sent for entries above threshold")
}

type mockHandler struct {
	nodeName              string
	defaultAgentName      string
	defaultComponentClass string
	checkName             string
}

func (mh *mockHandler) ProcessLine(message string) (*pb.HealthEvents, error) {
	if !strings.Contains(message, "sxid123") {
		return nil, nil
	}
	event := &pb.HealthEvent{
		Version:            1,
		Agent:              mh.defaultAgentName,
		CheckName:          mh.checkName,
		ComponentClass:     mh.defaultComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted: []*pb.Entity{
			{EntityType: "GPU", EntityValue: "44"},
		},
		Message:           "TestMessage",
		IsFatal:           true,
		IsHealthy:         false,
		NodeName:          mh.nodeName,
		RecommendedAction: pb.RecommenedAction_NODE_REBOOT,
		ErrorCode:         []string{"123"},
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}, nil
}

// moveSXIDAndActionMappingToTempDir moves the SXID and action mapping files to a temporary directory
func moveSXIDAndActionMappingToTempDir() (string, string, error) {
	tempDir := "/tmp/test-syslog-monitor/"
	sxidFilePath := filepath.Join(tempDir, "sxiderrorsmapping.csv")
	// Create temp directory
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return "", "", fmt.Errorf("failed to create temp directory: %v", err)
	}

	sxidContent := `
10001,NVSWITCH_ERR_HW_HOST_PRIV_ERROR,IGNORE,NONFATAL
10002,NVSWITCH_ERR_HW_HOST_PRIV_TIMEOUT,IGNORE,NONFATAL
10003,NVSWITCH_ERR_HW_HOST_UNHANDLED_INTERRUPT,RESET_FABRIC,FATAL
10004,NVSWITCH_ERR_HW_HOST_THERMAL_EVENT_START,CHECK_THERMALS,FATAL
`

	actionMappingPath := filepath.Join(tempDir, "actionmapping.ini")
	actionFilePathContent := `
	[errorrecommendactiontoplatformconnectormapping]
		NODE_REBOOT = 1
		UNEXPECTED_ERR_REPORT_ISSUE = 5
		WORKFLOW_XID_13_31 = 7
		IGNORE = 0
		WORKFLOW_ECC_DBE_SRAM = 8
		REPORT_ISSUE = 5
		RUN_FIELDDIAG = 6
		RESTART_APP = 4
		RESET_GPU = 2
		RESOLUTION_BUCKET_TBD = 99
		WORKFLOW_NVLINK5_ERR = 9
		WORKFLOW_XID_45 = 10
		WORKFLOW_XID_48 = 11
		CHECK_MECHANICALS = 12
		WORKFLOW_NVLINK_ERR = 13
		UPDATE_SWFW = 14
		RESTART_VM = 15
		CHECK_THERMALS = 19
		CHECK_FM_CONFIG = 18
		RESTART_BM = 24
		CHECK_UVM = 23
	`

	// Write files
	if err := os.WriteFile(sxidFilePath, []byte(sxidContent), 0644); err != nil {
		return "", "", fmt.Errorf("failed to write SXID file: %v", err)
	}

	if err := os.WriteFile(actionMappingPath, []byte(actionFilePathContent), 0644); err != nil {
		return "", "", fmt.Errorf("failed to write action mapping file: %v", err)
	}

	return sxidFilePath, actionMappingPath, nil
}
