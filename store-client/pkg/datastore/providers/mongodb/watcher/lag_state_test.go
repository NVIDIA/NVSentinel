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

package watcher

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// clusterTimeOf asserts on a concrete BSON type, so this pins the type the driver actually
// decodes a change event's clusterTime into. If that changes, the assertion would start failing
// silently and every consumer would report an unknown lag.
func TestClusterTimeOf_BSONTimestamp_ReturnsServerTime(t *testing.T) {
	raw, err := bson.Marshal(bson.M{"clusterTime": bson.Timestamp{T: 1788779000, I: 3}})
	require.NoError(t, err)

	var event bson.M
	require.NoError(t, bson.Unmarshal(raw, &event))

	require.IsType(t, bson.Timestamp{}, event["clusterTime"],
		"clusterTimeOf asserts on this type")

	assert.Equal(t, time.Unix(1788779000, 0).UTC(), clusterTimeOf(event))
}

// An event with no usable clusterTime yields the zero time, which the tracker ignores rather
// than recording as an observation.
func TestClusterTimeOf_MissingOrWrongType_ReturnsZero(t *testing.T) {
	tests := map[string]bson.M{
		"missing":    {"operationType": "insert"},
		"wrong type": {"clusterTime": "1788779000"},
		"nil":        {"clusterTime": nil},
	}

	for name, event := range tests {
		t.Run(name, func(t *testing.T) {
			assert.True(t, clusterTimeOf(event).IsZero())
		})
	}
}

// The closed-cursor case is the one TryNext cannot express: it returns false with no error, the
// same as an empty batch. Reading it as an empty batch records "caught up" on every tick against
// a dead stream, which is the spin the existing Next loop has today.
func TestClassifyStreamStep_EachStreamState_ReturnsExpectedStep(t *testing.T) {
	streamErr := errors.New("connection reset")

	tests := map[string]struct {
		hasNext      bool
		csErr        error
		cursorID     int64
		expectedStep streamStep
		expectedErr  error
	}{
		"event available": {
			hasNext:      true,
			cursorID:     42,
			expectedStep: stepEvent,
		},
		"empty batch on a live cursor": {
			cursorID:     42,
			expectedStep: stepCaughtUp,
		},
		"stream error": {
			csErr:        streamErr,
			cursorID:     42,
			expectedStep: stepFailed,
			expectedErr:  streamErr,
		},
		"cursor closed by the server": {
			cursorID:     0,
			expectedStep: stepFailed,
			expectedErr:  errChangeStreamClosed,
		},
		"error takes precedence over a closed cursor": {
			csErr:        streamErr,
			cursorID:     0,
			expectedStep: stepFailed,
			expectedErr:  streamErr,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			step, err := classifyStreamStep(test.hasNext, test.csErr, test.cursorID)

			assert.Equal(t, test.expectedStep, step)

			if test.expectedErr == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, test.expectedErr)
			}
		})
	}
}

// A watcher that has not read anything must report neither timestamp, so callers treat its lag
// as unknown rather than as zero.
func TestLagState_BeforeFirstRead_ReportsUnknown(t *testing.T) {
	watcher := &ChangeStreamWatcher{}

	lastEmptyBatch, lastEventRead := watcher.LagState()

	assert.True(t, lastEmptyBatch.IsZero())
	assert.True(t, lastEventRead.IsZero())
}

func TestLagState_AfterLoopRecords_ReportsBothTimestamps(t *testing.T) {
	watcher := &ChangeStreamWatcher{}

	caughtUp := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	eventRead := caughtUp.Add(-time.Minute)

	watcher.lag.RecordCaughtUp(caughtUp)
	watcher.lag.RecordEventRead(eventRead)

	lastEmptyBatch, lastEventRead := watcher.LagState()

	assert.Equal(t, caughtUp, lastEmptyBatch)
	assert.Equal(t, eventRead, lastEventRead)
}
