// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package nvgrpc

import (
	"testing"

	"github.com/go-logr/logr"
)

func TestClientConnFor(t *testing.T) {
	t.Run("Config defaulting does not mutate original", func(t *testing.T) {
		cfg := &Config{}
		conn, err := ClientConnFor(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer conn.Close()

		if cfg.Target != "" {
			t.Errorf("Original config was mutated! Target is %q", cfg.Target)
		}
	})

	t.Run("Config.Default() works correctly", func(t *testing.T) {
		cfg := &Config{}
		cfg.Default()

		if cfg.Target == "" {
			t.Error("Target was not defaulted")
		}
		if cfg.UserAgent == "" {
			t.Error("UserAgent was not defaulted")
		}
	})

	t.Run("Client creation respects WithLogger option", func(t *testing.T) {
		cfg := &Config{Target: "unix:///tmp/test.sock"}
		conn, err := ClientConnFor(cfg, WithLogger(logr.Discard()))
		if err != nil {
			t.Fatalf("failed to create client: %v", err)
		}
		conn.Close()
	})
}
