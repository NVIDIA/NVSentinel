// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package nvidiasmi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeNvidiaSmi puts an nvidia-smi on PATH that runs script.
func fakeNvidiaSmi(t *testing.T, script string) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte("#!/bin/sh\n"+script+"\n"), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRun_ReturnsOutput(t *testing.T) {
	fakeNvidiaSmi(t, `echo "$@"`)

	output, err := Run(context.Background(), "topo", "-m")

	require.NoError(t, err)
	assert.Equal(t, "topo -m\n", string(output))
}

// A process that exits when killed is a plain timeout, not an abandonment, so it must not
// trigger the carry-over of previous metadata.
func TestRun_KilledProcessIsNotAbandoned(t *testing.T) {
	fakeNvidiaSmi(t, "exec sleep 30")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	_, err := Run(ctx)

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrAbandoned)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// The background sleep keeps the output pipe open after the kill, so CombinedOutput does not
// return. That is the same wait Run sees on a process stuck in uninterruptible sleep, which a
// test cannot create.
func TestRun_AbandonsProcessThatOutlivesTimeout(t *testing.T) {
	defaultGrace := exitGrace
	exitGrace = 100 * time.Millisecond

	t.Cleanup(func() { exitGrace = defaultGrace })

	fakeNvidiaSmi(t, "sleep 30 &\nsleep 30")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	_, err := Run(ctx)

	require.ErrorIs(t, err, ErrAbandoned)
	assert.Less(t, time.Since(start), 5*time.Second)
}
