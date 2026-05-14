// Copyright 2026 k8s-gpu-mcp-server contributors
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

package mcp

import (
	"strings"
	"testing"

	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
)

// TestNew_RejectsEmptyHTTPAddr verifies that New refuses to construct a Server
// when no HTTP listen address is supplied. The validation exists because a
// Server without an addr would silently bind to ":0" inside the mcp-go
// transport, which is never what the operator wanted in production.
func TestNew_RejectsEmptyHTTPAddr(t *testing.T) {
	_, err := New(Config{
		HTTPAddr: "",
		Store:    store.NewFakeReader(),
	})
	if err == nil {
		t.Fatal("New should reject empty HTTPAddr, got nil error")
	}

	if !strings.Contains(err.Error(), "HTTPAddr") {
		t.Errorf("error should mention HTTPAddr; got %q", err.Error())
	}
}

// TestNew_RejectsNilStore verifies that New refuses to construct a Server with
// a nil Store. Tools read exclusively through the Store interface, so a nil
// here would panic on the first tool invocation; the up-front check is what
// turns that into a process-start error instead.
func TestNew_RejectsNilStore(t *testing.T) {
	_, err := New(Config{
		HTTPAddr: ":18080",
		Store:    nil,
	})
	if err == nil {
		t.Fatal("New should reject nil Store, got nil error")
	}

	if !strings.Contains(err.Error(), "Store") {
		t.Errorf("error should mention Store; got %q", err.Error())
	}
}

// TestNew_ReturnsServerForValidConfig verifies the happy path: a Config with
// the required fields populated yields a non-nil *Server. This exercises the
// mcp-go server construction path and the registerTools hook, so a regression
// in either of those would surface here as a non-nil error.
func TestNew_ReturnsServerForValidConfig(t *testing.T) {
	srv, err := New(Config{
		Version:  "test",
		HTTPAddr: ":18080",
		Store:    store.NewFakeReader(),
	})
	if err != nil {
		t.Fatalf("unexpected error for valid config: %v", err)
	}

	if srv == nil {
		t.Fatal("New returned nil *Server with no error")
	}
}
