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

package statefile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoundTrip(t *testing.T) {
	f := File{Path: filepath.Join(t.TempDir(), "sub", "state.json")}

	want := State{UnreachableRaised: true, CollectionRaised: []string{"xid", "gpu"}, XidLastTo: 1700000000}
	require.NoError(t, f.Save(want))

	got, err := f.Load()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestMissingFileIsEmptyState(t *testing.T) {
	got, err := File{Path: filepath.Join(t.TempDir(), "none.json")}.Load()
	require.NoError(t, err)
	assert.Equal(t, State{}, got)
}

func TestCorruptFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, err := File{Path: path}.Load()
	assert.Error(t, err)
}

func TestEmptyPathDisablesTheFile(t *testing.T) {
	f := File{}
	require.NoError(t, f.Save(State{UnreachableRaised: true}))

	got, err := f.Load()
	require.NoError(t, err)
	assert.Equal(t, State{}, got)
}

func TestFailedSaveKeepsTheOldStateAndLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	f := File{Path: filepath.Join(dir, "state.json")}

	want := State{UnreachableRaised: true, CollectionRaised: []string{"xid"}}
	require.NoError(t, f.Save(want))

	// A directory in the way makes the final rename fail.
	blocked := File{Path: filepath.Join(dir, "blocked")}
	require.NoError(t, os.Mkdir(blocked.Path, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(blocked.Path, "keep"), nil, 0o600))
	require.Error(t, blocked.Save(State{}))

	got, err := f.Load()
	require.NoError(t, err)
	assert.Equal(t, want, got, "the existing state file is untouched")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}

	assert.ElementsMatch(t, []string{"state.json", "blocked"}, names, "no temporary file is left behind")
}
