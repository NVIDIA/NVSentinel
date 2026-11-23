//go:build !systemd

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
package syslogmonitor

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStubJournal_InjectedLineHasNoBootID verifies that the stub journal
// returns no boot ID for an injected line. The boot-start scan would skip a
// line whose boot ID differs from the current one, and the end-to-end tests
// inject lines right after the monitor restarts.
func TestStubJournal_InjectedLineHasNoBootID(t *testing.T) {
	journal = []string{"NVRM: Xid (PCI:0000:38:00): 79, GPU has fallen off the bus"}
	t.Cleanup(func() { journal = make([]string, 0) })

	j := &StubJournal{currentPosition: 0}

	bootID, err := j.GetData(FieldBootID)
	require.NoError(t, err)
	require.Empty(t, bootID)

	message, err := j.GetData(FieldMessage)
	require.NoError(t, err)
	require.Equal(t, journal[0], message)
}
