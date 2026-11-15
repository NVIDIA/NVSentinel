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
// limitations under the License.
package syslogmonitor

import (
	"context"
	"testing"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/stretchr/testify/require"
)

// TestStubJournal_LineBeforeFirstPollIsHandled verifies that the boot-start
// scan handles a line that is in the stub journal before the first poll.
// The end-to-end tests inject lines right after the monitor restarts.
func TestStubJournal_LineBeforeFirstPollIsHandled(t *testing.T) {
	journal = []string{"NVRM: Xid (PCI:0000:38:00): 79, GPU has fallen off the bus"}
	t.Cleanup(func() { journal = make([]string, 0) })

	check := CheckDefinition{
		Name:        "bootCheck",
		JournalPath: TEST_JOURNAL_PATH,
	}

	sm, err := NewSyslogMonitorWithFactory(
		TEST_NODE,
		[]CheckDefinition{check},
		&mockPlatformConnectorClient{},
		TEST_AGENT,
		TEST_COMPONENT,
		"60s",
		t.TempDir()+"/state.json",
		&StubJournalFactory{bootID: "stub-boot"},
		"http://localhost:8080",
		"/tmp/metadata.json",
		pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		"", "",
		nil,
		"tcp://test",
		30*time.Minute,
	)
	require.NoError(t, err)

	// The stub image starts with no state file. Apply that boot ID change
	// by hand, because the kernel boot ID is empty off Linux.
	sm.currentBootID = "boot-2"
	sm.handleBootIDChange("", "boot-2")

	recorder := &recordingHandler{}
	sm.checkToHandlerMap[check.Name] = recorder

	require.NoError(t, sm.Run(context.Background()))

	require.Len(t, recorder.messages, 1, "the line injected before the first poll must be handled")
}
