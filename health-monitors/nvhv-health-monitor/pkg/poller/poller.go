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

// Package poller calls NVHV's verdict API on a fixed interval.
package poller

import (
	"context"
	"log/slog"
	"time"

	healthv1 "github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdictpb/health/v1"
)

// Evaluator returns one verdict for the node. GRPCEvaluator is the production
// implementation.
type Evaluator interface {
	Evaluate(ctx context.Context) (*healthv1.NodeVerdict, error)
}

// Handler receives the result of one poll: exactly one of verdict and err is
// set.
type Handler func(ctx context.Context, verdict *healthv1.NodeVerdict, err error)

// Poller calls Evaluate every Interval and hands the result to Handle.
type Poller struct {
	Evaluator Evaluator
	Handle    Handler
	Interval  time.Duration
	// Timeout bounds one Evaluate call. It should not exceed Interval, so a
	// call never outlives the next poll.
	Timeout time.Duration
}

// Run polls immediately and then every Interval until ctx is done.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.Interval)
	defer ticker.Stop()

	for {
		p.pollOnce(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	callCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	verdict, err := p.Evaluator.Evaluate(callCtx)
	if ctx.Err() != nil {
		return // shutting down; not an NVHV failure
	}

	if err != nil {
		slog.Warn("NVHV Evaluate failed", "error", err)
	}

	p.Handle(ctx, verdict, err)
}
