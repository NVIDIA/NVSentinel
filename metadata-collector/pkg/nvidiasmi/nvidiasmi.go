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

// Package nvidiasmi runs nvidia-smi without letting a wedged GPU driver block the caller.
package nvidiasmi

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type result struct {
	output []byte
	err    error
}

// Run runs nvidia-smi with args and returns its combined output. ctx should carry the timeout.
//
// exec.Cmd cannot bound this on its own: Wait returns only once the process exits, and a
// process in uninterruptible sleep on a wedged driver does not act on the kill. So Run returns
// as soon as ctx ends, whether or not the process has exited, leaving a stuck process and the
// goroutine waiting on it behind.
func Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nvidia-smi", args...)

	// Buffered so the goroutine can still send, and exit, if Run has already returned.
	done := make(chan result, 1)

	go func() {
		output, err := cmd.CombinedOutput()
		done <- result{output: output, err: err}
	}()

	select {
	case r := <-done:
		return r.output, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("nvidia-smi %s: %w", strings.Join(args, " "), ctx.Err())
	}
}
