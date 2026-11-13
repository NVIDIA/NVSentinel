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
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// ErrAbandoned means nvidia-smi was still running after its context ended and the kill, and
// Run stopped waiting for it.
var ErrAbandoned = errors.New("nvidia-smi did not exit after its timeout")

// exitGrace is how long Run keeps waiting once ctx has ended and the process has been killed.
// A var so tests can shorten it.
var exitGrace = 10 * time.Second

type result struct {
	output []byte
	err    error
}

// Run runs nvidia-smi with args and returns its combined output. ctx should carry the timeout.
//
// exec.Cmd cannot bound this on its own: Wait returns only once the process exits, and a
// process in uninterruptible sleep on a wedged driver does not act on the kill. So once ctx has
// ended, Run waits exitGrace and then returns ErrAbandoned, leaving the process and the
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
	}

	select {
	case r := <-done:
		return r.output, r.err
	case <-time.After(exitGrace):
		return nil, fmt.Errorf("%w (waited %s after %w)", ErrAbandoned, exitGrace, ctx.Err())
	}
}
