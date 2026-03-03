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

package helpers

import (
	"context"
	"testing"
	"time"
)

func TestHandleOutOfSequenceLabel_SkipToFinalNoRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := &labelWatcherState{
		labelValueSequence:  []string{"a", "b", "c"},
		nodeName:            "test-node",
		waitForLabelRemoval: false,
		currentLabelIndex:   1,
		prevLabelValue:      "a",
	}

	success := make(chan bool, 1)

	state.handleOutOfSequenceLabel(t, ctx, "c", success)

	select {
	case result := <-success:
		if !result {
			t.Fatal("expected SUCCESS (true), got FAILURE (false)")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for result — success was never sent")
	}

	if state.currentLabelIndex != 3 {
		t.Errorf("expected currentLabelIndex=3, got %d", state.currentLabelIndex)
	}

	if state.prevLabelValue != "c" {
		t.Errorf("expected prevLabelValue='c', got '%s'", state.prevLabelValue)
	}

	if !state.resultSent {
		t.Error("expected resultSent=true")
	}
}

func TestHandleOutOfSequenceLabel_SkipToFinalWithRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	state := &labelWatcherState{
		labelValueSequence:  []string{"a", "b", "c"},
		nodeName:            "test-node",
		waitForLabelRemoval: true,
		currentLabelIndex:   1,
		prevLabelValue:      "a",
	}

	success := make(chan bool, 1)

	state.handleOutOfSequenceLabel(t, ctx, "c", success)

	select {
	case <-success:
		t.Fatal("expected no result sent when waitForLabelRemoval=true, but got one")
	case <-time.After(100 * time.Millisecond):
	}

	if state.currentLabelIndex != 3 {
		t.Errorf("expected currentLabelIndex=3, got %d", state.currentLabelIndex)
	}

	if state.prevLabelValue != "c" {
		t.Errorf("expected prevLabelValue='c', got '%s'", state.prevLabelValue)
	}

	if state.resultSent {
		t.Error("expected resultSent=false when waiting for removal")
	}
}

func TestHandleMatchedLabel_FinalNoRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := &labelWatcherState{
		labelValueSequence:  []string{"a", "b"},
		nodeName:            "test-node",
		waitForLabelRemoval: false,
		currentLabelIndex:   1,
		prevLabelValue:      "a",
	}

	success := make(chan bool, 1)

	state.handleMatchedLabel(t, ctx, "b", success)

	select {
	case result := <-success:
		if !result {
			t.Fatal("expected SUCCESS (true), got FAILURE (false)")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for result — success was never sent")
	}

	if state.currentLabelIndex != 2 {
		t.Errorf("expected currentLabelIndex=2, got %d", state.currentLabelIndex)
	}

	if state.prevLabelValue != "b" {
		t.Errorf("expected prevLabelValue='b', got '%s'", state.prevLabelValue)
	}

	if !state.resultSent {
		t.Error("expected resultSent=true")
	}
}

func TestHandleOutOfSequenceLabel_MissedEarlyLabels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := &labelWatcherState{
		labelValueSequence:  []string{"a", "b", "c"},
		nodeName:            "test-node",
		waitForLabelRemoval: false,
		currentLabelIndex:   0,
		prevLabelValue:      "",
	}

	success := make(chan bool, 1)

	state.handleOutOfSequenceLabel(t, ctx, "unknown-value", success)

	select {
	case result := <-success:
		if result {
			t.Fatal("expected FAILURE (false), got SUCCESS (true)")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for result — failure was never sent")
	}

	if state.currentLabelIndex != 0 {
		t.Errorf("expected currentLabelIndex=0 (unchanged), got %d", state.currentLabelIndex)
	}

	if !state.resultSent {
		t.Error("expected resultSent=true")
	}
}

func TestHandleOutOfSequenceLabel_UnexpectedLabel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := &labelWatcherState{
		labelValueSequence:  []string{"a", "b", "c"},
		nodeName:            "test-node",
		waitForLabelRemoval: false,
		currentLabelIndex:   1,
		prevLabelValue:      "a",
	}

	success := make(chan bool, 1)

	state.handleOutOfSequenceLabel(t, ctx, "unknown-value", success)

	select {
	case result := <-success:
		if result {
			t.Fatal("expected FAILURE (false), got SUCCESS (true)")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for result — failure was never sent")
	}

	if state.currentLabelIndex != 1 {
		t.Errorf("expected currentLabelIndex=1 (unchanged), got %d", state.currentLabelIndex)
	}

	if !state.resultSent {
		t.Error("expected resultSent=true")
	}
}

func TestHandleLabelAbsent_PrematureRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := &labelWatcherState{
		labelValueSequence:  []string{"a", "b", "c"},
		nodeName:            "test-node",
		waitForLabelRemoval: true,
		currentLabelIndex:   1,
		prevLabelValue:      "a",
	}

	success := make(chan bool, 1)

	state.handleLabelAbsent(t, ctx, success)

	select {
	case result := <-success:
		if result {
			t.Fatal("expected FAILURE (false) for premature removal, got SUCCESS (true)")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for result — failure was never sent")
	}

	if !state.resultSent {
		t.Error("expected resultSent=true")
	}
}

func TestHandleLabelPresent_DuplicateUpdateIgnored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	state := &labelWatcherState{
		labelValueSequence:  []string{"a", "b"},
		nodeName:            "test-node",
		waitForLabelRemoval: false,
		currentLabelIndex:   1,
		prevLabelValue:      "a",
	}

	success := make(chan bool, 1)

	state.handleLabelPresent(t, ctx, "a", success)

	select {
	case <-success:
		t.Fatal("expected no result sent for duplicate update, but got one")
	case <-time.After(100 * time.Millisecond):
	}

	if state.currentLabelIndex != 1 {
		t.Errorf("expected currentLabelIndex=1 (unchanged), got %d", state.currentLabelIndex)
	}

	if state.prevLabelValue != "a" {
		t.Errorf("expected prevLabelValue='a' (unchanged), got '%s'", state.prevLabelValue)
	}

	if state.resultSent {
		t.Error("expected resultSent=false")
	}
}

func TestHandleLabelAbsent_SuccessfulRemoval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := &labelWatcherState{
		labelValueSequence:  []string{"a", "b"},
		nodeName:            "test-node",
		waitForLabelRemoval: true,
		currentLabelIndex:   2,
		prevLabelValue:      "b",
	}

	success := make(chan bool, 1)

	state.handleLabelAbsent(t, ctx, success)

	select {
	case result := <-success:
		if !result {
			t.Fatal("expected SUCCESS (true) for completed removal, got FAILURE (false)")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for result — success was never sent")
	}

	if !state.resultSent {
		t.Error("expected resultSent=true")
	}
}

func TestResultSentGuard_PreventsDuplicateSend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := &labelWatcherState{
		labelValueSequence:  []string{"a"},
		nodeName:            "test-node",
		waitForLabelRemoval: false,
		currentLabelIndex:   0,
		prevLabelValue:      "",
	}

	success := make(chan bool, 2)

	state.handleMatchedLabel(t, ctx, "a", success)
	state.handleLabelAbsent(t, ctx, success)

	if len(success) != 1 {
		t.Errorf("expected exactly 1 result on channel, got %d", len(success))
	}

	if !state.resultSent {
		t.Error("expected resultSent=true")
	}
}
