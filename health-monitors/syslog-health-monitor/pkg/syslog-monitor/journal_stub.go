//go:build !systemd

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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"
)

const (
	JOURNAL_CLOSED_ERROR_MESSAGE = "journal is closed"
	HTTP_SERVER_PORT             = "9091"
)

var journal []string

func init() {
	journal = make([]string, 0)
}

// StubJournal is a simple array-based implementation
type StubJournal struct {
	closed          bool
	currentPosition int
	bootID          string
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
// Returns the boot ID that was generated when the StubJournalFactory was created
func (j *StubJournal) GetBootID() (string, error) {
	if j.closed {
		return "", errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	return j.bootID, nil
}

// GetCursor returns a cursor that can be used to seek to the current location
func (j *StubJournal) GetCursor() (string, error) {
	if j.closed {
		return "", errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	return strconv.Itoa(j.currentPosition), nil
}

// GetData retrieves a field from the current journal entry
func (j *StubJournal) GetData(field string) (string, error) {
	if j.closed {
		return "", errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	if j.currentPosition < 0 || j.currentPosition >= len(journal) {
		return "", fmt.Errorf("invalid current position: %d", j.currentPosition)
	}

	return journal[j.currentPosition], nil
}

// Next moves to the next journal entry
func (j *StubJournal) Next() (uint64, error) {
	if j.closed {
		return 0, errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	if j.currentPosition+1 >= len(journal) {
		return 0, io.EOF
	}

	j.currentPosition++

	return 1, nil
}

// Previous moves to the previous journal entry
func (j *StubJournal) Previous() (uint64, error) {
	if j.closed {
		return 0, errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	// Special case: at position 0, stay there
	if j.currentPosition == 0 && len(journal) > 0 {
		return 1, nil
	}

	// At position -1 (before start)
	if j.currentPosition == -1 {
		// If journal has entries, indicate there's something at position 0
		// Don't move position, just signal that an entry exists
		if len(journal) > 0 {
			return 1, nil
		}
		// Empty journal
		return 0, io.EOF
	}

	// Normal case: move backwards
	if j.currentPosition-1 < 0 {
		j.currentPosition = -1
		return 0, io.EOF
	}

	j.currentPosition--

	return 1, nil
}

// SeekCursor seeks to a position indicated by a cursor
func (j *StubJournal) SeekCursor(cursor string) error {
	if j.closed {
		return errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	index, err := strconv.Atoi(cursor)
	if err != nil {
		return fmt.Errorf("invalid cursor format: %s", cursor)
	}

	j.currentPosition = index

	return nil
}

// SeekTail seeks to the end of the journal
// For stub journal, we always set position to -1 (before start) so that
// Previous() returns EOF and the cursor is saved as "-1", ensuring ALL messages
// are processed on the next run (starting from index 0).
// This is appropriate for testing where you want predictable, complete processing.
func (j *StubJournal) SeekTail() error {
	if j.closed {
		return errors.New(JOURNAL_CLOSED_ERROR_MESSAGE)
	}

	// Always position before start, regardless of journal contents
	j.currentPosition = -1

	return nil
}

// StubJournalFactory creates stub journal instances
type StubJournalFactory struct {
	bootID string
}

// NewJournal creates a new system journal instance
func (f *StubJournalFactory) NewJournal() (Journal, error) {
	return &StubJournal{
		closed:          false,
		currentPosition: -1,
		bootID:          f.bootID,
	}, nil
}

// NewJournalFromDir creates a journal from the specified directory
func (f *StubJournalFactory) NewJournalFromDir(path string) (Journal, error) {
	return &StubJournal{
		closed:          false,
		currentPosition: -1,
		bootID:          f.bootID,
	}, nil
}

// RequiresFileSystemCheck implements the JournalFactory interface
func (f *StubJournalFactory) RequiresFileSystemCheck() bool {
	return false // Stub journals don't need filesystem validation
}

// NewStubJournalFactory creates a factory for stub journal instances
func NewStubJournalFactory() JournalFactory {
	return &StubJournalFactory{
		bootID: fmt.Sprintf("stub-boot-%d", time.Now().Unix()),
	}
}

// GetDefaultJournalFactory returns a stub factory in non-systemd builds
func GetDefaultJournalFactory() JournalFactory {
	// Clear journal on factory creation (simulates fresh start on pod restart)
	journal = make([]string, 0)

	slog.Info("Stub journal cleared for new factory initialization")

	// Clear state file in stub mode to ensure clean slate for testing
	// This is necessary because boot ID change detection doesn't work in stub mode
	stateFilePath := "/var/run/syslog_monitor/state.json"
	if err := os.Remove(stateFilePath); err != nil && !os.IsNotExist(err) {
		slog.Warn("Failed to clear state file in stub mode", "path", stateFilePath, "error", err)
	} else if err == nil {
		slog.Info("State file cleared for testing (stub mode)", "path", stateFilePath)
	}

	go func() {
		http.HandleFunc("/add", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Failed to read body", http.StatusBadRequest)
				return
			}

			slog.Info("Adding message to journal", "message", string(body))

			journal = append(journal, string(body))

			slog.Info("Adding message to journal", "message",
				string(body), "index", len(journal)-1, "totalEntries", len(journal))

			w.WriteHeader(http.StatusOK)
		})

		slog.Info("starting HTTP server", "port", HTTP_SERVER_PORT)

		//nolint:gosec // stub server for tests; TLS and timeouts not required
		err := http.ListenAndServe(":"+HTTP_SERVER_PORT, nil)
		if err != nil {
			slog.Error("failed to start HTTP server", "error", err)
			os.Exit(1)
		}
	}()

	return NewStubJournalFactory()
}
