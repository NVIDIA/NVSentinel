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
// limitations under the License.package faultdiagnostics
package syslogmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/types"
	"github.com/stretchr/testify/assert"
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
	JOURNAL_CLOSED_ERROR        = "journal is closed"
)

// MockJournalEntry represents a single journal entry
type MockJournalEntry struct {
	Message   string
	BootID    string
	Cursor    string
	Fields    map[string]string
	Timestamp string
}

// MockJournal is a mock implementation of the Journal interface for testing
type MockJournal struct {
	Entries         []MockJournalEntry
	Path            string
	CurrentPosition int
	MatchFilters    []string
	Closed          bool
	TestBootID      string
	TestCursor      string
	FailNextEntry   bool
	FailGetBootID   bool
	FailGetCursor   bool
	FailGetData     bool
	FailSeekCursor  bool
	FailSeekTail    bool
}

// AddMatch adds a match filter for journal entries
func (j *MockJournal) AddMatch(match string) error {
	if j.Closed {
		return errors.New(JOURNAL_CLOSED_ERROR)
	}

	j.MatchFilters = append(j.MatchFilters, match)

	return nil
}

// Close closes the journal
func (j *MockJournal) Close() error {
	j.Closed = true
	return nil
}

// GetBootID retrieves the current boot ID
func (j *MockJournal) GetBootID() (string, error) {
	if j.Closed {
		return "", errors.New(JOURNAL_CLOSED_ERROR)
	}

	if j.FailGetBootID {
		return "", fmt.Errorf("forced GetBootID failure")
	}

	if j.TestBootID != "" {
		return j.TestBootID, nil
	}

	if j.CurrentPosition >= 0 && j.CurrentPosition < len(j.Entries) {
		return j.Entries[j.CurrentPosition].BootID, nil
	}

	return "mock-boot-id", nil
}

// GetCursor returns a cursor that can be used to seek to the current location
func (j *MockJournal) GetCursor() (string, error) {
	if j.Closed {
		return "", errors.New(JOURNAL_CLOSED_ERROR)
	}

	if j.FailGetCursor {
		return "", fmt.Errorf("forced GetCursor failure")
	}

	if j.TestCursor != "" {
		return j.TestCursor, nil
	}

	if j.CurrentPosition >= 0 && j.CurrentPosition < len(j.Entries) {
		return j.Entries[j.CurrentPosition].Cursor, nil
	}

	return "mock-cursor", nil
}

// GetData retrieves a field from the current journal entry
func (j *MockJournal) GetData(field string) (string, error) {
	if j.Closed {
		return "", errors.New(JOURNAL_CLOSED_ERROR)
	}

	if j.FailGetData {
		return "", fmt.Errorf("forced GetData failure")
	}

	if j.CurrentPosition < 0 || j.CurrentPosition >= len(j.Entries) {
		return "", fmt.Errorf("invalid cursor position")
	}

	if field == "MESSAGE" {
		return j.Entries[j.CurrentPosition].Message, nil
	}

	value, ok := j.Entries[j.CurrentPosition].Fields[field]
	if !ok {
		return "", fmt.Errorf("field not found: %s", field)
	}

	return value, nil
}

// Next moves to the next journal entry
func (j *MockJournal) Next() (uint64, error) {
	if j.Closed {
		return 0, errors.New(JOURNAL_CLOSED_ERROR)
	}

	if j.FailNextEntry {
		return 0, fmt.Errorf("forced Next failure")
	}

	j.CurrentPosition++
	if j.CurrentPosition >= len(j.Entries) {
		return 0, io.EOF
	}

	return 1, nil
}

// Previous moves to the previous journal entry
func (j *MockJournal) Previous() (uint64, error) {
	if j.Closed {
		return 0, errors.New(JOURNAL_CLOSED_ERROR)
	}

	j.CurrentPosition--
	if j.CurrentPosition < 0 {
		j.CurrentPosition = -1
		return 0, io.EOF
	}

	return 1, nil
}

// SeekCursor seeks to a position indicated by a cursor
func (j *MockJournal) SeekCursor(cursor string) error {
	if j.Closed {
		return errors.New(JOURNAL_CLOSED_ERROR)
	}

	if j.FailSeekCursor {
		return fmt.Errorf("forced SeekCursor failure")
	}

	for i, entry := range j.Entries {
		if entry.Cursor == cursor {
			j.CurrentPosition = i
			return nil
		}
	}

	return fmt.Errorf("cursor not found: %s", cursor)
}

// SeekTail seeks to the end of the journal
func (j *MockJournal) SeekTail() error {
	if j.Closed {
		return errors.New(JOURNAL_CLOSED_ERROR)
	}

	if j.FailSeekTail {
		return fmt.Errorf("forced SeekTail failure")
	}

	if len(j.Entries) > 0 {
		j.CurrentPosition = len(j.Entries) - 1
	} else {
		j.CurrentPosition = -1
	}

	return nil
}

// MockJournalFactory creates mock journal instances
type MockJournalFactory struct {
	JournalsByPath map[string]*MockJournal
	DefaultJournal *MockJournal
}

// NewJournal creates a new system journal instance
func (f *MockJournalFactory) NewJournal() (Journal, error) {
	if f.DefaultJournal != nil {
		return f.DefaultJournal, nil
	}

	return &MockJournal{
		CurrentPosition: -1,
		TestBootID:      "mock-boot-id",
	}, nil
}

// NewJournalFromDir creates a journal from the specified directory
func (f *MockJournalFactory) NewJournalFromDir(path string) (Journal, error) {
	journal, ok := f.JournalsByPath[path]
	if !ok {
		return nil, fmt.Errorf("no mock journal configured for path: %s", path)
	}

	return journal, nil
}

// RequiresFileSystemCheck implements the JournalFactory interface
func (f *MockJournalFactory) RequiresFileSystemCheck() bool {
	return false // Mock journals don't need filesystem validation
}

// NewMockJournalFactory creates a factory for mock journal instances
func NewMockJournalFactory() *MockJournalFactory {
	return &MockJournalFactory{
		JournalsByPath: make(map[string]*MockJournal),
	}
}

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

	filePath := testStateFile

	monitor, err := NewSyslogMonitor(args.NodeName,
		args.Checks, args.PcClient, args.DefaultAgentName, args.DefaultComponentClass, args.PollingInterval, filePath, "http://localhost:8080")
	assert.NoError(t, err)
	assert.NotNil(t, monitor)
	assert.Equal(t, args.NodeName, monitor.nodeName)
	assert.Equal(t, args.Checks, monitor.checks)
	assert.NotNil(t, monitor.journalFactory, "Journal factory should not be nil")
	assert.Equal(t, testStateFile, monitor.stateFilePath)

	// Test case 2: With specific fake factory
	fakeJournalFactory := NewFakeJournalFactory()
	fakeJournal := NewFakeJournal()
	fakeJournal.Path = "/fake/journal"
	fakeJournalFactory.AddJournal("/some/path", fakeJournal)

	testStateFile2 := "/tmp/test-syslog-monitor-state2.json"
	defer os.Remove(testStateFile2) // Cleanup

	filePath = testStateFile2
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
		RecommendedAction: pb.RecommenedAction_RESTART_BM,
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

	sm, err := NewSyslogMonitorWithFactory(
		TEST_NODE,
		[]CheckDefinition{check},
		&mockPlatformConnectorClient{},
		TEST_AGENT,
		TEST_COMPONENT,
		"60s",
		testStateFile,
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
		RecommendedAction: pb.RecommenedAction_RESTART_BM,
		ErrorCode:         []string{"123"},
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}, nil
}

// TestJournalStateManagement tests state persistence and recovery
func TestJournalStateManagement(t *testing.T) {
	testStateFile := "/tmp/test-syslog-monitor-state-mgmt.json"
	defer os.Remove(testStateFile)

	check := CheckDefinition{
		Name:        "stateCheck",
		JournalPath: TEST_JOURNAL_PATH,
	}

	mockJournal := &MockJournal{
		Entries: []MockJournalEntry{
			{Message: "entry1", Cursor: "cursor-1", BootID: "boot-1"},
			{Message: "entry2", Cursor: "cursor-2", BootID: "boot-1"},
			{Message: "entry3", Cursor: "cursor-3", BootID: "boot-1"},
		},
		CurrentPosition: -1,
		TestBootID:      "boot-1",
	}

	mockFactory := NewMockJournalFactory()
	mockFactory.JournalsByPath[check.JournalPath] = mockJournal

	sm, err := NewSyslogMonitorWithFactory(
		TEST_NODE,
		[]CheckDefinition{check},
		&mockPlatformConnectorClient{},
		TEST_AGENT,
		TEST_COMPONENT,
		"60s",
		testStateFile,
		mockFactory,
		"http://localhost:8080",
	)
	assert.NoError(t, err)

	sm.checkToHandlerMap["stateCheck"] = &mockHandler{
		nodeName:              "test",
		defaultAgentName:      "syslog-health-monitor",
		defaultComponentClass: "GPU",
		checkName:             "stateCheck",
	}

	err = sm.executeCheck(check)
	assert.NoError(t, err)

	cursor, exists := sm.checkLastCursors[check.Name]
	assert.True(t, exists)
	assert.NotEmpty(t, cursor)

	err = sm.saveCurrentState()
	assert.NoError(t, err)

	sm2, err := NewSyslogMonitorWithFactory(
		TEST_NODE,
		[]CheckDefinition{check},
		&mockPlatformConnectorClient{},
		TEST_AGENT,
		TEST_COMPONENT,
		"60s",
		testStateFile,
		mockFactory,
		"http://localhost:8080",
	)
	assert.NoError(t, err)

	loadedCursor, exists := sm2.checkLastCursors[check.Name]
	assert.True(t, exists)
	assert.Equal(t, cursor, loadedCursor, "Loaded cursor should match saved cursor")
}

// TestBootIDChangeHandling tests boot ID change detection
func TestBootIDChangeHandling(t *testing.T) {
	testStateFile := "/tmp/test-syslog-monitor-bootid.json"
	defer os.Remove(testStateFile)

	check := CheckDefinition{
		Name:        "bootCheck",
		JournalPath: TEST_JOURNAL_PATH,
	}

	mockJournal := &MockJournal{
		Entries: []MockJournalEntry{
			{Message: "entry1", Cursor: "cursor-1", BootID: "boot-2"},
		},
		CurrentPosition: -1,
		TestBootID:      "boot-2",
	}

	mockFactory := NewMockJournalFactory()
	mockFactory.JournalsByPath[check.JournalPath] = mockJournal

	initialState := syslogMonitorState{
		BootID:           "boot-1",
		CheckLastCursors: map[string]string{"bootCheck": "old-cursor"},
	}
	stateData, _ := json.Marshal(initialState)
	os.WriteFile(testStateFile, stateData, 0644)

	sm, err := NewSyslogMonitorWithFactory(
		TEST_NODE,
		[]CheckDefinition{check},
		&mockPlatformConnectorClient{},
		TEST_AGENT,
		TEST_COMPONENT,
		"60s",
		testStateFile,
		mockFactory,
		"http://localhost:8080",
	)
	assert.NoError(t, err)

	cursor, exists := sm.checkLastCursors[check.Name]
	if exists {
		assert.Empty(t, cursor, "Cursor should be cleared after boot ID change")
	}
}

// TestRunMultipleChecks tests running multiple checks in sequence
func TestRunMultipleChecks(t *testing.T) {
	check1 := CheckDefinition{
		Name:        XIDErrorCheck,
		JournalPath: "/path1",
	}
	check2 := CheckDefinition{
		Name:        SXIDErrorCheck,
		JournalPath: "/path2",
	}

	mockJournal1 := &MockJournal{
		Entries:         []MockJournalEntry{{Message: "msg1", Cursor: "c1", BootID: "b1"}},
		CurrentPosition: -1,
		TestBootID:      "b1",
	}
	mockJournal2 := &MockJournal{
		Entries:         []MockJournalEntry{{Message: "msg2", Cursor: "c2", BootID: "b1"}},
		CurrentPosition: -1,
		TestBootID:      "b1",
	}

	mockFactory := NewMockJournalFactory()
	mockFactory.JournalsByPath["/path1"] = mockJournal1
	mockFactory.JournalsByPath["/path2"] = mockJournal2

	testStateFile := "/tmp/test-syslog-monitor-multi.json"
	defer os.Remove(testStateFile)

	sm, err := NewSyslogMonitorWithFactory(
		TEST_NODE,
		[]CheckDefinition{check1, check2},
		&mockPlatformConnectorClient{},
		TEST_AGENT,
		TEST_COMPONENT,
		"60s",
		testStateFile,
		mockFactory,
		"http://localhost:8080",
	)
	assert.NoError(t, err)

	err = sm.Run()
	assert.NoError(t, err)

	assert.NotNil(t, sm.checkToHandlerMap[XIDErrorCheck], "XID handler should be initialized")
	assert.NotNil(t, sm.checkToHandlerMap[SXIDErrorCheck], "SXID handler should be initialized")
}
