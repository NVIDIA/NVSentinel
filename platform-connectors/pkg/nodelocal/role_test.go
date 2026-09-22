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

package nodelocal

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNew_RequiresTheSocket(t *testing.T) {
	_, err := New(Options{})
	require.ErrorContains(t, err, "-socket is required")

	_, err = New(Options{Socket: "/var/run/nvsentinel.sock"})
	require.NoError(t, err)
}

// TestListen_ReplacesAStaleSocketAndOpensItToEveryPublisher: a file left by
// a previous run does not block the bind, and the socket is world-writable
// for the non-root publishers.
func TestListen_ReplacesAStaleSocketAndOpensItToEveryPublisher(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "pc.sock")
	require.NoError(t, os.WriteFile(socket, []byte("stale"), 0o600))

	lis, err := listen(context.Background(), socket)
	require.NoError(t, err)

	t.Cleanup(func() { _ = lis.Close() })

	info, err := os.Stat(socket)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o666), info.Mode().Perm())
	require.Equal(t, os.ModeSocket, info.Mode().Type())
}
