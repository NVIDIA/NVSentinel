// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package statefile keeps which outage events the monitor has raised, so a
// restarted monitor still sends their healthy events and does not send the
// unhealthy ones again.
package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// State is what is saved.
type State struct {
	// UnreachableRaised is true while an NVHVUnreachable event is raised.
	UnreachableRaised bool `json:"unreachableRaised"`
	// CollectionRaised lists the categories (the verdict's own category names,
	// such as "xid") with a raised NVHV<kind>CollectionFailure event.
	CollectionRaised []string `json:"collectionRaised"`
	// XidLastTo is the end, in Unix seconds, of the last Xid window that was
	// read and delivered. The next window starts there. Zero means none yet.
	XidLastTo int64 `json:"xidLastTo,omitempty"`
}

// File is a state file. An empty path disables it: Load returns an empty State
// and Save does nothing.
type File struct {
	Path string
}

// Load reads the state. A missing file is an empty State, not an error.
func (f File) Load() (State, error) {
	var s State

	if f.Path == "" {
		return s, nil
	}

	data, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}

	if err != nil {
		return State{}, fmt.Errorf("failed to read the state file %s: %w", f.Path, err)
	}

	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("failed to parse the state file %s: %w", f.Path, err)
	}

	return s, nil
}

// Save writes the state crash-safely. It writes a temporary file in the same
// directory, flushes it to disk, renames it over the state file, and flushes the
// directory, so after a crash or power loss the file holds either the old state
// or the new one, never a partial write.
func (f File) Save(s State) error {
	if f.Path == "" {
		return nil
	}

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("failed to encode the state to save in %s: %w", f.Path, err)
	}

	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create the state directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return fmt.Errorf("failed to create a temporary state file in %s: %w", dir, err)
	}

	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("failed to write the temporary state file %s: %w", tmp.Name(), err)
	}

	// Flush the data before the rename, or a crash could leave the new name
	// pointing at an empty file.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("failed to sync the temporary state file %s: %w", tmp.Name(), err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close the temporary state file %s: %w", tmp.Name(), err)
	}

	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return fmt.Errorf("failed to replace the state file %s with the new one: %w", f.Path, err)
	}

	return syncDir(dir)
}

// syncDir flushes the directory entry, so the rename itself survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("failed to open the state directory %s: %w", dir, err)
	}

	defer d.Close()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("failed to sync the state directory %s: %w", dir, err)
	}

	return nil
}
