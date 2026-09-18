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

package analyzer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The sticky window is not configured anywhere: it is read back out of the rule's
// own pipeline text by ParseXidConfigFromPipeline, which is what lets the Go
// detector mirror the MongoDB rule on PostgreSQL. That makes the rule text and
// this parser an interface, so a change to the sticky stages can silently leave
// the detector on its defaults. These cases pin it.
//
// The stages below are copied from the shipped rules. The 30 second case is the
// decisive one: 3 hours is the default, so a parse that found nothing would still
// report 3 hours and look correct.

const stickyStageBound30 = `{
  "$addFields": {
    "stickyXidWithin3Hours": {
      "$cond": {
        "if": "$isStickyXid",
        "then": {
          "$and": [
            {"$ne": ["$lastStickyTimestamp", null]},
            {"$lte": [{"$subtract": ["$healthevent.generatedtimestamp.seconds", "$lastStickyTimestamp"]}, 30]}
          ]
        },
        "else": false
      }
    }
  }
}`

const stickyStageBound10800 = `{
  "$addFields": {
    "stickyXidWithin3Hours": {
      "$cond": {
        "if": "$isStickyXid",
        "then": {
          "$and": [
            {"$ne": ["$lastStickyTimestamp", null]},
            {"$lte": [{"$subtract": ["$healthevent.generatedtimestamp.seconds", "$lastStickyTimestamp"]}, 10800]}
          ]
        },
        "else": false
      }
    }
  }
}`

// lastStickyTimestampStage is the window stage that replaced the $push of $$ROOT.
// It must not disturb burst window parsing, which keys on the burstId output.
const lastStickyTimestampStage = `{
  "$setWindowFields": {
    "sortBy": {"healthevent.generatedtimestamp.seconds": 1},
    "output": {
      "lastStickyTimestamp": {
        "$max": {
          "$cond": {
            "if": "$isStickyXid",
            "then": "$healthevent.generatedtimestamp.seconds",
            "else": null
          }
        },
        "window": {"documents": ["unbounded", -1]}
      }
    }
  }
}`

const burstIDStage = `{
  "$setWindowFields": {
    "sortBy": {"healthevent.generatedtimestamp.seconds": 1},
    "output": {
      "burstId": {
        "$sum": {
          "$cond": {
            "if": {"$eq": ["$prevTimestamp", null]},
            "then": 1,
            "else": {
              "$cond": {
                "if": {"$gt": [{"$subtract": ["$healthevent.generatedtimestamp.seconds", "$prevTimestamp"]}, 180]},
                "then": 1,
                "else": 0
              }
            }
          }
        },
        "window": {"documents": ["unbounded", "current"]}
      }
    }
  }
}`

func TestParseXidConfigFromPipeline_StickyWindowComesFromTheScalarStage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage string
		want  time.Duration
	}{
		{"development bound overrides the default", stickyStageBound30, 30 * time.Second},
		{"production bound", stickyStageBound10800, 3 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ParseXidConfigFromPipeline([]string{lastStickyTimestampStage, tc.stage})
			assert.Equal(t, tc.want, cfg.StickyWindow)
		})
	}
}

func TestParseXidConfigFromPipeline_LastStickyTimestampLeavesBurstWindowAlone(t *testing.T) {
	// The replaced stage is a $setWindowFields, same stage type the burst window is
	// read from, so confirm the two do not interfere in either order.
	cfg := ParseXidConfigFromPipeline([]string{lastStickyTimestampStage, burstIDStage, stickyStageBound30})
	assert.Equal(t, 3*time.Minute, cfg.BurstWindow)
	assert.Equal(t, 30*time.Second, cfg.StickyWindow)

	reordered := ParseXidConfigFromPipeline([]string{burstIDStage, lastStickyTimestampStage, stickyStageBound30})
	assert.Equal(t, cfg, reordered)
}
