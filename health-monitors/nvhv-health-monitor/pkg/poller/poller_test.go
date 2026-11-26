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

package poller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdict"
)

type fakeEvaluator struct {
	mu      sync.Mutex
	calls   int
	results []error // error to return per call; nil means success
}

func (f *fakeEvaluator) Evaluate(ctx context.Context) (*verdict.Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	i := f.calls
	f.calls++

	if i < len(f.results) && f.results[i] != nil {
		return nil, f.results[i]
	}

	return &verdict.Verdict{}, nil
}

func TestPollerReportsEachResult(t *testing.T) {
	boom := errors.New("boom")
	ev := &fakeEvaluator{results: []error{nil, boom, nil}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []error

	p := &Poller{
		Evaluator: ev,
		Interval:  time.Millisecond,
		Timeout:   time.Second,
		Handle: func(_ context.Context, v *verdict.Verdict, err error) {
			if err == nil {
				assert.NotNil(t, v)
			} else {
				assert.Nil(t, v)
			}

			got = append(got, err)
			if len(got) == 3 {
				cancel()
			}
		},
	}

	p.Run(ctx)

	require.Len(t, got, 3)
	assert.NoError(t, got[0])
	assert.ErrorIs(t, got[1], boom)
	assert.NoError(t, got[2])
}

func TestPollerStopsWithoutReportingOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	p := &Poller{
		Evaluator: &fakeEvaluator{results: []error{context.Canceled}},
		Interval:  time.Hour,
		Timeout:   time.Second,
		Handle:    func(context.Context, *verdict.Verdict, error) { called = true },
	}

	p.Run(ctx)

	assert.False(t, called)
}
