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
package syslogmonitor

import (
	"fmt"
	"io"
	"strings"
)

// FakeJournalEntry represents a single entry in a fake journal
type FakeJournalEntry struct {
	Fields map[string]string
	Cursor string
}

// FakeJournal is a test implementation of the Journal interface
type FakeJournal struct {
	Entries         []FakeJournalEntry
	CurrentPosition int
	Matches         []string
	Path            string
	Closed          bool
}

// NewFakeJournal creates a new fake journal
func NewFakeJournal() *FakeJournal {
	return &FakeJournal{
		Entries:         []FakeJournalEntry{},
		CurrentPosition: -1, // Initially positioned before the first entry
		Matches:         []string{},
	}
}

// AddEntry adds a new entry to the fake journal
func (j *FakeJournal) AddEntry(fields map[string]string, cursor string) {
	j.Entries = append(j.Entries, FakeJournalEntry{
		Fields: fields,
		Cursor: cursor,
	})
}

// AddEntryWithMessage adds a new entry with a given message to the fake journal
func (j *FakeJournal) AddEntryWithMessage(message string, cursor string) {
	fields := map[string]string{
		FieldMessage: message,
	}
	j.AddEntry(fields, cursor)
}

// AddMatch implements the Journal interface
func (j *FakeJournal) AddMatch(match string) error {
	if j.Closed {
		return fmt.Errorf("journal is closed")
	}

	j.Matches = append(j.Matches, match)

	return nil
}

// Close implements the Journal interface
func (j *FakeJournal) Close() error {
	// For test purposes, set the closed flag but don't prevent further operations
	// This helps in TestJournalProcessingLogic where the same journal is reused
	j.Closed = true

	return nil
}

// GetBootID implements the Journal interface
func (j *FakeJournal) GetBootID() (string, error) {
	// Allow even if closed for test purposes
	return "fake-boot-id", nil
}

// GetCursor implements the Journal interface
func (j *FakeJournal) GetCursor() (string, error) {
	// Allow even if closed for test purposes
	if j.CurrentPosition < 0 || j.CurrentPosition >= len(j.Entries) {
		return "", fmt.Errorf("invalid cursor position")
	}

	return j.Entries[j.CurrentPosition].Cursor, nil
}

// GetData implements the Journal interface
func (j *FakeJournal) GetData(field string) (string, error) {
	// Allow even if closed for test purposes
	if j.CurrentPosition < 0 || j.CurrentPosition >= len(j.Entries) {
		return "", fmt.Errorf("invalid cursor position")
	}

	value, ok := j.Entries[j.CurrentPosition].Fields[field]
	if !ok {
		return "", nil // Returning empty string for non-existing fields is consistent with real journal behavior
	}

	return value, nil
}

// Next implements the Journal interface
func (j *FakeJournal) Next() (uint64, error) {
	// Allow even if closed for test purposes
	// Filter entries based on matches if any are defined
	if len(j.Matches) > 0 {
		// Start searching from the next position
		startPos := j.CurrentPosition + 1
		if startPos >= len(j.Entries) {
			return 0, io.EOF
		}

		for pos := startPos; pos < len(j.Entries); pos++ {
			if j.matchesFilters(j.Entries[pos]) {
				j.CurrentPosition = pos
				return 1, nil
			}
		}

		return 0, io.EOF
	} else {
		// No filters, just advance to next position
		j.CurrentPosition++
		if j.CurrentPosition >= len(j.Entries) {
			j.CurrentPosition = len(j.Entries) // Position after the last entry
			return 0, io.EOF
		}

		return 1, nil
	}
}

// Previous implements the Journal interface
func (j *FakeJournal) Previous() (uint64, error) {
	// Allow even if closed for test purposes
	// Special case: if we're at position 0 (which is the first entry),
	// we'll stay at position 0 and return success. This is needed for tests
	// that expect to call Previous() after SeekTail() when SeekTail positions
	// at the beginning of the journal
	if j.CurrentPosition == 0 && len(j.Entries) > 0 {
		return 1, nil
	}

	// Filter entries based on matches if any are defined
	if len(j.Matches) > 0 {
		// Start searching from the previous position
		startPos := j.CurrentPosition - 1
		if startPos < 0 {
			return 0, io.EOF
		}

		for pos := startPos; pos >= 0; pos-- {
			if j.matchesFilters(j.Entries[pos]) {
				j.CurrentPosition = pos
				return 1, nil
			}
		}

		return 0, io.EOF
	} else {
		// No filters, just move to previous position
		j.CurrentPosition--
		if j.CurrentPosition < 0 {
			j.CurrentPosition = -1 // Position before the first entry
			return 0, io.EOF
		}

		return 1, nil
	}
}

// SeekCursor implements the Journal interface
func (j *FakeJournal) SeekCursor(cursor string) error {
	// Allow even if closed for test purposes
	for i, entry := range j.Entries {
		if entry.Cursor == cursor {
			j.CurrentPosition = i
			return nil
		}
	}

	return fmt.Errorf("cursor not found: %s", cursor)
}

// SeekTail implements the Journal interface
func (j *FakeJournal) SeekTail() error {
	// Allow even if closed for test purposes
	if len(j.Entries) == 0 {
		j.CurrentPosition = -1

		return nil
	}

	if len(j.Matches) == 0 {
		// For test purposes, position at the FIRST entry (index 0) instead of the last
		// to force processing the entire journal in the first run
		j.CurrentPosition = 0

		return nil
	}

	// Find the last entry that matches all filters
	for i := len(j.Entries) - 1; i >= 0; i-- {
		if j.matchesFilters(j.Entries[i]) {
			j.CurrentPosition = i

			return nil
		}
	}

	// No matches
	j.CurrentPosition = len(j.Entries)

	return nil
}

// matchesFilters checks if an entry matches all the filters
func (j *FakeJournal) matchesFilters(entry FakeJournalEntry) bool {
	for _, match := range j.Matches {
		parts := strings.SplitN(match, "=", 2)
		if len(parts) != 2 {
			// Invalid match format
			continue
		}

		field := parts[0]
		value := parts[1]

		entryValue, exists := entry.Fields[field]
		if !exists || entryValue != value {
			return false
		}
	}

	return true
}

// FakeJournalFactory creates fake journal instances for testing
type FakeJournalFactory struct {
	Journals       map[string]*FakeJournal
	DefaultJournal *FakeJournal
}

// NewFakeJournalFactory creates a new fake journal factory
func NewFakeJournalFactory() *FakeJournalFactory {
	return &FakeJournalFactory{
		Journals:       make(map[string]*FakeJournal),
		DefaultJournal: NewFakeJournal(),
	}
}

// AddJournal adds a journal for a specific path
func (f *FakeJournalFactory) AddJournal(path string, journal *FakeJournal) {
	f.Journals[path] = journal
}

// NewJournal implements the JournalFactory interface
func (f *FakeJournalFactory) NewJournal() (Journal, error) {
	return f.DefaultJournal, nil
}

// NewJournalFromDir implements the JournalFactory interface
func (f *FakeJournalFactory) NewJournalFromDir(path string) (Journal, error) {
	journal, ok := f.Journals[path]
	if !ok {
		// Create a new empty journal for this path
		journal = NewFakeJournal()
		journal.Path = path
		f.Journals[path] = journal
	}

	return journal, nil
}

// RequiresFileSystemCheck implements the JournalFactory interface
func (f *FakeJournalFactory) RequiresFileSystemCheck() bool {
	return false // Fake journals don't need filesystem validation
}

// PrepareFakeJournalWithEntries creates a fake journal with test entries
func PrepareFakeJournalWithEntries(messages []string) *FakeJournal {
	journal := NewFakeJournal()

	for i, msg := range messages {
		cursor := fmt.Sprintf("fake-cursor-%d", i)
		journal.AddEntryWithMessage(msg, cursor)
	}

	return journal
}

// AddPatternsToJournal adds entries that match specific patterns to a journal
func AddPatternsToJournal(journal *FakeJournal, patterns []string, matchCount int) {
	for i, pattern := range patterns {
		// Create a simple entry that matches the pattern
		message := fmt.Sprintf("Test message matching pattern: %s", pattern)

		// Add multiple entries for each pattern if needed
		for j := 0; j < matchCount; j++ {
			cursor := fmt.Sprintf("pattern-cursor-%d-%d", i, j)
			journal.AddEntryWithMessage(message, cursor)
		}
	}
}
