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

// Journal defines the interface for interacting with system journal
type Journal interface {
	// AddMatch adds a match filter for journal entries
	AddMatch(match string) error

	// Close closes the journal
	Close() error

	// GetBootID retrieves the current boot ID
	GetBootID() (string, error)

	// GetCursor returns a cursor that can be used to seek to the current location
	GetCursor() (string, error)

	// GetData retrieves a field from the current journal entry
	GetData(field string) (string, error)

	// Next moves to the next journal entry
	Next() (uint64, error)

	// Previous moves to the previous journal entry
	Previous() (uint64, error)

	// SeekCursor seeks to a position indicated by a cursor
	SeekCursor(cursor string) error

	// SeekTail seeks to the end of the journal
	SeekTail() error
}

// JournalFactory creates journal instances
type JournalFactory interface {
	// NewJournal creates a new system journal instance
	NewJournal() (Journal, error)

	// NewJournalFromDir creates a journal from the specified directory
	NewJournalFromDir(path string) (Journal, error)

	// RequiresFileSystemCheck returns true if the factory requires filesystem path validation
	RequiresFileSystemCheck() bool
}
