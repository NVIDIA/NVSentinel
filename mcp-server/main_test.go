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

package main

import (
	"strings"
	"testing"
)

func TestCreateMetricsServer_InvalidPortReturnsError(t *testing.T) {
	_, err := CreateMetricsServer("not-a-port")
	if err == nil {
		t.Fatal("expected error for non-numeric port, got nil")
	}

	if !strings.Contains(err.Error(), "invalid metrics port") {
		t.Errorf("error message should mention the failing port; got %q", err.Error())
	}
}

func TestCreateMetricsServer_ValidPortReturnsServer(t *testing.T) {
	srv, err := CreateMetricsServer("9090")
	if err != nil {
		t.Fatalf("unexpected error for valid port: %v", err)
	}

	if srv == nil {
		t.Fatal("CreateMetricsServer returned nil server with no error")
	}
}
