//go:build systemd

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
	"github.com/coreos/go-systemd/v22/sdjournal"
)

// RealJournal implements the Journal interface using sdjournal
type RealJournal struct {
	journal *sdjournal.Journal
}

// AddMatch adds a match filter for journal entries
func (j *RealJournal) AddMatch(match string) error {
	return j.journal.AddMatch(match)
}

// Close closes the journal
func (j *RealJournal) Close() error {
	return j.journal.Close()
}

// GetBootID retrieves the current boot ID
func (j *RealJournal) GetBootID() (string, error) {
	return j.journal.GetBootID()
}

// GetCursor returns a cursor that can be used to seek to the current location
func (j *RealJournal) GetCursor() (string, error) {
	return j.journal.GetCursor()
}

// GetData retrieves a field from the current journal entry
func (j *RealJournal) GetData(field string) (string, error) {
	return j.journal.GetData(field)
}

// Next moves to the next journal entry
func (j *RealJournal) Next() (uint64, error) {
	return j.journal.Next()
}

// Previous moves to the previous journal entry
func (j *RealJournal) Previous() (uint64, error) {
	return j.journal.Previous()
}

// SeekCursor seeks to a position indicated by a cursor
func (j *RealJournal) SeekCursor(cursor string) error {
	return j.journal.SeekCursor(cursor)
}

// SeekTail seeks to the end of the journal
func (j *RealJournal) SeekTail() error {
	return j.journal.SeekTail()
}

// RealJournalFactory creates journal instances using the real systemd journal
type RealJournalFactory struct{}

// NewJournal creates a new system journal instance
func (f *RealJournalFactory) NewJournal() (Journal, error) {
	journal, err := sdjournal.NewJournal()
	if err != nil {
		return nil, err
	}
	return &RealJournal{journal: journal}, nil
}

// NewJournalFromDir creates a journal from the specified directory
func (f *RealJournalFactory) NewJournalFromDir(path string) (Journal, error) {
	journal, err := sdjournal.NewJournalFromDir(path)
	if err != nil {
		return nil, err
	}
	return &RealJournal{journal: journal}, nil
}

// RequiresFileSystemCheck implements the JournalFactory interface
func (f *RealJournalFactory) RequiresFileSystemCheck() bool {
	return true // Real journals need filesystem validation
}

// NewRealJournalFactory creates a factory for real journal instances
func NewRealJournalFactory() JournalFactory {
	return &RealJournalFactory{}
}

// GetDefaultJournalFactory returns a real factory in systemd builds
func GetDefaultJournalFactory() JournalFactory {
	return NewRealJournalFactory()
}
