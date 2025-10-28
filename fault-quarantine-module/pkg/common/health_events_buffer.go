// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package common

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"

	"go.mongodb.org/mongo-driver/bson"
)

// healthEventInfo represents information about a health event
type HealthEventInfo struct {
	HealthEventWithStatus *model.HealthEventWithStatus
	EventBson             bson.M
	HasProcessed          bool
}

// HealthEventBuffer represents a buffer of health events using an array
type HealthEventBuffer struct {
	events []HealthEventInfo
	ctx    context.Context
}

// NewHealthEventBuffer creates a new health event buffer.
func NewHealthEventBuffer(ctx context.Context) *HealthEventBuffer {
	return &HealthEventBuffer{
		events: make([]HealthEventInfo, 0),
		ctx:    ctx,
	}
}

// Add adds a health event at the end of the buffer.
func (b *HealthEventBuffer) Add(event *model.HealthEventWithStatus, eventBson bson.M) {
	b.events = append(b.events, HealthEventInfo{
		HealthEventWithStatus: event,
		EventBson:             eventBson,
		HasProcessed:          false,
	})
}

// RemoveAt removes an element at the specified index.
func (b *HealthEventBuffer) RemoveAt(index int) error {
	if index < 0 || index >= len(b.events) {
		return fmt.Errorf("index out of bounds: %d", index)
	}

	slog.Debug("Removing event at index",
		"index", index,
		"event", b.events[index].HealthEventWithStatus,
	)

	// Remove the element at index
	b.events = append(b.events[:index], b.events[index+1:]...)

	return nil
}

// Length returns the current number of elements in the buffer.
func (b *HealthEventBuffer) Length() int {
	return len(b.events)
}

// Get returns the element at the specified index without removing it.
func (b *HealthEventBuffer) Get(index int) (*HealthEventInfo, error) {
	if index < 0 || index >= len(b.events) {
		return nil, fmt.Errorf("index out of bounds or buffer is empty") // Index out of bounds or buffer is empty
	}

	return &b.events[index], nil
}
