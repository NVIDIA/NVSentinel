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

package client

import (
	"bytes"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLowWaterMarkTracker_Sequential(t *testing.T) {
	tracker := NewLowWaterMarkTracker()

	seq1 := tracker.Register([]byte("token-1"))
	seq2 := tracker.Register([]byte("token-2"))
	seq3 := tracker.Register([]byte("token-3"))

	require.Equal(t, uint64(1), seq1)
	require.Equal(t, uint64(2), seq2)
	require.Equal(t, uint64(3), seq3)
	require.Equal(t, 3, tracker.InFlightCount())

	adv1 := tracker.MarkDone(seq1)
	assert.Equal(t, []byte("token-1"), adv1)
	assert.Equal(t, 2, tracker.InFlightCount())

	adv2 := tracker.MarkDone(seq2)
	assert.Equal(t, []byte("token-2"), adv2)
	assert.Equal(t, 1, tracker.InFlightCount())

	adv3 := tracker.MarkDone(seq3)
	assert.Equal(t, []byte("token-3"), adv3)
	assert.Equal(t, 0, tracker.InFlightCount())
	assert.Equal(t, []byte("token-3"), tracker.LastCheckpointedToken())
}

func TestLowWaterMarkTracker_OutOfOrder(t *testing.T) {
	tracker := NewLowWaterMarkTracker()

	seq1 := tracker.Register([]byte("token-1"))
	seq2 := tracker.Register([]byte("token-2"))
	seq3 := tracker.Register([]byte("token-3"))

	// Complete seq3 first: cannot advance because seq1 and seq2 are pending
	adv := tracker.MarkDone(seq3)
	assert.Nil(t, adv)
	assert.Equal(t, 3, tracker.InFlightCount())

	// Complete seq2: still cannot advance because seq1 is pending
	adv = tracker.MarkDone(seq2)
	assert.Nil(t, adv)
	assert.Equal(t, 3, tracker.InFlightCount())

	// Complete seq1: now seq1, seq2, seq3 are all complete -> advances to token-3!
	adv = tracker.MarkDone(seq1)
	assert.Equal(t, []byte("token-3"), adv)
	assert.Equal(t, 0, tracker.InFlightCount())
	assert.Equal(t, []byte("token-3"), tracker.LastCheckpointedToken())
}

func TestLowWaterMarkTracker_Gaps(t *testing.T) {
	tracker := NewLowWaterMarkTracker()

	seq1 := tracker.Register([]byte("token-1"))
	seq2 := tracker.Register([]byte("token-2"))
	seq3 := tracker.Register([]byte("token-3"))
	seq4 := tracker.Register([]byte("token-4"))

	// Complete seq1 -> advances to token-1
	adv := tracker.MarkDone(seq1)
	assert.Equal(t, []byte("token-1"), adv)
	assert.Equal(t, 3, tracker.InFlightCount())

	// Complete seq3 -> gap at seq2, cannot advance
	adv = tracker.MarkDone(seq3)
	assert.Nil(t, adv)
	assert.Equal(t, 3, tracker.InFlightCount())

	// Complete seq2 -> fills gap, advances past seq2 and seq3 to token-3
	adv = tracker.MarkDone(seq2)
	assert.Equal(t, []byte("token-3"), adv)
	assert.Equal(t, 1, tracker.InFlightCount())

	// Complete seq4 -> advances to token-4
	adv = tracker.MarkDone(seq4)
	assert.Equal(t, []byte("token-4"), adv)
	assert.Equal(t, 0, tracker.InFlightCount())
}

func TestLowWaterMarkTracker_UnknownSequence(t *testing.T) {
	tracker := NewLowWaterMarkTracker()

	adv := tracker.MarkDone(999)
	assert.Nil(t, adv)
	assert.Equal(t, 0, tracker.InFlightCount())
}

func TestLowWaterMarkTracker_ConcurrentStress(t *testing.T) {
	tracker := NewLowWaterMarkTracker()
	const count = 500

	seqs := make([]uint64, count)
	for i := range count {
		token := []byte(fmt.Sprintf("token-%04d", i+1))
		seqs[i] = tracker.Register(token)
	}

	require.Equal(t, count, tracker.InFlightCount())

	var wg sync.WaitGroup
	wg.Add(count)

	// Complete in arbitrary concurrent order
	for i := range count {
		go func(seq uint64) {
			defer wg.Done()
			tracker.MarkDone(seq)
		}(seqs[i])
	}

	wg.Wait()

	// After all complete, InFlightCount must be 0 and LastCheckpointedToken must be token-0500
	assert.Equal(t, 0, tracker.InFlightCount())
	expectedFinalToken := []byte(fmt.Sprintf("token-%04d", count))
	assert.True(t, bytes.Equal(expectedFinalToken, tracker.LastCheckpointedToken()))
}
