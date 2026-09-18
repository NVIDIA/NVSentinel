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

// Package connectors defines how the platform connector server hands a batch
// of health events to the connectors that act on it, and how connectors
// compose.
//
// A Connector receives one batch. In the node-local DaemonSet every connector
// drains its own ring buffer, so the buffer is the Connector the server
// sees: it accepts the batch at once and the connector processes it later
// with its own retries. The deployment platform connector has no queue: its
// connectors process the batch inside the request, and the reply follows
// their result.
//
// Set hands one batch to several connectors and reports every failure.
package connectors

import (
	"context"
	"errors"
	"sync"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// Connector receives one batch of health events. A nil error means the
// connector is done with the batch as far as the caller is concerned: queued,
// stored, forwarded or counted, as the connector defines it.
type Connector interface {
	ProcessBatch(ctx context.Context, he *pb.HealthEvents) error
}

// Set hands one batch to every member at the same time and waits for all of
// them. Every member runs to completion on the caller's context: each one's
// work is idempotent and stands on its own, so a resend of the batch finds it
// done. The result joins every failure, so the log line names each member
// that failed.
type Set []Connector

// ProcessBatch implements Connector.
func (s Set) ProcessBatch(ctx context.Context, he *pb.HealthEvents) error {
	errs := make([]error, len(s))

	var wg sync.WaitGroup

	for i, c := range s {
		wg.Go(func() {
			errs[i] = c.ProcessBatch(ctx, he)
		})
	}

	wg.Wait()

	return errors.Join(errs...)
}
