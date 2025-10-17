//go:build !systemd
// +build !systemd

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
	"errors"
	"io"
)

const (
	JOURNAL_CLOSED_ERROR_MESSAGE = "journal is closed"
)

// StubJournal is a no-op implementation of the Journal interface
type StubJournal struct {
	closed bool
}

// AddMatch adds a match filter for journal entries
func (j *StubJournal) AddMatch(match string) error {
	if j.closed {
		return errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	return nil
}

// Close closes the journal
func (j *StubJournal) Close() error {
	j.closed = true
	return nil
}

// GetBootID retrieves the current boot ID
func (j *StubJournal) GetBootID() (string, error) {
	if j.closed {
		return "", errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	return "stub-boot-id", nil
}

// GetCursor returns a cursor that can be used to seek to the current location
func (j *StubJournal) GetCursor() (string, error) {
	if j.closed {
		return "", errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	return "stub-cursor", nil
}

// GetData retrieves a field from the current journal entry
func (j *StubJournal) GetData(field string) (string, error) {
	if j.closed {
		return "", errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	return "", nil
}

// Next moves to the next journal entry
func (j *StubJournal) Next() (uint64, error) {
	if j.closed {
		return 0, errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	return 0, io.EOF
}

// Previous moves to the previous journal entry
func (j *StubJournal) Previous() (uint64, error) {
	if j.closed {
		return 0, errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	return 0, io.EOF
}

// SeekCursor seeks to a position indicated by a cursor
func (j *StubJournal) SeekCursor(cursor string) error {
	if j.closed {
		return errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	return nil
}

// SeekTail seeks to the end of the journal
func (j *StubJournal) SeekTail() error {
	if j.closed {
		return errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	return nil
}

// StubJournalFactory creates stub journal instances
type StubJournalFactory struct{}

// NewJournal creates a new system journal instance
func (f *StubJournalFactory) NewJournal() (Journal, error) {
	return &StubJournal{}, nil
}

// NewJournalFromDir creates a journal from the specified directory
func (f *StubJournalFactory) NewJournalFromDir(path string) (Journal, error) {
	return &StubJournal{}, nil
}

// RequiresFileSystemCheck implements the JournalFactory interface
func (f *StubJournalFactory) RequiresFileSystemCheck() bool {
	return false // Stub journals don't need filesystem validation
}

// NewStubJournalFactory creates a factory for stub journal instances
func NewStubJournalFactory() JournalFactory {
	return &StubJournalFactory{}
}

// GetDefaultJournalFactory returns a stub factory in non-systemd builds
func GetDefaultJournalFactory() JournalFactory {
	return NewStubJournalFactory()
}
