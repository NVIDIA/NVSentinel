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
package syslogmonitor

import (
	"fmt"
	"io"
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
		return fmt.Errorf("journal is closed")
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
		return "", fmt.Errorf("journal is closed")
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
		return "", fmt.Errorf("journal is closed")
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
		return "", fmt.Errorf("journal is closed")
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
		return 0, fmt.Errorf("journal is closed")
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
		return 0, fmt.Errorf("journal is closed")
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
		return fmt.Errorf("journal is closed")
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
		return fmt.Errorf("journal is closed")
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
